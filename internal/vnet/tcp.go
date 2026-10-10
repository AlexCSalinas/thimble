package vnet

import (
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const dialTimeout = 30 * time.Second

func addrOf(a tcpip.Address) netip.Addr {
	ip, _ := netip.AddrFromSlice(a.AsSlice())
	return ip
}

// record stores an event and logs it: denials always, the rest if Verbose.
func (n *Net) record(e Event) {
	n.events.add(e)
	if e.Verdict == "deny" || n.opt.Verbose {
		name := ""
		if e.Name != "" {
			name = " (" + e.Name + ")"
		}
		n.opt.Logf("net %s %s %s %s%s: %s", n.opt.Label, e.Verdict, e.Proto, e.Dst, name, e.Reason)
	}
}

// handleTCP decides the fate of one guest connection attempt. When the
// destination IP alone settles it, it dials first, so a refused or
// unreachable host looks like one to the guest. When only a hostname can
// settle it, it accepts the guest's connection, reads the TLS SNI or HTTP
// Host, and only then dials.
func (n *Net) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	ip := addrOf(id.LocalAddress)
	dst := net.JoinHostPort(ip.String(), strconv.Itoa(int(id.LocalPort)))
	ev := Event{Time: time.Now(), Proto: "tcp", Dst: dst}

	v, why := n.policy.Check(ip)
	if v == Deny {
		ev.Verdict, ev.Reason = "deny", why
		n.record(ev)
		r.Complete(true) // RST: the guest sees "connection refused"
		return
	}

	var guest *gonet.TCPConn
	var out net.Conn
	var first []byte

	accept := func() bool {
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		r.Complete(false)
		if err != nil {
			return false
		}
		guest = gonet.NewTCPConn(&wq, ep)
		return true
	}

	if v == NeedName {
		if !accept() {
			return
		}
		name, _, buf := peek(guest)
		first = buf
		ev.Name = name
		v, why = n.policy.Decide(ip, name)
		if v != Allow {
			ev.Verdict, ev.Reason = "deny", why
			n.record(ev)
			guest.Close()
			return
		}
	}

	out, err := net.DialTimeout("tcp", dst, dialTimeout)
	if err != nil {
		ev.Verdict, ev.Reason = "allow", "dial failed: "+err.Error()
		n.record(ev)
		if guest != nil {
			guest.Close()
		} else {
			r.Complete(true)
		}
		return
	}
	if guest == nil && !accept() {
		out.Close()
		return
	}
	ev.Verdict, ev.Reason = "allow", why
	if len(first) > 0 {
		out.Write(first)
	}
	start := time.Now()
	up, down := pipe(guest, out)
	ev.BytesOut, ev.BytesIn = up+int64(len(first)), down
	ev.Millis = float64(time.Since(start)) / float64(time.Millisecond)
	n.record(ev)
}

// pipe copies both ways until both sides finish and reports the bytes moved
// guest->internet and internet->guest.
func pipe(guest, out net.Conn) (up, down int64) {
	var wg sync.WaitGroup
	var u, d atomic.Int64
	half := func(dst, src net.Conn, n *atomic.Int64) {
		defer wg.Done()
		c, _ := io.Copy(dst, src)
		n.Add(c)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}
	wg.Add(2)
	go half(out, guest, &u)
	go half(guest, out, &d)
	wg.Wait()
	guest.Close()
	out.Close()
	return u.Load(), d.Load()
}

// allowUDP applies the policy to a UDP packet by destination IP. DNS (port
// 53) always passes so a guest with no internet can still resolve names for
// the allowed domains. UDP has no hostname to peek at, so a domain rule
// cannot admit it: QUIC to an allowed site is dropped and the client falls
// back to TCP.
func (n *Net) allowUDP(id stack.TransportEndpointID) bool {
	if id.LocalPort == 53 {
		return true
	}
	ip := addrOf(id.LocalAddress)
	if ip.IsMulticast() || ip.IsUnspecified() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return true
	}
	if v, _ := n.policy.Check(ip); v == Allow {
		return true
	}
	dst := net.JoinHostPort(ip.String(), strconv.Itoa(int(id.LocalPort)))
	now := time.Now().Unix()
	n.udpMu.Lock()
	last := n.udpSeen[dst]
	if now-last >= 5 { // one event per destination per 5 s, not one per datagram
		n.udpSeen[dst] = now
		n.udpMu.Unlock()
		n.record(Event{Time: time.Now(), Proto: "udp", Dst: dst, Verdict: "deny", Reason: "denyOut ip"})
	} else {
		n.udpMu.Unlock()
	}
	return false
}
