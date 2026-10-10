// Package vnet gives each sandbox a private virtual network that lives in
// the thimble process. The VM's NIC is a datagram socketpair (the framework's
// file-handle attachment): every ethernet frame the guest sends arrives here,
// where a userspace TCP/IP stack (gVisor's netstack, via gvisor-tap-vsock)
// answers DHCP and DNS and terminates guest connections.
//
// Compared to the framework's NAT this buys three things:
//   - the guest has no path out except through this process, so policy
//     (allowlists, logging, secret injection) cannot be bypassed;
//   - every sandbox is on its own network with the same subnet and the same
//     guest IP, so VMs restored from one snapshot no longer collide on a
//     MAC/DHCP lease (NOTES.md, phase 6);
//   - the host knows which sandbox each connection belongs to.
package vnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

const (
	Subnet     = "192.168.127.0/24"
	GatewayIP  = "192.168.127.1"
	GuestIP    = "192.168.127.2"
	gatewayMAC = "5a:94:ef:e4:0c:dd"
)

// Net is one sandbox's network. Close tears it down.
type Net struct {
	vn     *virtualnetwork.VirtualNetwork
	cancel context.CancelFunc
	host   net.Conn
	once   sync.Once
}

// New builds the network for a guest whose NIC has the given MAC and returns
// it with the file to hand to the VM as its network attachment.
func New(guestMAC string) (*Net, *os.File, error) {
	vn, err := virtualnetwork.New(&types.Configuration{
		MTU:               1500,
		Subnet:            Subnet,
		GatewayIP:         GatewayIP,
		GatewayMacAddress: gatewayMAC,
		DHCPStaticLeases:  map[string]string{GuestIP: guestMAC},
		GatewayVirtualIPs: []string{GatewayIP},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("virtual network: %w", err)
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

	ctx, cancel := context.WithCancel(context.Background())
	n := &Net{vn: vn, cancel: cancel, host: host}
	go vn.AcceptVfkit(ctx, host)
	return n, guestEnd, nil
}

func (n *Net) Close() {
	n.once.Do(func() {
		n.cancel()
		n.host.Close()
	})
}
