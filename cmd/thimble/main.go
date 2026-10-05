// thimble is a macOS-native microVM sandbox orchestrator in the spirit of E2B.
//
// Phase 1: `thimble boot` starts one Linux guest with its serial console on
// the terminal and reports boot latency and host memory cost.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alexcsalinas/thimble/internal/hostmem"
	"github.com/alexcsalinas/thimble/internal/rawterm"
	"github.com/alexcsalinas/thimble/internal/vm"
)

const readyMarker = "THIMBLE_BOOT_OK"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "boot":
		err = boot(os.Args[2:])
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
  thimble boot   -kernel K -initrd I [-mem MiB] [-cpus N] [-cmdline S] [-bench]
  thimble limits`)
}

func boot(args []string) error {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	kernel := fs.String("kernel", "build/guest/vmlinux", "uncompressed arm64 kernel Image")
	initrd := fs.String("initrd", "build/guest/initramfs.cpio.gz", "initramfs")
	cmdline := fs.String("cmdline", "console=hvc0 loglevel=4 panic=-1", "kernel command line")
	mem := fs.Uint64("mem", 256, "guest memory in MiB")
	cpus := fs.Uint("cpus", 1, "guest vCPUs")
	bench := fs.Bool("bench", false, "boot, wait for the ready marker, report, stop")
	showConsole := fs.Bool("console", true, "show guest console output (always on when interactive)")
	timeout := fs.Duration("timeout", 30*time.Second, "give up if the ready marker is not seen")
	fs.Parse(args)

	minMem, _, _, _ := vm.Limits()
	if *mem<<20 < minMem {
		return fmt.Errorf("-mem %d MiB is below the framework minimum of %s", *mem, hostmem.MiB(minMem))
	}
	interactive := !*bench && rawterm.IsTerminal(os.Stdin)
	echo := interactive || *showConsole

	// Guest console goes through pipes so we can watch for the ready marker
	// (output) and intercept the detach key (input).
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return err
	}

	m, err := vm.New(vm.Config{
		Kernel: *kernel, Initrd: *initrd, Cmdline: *cmdline,
		CPUs: *cpus, MemoryMiB: *mem,
		ConsoleIn: inR, ConsoleOut: outW,
	})
	if err != nil {
		return err
	}

	readyCh := make(chan ready, 1)
	firstByte := make(chan time.Time, 1)
	go watchConsole(outR, echo, firstByte, readyCh)

	var restore func()
	if interactive {
		st, err := rawterm.MakeRaw(os.Stdin)
		if err != nil {
			return err
		}
		restore = func() { st.Restore() }
		defer restore()
		fmt.Fprintf(os.Stderr, "thimble: console attached, Ctrl-] to detach and kill the vm\r\n")
		go forwardStdin(inW, func() { m.Stop() })
	} else {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() { <-sig; m.Stop() }()
	}

	// The guest's memory lives in a framework helper process, not in ours.
	// Snapshot the helper pids around Start to learn which one is ours.
	helpersBefore, _ := hostmem.Helpers()
	selfBase, _ := hostmem.Self()

	t0 := time.Now()
	if err := m.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	helpersAfter, _ := hostmem.Helpers()
	helper := hostmem.NewHelper(helpersBefore, helpersAfter)
	memOf := func() hostmem.Usage {
		if helper == 0 {
			return hostmem.Usage{}
		}
		u, _ := hostmem.Of(helper)
		return u
	}

	var tFirst, tReady time.Time
	var markerLine string
	select {
	case tFirst = <-firstByte:
	case <-time.After(*timeout):
		m.Stop()
		return fmt.Errorf("no console output within %s", *timeout)
	}
	select {
	case r := <-readyCh:
		tReady, markerLine = r.at, r.line
	case <-time.After(*timeout):
		m.Stop()
		return fmt.Errorf("guest never printed %s within %s", readyMarker, *timeout)
	}
	atReady := memOf()
	if !interactive && !*bench {
		// Scripted use: `printf 'uname -a\npoweroff\n' | thimble boot`. Start
		// pumping only now, so nothing is written before the guest's tty exists.
		go forwardStdin(inW, func() {})
	}

	report := func(settled hostmem.Usage) string {
		var b strings.Builder
		w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\r\n", a...) }
		w("")
		w("--- thimble boot report ---")
		w("guest:              %d vCPU, %d MiB, kernel %s", *cpus, *mem, *kernel)
		w("first console byte: %8.1f ms  (vm.Start -> kernel printed something)", ms(tFirst.Sub(t0)))
		w("userspace ready:    %8.1f ms  (vm.Start -> /init printed %s)", ms(tReady.Sub(t0)), readyMarker)
		if u := guestUptime(markerLine); u != "" {
			w("guest's own clock:  %8s s   (/proc/uptime when /init ran)", u)
		}
		w("guest meminfo:      %s", strings.TrimSpace(markerLine[strings.Index(markerLine, " ")+1:]))
		w("host memory (resident / phys_footprint):")
		w("  thimble itself:          %10s / %s", hostmem.MiB(selfBase.Resident), hostmem.MiB(selfBase.Footprint))
		if helper == 0 {
			w("  vm helper:               not found (could not identify com.apple.Virtualization.VirtualMachine pid)")
		} else {
			w("  vm helper pid %d, at ready: %10s / %s", helper, hostmem.MiB(atReady.Resident), hostmem.MiB(atReady.Footprint))
			if settled.Footprint != 0 {
				w("  vm helper, 2s after ready: %10s / %s", hostmem.MiB(settled.Resident), hostmem.MiB(settled.Footprint))
			}
		}
		return b.String()
	}

	if *bench {
		time.Sleep(2 * time.Second)
		settled := memOf()
		if err := m.Stop(); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		if err := m.WaitStopped(10 * time.Second); err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, report(settled))
		return nil
	}

	fmt.Fprint(os.Stderr, report(hostmem.Usage{}))
	if err := m.WaitStopped(0); err != nil {
		return err
	}
	if restore != nil {
		restore()
	}
	fmt.Fprintln(os.Stderr, "\nthimble: vm stopped")
	return nil
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// watchConsole copies guest output to stdout and signals the first byte and
// the ready marker line.
func watchConsole(r io.Reader, echo bool, firstByte chan<- time.Time, readyCh chan<- ready) {
	var (
		first   = true
		seen    = false
		lineBuf bytes.Buffer
		buf     = make([]byte, 4096)
	)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if first {
				first = false
				firstByte <- time.Now()
			}
			if echo {
				os.Stdout.Write(buf[:n])
			}
			if !seen {
				lineBuf.Write(buf[:n])
				for {
					line, rest, ok := bytes.Cut(lineBuf.Bytes(), []byte("\n"))
					if !ok {
						break
					}
					if i := bytes.Index(line, []byte(readyMarker)); i >= 0 {
						seen = true
						readyCh <- ready{at: time.Now(), line: strings.TrimRight(string(line[i:]), "\r")}
						lineBuf.Reset()
						break
					}
					lineBuf = *bytes.NewBuffer(append([]byte(nil), rest...))
				}
			}
		}
		if err != nil {
			return
		}
	}
}

type ready struct {
	at   time.Time
	line string
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

func guestUptime(line string) string {
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "uptime="); ok {
			return strings.TrimSuffix(v, "s")
		}
	}
	return ""
}
