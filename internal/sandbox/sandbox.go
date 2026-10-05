// Package sandbox is thimble's orchestrator core: the in-memory table of
// sandboxes and the operations the control plane exposes on them. Every
// sandbox is a VM restored from one template snapshot, reached through envd
// over vsock. No database; the process is the source of truth, which is also
// how E2B's orchestrator holds its sandbox map (`sandbox.Map`), with Redis and
// Postgres layered on for the API and billing.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alexcsalinas/thimble/internal/envd"
	"github.com/alexcsalinas/thimble/internal/hostmem"
	"github.com/alexcsalinas/thimble/internal/snapshot"
	"github.com/alexcsalinas/thimble/internal/vm"
)

var (
	ErrNotFound  = errors.New("sandbox not found")
	ErrNotPaused = errors.New("sandbox is not paused")
	ErrPaused    = errors.New("sandbox is paused")
)

type State string

const (
	Running State = "running"
	Paused  State = "paused"
)

type Sandbox struct {
	ID          string
	TemplateID  string
	Metadata    map[string]string
	EnvVars     map[string]string
	StartedAt   time.Time
	EndAt       time.Time
	AccessToken string
	EnvdVersion string
	AutoPause   bool
	CPUs        uint
	MemMiB      uint64

	mu        sync.Mutex
	state     State
	m         *vm.Machine
	helper    int
	transport *http.Transport // HTTP over this sandbox's vsock, for the proxy
	timer     *time.Timer
	pausedDir string // where the paused state lives
	meta      snapshot.Meta
}

func (s *Sandbox) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Transport returns an HTTP transport that reaches this sandbox's envd, or
// nil when the sandbox is not running.
func (s *Sandbox) Transport() *http.Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != Running {
		return nil
	}
	return s.transport
}

// Memory reports the helper process's footprint, zero if not running.
func (s *Sandbox) Memory() hostmem.Usage {
	s.mu.Lock()
	h, st := s.helper, s.state
	s.mu.Unlock()
	if st != Running || h == 0 {
		return hostmem.Usage{}
	}
	u, _ := hostmem.Of(h)
	return u
}

type Config struct {
	SnapshotDir string // the one template every create restores from
	IdleMiB     uint64 // balloon target after restore (0 = no balloon)
	StateDir    string // where paused sandboxes' state files go
	Logf        func(format string, a ...any)
}

type Manager struct {
	cfg   Config
	meta  snapshot.Meta
	state string
	mu    sync.Mutex
	sbx   map[string]*Sandbox
	// VM creation is serialised so the helper-pid diff in create() is
	// unambiguous. Restores take ~300 ms; fine for a laptop.
	createMu sync.Mutex
}

func NewManager(cfg Config) (*Manager, error) {
	meta, state, err := snapshot.Load(cfg.SnapshotDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(cfg.SnapshotDir, "paused")
	}
	return &Manager{cfg: cfg, meta: meta, state: state, sbx: map[string]*Sandbox{}}, nil
}

func newID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "i" + hex.EncodeToString(b)[:19]
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Create restores the template snapshot into a new sandbox.
func (mg *Manager) Create(ctx context.Context, templateID string, timeout time.Duration, metadata, envVars map[string]string, autoPause bool) (*Sandbox, error) {
	s := &Sandbox{
		ID: newID(), TemplateID: templateID, Metadata: metadata, EnvVars: envVars,
		AccessToken: newToken(), EnvdVersion: mg.meta.EnvdVersion, AutoPause: autoPause,
		CPUs: mg.meta.CPUs, MemMiB: mg.meta.MemMiB, meta: mg.meta,
	}
	if s.Metadata == nil {
		s.Metadata = map[string]string{}
	}
	t0 := time.Now()
	if err := mg.start(ctx, s, mg.state); err != nil {
		return nil, err
	}
	s.StartedAt = time.Now()
	mg.mu.Lock()
	mg.sbx[s.ID] = s
	mg.mu.Unlock()
	mg.setTimeout(s, timeout)
	mg.cfg.Logf("create %s template=%s timeout=%s took %.0f ms footprint %s", s.ID, templateID, timeout, ms(time.Since(t0)), hostmem.MiB(s.Memory().Footprint))
	return s, nil
}

// start builds a VM from state and brings envd up. s.mu must not be held.
func (mg *Manager) start(ctx context.Context, s *Sandbox, state string) error {
	mg.createMu.Lock()
	defer mg.createMu.Unlock()

	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	inR, _, err := os.Pipe()
	if err != nil {
		return err
	}
	go io.Copy(io.Discard, outR)

	before, _ := hostmem.Helpers()
	m, err := vm.New(vm.Config{
		Kernel: s.meta.Kernel, Initrd: s.meta.Initrd, Cmdline: s.meta.Cmdline,
		CPUs: s.meta.CPUs, MemoryMiB: s.meta.MemMiB, MAC: s.meta.MAC, MachineID: s.meta.MachineID,
		ConsoleIn: inR, ConsoleOut: outW,
	})
	if err != nil {
		return err
	}
	if err := m.Restore(state); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := m.Resume(); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	after, _ := hostmem.Helpers()

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dev := m.Vsock()
			if dev == nil {
				return nil, errors.New("vm has no vsock device")
			}
			return dev.Connect(envd.Port)
		},
		MaxIdleConns: 8, IdleConnTimeout: 90 * time.Second, DisableCompression: true,
	}
	c := envd.New(func(ctx context.Context) (net.Conn, error) { return tr.DialContext(ctx, "", "") }, "")
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := c.WaitHealthy(hctx); err != nil {
		m.Stop()
		return err
	}
	// /init sets the access token the SDK will present on every envd call,
	// the default user/workdir, env vars, and corrects the guest clock, which
	// is frozen at snapshot time until now.
	hdr, err := c.Init(hctx, envd.InitRequest{
		AccessToken: s.AccessToken, EnvVars: s.EnvVars,
		DefaultUser: "user", DefaultWorkdir: "/home/user",
	})
	if err != nil {
		m.Stop()
		return fmt.Errorf("envd init: %w", err)
	}
	if v := hdr.Get("X-Envd-Version"); v != "" {
		s.EnvdVersion = v
	}
	if mg.cfg.IdleMiB > 0 {
		m.SetMemoryTarget(mg.cfg.IdleMiB << 20)
	}

	s.mu.Lock()
	s.m, s.transport, s.helper, s.state = m, tr, hostmem.NewHelper(before, after), Running
	s.mu.Unlock()
	return nil
}

