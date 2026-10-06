// Package sandbox is thimble's orchestrator core: the in-memory table of
// sandboxes and the operations the control plane exposes on them. Every
// sandbox is a VM booted from an APFS clone of one template disk, reached
// through envd over vsock. No database; the process is the source of truth,
// which is also how E2B's orchestrator holds its sandbox map (`sandbox.Map`),
// with Redis and Postgres layered on for the API and billing.
//
// Create is a cold boot, not a snapshot restore. A restored VM must present
// the MAC its state was saved with, and the framework's NAT drops frames from
// any other MAC, so N sandboxes restored from one snapshot share one MAC and
// one DHCP lease and only one of them has a working network (NOTES.md,
// phase 6). A boot from disk takes ~0.5 s and gets a fresh MAC. Pause and
// resume still save and restore memory, per sandbox, with that sandbox's MAC.
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

	"golang.org/x/sys/unix"

	"github.com/alexcsalinas/thimble/internal/envd"
	"github.com/alexcsalinas/thimble/internal/hostmem"
	"github.com/alexcsalinas/thimble/internal/snapshot"
	"github.com/alexcsalinas/thimble/internal/vm"
)

var (
	ErrNotFound  = errors.New("sandbox not found")
	ErrNotPaused = errors.New("sandbox is not paused")
	ErrPaused    = errors.New("sandbox is paused")
	ErrFull      = errors.New("sandbox limit reached")
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
	dir       string        // <DataDir>/<id>: rootfs.img clone, and state.vzvmstate while paused
	meta      snapshot.Meta // the configuration this sandbox's VM was built with; a resume must repeat it
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

// Config describes the one template every sandbox is created from.
type Config struct {
	Kernel       string // uncompressed arm64 Image
	Initrd       string // the small boot initramfs (loads modules, switch_roots to the disk)
	Cmdline      string // kernel command line; "thimble.root=/dev/vda" is appended
	Disk         string // template root disk; every sandbox gets an APFS clone
	MemMiB       uint64
	CPUs         uint
	DataDir      string // per-sandbox directories live here
	IdleMiB      uint64 // balloon target after a resume from pause (0 = leave the balloon alone)
	MaxSandboxes int    // running + paused; 0 = unlimited
	Logf         func(format string, a ...any)
}

type Manager struct {
	cfg     Config
	diskMiB uint64
	mu      sync.Mutex
	sbx     map[string]*Sandbox
	// VM creation is serialised so the helper-pid diff in start() is
	// unambiguous. A boot takes ~0.5 s; fine for a laptop.
	createMu sync.Mutex
}

func NewManager(cfg Config) (*Manager, error) {
	for _, p := range []string{cfg.Kernel, cfg.Initrd, cfg.Disk} {
		if _, err := os.Stat(p); err != nil {
			return nil, err
		}
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.MemMiB == 0 {
		cfg.MemMiB = 512
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "build/sandboxes"
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	// Leftovers from a previous process: their VMs died with it.
	if old, _ := os.ReadDir(cfg.DataDir); len(old) > 0 {
		for _, e := range old {
			os.RemoveAll(filepath.Join(cfg.DataDir, e.Name()))
		}
		cfg.Logf("removed %d stale sandbox director%s from %s", len(old), map[bool]string{true: "y", false: "ies"}[len(old) == 1], cfg.DataDir)
	}
	st, _ := os.Stat(cfg.Disk)
	return &Manager{cfg: cfg, diskMiB: uint64(st.Size()) >> 20, sbx: map[string]*Sandbox{}}, nil
}

// DiskMiB is the size of the template disk, which every sandbox sees as its root.
func (mg *Manager) DiskMiB() uint64 { return mg.diskMiB }

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

// Create clones the template disk and boots a new sandbox from it.
func (mg *Manager) Create(ctx context.Context, templateID string, timeout time.Duration, metadata, envVars map[string]string, autoPause bool) (*Sandbox, error) {
	s := &Sandbox{
		ID: newID(), TemplateID: templateID, Metadata: metadata, EnvVars: envVars,
		AccessToken: newToken(), AutoPause: autoPause,
		CPUs: mg.cfg.CPUs, MemMiB: mg.cfg.MemMiB,
	}
	if s.Metadata == nil {
		s.Metadata = map[string]string{}
	}
	s.dir = filepath.Join(mg.cfg.DataDir, s.ID)
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	s.meta = snapshot.Meta{
		Kernel: abs(mg.cfg.Kernel), Initrd: abs(mg.cfg.Initrd),
		Cmdline: strings.TrimSpace(mg.cfg.Cmdline + " thimble.root=/dev/vda"),
		Disk:    abs(filepath.Join(s.dir, "rootfs.img")),
		MemMiB:  mg.cfg.MemMiB, CPUs: mg.cfg.CPUs,
	}

	mg.mu.Lock()
	if mg.cfg.MaxSandboxes > 0 && len(mg.sbx) >= mg.cfg.MaxSandboxes {
		mg.mu.Unlock()
		return nil, ErrFull
	}
	mg.sbx[s.ID] = s // reserve the slot; removed again below on failure
	mg.mu.Unlock()

	t0 := time.Now()
	err := os.MkdirAll(s.dir, 0o755)
	if err == nil {
		err = cloneFile(mg.cfg.Disk, s.meta.Disk)
	}
	if err == nil {
		err = mg.start(ctx, s, "")
	}
	if err != nil {
		mg.mu.Lock()
		delete(mg.sbx, s.ID)
		mg.mu.Unlock()
		os.RemoveAll(s.dir)
		return nil, err
	}
	s.StartedAt = time.Now()
	mg.setTimeout(s, timeout)
	mg.cfg.Logf("create %s template=%s timeout=%s took %.0f ms footprint %s", s.ID, templateID, timeout, ms(time.Since(t0)), hostmem.MiB(s.Memory().Footprint))
	return s, nil
}

// start builds a VM for s and brings envd up: a cold boot when state is
// empty, otherwise a restore of that state file. s.mu must not be held.
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
		Kernel: s.meta.Kernel, Initrd: s.meta.Initrd, Cmdline: s.meta.Cmdline, Disk: s.meta.Disk,
		CPUs: s.meta.CPUs, MemoryMiB: s.meta.MemMiB, MAC: s.meta.MAC, MachineID: s.meta.MachineID,
		ConsoleIn: inR, ConsoleOut: outW,
	})
	if err != nil {
		return err
	}
	// First boot: the framework picked a MAC and machine id; a later resume
	// from a paused state must present the same ones.
	s.meta.MAC, s.meta.MachineID = m.MAC(), m.MachineID()
	if state == "" {
		if err := m.Start(); err != nil {
			return fmt.Errorf("start: %w", err)
		}
	} else {
		if err := m.Restore(state); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		if err := m.Resume(); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
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
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.WaitHealthy(hctx); err != nil {
		m.Stop()
		return err
	}
	// /init sets the access token the SDK will present on every envd call,
	// the default user/workdir, env vars, and the guest clock (which, after a
	// resume, is still at pause time).
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
	// A restore materialises every guest page on the host; the balloon
	// hands the free ones back. A cold boot only touched what it used, so
	// the balloon is left alone and the guest keeps its full memory.
	if state != "" && mg.cfg.IdleMiB > 0 {
		m.SetMemoryTarget(mg.cfg.IdleMiB << 20)
	}

	s.mu.Lock()
	s.m, s.transport, s.helper, s.state = m, tr, hostmem.NewHelper(before, after), Running
	s.mu.Unlock()
	return nil
}

// cloneFile makes dst a copy-on-write clone of src (APFS clonefile: instant,
// shares blocks until written). Falls back to a plain copy on filesystems
// without it, which is slow for a multi-GiB sparse image but correct.
func cloneFile(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("clonefile %s: %w", dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (mg *Manager) Get(id string) (*Sandbox, error) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	s, ok := mg.sbx[id]
	if !ok || s.State() == "" { // reserved but not yet booted
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
		st := s.State()
		if st == "" {
			continue
		}
		if len(states) > 0 {
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

// Kill stops the sandbox, deletes its disk clone and any paused state, and
// forgets it.
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
		s.m.WaitStopped(10 * time.Second) // the image is locked until the helper lets go
		s.m = nil
	}
	os.RemoveAll(s.dir)
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

// Pause saves the sandbox's memory and device state next to its disk and
// tears down its VM, so a paused sandbox costs no host memory at all (the
// helper process exits). The disk clone stays; it is the sandbox's root.
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
	path := filepath.Join(s.dir, snapshot.StateFile)
	os.Remove(path) // the framework refuses to overwrite
	if err := s.m.Pause(); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := s.m.Save(path); err != nil {
		s.m.Resume()
		return fmt.Errorf("save: %w", err)
	}
	s.m.Stop()
	s.m.WaitStopped(10 * time.Second)
	st, _ := os.Stat(path)
	s.m, s.transport, s.helper, s.state = nil, nil, 0, Paused
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
	s.mu.Unlock()
	t0 := time.Now()
	state := filepath.Join(s.dir, snapshot.StateFile)
	if err := mg.start(ctx, s, state); err != nil {
		return err
	}
	os.Remove(state) // memory has moved on; the file could not be resumed again
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
