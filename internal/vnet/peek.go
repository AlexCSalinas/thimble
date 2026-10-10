package vnet

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	peekWait = 300 * time.Millisecond // clients speak first; a server-first protocol just times out
	peekMax  = 16 << 10
)

// peek reads a connection's first bytes and says which hostname the client
// is asking for: the SNI of a TLS ClientHello or the Host of an HTTP request.
// It returns everything it read so the caller can replay it upstream. A
// protocol that is neither, or a client that stays silent, yields name "".
func peek(c net.Conn) (name, proto string, buf []byte) {
	c.SetReadDeadline(time.Now().Add(peekWait))
	defer c.SetReadDeadline(time.Time{})

	first := make([]byte, 5)
	n, err := io.ReadFull(c, first)
	buf = append(buf, first[:n]...)
	if err != nil {
		return "", "", buf
	}

	if first[0] == 0x16 && first[1] == 0x03 { // TLS handshake record
		recLen := int(first[3])<<8 | int(first[4])
		if recLen > peekMax {
			return "", "tls", buf
		}
		rec := make([]byte, recLen)
		m, _ := io.ReadFull(c, rec)
		buf = append(buf, rec[:m]...)
		return sni(buf), "tls", buf
	}

	// Plaintext HTTP: read until the end of the headers.
	for !bytes.Contains(buf, []byte("\r\n\r\n")) && len(buf) < peekMax {
		tmp := make([]byte, 2048)
		m, err := c.Read(tmp)
		buf = append(buf, tmp[:m]...)
		if err != nil {
			break
		}
	}
	if req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(buf))); err == nil {
		host := req.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		return host, "http", buf
	}
	return "", "", buf
}

var errSNI = errors.New("sni captured")

// sni extracts the server name from a buffered ClientHello by letting
// crypto/tls parse it and stopping the handshake as soon as the callback
// fires, so there is no hand-written TLS parser to get wrong.
func sni(hello []byte) string {
	var name string
	srv := tls.Server(&replay{r: bytes.NewReader(hello)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			name = h.ServerName
			return nil, errSNI
		},
	})
	srv.Handshake()
	return name
}

// replay is a net.Conn that serves fixed bytes and swallows writes.
type replay struct {
	net.Conn
	r *bytes.Reader
}

func (p *replay) Read(b []byte) (int, error)       { return p.r.Read(b) }
func (p *replay) Write(b []byte) (int, error)      { return len(b), nil }
func (p *replay) Close() error                     { return nil }
func (p *replay) SetDeadline(time.Time) error      { return nil }
func (p *replay) SetReadDeadline(time.Time) error  { return nil }
func (p *replay) SetWriteDeadline(time.Time) error { return nil }
func (p *replay) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (p *replay) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
