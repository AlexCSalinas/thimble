package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/alexcsalinas/thimble/internal/api"
	"github.com/alexcsalinas/thimble/internal/sandbox"
)

// serve runs the control plane (phase 4) and the envd proxy. Point the stock
// E2B SDK at it with:
//
//	E2B_API_KEY=anything E2B_API_URL=http://localhost:3000 E2B_SANDBOX_URL=http://localhost:49983
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	kernel := fs.String("kernel", "build/guest/vmlinux", "uncompressed arm64 kernel Image")
	initrd := fs.String("initrd", "build/guest/boot.cpio.gz", "boot initramfs (modules + switch_root to the disk)")
	disk := fs.String("disk", "build/guest/rootfs.img", "template root disk from `thimble mkdisk`; each sandbox boots an APFS clone")
	cmdline := fs.String("cmdline", "console=hvc0 loglevel=4 panic=-1", "kernel command line (thimble.root=/dev/vda is appended)")
	mem := fs.Uint64("mem", 512, "guest memory per sandbox in MiB (a ceiling: the host only backs pages the guest touches)")
	cpus := fs.Uint("cpus", 1, "vCPUs per sandbox")
	data := fs.String("data", "build/sandboxes", "directory for per-sandbox disk clones and paused state")
	max := fs.Int("max", 0, "maximum sandboxes (running + paused); 0 = unlimited")
	apiAddr := fs.String("api", "127.0.0.1:3000", "control plane listen address")
	envdAddr := fs.String("envd", "127.0.0.1:49983", "envd proxy listen address")
	idle := fs.Uint64("idle", 0, "after a resume from pause, inflate the balloon so the guest keeps only this many MiB (0 = off)")
	vnetOn := fs.Bool("vnet", false, "give each sandbox a private userspace network (all traffic passes through this process) instead of the framework NAT")
	apiKey := fs.String("api-key", os.Getenv("THIMBLE_API_KEY"), "required X-API-Key value (empty = accept any non-empty key)")
	fs.Parse(args)

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	mg, err := sandbox.NewManager(sandbox.Config{
		Kernel: *kernel, Initrd: *initrd, Disk: *disk, Cmdline: *cmdline,
		MemMiB: *mem, CPUs: *cpus, DataDir: *data, MaxSandboxes: *max, IdleMiB: *idle, VNet: *vnetOn, Logf: log.Printf,
	})
	if err != nil {
		return err
	}
	defer mg.Close()
	log.Printf("template: %s (%d MiB disk), %d MiB RAM, %d vCPU per sandbox", *disk, mg.DiskMiB(), *mem, *cpus)

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; fmt.Fprintln(os.Stderr, "thimble: shutting down"); cancel() }()

	return api.Serve(ctx, api.New(mg, *apiKey, log.Printf), *apiAddr, *envdAddr)
}
