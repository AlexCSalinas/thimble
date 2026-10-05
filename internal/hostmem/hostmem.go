// Package hostmem reports how much physical memory a sandbox costs the host.
//
// On macOS 13+ Virtualization.framework does not run the guest inside the
// calling process. Each VZVirtualMachine gets its own XPC helper,
// com.apple.Virtualization.VirtualMachine, launched by launchd (so its parent
// is pid 1, not us). Guest RAM lives in that helper. Measuring our own process
// therefore tells you nothing about the guest; you have to find the helper.
//
// Two numbers are reported for a process:
//
//   - resident: classic RSS, pages currently in RAM.
//   - footprint: phys_footprint, what Activity Monitor's "Memory" column shows
//     and what macOS uses for memory-pressure decisions. It includes compressed
//     and wired pages. Budget against this one.
package hostmem

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int thimble_rusage(int pid, uint64_t *rss, uint64_t *footprint) {
	struct rusage_info_v4 ri;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)&ri) != 0) return -1;
	*rss = ri.ri_resident_size;
	*footprint = ri.ri_phys_footprint;
	return 0;
}

// Returns number of pids written, or -1.
static int thimble_listpids(int *buf, int cap) {
	int n = proc_listpids(PROC_ALL_PIDS, 0, buf, cap * sizeof(int));
	if (n <= 0) return -1;
	return n / sizeof(int);
}

static int thimble_pidpath(int pid, char *buf, int cap) {
	return proc_pidpath(pid, buf, cap);
}
*/
import "C"

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unsafe"
)

const helperName = "com.apple.Virtualization.VirtualMachine"

type Usage struct {
	Resident  uint64 // bytes
	Footprint uint64 // bytes
}

// Of reports memory usage of pid. Works for any process we own.
func Of(pid int) (Usage, error) {
	var rss, fp C.uint64_t
	if C.thimble_rusage(C.int(pid), &rss, &fp) != 0 {
		return Usage{}, fmt.Errorf("proc_pid_rusage(%d) failed", pid)
	}
	return Usage{Resident: uint64(rss), Footprint: uint64(fp)}, nil
}

// Self is Of(our pid).
func Self() (Usage, error) { return Of(os.Getpid()) }

// Helpers lists pids of all running Virtualization.framework VM helper
// processes, sorted. Diff the result from before and after VirtualMachine.Start
// to find the helper backing a given VM.
func Helpers() ([]int, error) {
	buf := make([]C.int, 4096)
	n := C.thimble_listpids(&buf[0], C.int(len(buf)))
	if n < 0 {
		return nil, fmt.Errorf("proc_listpids failed")
	}
	path := (*C.char)(C.malloc(C.PROC_PIDPATHINFO_MAXSIZE))
	defer C.free(unsafe.Pointer(path))
	var out []int
	for _, p := range buf[:n] {
		if p == 0 {
			continue
		}
		if C.thimble_pidpath(p, path, C.PROC_PIDPATHINFO_MAXSIZE) <= 0 {
			continue // not ours to inspect, or gone
		}
		if strings.HasSuffix(C.GoString(path), "/"+helperName) {
			out = append(out, int(p))
		}
	}
	sort.Ints(out)
	return out, nil
}

// NewHelper returns the pid in after that is not in before, or 0.
func NewHelper(before, after []int) int {
	seen := map[int]bool{}
	for _, p := range before {
		seen[p] = true
	}
	for _, p := range after {
		if !seen[p] {
			return p
		}
	}
	return 0
}

func MiB(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
