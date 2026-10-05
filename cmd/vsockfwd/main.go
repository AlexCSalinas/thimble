//go:build linux

// vsockfwd runs inside the guest. It listens on an AF_VSOCK port and splices
// each connection to a TCP address, so that envd (which only knows TCP) can be
// reached from the host over vsock without changing it.
//
// Usage: vsockfwd <vsock-port> <host:port>
//
// It uses raw syscalls because the standard library's net package has no
// vsock support and syscall has no SockaddrVM; the sockaddr is 16 bytes.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

const vmaddrCIDAny = 0xFFFFFFFF

// sockaddrVM mirrors struct sockaddr_vm from <linux/vm_sockets.h>.
type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Flags     uint8
	Zero      [3]uint8
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: vsockfwd <vsock-port> <host:port>")
		os.Exit(2)
	}
	port, err := strconv.ParseUint(os.Args[1], 10, 32)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vsockfwd: bad port:", err)
		os.Exit(2)
	}
	target := os.Args[2]

	fd, err := syscall.Socket(syscall.AF_VSOCK, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		fatal("socket", err)
	}
	sa := sockaddrVM{Family: syscall.AF_VSOCK, Port: uint32(port), CID: vmaddrCIDAny}
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		fatal("bind", e)
	}
	if err := syscall.Listen(fd, 64); err != nil {
		fatal("listen", err)
	}
	fmt.Fprintf(os.Stderr, "vsockfwd: vsock:%d -> %s\n", port, target)
	for {
		var peer sockaddrVM
		plen := uint32(unsafe.Sizeof(peer))
		nfd, _, e := syscall.Syscall(syscall.SYS_ACCEPT4, uintptr(fd), uintptr(unsafe.Pointer(&peer)), uintptr(unsafe.Pointer(&plen)))
		if e != 0 {
			if e == syscall.EINTR {
				continue
			}
			fatal("accept", e)
		}
		go serve(os.NewFile(uintptr(nfd), "vsock"), target)
	}
}

func serve(c *os.File, target string) {
	defer c.Close()
	t, err := net.Dial("tcp", target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vsockfwd: dial:", err)
		return
	}
	defer t.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(t, c); t.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(c, t); done <- struct{}{} }()
	<-done
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "vsockfwd: %s: %v\n", what, err)
	os.Exit(1)
}
