package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/alexcsalinas/thimble/internal/envd"
	"github.com/alexcsalinas/thimble/internal/hostmem"
	"github.com/alexcsalinas/thimble/internal/vm"
)

const readyMarker = "THIMBLE_BOOT_OK"

// launchOpts is what every subcommand needs to bring a guest up.
type launchOpts struct {
	kernel, initrd, cmdline string
	disk                    string // optional raw image for virtio-blk
	mem                     uint64
	cpus                    uint
	echo                    bool      // copy guest console to stdout
	tee                     io.Writer // optional second sink for console output
	noWait                  bool      // return right after Start instead of waiting for the ready marker
	timeout                 time.Duration
}

// guest is a running VM plus the timings and pids gathered while booting it.
type guest struct {
	m        *vm.Machine
	consoleW io.Writer // host -> guest console
	t0       time.Time // just before vm.Start
	tFirst   time.Time // first console byte
	tReady   time.Time // ready marker seen
	marker   string    // the marker line, carries guest uptime and meminfo
	helper   int       // pid of the framework's VM helper process, 0 if unknown
	selfMem  hostmem.Usage
	readyCh  chan ready
	firstCh  chan time.Time
}

type ready struct {
	at   time.Time
	line string
}

// launch starts the VM and returns once the guest's /init has printed the
// ready marker. The console keeps streaming to stdout afterwards if echo is
// set.
func launch(o launchOpts) (*guest, error) {
	minMem, _, _, _ := vm.Limits()
	if o.mem<<20 < minMem {
		return nil, fmt.Errorf("-mem %d MiB is below the framework minimum of %s", o.mem, hostmem.MiB(minMem))
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	m, err := vm.New(vm.Config{
		Kernel: o.kernel, Initrd: o.initrd, Cmdline: o.cmdline,
		CPUs: o.cpus, MemoryMiB: o.mem, Disk: o.disk,
		ConsoleIn: inR, ConsoleOut: outW,
	})
	if err != nil {
		return nil, err
	}
	g := &guest{m: m, consoleW: inW, readyCh: make(chan ready, 1), firstCh: make(chan time.Time, 1)}
	go watchConsole(outR, o.echo, o.tee, g.firstCh, g.readyCh)

	// The guest's memory lives in a framework helper process, not in ours.
	// Snapshot the helper pids around Start to learn which one is ours.
	helpersBefore, _ := hostmem.Helpers()
	g.selfMem, _ = hostmem.Self()

	g.t0 = time.Now()
	if err := m.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	helpersAfter, _ := hostmem.Helpers()
	g.helper = hostmem.NewHelper(helpersBefore, helpersAfter)
	if o.noWait {
		return g, nil
	}

	select {
	case g.tFirst = <-g.firstCh:
	case <-time.After(o.timeout):
		m.Stop()
		return nil, fmt.Errorf("no console output within %s", o.timeout)
	}
	select {
	case r := <-g.readyCh:
		g.tReady, g.marker = r.at, r.line
	case <-time.After(o.timeout):
		m.Stop()
		return nil, fmt.Errorf("guest never printed %s within %s", readyMarker, o.timeout)
	}
	return g, nil
}

// mem reports the helper process's current usage (zero if unknown).
func (g *guest) mem() hostmem.Usage {
	if g.helper == 0 {
		return hostmem.Usage{}
	}
	u, _ := hostmem.Of(g.helper)
	return u
}

// envdClient returns a client that reaches envd over vsock. Each HTTP
// connection is a fresh vsock connection to port 49983, which cmd/vsockfwd
// in the guest forwards to envd's TCP listener.
func (g *guest) envdClient(token string) *envd.Client {
	return envd.New(func(ctx context.Context) (net.Conn, error) {
		dev := g.m.Vsock()
		if dev == nil {
			return nil, fmt.Errorf("vm has no vsock device")
		}
		return dev.Connect(envd.Port)
	}, token)
}

func (g *guest) bootReport() string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\r\n", a...) }
	w("first console byte: %8.1f ms  (vm.Start -> kernel printed something)", ms(g.tFirst.Sub(g.t0)))
	w("userspace ready:    %8.1f ms  (vm.Start -> /init printed %s)", ms(g.tReady.Sub(g.t0)), readyMarker)
	if u := guestUptime(g.marker); u != "" {
		w("guest's own clock:  %8s s   (/proc/uptime when /init ran)", u)
	}
	if i := strings.Index(g.marker, " "); i >= 0 {
		w("guest meminfo:      %s", strings.TrimSpace(g.marker[i+1:]))
	}
	return b.String()
}

func (g *guest) memReport(label string) string {
	if g.helper == 0 {
		return fmt.Sprintf("host memory: vm helper pid not found\r\n")
	}
	u := g.mem()
	return fmt.Sprintf("host memory, %s: helper pid %d resident %s, phys_footprint %s (thimble itself: %s)\r\n",
		label, g.helper, hostmem.MiB(u.Resident), hostmem.MiB(u.Footprint), hostmem.MiB(g.selfMem.Footprint))
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// watchConsole copies guest output to stdout and signals the first byte and
// the ready marker line.
func watchConsole(r io.Reader, echo bool, tee io.Writer, firstByte chan<- time.Time, readyCh chan<- ready) {
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
			if tee != nil {
				tee.Write(buf[:n])
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

func guestUptime(line string) string {
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "uptime="); ok {
			return strings.TrimSuffix(v, "s")
		}
	}
	return ""
}
