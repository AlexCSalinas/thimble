package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// mkdisk builds the template's root disk: a sparse raw image with an ext4
// filesystem holding the full initramfs' contents plus packages from the
// Alpine mirror. macOS has no mkfs.ext4, so the guest does the work: the
// full initramfs is booted with the blank image attached and
// `thimble.mkdisk=1` on the command line, and its /init formats, copies,
// runs `apk add` and powers off. The host streams the console and checks
// for the THIMBLE_MKDISK_OK line.
//
// E2B's equivalent (`pkg/template/build`) extracts an OCI image into ext4
// on the Linux host; here the only Linux available is the guest itself.
func mkdisk(args []string) error {
	fs := flag.NewFlagSet("mkdisk", flag.ExitOnError)
	opts := guestFlags(fs)
	out := fs.String("out", "build/guest/rootfs.img", "raw disk image to create")
	sizeMiB := fs.Uint64("size", 4096, "disk size in MiB (sparse; only written blocks cost space)")
	pkgs := fs.String("pkgs", "", "Alpine packages to apk add into the image (space or comma separated)")
	fs.Parse(args)

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	os.Remove(*out)
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(*sizeMiB) << 20); err != nil {
		return err
	}
	f.Close()

	o := opts(true)
	o.disk = *out
	o.noWait = true
	if o.mem < 1024 {
		o.mem = 1024 // apk add of python3 + nodejs wants room; configured size is a ceiling, not a cost
	}
	list := strings.Join(strings.Fields(strings.ReplaceAll(*pkgs, ",", " ")), ",")
	o.cmdline += " thimble.mkdisk=1"
	if list != "" {
		o.cmdline += " thimble.pkgs=" + list
	}
	console := &lockedBuffer{}
	o.tee = console

	t0 := time.Now()
	g, err := launch(o)
	if err != nil {
		return err
	}
	stopOnSignal(g)
	if err := g.m.WaitStopped(15 * time.Minute); err != nil {
		g.m.Stop()
		return fmt.Errorf("mkdisk guest: %w", err)
	}
	log := console.String()
	switch {
	case strings.Contains(log, "THIMBLE_MKDISK_OK"):
	case strings.Contains(log, "THIMBLE_MKDISK_FAIL"):
		os.Remove(*out)
		return fmt.Errorf("mkdisk failed inside the guest (see console output above)")
	default:
		os.Remove(*out)
		return fmt.Errorf("mkdisk guest stopped without reporting a result")
	}
	st, _ := os.Stat(*out)
	used := diskUsage(*out)
	fmt.Fprintf(os.Stderr, "\r\n--- thimble mkdisk report ---\r\n")
	fmt.Fprintf(os.Stderr, "wrote %s: %d MiB sparse, %.0f MiB allocated, in %.1f s\r\n", *out, st.Size()>>20, float64(used)/(1<<20), time.Since(t0).Seconds())
	return nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
