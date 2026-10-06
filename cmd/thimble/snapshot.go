package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alexcsalinas/thimble/internal/envd"
	"github.com/alexcsalinas/thimble/internal/hostmem"
	"github.com/alexcsalinas/thimble/internal/snapshot"
	"github.com/alexcsalinas/thimble/internal/vm"
)

var keepInflated bool

// snapshot boots a guest, waits until envd answers, pauses the VM and saves
// its memory and device state. This is the template "build" step: E2B does
// the same with Firecracker's snapshot API after the template's envd is up,
// and every later sandbox is a resume of that file.
func snapshotCmd(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	opts := guestFlags(fs)
	out := fs.String("out", "build/snap", "directory to write the snapshot into")
	showConsole := fs.Bool("console", false, "show the guest console while booting")
	fs.Parse(args)

	o := opts(*showConsole)
	g, err := launch(o)
	if err != nil {
		return err
	}
	defer g.m.Stop()
	stopOnSignal(g)
	if err := g.m.ValidateSaveRestore(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := g.envdClient("")
	if err := c.WaitHealthy(ctx); err != nil {
		return err
	}
	hdr, err := c.Init(ctx, envd.InitRequest{DefaultUser: "user", DefaultWorkdir: "/home/user"})
	if err != nil {
		return err
	}
	tHealthy := time.Now()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	path, _ := filepath.Abs(filepath.Join(*out, snapshot.StateFile))
	os.Remove(path) // the framework refuses to overwrite
	tPause := time.Now()
	if err := g.m.Pause(); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	tPaused := time.Now()
	if err := g.m.Save(path); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	tSaved := time.Now()

	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	meta := snapshot.Meta{
		Kernel: abs(o.kernel), Initrd: abs(o.initrd), Cmdline: o.cmdline, Disk: absIf(o.disk),
		MemMiB: o.mem, CPUs: o.cpus, MAC: g.m.MAC(), MachineID: g.m.MachineID(),
		EnvdVersion: hdr.Get("X-Envd-Version"), Created: time.Now(),
	}
	if err := meta.Save(*out); err != nil {
		return err
	}
	st, _ := os.Stat(path)

	fmt.Fprint(os.Stderr, "\r\n--- thimble snapshot report ---\r\n", g.bootReport())
	fmt.Fprintf(os.Stderr, "envd ready:         %8.1f ms  (vm.Start -> healthy + initialised)\r\n", ms(tHealthy.Sub(g.t0)))
	fmt.Fprintf(os.Stderr, "pause:              %8.1f ms\r\n", ms(tPaused.Sub(tPause)))
	fmt.Fprintf(os.Stderr, "save state:         %8.1f ms  -> %s (%s)\r\n", ms(tSaved.Sub(tPaused)), path, hostmem.MiB(uint64(st.Size())))
	fmt.Fprint(os.Stderr, g.memReport("at save"))
	fmt.Fprintf(os.Stderr, "wrote %s\r\n", filepath.Join(*out, "snapshot.json"))
	return nil
}

// restored is one sandbox brought up from a snapshot.
type restored struct {
	m        *vm.Machine
	helper   int
	tRestore time.Duration // RestoreMachineStateFromURL
	tResume  time.Duration // Resume (paused -> running)
	tHealthy time.Duration // until envd answered over vsock
	tInit    time.Duration // until POST /init done (clock resynced)
	squeezed string        // balloon squeeze summary, if done
}

// restore creates N sandboxes from a snapshot and reports per-sandbox
// latency and the total host memory of their helper processes. With
// -n 1,5,10 it runs each group in turn, stopping the group before the next.
func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dir := fs.String("snapshot", "build/snap", "snapshot directory from `thimble snapshot`")
	ns := fs.String("n", "1,5,10", "comma-separated group sizes to restore")
	diskOverride := fs.String("disk", "", "attach this disk instead of the one recorded in the snapshot (tests restore with a cloned image)")
	cmd := fs.String("cmd", "echo hello from $(hostname) pid $$", "command to run in each restored sandbox, empty to skip")
	keep := fs.Bool("keep", false, "leave the last group running (Ctrl-C to stop)")
	squeeze := fs.Uint64("idle", 96, "after restore, inflate the balloon so the guest keeps only this many MiB; stays inflated (0 = off)")
	deflate := fs.Bool("deflate", false, "with -idle: deflate again right away (for comparison; costs more than never inflating)")
	watch := fs.Duration("watch", 0, "after each group is up, sample total footprint every 500ms for this long")
	fs.Parse(args)
	keepInflated = !*deflate

	meta, state, err := snapshot.Load(*dir)
	if err != nil {
		return err
	}

	var groups []int
	for _, s := range strings.Split(*ns, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			return fmt.Errorf("-n: bad group size %q", s)
		}
		groups = append(groups, n)
	}

	fmt.Fprintf(os.Stderr, "snapshot: %s (%d MiB, %d vCPU, envd %s, saved %s)\n", state, meta.MemMiB, meta.CPUs, meta.EnvdVersion, meta.Created.Format(time.RFC3339))
	for gi, n := range groups {
		fmt.Fprintf(os.Stderr, "\n=== restoring %d sandbox(es) ===\n", n)
		var sbx []*restored
		var sumRes, sumFoot uint64
		tGroup := time.Now()
		for i := 0; i < n; i++ {
			if *diskOverride != "" {
				meta.Disk = *diskOverride
			}
			r, err := restoreOne(meta, state, *cmd, *squeeze)
			if err != nil {
				for _, s := range sbx {
					s.m.Stop()
				}
				return fmt.Errorf("sandbox %d: %w", i+1, err)
			}
			sbx = append(sbx, r)
			fmt.Fprintf(os.Stderr, "  #%-2d restore %6.1f ms  resume %5.1f ms  envd healthy %6.1f ms  init %5.1f ms  (helper pid %d)\n",
				i+1, ms(r.tRestore), ms(r.tResume), ms(r.tHealthy), ms(r.tInit), r.helper)
			if r.squeezed != "" {
				fmt.Fprintf(os.Stderr, "      balloon: %s\n", r.squeezed)
			}
		}
		wall := time.Since(tGroup)
		for t := time.Duration(0); t < *watch; t += 500 * time.Millisecond {
			time.Sleep(500 * time.Millisecond)
			var f uint64
			for _, s := range sbx {
				u, _ := hostmem.Of(s.helper)
				f += u.Footprint
			}
			fmt.Fprintf(os.Stderr, "      t+%-4s total footprint %s\n", (t + 500*time.Millisecond).Round(time.Second), hostmem.MiB(f))
		}
		time.Sleep(500 * time.Millisecond) // let page-ins settle before measuring
		for _, s := range sbx {
			u, _ := hostmem.Of(s.helper)
			sumRes += u.Resident
			sumFoot += u.Footprint
		}
		fmt.Fprintf(os.Stderr, "  total: %d sandboxes live in %.1f ms wall; helpers resident %s, phys_footprint %s (%s per sandbox)\n",
			n, ms(wall), hostmem.MiB(sumRes), hostmem.MiB(sumFoot), hostmem.MiB(sumFoot/uint64(n)))

		if *keep && gi == len(groups)-1 {
			fmt.Fprintln(os.Stderr, "thimble: sandboxes kept running, Ctrl-C to stop")
			sig := make(chan os.Signal, 1)
			waitSignal(sig)
		}
		for _, s := range sbx {
			s.m.Stop()
		}
		for _, s := range sbx {
			s.m.WaitStopped(10 * time.Second)
		}
	}
	return nil
}

