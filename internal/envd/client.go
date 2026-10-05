// Package envd is a minimal host-side client for E2B's in-guest agent.
//
// envd exposes two things on one port (49983): a small REST API (/health,
// /init, /files, ...) and Connect RPC services (process.Process,
// filesystem.Filesystem). This package implements just enough of both for
// thimble: health, init, and streaming process start. It speaks Connect's
// JSON codec over HTTP/1.1 using only net/http, so the module stays on the
// standard library.
//
// Transport is pluggable. In thimble it is a vsock connection to the guest,
// forwarded to envd's TCP port by cmd/vsockfwd; E2B uses plain TCP over a
// veth pair.
package envd

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const Port = 49983

type Client struct {
	http  *http.Client
	token string
}

// New builds a client whose every request is dialed with dial. token, if
// non-empty, is sent as X-Access-Token.
func New(dial func(ctx context.Context) (net.Conn, error), token string) *Client {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: 0,
	}
	return &Client{http: &http.Client{Transport: tr}, token: token}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("X-Access-Token", c.token)
	}
	return c.http.Do(req)
}

func (c *Client) url(path string) string { return "http://envd" + path }

// Health succeeds when envd answers GET /health with 204.
func (c *Client) Health(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/health"), nil)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// WaitHealthy polls Health until it succeeds or ctx ends.
func (c *Client) WaitHealthy(ctx context.Context) error {
	for {
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.Health(hctx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("envd never became healthy: last error: %w", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// InitRequest mirrors POST /init in spec/envd.yaml. This is what E2B's
// orchestrator sends right after resuming a sandbox.
type InitRequest struct {
	AccessToken    string            `json:"accessToken,omitempty"`
	EnvVars        map[string]string `json:"envVars,omitempty"`
	DefaultUser    string            `json:"defaultUser,omitempty"`
	DefaultWorkdir string            `json:"defaultWorkdir,omitempty"`
	Timestamp      string            `json:"timestamp,omitempty"`
}

func (c *Client) Init(ctx context.Context, in InitRequest) (http.Header, error) {
	if in.Timestamp == "" {
		in.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	body, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/init"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("init: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return resp.Header, nil
}

// ProcessConfig mirrors process.ProcessConfig.
type ProcessConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Envs map[string]string `json:"envs,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
}

// Event is one process.ProcessEvent; exactly one field is set.
type Event struct {
	Start *struct {
		Pid uint32 `json:"pid"`
	} `json:"start,omitempty"`
	Data *struct {
		Stdout []byte `json:"stdout,omitempty"`
		Stderr []byte `json:"stderr,omitempty"`
		Pty    []byte `json:"pty,omitempty"`
	} `json:"data,omitempty"`
	End *struct {
		ExitCode int32   `json:"exitCode"`
		Exited   bool    `json:"exited"`
		Status   string  `json:"status"`
		Error    *string `json:"error,omitempty"`
	} `json:"end,omitempty"`
	Keepalive *struct{} `json:"keepalive,omitempty"`
}

// Start runs a process and streams its events to onEvent until the stream
// ends. It implements the Connect protocol's server-streaming call
// POST /process.Process/Start with the JSON codec: each message is an
// envelope of 1 flag byte + 4-byte big-endian length + payload, and the
// final envelope (flag bit 0x02) carries {"error": ...} or {}.
func (c *Client) Start(ctx context.Context, cfg ProcessConfig, onEvent func(Event)) error {
	payload, _ := json.Marshal(map[string]any{"process": cfg})
	var body bytes.Buffer
	writeEnvelope(&body, 0, payload)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/process.Process/Start"), &body)
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("process.Start: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	for {
		flags, msg, err := readEnvelope(resp.Body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("process.Start: stream ended without end-of-stream envelope")
			}
			return err
		}
		if flags&0x02 != 0 {
			var end struct {
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(msg, &end); err != nil {
				return fmt.Errorf("bad end-of-stream envelope: %w", err)
			}
			if end.Error != nil {
				return fmt.Errorf("process.Start: %s: %s", end.Error.Code, end.Error.Message)
			}
			return nil
		}
		var ev struct {
			Event Event `json:"event"`
		}
		if err := json.Unmarshal(msg, &ev); err != nil {
			return fmt.Errorf("bad event: %w", err)
		}
		onEvent(ev.Event)
	}
}

func writeEnvelope(w io.Writer, flags byte, payload []byte) {
	var hdr [5]byte
	hdr[0] = flags
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	w.Write(hdr[:])
	w.Write(payload)
}

func readEnvelope(r io.Reader) (flags byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > 64<<20 {
		return 0, nil, fmt.Errorf("envelope too large: %d", n)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}
