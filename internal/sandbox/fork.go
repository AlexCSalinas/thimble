package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alexcsalinas/thimble/internal/envd"
	"github.com/alexcsalinas/thimble/internal/snapshot"
	"github.com/alexcsalinas/thimble/internal/vnet"
)

const MaxFork = 16

var (
	ErrNoFork  = errors.New("fork needs the userspace network: start the server with -vnet (clones share a MAC, which only works when each VM has its own network)")
	ErrForkArg = errors.New("fork count must be between 1 and 16")
)

// Fork clones a running sandbox n times. Each child resumes from the
// parent's exact memory, processes and disk, then diverges: its writes go to
// its own copy-on-write disk clone and its own copy of RAM.
//
// The mechanism is pause, clone, resume:
//
//  1. Pause the parent. The framework saves memory and device state and the
//     helper process exits, which also flushes the disk, so the image on disk
//     matches the saved memory.
//  2. APFS-clone the disk and the state file once per child (clonefile(2):
//     instant, shares blocks until written).
//  3. Resume the parent from its own state, so it is down for one pause and
//     one restore, not for the whole fan-out.
//  4. Restore each child from its clone. A restore must present the saved
//     MAC and machine id, so every child has the same ones; that is fine
//     because each VM sits on its own private network (internal/vnet), which
//     is why fork needs -vnet.
//
// The parent's open connections to the outside die with the fork: the
// guests still believe they are established, the host side is gone.
func (mg *Manager) Fork(ctx context.Context, id string, n int, timeout time.Duration) ([]*Sandbox, error) {
	if !mg.cfg.VNet {
		return nil, ErrNoFork
	}
	if n < 1 || n > MaxFork {
		return nil, ErrForkArg
	}
	parent, err := mg.Get(id)
	if err != nil {
		return nil, err
	}
	mg.mu.Lock()
	if mg.cfg.MaxSandboxes > 0 && len(mg.sbx)+n > mg.cfg.MaxSandboxes {
		mg.mu.Unlock()
		return nil, ErrFull
	}
	mg.mu.Unlock()

	parent.mu.Lock()
	remaining := time.Until(parent.EndAt)
	parent.mu.Unlock()
	if remaining < 30*time.Second {
		remaining = 30 * time.Second
	}

	t0 := time.Now()
	if err := mg.Pause(id); err != nil { // ErrPaused if it was not running
		return nil, err
	}
	tPaused := time.Since(t0)

	pstate := filepath.Join(parent.dir, snapshot.StateFile)
	kids := make([]*Sandbox, 0, n)
	var prepErr error
	for i := 0; i < n; i++ {
		k, err := mg.prepChild(parent, pstate)
		if err != nil {
			prepErr = err
			break
		}
		kids = append(kids, k)
	}

	// The parent goes back first; the children's files are already cloned.
	if err := mg.Resume(ctx, id, remaining); err != nil {
		mg.killAll(kids)
		return nil, fmt.Errorf("resume parent: %w", err)
	}
	tParent := time.Since(t0)
	if prepErr != nil {
		mg.killAll(kids)
		return nil, prepErr
	}

	for _, k := range kids {
		if err := mg.start(ctx, k, filepath.Join(k.dir, snapshot.StateFile)); err != nil {
			mg.killAll(kids)
			return nil, fmt.Errorf("start fork %s: %w", k.ID, err)
		}
		os.Remove(filepath.Join(k.dir, snapshot.StateFile))
		k.StartedAt = time.Now()
		mg.setTimeout(k, timeout)
	}
	mg.cfg.Logf("fork %s x%d: parent paused %.0f ms, parent back at %.0f ms, children up at %.0f ms",
		id, n, ms(tPaused), ms(tParent), ms(time.Since(t0)))
	return kids, nil
}

// prepChild registers a new sandbox whose disk and saved state are clones of
// the parent's. It is not started.
func (mg *Manager) prepChild(parent *Sandbox, pstate string) (*Sandbox, error) {
	pol, err := vnet.NewPolicy(parent.policy.Spec())
	if err != nil {
		return nil, err
	}
	k := &Sandbox{
		ID: newID(), TemplateID: parent.TemplateID, EnvVars: parent.EnvVars,
		// envd only accepts a new access token from a caller that already
		// presents the current one (Firecracker's metadata service is what
		// authorises a reset, and there is none here), so a fork keeps its
		// parent's. Siblings are reached by their own ids, one user's own VMs.
		AccessToken: parent.AccessToken, EnvdVersion: parent.EnvdVersion, AutoPause: parent.AutoPause,
		CPUs: parent.CPUs, MemMiB: parent.MemMiB, policy: pol, forked: true,
		Metadata: map[string]string{},
	}
	for key, v := range parent.Metadata {
		k.Metadata[key] = v
	}
	k.Metadata["thimble.fork-of"] = parent.ID
	k.dir = filepath.Join(mg.cfg.DataDir, k.ID)
	k.meta = parent.meta
	abs, _ := filepath.Abs(filepath.Join(k.dir, "rootfs.img"))
	k.meta.Disk = abs

	if err := os.MkdirAll(k.dir, 0o755); err != nil {
		return nil, err
	}
	if err := cloneFile(parent.meta.Disk, k.meta.Disk); err != nil {
		os.RemoveAll(k.dir)
		return nil, fmt.Errorf("clone disk: %w", err)
	}
	if err := cloneFile(pstate, filepath.Join(k.dir, snapshot.StateFile)); err != nil {
		os.RemoveAll(k.dir)
		return nil, fmt.Errorf("clone state: %w", err)
	}
	mg.mu.Lock()
	mg.sbx[k.ID] = k
	mg.mu.Unlock()
	return k, nil
}

func (mg *Manager) killAll(ks []*Sandbox) {
	for _, k := range ks {
		mg.Kill(k.ID)
	}
}

// reseed gives a forked guest its own randomness. The kernel's entropy pool
// is part of the saved memory, so every child would otherwise draw the same
// "random" numbers as its siblings (session ids, keys, uuids). Writing fresh
// host randomness to /dev/urandom mixes it into the pool.
func (mg *Manager) reseed(ctx context.Context, c *envd.Client) {
	b := make([]byte, 64)
	rand.Read(b)
	err := c.Start(ctx, envd.ProcessConfig{Cmd: "/bin/sh", Args: []string{"-c", "echo " + hex.EncodeToString(b) + " > /dev/urandom"}}, func(envd.Event) {})
	if err != nil {
		mg.cfg.Logf("reseed: %v", err)
	}
}
