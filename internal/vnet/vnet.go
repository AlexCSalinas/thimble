// Package vnet gives each sandbox a private virtual network that lives in
// the thimble process. The VM's NIC is a datagram socketpair (the framework's
// file-handle attachment): every ethernet frame the guest sends arrives here,
// where a userspace TCP/IP stack (gVisor's netstack, assembled from
// gvisor-tap-vsock's pieces) answers DHCP and DNS and terminates guest
// connections.
//
// Compared to the framework's NAT this buys three things:
//   - the guest has no path out except through this process, so egress policy
//     and logging cannot be bypassed from inside;
//   - every sandbox is on its own network with the same subnet and the same
//     guest IP, so VMs restored from one snapshot no longer collide on a
//     MAC/DHCP lease (NOTES.md, phase 6);
//   - the host knows which sandbox each connection belongs to.
//
// The stack is built here instead of calling gvisor-tap-vsock's
// virtualnetwork.New because its TCP forwarder dials out unconditionally.
// Ours asks the sandbox's Policy first.
package vnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const (
	Subnet     = "192.168.127.0/24"
	GatewayIP  = "192.168.127.1"
	GuestIP    = "192.168.127.2"
	gatewayMAC = "5a:94:ef:e4:0c:dd"
	nic        = tcpip.NICID(1)
	eventLog   = 256 // events kept per sandbox
)

// Options configures one sandbox's network.
type Options struct {
	GuestMAC string  // the NIC's MAC; DHCP pins GuestIP to it
	Policy   *Policy // nil means allow everything
	Label    string  // sandbox id, for log lines
	Logf     func(format string, a ...any)
	Verbose  bool // log allowed connections too; denies are always logged
}

// Net is one sandbox's network. Close tears it down.
type Net struct {
	opt    Options
	policy *Policy
	events *ring
	stack  *stack.Stack
	sw     *tap.Switch
	cancel context.CancelFunc
	ctx    context.Context
	host   net.Conn
	once   sync.Once

	udpMu   sync.Mutex
	udpSeen map[string]int64 // dst -> unix seconds of last logged deny
}

// New builds the network and returns it with the file to hand to the VM as
// its network attachment.
func New(opt Options) (*Net, *os.File, error) {
	if opt.Policy == nil {
		opt.Policy, _ = NewPolicy(Spec{})
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	cfg := &types.Configuration{
		MTU:               1500,
		Subnet:            Subnet,
		GatewayIP:         GatewayIP,
		GatewayMacAddress: gatewayMAC,
		DHCPStaticLeases:  map[string]string{GuestIP: opt.GuestMAC},
		GatewayVirtualIPs: []string{GatewayIP},
	}

	ipPool, err := tap.NewIPPool(cfg.Subnet)
	if err != nil {
		return nil, nil, err
	}
	if err := ipPool.Reserve(cfg.GatewayIP, cfg.GatewayMacAddress); err != nil {
		return nil, nil, err
	}
	for ip, mac := range cfg.DHCPStaticLeases {
		if err := ipPool.Reserve(ip, mac); err != nil {
			return nil, nil, err
		}
	}
	link, err := tap.NewLinkEndpoint(false, uint32(cfg.MTU), cfg.GatewayMacAddress, cfg.GatewayIP, cfg.GatewayVirtualIPs)
	if err != nil {
		return nil, nil, err
	}
	sw := tap.NewSwitch(false)
	link.Connect(sw)
	sw.Connect(link)

	s, err := newStack(cfg, link)
	if err != nil {
		return nil, nil, err
	}

	n := &Net{opt: opt, policy: opt.Policy, events: newRing(eventLog), stack: s, sw: sw, udpSeen: map[string]int64{}}
	if err := n.addServices(cfg, ipPool); err != nil {
		return nil, nil, err
	}

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, err
	}
	// The framework drops frames if the socket buffers are small; these are
	// the values Apple suggests (send 1 MiB, receive 4x that).
	for _, fd := range fds {
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 1<<20)
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
	}
	guestEnd := os.NewFile(uintptr(fds[0]), "vnet-guest")
	hostFile := os.NewFile(uintptr(fds[1]), "vnet-host")
	host, err := net.FileConn(hostFile)
	hostFile.Close()
	if err != nil {
		guestEnd.Close()
		return nil, nil, err
	}
	n.host = host
	n.ctx, n.cancel = context.WithCancel(context.Background())
	go sw.Accept(n.ctx, host, types.VfkitProtocol)
	return n, guestEnd, nil
}

// Policy is the live egress policy; replace its rules with Set.
func (n *Net) Policy() *Policy { return n.policy }

// Events returns the sandbox's recent connections, oldest first.
func (n *Net) Events() []Event { return n.events.snapshot() }

func (n *Net) Close() {
	n.once.Do(func() {
		n.cancel()
		n.host.Close()
		n.stack.Close()
	})
}

func newStack(cfg *types.Configuration, endpoint stack.LinkEndpoint) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	if err := s.CreateNIC(nic, endpoint); err != nil {
		return nil, errors.New(err.String())
	}
	if err := s.AddProtocolAddress(nic, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4()).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		return nil, errors.New(err.String())
	}
	// Accept packets for any destination: the guest talks to the whole
	// internet and the forwarders terminate all of it.
	s.SetSpoofing(nic, true)
	s.SetPromiscuousMode(nic, true)

	_, parsed, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return nil, err
	}
	subnet, err := tcpip.NewSubnet(tcpip.AddrFromSlice(parsed.IP), tcpip.MaskFromBytes(parsed.Mask))
	if err != nil {
		return nil, err
	}
	s.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: nic}})
	return s, nil
}

// addServices wires the gateway's DNS and DHCP servers and the forwarders
// that carry guest traffic to the real network.
func (n *Net) addServices(cfg *types.Configuration, ipPool *tap.IPPool) error {
	s := n.stack
	var natLock sync.Mutex
	noNAT := map[tcpip.Address]tcpip.Address{}

	tcpFwd := tcp.NewForwarder(s, 0, 128, func(r *tcp.ForwarderRequest) { go n.handleTCP(r) })
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	udpFwd := forwarder.UDP(s, noNAT, &natLock, false)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		if !n.allowUDP(id) {
			return true // consumed: dropped
		}
		return udpFwd.HandlePacket(id, pkt)
	})

	icmpFwd := forwarder.ICMP(s, noNAT, &natLock)
	s.SetTransportProtocolHandler(icmp.ProtocolNumber4, icmpFwd.HandlePacket)

	gw := tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4())
	udpConn, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: nic, Addr: gw, Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return err
	}
	tcpLn, err := gonet.ListenTCP(s, tcpip.FullAddress{NIC: nic, Addr: gw, Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		return err
	}
	dnsSrv, err := dns.New(udpConn, tcpLn, cfg.DNS)
	if err != nil {
		return err
	}
	go dnsSrv.Serve()
	go dnsSrv.ServeTCP()

	dhcpSrv, err := dhcp.New(cfg, s, ipPool)
	if err != nil {
		return fmt.Errorf("dhcp: %w", err)
	}
	go dhcpSrv.Serve()
	return nil
}