func restoreOne(meta snapshot.Meta, state, cmd string, squeezeMiB uint64) (*restored, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	inR, _, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	go io.Copy(io.Discard, outR)

	before, _ := hostmem.Helpers()
	m, err := vm.New(vm.Config{
		Kernel: meta.Kernel, Initrd: meta.Initrd, Cmdline: meta.Cmdline, Disk: meta.Disk,
		CPUs: meta.CPUs, MemoryMiB: meta.MemMiB, MAC: meta.MAC, MachineID: meta.MachineID,
		ConsoleIn: inR, ConsoleOut: outW,
	})
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	if err := m.Restore(state); err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	t1 := time.Now()
	if err := m.Resume(); err != nil {
		return nil, fmt.Errorf("resume: %w", err)
	}
	t2 := time.Now()
	after, _ := hostmem.Helpers()
	r := &restored{m: m, helper: hostmem.NewHelper(before, after), tRestore: t1.Sub(t0), tResume: t2.Sub(t1)}

	g := &guest{m: m}
	c := g.envdClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.WaitHealthy(ctx); err != nil {
		m.Stop()
		return nil, err
	}
	r.tHealthy = time.Since(t0)
	// Re-init: the guest's clock is frozen at snapshot time until this.
	if _, err := c.Init(ctx, envd.InitRequest{DefaultUser: "user", DefaultWorkdir: "/home/user"}); err != nil {
		m.Stop()
		return nil, err
	}
	r.tInit = time.Since(t0)

	if squeezeMiB > 0 {
		r.squeezed = squeeze(m, r.helper, meta.MemMiB, squeezeMiB)
	}

	if cmd != "" {
		var out strings.Builder
		err := c.Start(ctx, envd.ProcessConfig{Cmd: "sh", Args: []string{"-c", cmd}}, func(ev envd.Event) {
			if ev.Data != nil {
				out.Write(ev.Data.Stdout)
				out.Write(ev.Data.Stderr)
			}
		})
		if err != nil {
			m.Stop()
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "      guest says: %s\n", strings.TrimSpace(out.String()))
	}
	return r, nil
}

// squeeze inflates the balloon to leave the guest only lowMiB, waits for the
// host footprint to stop falling, then deflates back to fullMiB. Returns a
// one-line summary. See NOTES.md "Why a restored sandbox costs 2.5x a booted one".
func squeeze(m *vm.Machine, helper int, fullMiB, lowMiB uint64) string {
	before, _ := hostmem.Of(helper)
	t0 := time.Now()
	if err := m.SetMemoryTarget(lowMiB << 20); err != nil {
		return "squeeze failed: " + err.Error()
	}
	// Inflation is asynchronous and done by the guest driver; poll the
	// helper's footprint until it has been stable for a few samples.
	last, stable := before.Footprint, 0
	for i := 0; i < 200 && stable < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		u, _ := hostmem.Of(helper)
		if u.Footprint < last-(1<<20) {
			stable = 0
		} else {
			stable++
		}
		last = u.Footprint
	}
	low, _ := hostmem.Of(helper)
	if keepInflated {
		return fmt.Sprintf("footprint %s -> %s inflated to %d MiB (kept inflated), in %.0f ms",
			hostmem.MiB(before.Footprint), hostmem.MiB(low.Footprint), lowMiB, ms(time.Since(t0)))
	}
	m.SetMemoryTarget(fullMiB << 20)
	time.Sleep(50 * time.Millisecond)
	after, _ := hostmem.Of(helper)
	return fmt.Sprintf("footprint %s -> %s inflated to %d MiB -> %s after deflate, in %.0f ms",
		hostmem.MiB(before.Footprint), hostmem.MiB(low.Footprint), lowMiB, hostmem.MiB(after.Footprint), ms(time.Since(t0)))
}

func absIf(p string) string {
	if p == "" {
		return ""
	}
	a, _ := filepath.Abs(p)
	return a
}
