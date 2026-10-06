// Package api implements the slice of E2B's control plane (spec/openapi.yml)
// that the SDKs need for Sandbox.create / connect / kill / list / set_timeout /
// get_info / pause, plus the envd proxy the SDK sends sandbox traffic to.
//
// E2B splits this across an API service (Postgres, Redis, auth), the
// orchestrator (gRPC, one per host) and a client proxy that routes
// `49983-<id>.<domain>` hostnames to the right orchestrator. thimble is all
// three in one process: the API mutates the in-memory sandbox table directly,
// and the proxy routes on the `E2b-Sandbox-Id` header the SDK already sends.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alexcsalinas/thimble/internal/sandbox"
)

type Server struct {
	mg     *sandbox.Manager
	apiKey string // if set, X-API-Key must equal it; otherwise any non-empty key passes
	logf   func(string, ...any)
}

func New(mg *sandbox.Manager, apiKey string, logf func(string, ...any)) *Server {
	return &Server{mg: mg, apiKey: apiKey, logf: logf}
}

// ---- control plane -------------------------------------------------------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /v2/sandboxes", s.auth(s.create))
	mux.HandleFunc("POST /sandboxes", s.auth(s.create))
	mux.HandleFunc("GET /v2/sandboxes", s.auth(s.list))
	mux.HandleFunc("GET /sandboxes", s.auth(s.list))
	mux.HandleFunc("POST /v2/sandboxes/{id}/connect", s.auth(s.connect))
	mux.HandleFunc("GET /sandboxes/{id}", s.auth(s.get))
	mux.HandleFunc("DELETE /sandboxes/{id}", s.auth(s.kill))
	mux.HandleFunc("POST /sandboxes/{id}/timeout", s.auth(s.timeout))
	mux.HandleFunc("POST /sandboxes/{id}/pause", s.auth(s.pause))
	mux.HandleFunc("POST /sandboxes/{id}/resume", s.auth(s.resume))
	return logRequests(mux, s.logf)
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" || (s.apiKey != "" && key != s.apiKey) {
			fail(w, http.StatusUnauthorized, "invalid or missing X-API-Key")
			return
		}
		h(w, r)
	}
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(apiError{Code: code, Message: msg})
}

func failErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		fail(w, http.StatusNotFound, "sandbox not found")
	case errors.Is(err, sandbox.ErrPaused), errors.Is(err, sandbox.ErrNotPaused):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, sandbox.ErrFull):
		fail(w, http.StatusTooManyRequests, err.Error())
	default:
		fail(w, http.StatusInternalServerError, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// sandboxBody is schema Sandbox: what create and connect return.
type sandboxBody struct {
	TemplateID      string  `json:"templateID"`
	SandboxID       string  `json:"sandboxID"`
	ClientID        string  `json:"clientID"`
	EnvdVersion     string  `json:"envdVersion"`
	EnvdAccessToken string  `json:"envdAccessToken"`
	Domain          *string `json:"domain"`
}

func toBody(s *sandbox.Sandbox) sandboxBody {
	return sandboxBody{TemplateID: s.TemplateID, SandboxID: s.ID, ClientID: "thimble", EnvdVersion: s.EnvdVersion, EnvdAccessToken: s.AccessToken, Domain: nil}
}

// sandboxDetail is schema SandboxDetail / ListedSandbox.
type sandboxDetail struct {
	sandboxBody
	StartedAt  string            `json:"startedAt"`
	EndAt      string            `json:"endAt"`
	CPUCount   uint              `json:"cpuCount"`
	MemoryMB   uint64            `json:"memoryMB"`
	DiskSizeMB uint64            `json:"diskSizeMB"`
	Metadata   map[string]string `json:"metadata"`
	State      sandbox.State     `json:"state"`
}

func (srv *Server) toDetail(s *sandbox.Sandbox) sandboxDetail {
	return sandboxDetail{
		sandboxBody: toBody(s),
		StartedAt:   s.StartedAt.UTC().Format(time.RFC3339Nano),
		EndAt:       s.EndAt.UTC().Format(time.RFC3339Nano),
		CPUCount:    s.CPUs, MemoryMB: s.MemMiB, DiskSizeMB: srv.mg.DiskMiB(),
		Metadata: s.Metadata, State: s.State(),
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TemplateID string            `json:"templateID"`
		Timeout    *int              `json:"timeout"`
		AutoPause  bool              `json:"autoPause"`
		Metadata   map[string]string `json:"metadata"`
		EnvVars    map[string]string `json:"envVars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if req.TemplateID == "" {
		fail(w, http.StatusBadRequest, "templateID is required")
		return
	}
	timeout := 300
	if req.Timeout != nil {
		timeout = *req.Timeout
	}
	sb, err := s.mg.Create(r.Context(), req.TemplateID, time.Duration(timeout)*time.Second, req.Metadata, req.EnvVars, req.AutoPause)
	if err != nil {
		failErr(w, fmt.Errorf("create: %w", err))
		return
	}
	writeJSON(w, http.StatusCreated, toBody(sb))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metadata := sandbox.ParseMetadataQuery(q.Get("metadata"))
	var states []sandbox.State
	for _, v := range q["state"] {
		for _, st := range strings.Split(v, ",") {
			if st != "" {
				states = append(states, sandbox.State(st))
			}
		}
	}
	all := s.mg.List(metadata, states)
	out := make([]sandboxDetail, 0, len(all))
	running := 0
	for _, sb := range all {
		out = append(out, s.toDetail(sb))
		if sb.State() == sandbox.Running {
			running++
		}
	}
	w.Header().Set("X-Total-Running", strconv.Itoa(running))
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	sb, err := s.mg.Get(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toDetail(sb))
}

func (s *Server) kill(w http.ResponseWriter, r *http.Request) {
	if err := s.mg.Kill(r.PathValue("id")); err != nil {
		failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) timeout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Timeout int `json:"timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if err := s.mg.SetTimeout(r.PathValue("id"), time.Duration(req.Timeout)*time.Second); err != nil {
		failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// connect: 200 if running (TTL only ever extended), 201 if it had to be resumed.
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Timeout *int `json:"timeout"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req) // optional body
	}
	timeout := 300 * time.Second
	if req.Timeout != nil {
		timeout = time.Duration(*req.Timeout) * time.Second
	}
	sb, err := s.mg.Get(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	if sb.State() == sandbox.Paused {
		if err := s.mg.Resume(r.Context(), sb.ID, timeout); err != nil {
			failErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, toBody(sb))
		return
	}
	s.mg.ExtendTimeout(sb, timeout)
	writeJSON(w, http.StatusOK, toBody(sb))
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	if err := s.mg.Pause(r.PathValue("id")); err != nil {
		failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Timeout *int `json:"timeout"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	timeout := 300 * time.Second
	if req.Timeout != nil {
		timeout = time.Duration(*req.Timeout) * time.Second
	}
	sb, err := s.mg.Get(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	if err := s.mg.Resume(r.Context(), sb.ID, timeout); err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toBody(sb))
}

// ---- envd proxy ------------------------------------------------------------

// ProxyHandler routes SDK traffic to the right sandbox's envd. The SDK sets
// `E2b-Sandbox-Id` on every envd request (it was introduced for E2B's stable
// `sandbox.<domain>` host), and X-Access-Token, which envd itself verifies.
// A sandbox that is unknown or paused answers 502, which the SDK reads as
// "not running".
func (s *Server) ProxyHandler() http.Handler {
	target, _ := url.Parse("http://envd")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("E2b-Sandbox-Id")
		if id == "" {
			// Fallback: `49983-<id>.<domain>` style hosts.
			if h, _, ok := strings.Cut(r.Host, "."); ok {
				if _, rest, ok := strings.Cut(h, "-"); ok {
					id = rest
				}
			}
		}
		sb, err := s.mg.Get(id)
		if err != nil && id == "" {
			// Plain GETs and POSTs of signed file URLs carry no header at
			// all (urllib, curl, a browser). E2B routes those by hostname.
			// With one shared localhost URL the signature itself is the
			// routing key: it is a hash over the sandbox's envd access
			// token, which only one sandbox has. Failing that, if exactly
			// one sandbox is running, it must be that one.
			running := s.mg.List(nil, []sandbox.State{sandbox.Running})
			if sig := r.URL.Query().Get("signature"); sig != "" {
				for _, cand := range running {
					if fileSignature(r, cand.AccessToken) == sig {
						sb, err = cand, nil
						break
					}
				}
			} else if len(running) == 1 {
				sb, err = running[0], nil
			}
		}
		if err != nil {
			fail(w, http.StatusBadGateway, "sandbox not running")
			return
		}
		tr := sb.Transport()
		if tr == nil {
			fail(w, http.StatusBadGateway, "sandbox not running")
			return
		}
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = target.Host
				pr.Out.Header.Del("E2b-Sandbox-Id")
				pr.Out.Header.Del("E2b-Sandbox-Port")
			},
			Transport:     eofOnError{tr},
			FlushInterval: -1, // Connect streams and file downloads must not be buffered
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				fail(w, http.StatusBadGateway, "envd: "+err.Error())
			},
		}
		rp.ServeHTTP(w, r)
	})
}

