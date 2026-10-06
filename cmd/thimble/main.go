// thimble is a macOS-native microVM sandbox orchestrator in the spirit of E2B.
//
//	thimble boot    phase 1: one guest with its serial console on the terminal
//	thimble run     phase 2: run a command in a guest through envd over vsock
//	thimble snapshot / restore  phase 3: save a booted guest, create N from it
//	thimble serve   phase 4+5: E2B-compatible API + envd proxy, pause/resume
//	thimble limits  what the framework allows on this host
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexcsalinas/thimble/internal/envd"
	"github.com/alexcsalinas/thimble/internal/hostmem"
	"github.com/alexcsalinas/thimble/internal/rawterm"
	"github.com/alexcsalinas/thimble/internal/vm"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "boot":
		err = boot(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "snapshot":
		err = snapshotCmd(os.Args[2:])
	case "restore":
		err = restore(os.Args[2:])
	case "serve":
		err = serve(os.Args[2:])
	case "mkdisk":
		err = mkdisk(os.Args[2:])
	case "limits":
		minMem, maxMem, minCPU, maxCPU := vm.Limits()
		fmt.Printf("memory: %s .. %s\ncpus:   %d .. %d\n", hostmem.MiB(minMem), hostmem.MiB(maxMem), minCPU, maxCPU)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "thimble:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  thimble boot   [-kernel K] [-initrd I] [-mem MiB] [-cpus N] [-cmdline S] [-bench]
  thimble run    [same flags] [-user U] [-cwd D] -- cmd [args...]
  thimble snapshot [same flags] [-out DIR]
  thimble restore  [-snapshot DIR] [-n 1,5,10] [-cmd S] [-keep]
  thimble mkdisk   [same flags] [-out IMG] [-size MiB] [-pkgs "a b c"]
  thimble serve    [-kernel K] [-initrd I] [-disk IMG] [-mem MiB] [-cpus N] [-data DIR] [-api ADDR] [-envd ADDR] [-api-key K]
  thimble limits`)
}

// guestFlags registers the flags shared by every subcommand that boots a VM.
func guestFlags(fs *flag.FlagSet) func(echo bool) launchOpts {
	kernel := fs.String("kernel", "build/guest/vmlinux", "uncompressed arm64 kernel Image")
	initrd := fs.String("initrd", "build/guest/initramfs.cpio.gz", "initramfs")
	cmdline := fs.String("cmdline", "console=hvc0 loglevel=4 panic=-1", "kernel command line")
	mem := fs.Uint64("mem", 256, "guest memory in MiB")
	cpus := fs.Uint("cpus", 1, "guest vCPUs")
	disk := fs.String("disk", "", "raw disk image to attach as /dev/vda (optional)")
	timeout := fs.Duration("timeout", 30*time.Second, "give up if the guest is not ready in time")
	return func(echo bool) launchOpts {
		return launchOpts{kernel: *kernel, initrd: *initrd, cmdline: *cmdline, disk: *disk, mem: *mem, cpus: *cpus, echo: echo, timeout: *timeout}
	}
}

func boot(args []string) error {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	opts := guestFlags(fs)
	bench := fs.Bool("bench", false, "boot, wait for the ready marker, report, stop")
	showConsole := fs.Bool("console", true, "show guest console output (always on when interactive)")
	fs.Parse(args)

	interactive := !*bench && rawterm.IsTerminal(os.Stdin)
	g, err := launch(opts(interactive || *showConsole))
	if err != nil {
		return err
	}
	stopOnSignal(g)

	if *bench {
		time.Sleep(2 * time.Second)
		fmt.Fprint(os.Stderr, "\r\n--- thimble boot report ---\r\n", g.bootReport(), g.memReport("2s after ready"))
		if err := g.m.Stop(); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		return g.m.WaitStopped(10 * time.Second)
	}

	fmt.Fprint(os.Stderr, "\r\n--- thimble boot report ---\r\n", g.bootReport(), g.memReport("at ready"))
	if interactive {
		st, err := rawterm.MakeRaw(os.Stdin)
		if err != nil {
			return err
		}
		defer st.Restore()
		fmt.Fprintf(os.Stderr, "thimble: console attached, Ctrl-] to detach and kill the vm\r\n")
		go forwardStdin(g.consoleW, func() { g.m.Stop() })
	} else {
		// Scripted use: `printf 'uname -a\npoweroff\n' | thimble boot`.
		go forwardStdin(g.consoleW, func() {})
	}
	if err := g.m.WaitStopped(0); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "\r\nthimble: vm stopped")
	return nil
}

// run boots a guest, waits for envd, and executes one command through
// envd's process service, streaming stdout/stderr. This is the whole of
// phase 2: it proves envd runs unchanged in the guest and that the vsock
// path carries Connect RPC.
func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	opts := guestFlags(fs)
	showConsole := fs.Bool("console", false, "also show the guest console (kernel + /init output)")
	user := fs.String("user", "user", "default user envd runs commands as")
	cwd := fs.String("cwd", "/home/user", "default working directory")
	keep := fs.Bool("keep", false, "leave the vm running after the command (Ctrl-C to stop)")
	fs.Parse(args)
	cmd := fs.Args()
	if len(cmd) == 0 {
		return fmt.Errorf("run: no command given (use: thimble run -- echo hello)")
	}

	g, err := launch(opts(*showConsole))
	if err != nil {
		return err
	}
	defer g.m.Stop()
	stopOnSignal(g)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := g.envdClient("")
	if err := c.WaitHealthy(ctx); err != nil {
		return err
	}
	tHealthy := time.Now()
	hdr, err := c.Init(ctx, envd.InitRequest{DefaultUser: *user, DefaultWorkdir: *cwd})
	if err != nil {
		return err
	}
	tInit := time.Now()

	var exit *int32
	tStart := time.Now()
	err = c.Start(ctx, envd.ProcessConfig{Cmd: cmd[0], Args: cmd[1:]}, func(ev envd.Event) {
		switch {
		case ev.Data != nil:
			os.Stdout.Write(ev.Data.Stdout)
			os.Stderr.Write(ev.Data.Stderr)
		case ev.End != nil:
			code := ev.End.ExitCode
			exit = &code
		}
	})
	if err != nil {
		return err
	}
	tEnd := time.Now()

	fmt.Fprint(os.Stderr, "\r\n--- thimble run report ---\r\n", g.bootReport())
	fmt.Fprintf(os.Stderr, "envd healthy:       %8.1f ms  (vm.Start -> GET /health over vsock = 204; envd %s)\r\n", ms(tHealthy.Sub(g.t0)), hdr.Get("X-Envd-Version"))
	fmt.Fprintf(os.Stderr, "envd initialised:   %8.1f ms  (POST /init: defaultUser=%s, took %.1f ms)\r\n", ms(tInit.Sub(g.t0)), *user, ms(tInit.Sub(tHealthy)))
	fmt.Fprintf(os.Stderr, "process round trip: %8.1f ms  (process.Process/Start -> end event, exit %d)\r\n", ms(tEnd.Sub(tStart)), deref(exit))
	fmt.Fprint(os.Stderr, g.memReport("after command"))
	if *keep {
		fmt.Fprintln(os.Stderr, "thimble: vm kept running, Ctrl-C to stop")
		return g.m.WaitStopped(0)
	}
	if exit != nil && *exit != 0 {
		os.Exit(int(*exit))
	}
	return nil
}

func deref(p *int32) int32 {
	if p == nil {
		return -1
	}
	return *p
}

func stopOnSignal(g *guest) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; g.m.Stop() }()
}

func waitSignal(sig chan os.Signal) {
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

// forwardStdin pumps keystrokes to the guest; Ctrl-] (0x1d, as in telnet)
// calls detach instead.
func forwardStdin(w io.Writer, detach func()) {
	in := bufio.NewReader(os.Stdin)
	for {
		b, err := in.ReadByte()
		if err != nil {
			return
		}
		if b == 0x1d {
			detach()
			return
		}
		w.Write([]byte{b})
	}
}