func (mg *Manager) Get(id string) (*Sandbox, error) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	s, ok := mg.sbx[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

// List returns sandboxes matching every metadata pair and any of states
// (empty = all), newest first.
func (mg *Manager) List(metadata map[string]string, states []State) []*Sandbox {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	var out []*Sandbox
	for _, s := range mg.sbx {
		if len(states) > 0 {
			st := s.State()
			ok := false
			for _, want := range states {
				ok = ok || st == want
			}
			if !ok {
				continue
			}
		}
		match := true
		for k, v := range metadata {
			if s.Metadata[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// Kill stops the sandbox and forgets it.
func (mg *Manager) Kill(id string) error {
	mg.mu.Lock()
	s, ok := mg.sbx[id]
	if ok {
		delete(mg.sbx, id)
	}
	mg.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.m != nil {
		s.m.Stop()
		s.m = nil
	}
	if s.pausedDir != "" {
		os.RemoveAll(s.pausedDir)
	}
	s.mu.Unlock()
	mg.cfg.Logf("kill %s", id)
	return nil
}

// SetTimeout makes the sandbox expire d from now. On expiry it is killed, or
// paused if it was created with autoPause.
func (mg *Manager) SetTimeout(id string, d time.Duration) error {
	s, err := mg.Get(id)
	if err != nil {
		return err
	}
	mg.setTimeout(s, d)
	return nil
}

func (mg *Manager) setTimeout(s *Sandbox, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.EndAt = time.Now().Add(d)
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(d, func() { mg.expire(s) })
}

// ExtendTimeout only ever moves EndAt later (connect semantics).
func (mg *Manager) ExtendTimeout(s *Sandbox, d time.Duration) {
	s.mu.Lock()
	later := time.Now().Add(d).After(s.EndAt)
	s.mu.Unlock()
	if later {
		mg.setTimeout(s, d)
	}
}

func (mg *Manager) expire(s *Sandbox) {
	if s.AutoPause {
		if err := mg.Pause(s.ID); err != nil {
			mg.cfg.Logf("auto-pause %s failed: %v", s.ID, err)
		}
		return
	}
	mg.cfg.Logf("expire %s", s.ID)
	mg.Kill(s.ID)
}

// Pause saves the sandbox's state to disk and tears down its VM, so a paused
// sandbox costs no host memory at all (the helper process exits). The balloon
// has been inflated since the sandbox was created, so the guest's free pages
// are already with the host and the state file holds only pages in use.
func (mg *Manager) Pause(id string) error {
	s, err := mg.Get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != Running {
		return ErrPaused
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	t0 := time.Now()
	before := hostmem.Usage{}
	if s.helper != 0 {
		before, _ = hostmem.Of(s.helper)
	}
	dir := filepath.Join(mg.cfg.StateDir, s.ID)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := s.m.Pause(); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	path := filepath.Join(dir, snapshot.StateFile)
	if err := s.m.Save(path); err != nil {
		s.m.Resume()
		return fmt.Errorf("save: %w", err)
	}
	s.m.Stop()
	s.m.WaitStopped(10 * time.Second)
	st, _ := os.Stat(path)
	s.m, s.transport, s.helper, s.state, s.pausedDir = nil, nil, 0, Paused, dir
	mg.cfg.Logf("pause %s: footprint %s -> 0 (helper exited), state file %s, took %.0f ms",
		s.ID, hostmem.MiB(before.Footprint), hostmem.MiB(uint64(st.Size())), ms(time.Since(t0)))
	return nil
}

// Resume brings a paused sandbox back from its own state file.
func (mg *Manager) Resume(ctx context.Context, id string, timeout time.Duration) error {
	s, err := mg.Get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.state != Paused {
		s.mu.Unlock()
		return ErrNotPaused
	}
	dir := s.pausedDir
	s.mu.Unlock()
	t0 := time.Now()
	if err := mg.start(ctx, s, filepath.Join(dir, snapshot.StateFile)); err != nil {
		return err
	}
	mg.setTimeout(s, timeout)
	mg.cfg.Logf("resume %s took %.0f ms footprint %s", s.ID, ms(time.Since(t0)), hostmem.MiB(s.Memory().Footprint))
	return nil
}

// Close kills everything; for shutdown.
func (mg *Manager) Close() {
	for _, s := range mg.List(nil, nil) {
		mg.Kill(s.ID)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// ParseMetadataQuery parses E2B's metadata filter: "k=v&k2=v2", URL-encoded.
func ParseMetadataQuery(q string) map[string]string {
	out := map[string]string{}
	if q == "" {
		return out
	}
	for _, kv := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(kv, "=")
		k, _ = url.QueryUnescape(k)
		v, _ = url.QueryUnescape(v)
		if k != "" {
			out[k] = v
		}
	}
	return out
}