// eofOnError handles the sandbox dying while a response is streaming (a
// command killed mid-run). ReverseProxy would abort the response and reset
// the client connection. Instead, for a Connect stream, we do what E2B's
// proxy does: emit an end-of-stream envelope with code "unavailable" at the
// frame boundary, which the SDK maps to a TimeoutException explaining the
// sandbox was killed. Any other body just ends.
type eofOnError struct{ rt http.RoundTripper }

func (e eofOnError) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := e.rt.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &eofBody{ReadCloser: resp.Body, connect: strings.HasPrefix(resp.Header.Get("Content-Type"), "application/connect+")}
	return resp, nil
}

type eofBody struct {
	io.ReadCloser
	connect bool
	trailer []byte // pending end-of-stream envelope after an upstream error
	failed  bool
}

func (b *eofBody) Read(p []byte) (int, error) {
	if b.failed {
		if len(b.trailer) == 0 {
			return 0, io.EOF
		}
		n := copy(p, b.trailer)
		b.trailer = b.trailer[n:]
		return n, nil
	}
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		b.failed = true
		if b.connect {
			msg, _ := json.Marshal(map[string]any{"error": map[string]string{
				"code": "unavailable", "message": "sandbox was killed or reached its end of life while the request was in flight",
			}})
			hdr := []byte{0x02, byte(len(msg) >> 24), byte(len(msg) >> 16), byte(len(msg) >> 8), byte(len(msg))}
			b.trailer = append(hdr, msg...)
		}
		return n, nil
	}
	return n, err
}

// fileSignature recomputes the SDK's v1 URL signature for a /files request
// (e2b/sandbox/signature.py): "v1_" + unpadded base64 of
// sha256("<path>:<read|write>:<username>:<token>[:<expiration>]").
func fileSignature(r *http.Request, token string) string {
	q := r.URL.Query()
	op := "read"
	if r.Method != http.MethodGet {
		op = "write"
	}
	raw := q.Get("path") + ":" + op + ":" + q.Get("username") + ":" + token
	if exp := q.Get("signature_expiration"); exp != "" {
		raw += ":" + exp
	}
	sum := sha256.Sum256([]byte(raw))
	return "v1_" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func logRequests(h http.Handler, logf func(string, ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		h.ServeHTTP(rec, r)
		logf("%s %s -> %d (%.1f ms)", r.Method, r.URL.RequestURI(), rec.code, float64(time.Since(t0))/float64(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Serve runs the API and the envd proxy until ctx ends.
func Serve(ctx context.Context, s *Server, apiAddr, envdAddr string) error {
	mk := func(addr string, h http.Handler) *http.Server {
		srv := &http.Server{Addr: addr, Handler: h}
		// The SDK's HTTP client speaks HTTP/2 when it can; allow it in the clear.
		var p http.Protocols
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		srv.Protocols = &p
		return srv
	}
	apiSrv := mk(apiAddr, s.Handler())
	envdSrv := mk(envdAddr, s.ProxyHandler())
	errc := make(chan error, 2)
	go func() { errc <- apiSrv.ListenAndServe() }()
	go func() { errc <- envdSrv.ListenAndServe() }()
	s.logf("api on http://%s, envd proxy on http://%s", apiAddr, envdAddr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		apiSrv.Shutdown(sctx)
		envdSrv.Shutdown(sctx)
		return nil
	}
}
