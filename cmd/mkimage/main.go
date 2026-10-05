// mkimage assembles the guest boot artifacts for thimble from stock Alpine
// packages, using nothing but the Go standard library. It produces:
//
//	<out>/vmlinux             the Alpine "virt" kernel, taken from the linux-virt apk
//	<out>/initramfs.cpio.gz   a root filesystem: Alpine minirootfs + the virtio,
//	                          vsock and ext4 kernel modules + a tiny /init
//
// Why not Docker or mkinitfs: neither exists on a stock Mac, and apk packages
// are plain (multi-stream gzip) tarballs, so extracting the handful of files
// we need is a few lines of archive/tar. See NOTES.md "Guest image".
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/alexcsalinas/thimble/internal/cpio"
)

// Modules the guest needs. Alpine's virt kernel builds virtio-pci and the
// virtio console in, but leaves these as modules. Dependencies are resolved
// from modules.dep, so only leaf names are listed here.
var wantModules = []string{
	"virtio_blk",                 // rootfs disk (phase 3)
	"virtio_net",                 // NAT networking
	"virtio_balloon",             // memory reclaim from idle sandboxes
	"virtio-rng",                 // entropy, keeps getrandom() from blocking at boot
	"vmw_vsock_virtio_transport", // host <-> envd channel
	"ext4",                       // rootfs disk filesystem (phase 3)
}

func main() {
	log.SetFlags(0)
	var (
		rootfsTar = flag.String("rootfs", "", "path to alpine-minirootfs-*.tar.gz")
		kernelApk = flag.String("kernel-apk", "", "path to linux-virt-*.apk")
		outDir    = flag.String("out", "build/guest", "output directory")
		extraDir  = flag.String("overlay", "", "optional directory copied over the rootfs (files become root-owned, mode preserved)")
	)
	flag.Parse()
	if *rootfsTar == "" || *kernelApk == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*rootfsTar, *kernelApk, *outDir, *extraDir); err != nil {
		log.Fatal(err)
	}
}

func run(rootfsTar, kernelApk, outDir, overlay string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Pass 1 over the apk: find the kernel and modules.dep.
	kver, deps, err := scanKernelApk(kernelApk, outDir)
	if err != nil {
		return err
	}
	order, err := resolveModules(deps, wantModules)
	if err != nil {
		return err
	}
	log.Printf("kernel %s, %d modules in load order:", kver, len(order))
	for _, m := range order {
		log.Printf("  %s", m)
	}

	out, err := os.Create(filepath.Join(outDir, "initramfs.cpio.gz"))
	if err != nil {
		return err
	}
	defer out.Close()
	gz, _ := gzip.NewWriterLevel(out, gzip.BestSpeed) // kernel decompress time matters more than size
	cw := cpio.NewWriter(gz)

	nfiles, err := copyTarIntoCpio(rootfsTar, cw)
	if err != nil {
		return fmt.Errorf("rootfs: %w", err)
	}
	log.Printf("rootfs: %d entries from %s", nfiles, filepath.Base(rootfsTar))

	// Pass 2 over the apk: the modules themselves, gunzipped so plain insmod
	// works (the kernel has no CONFIG_MODULE_DECOMPRESS).
	modDir := "lib/modules/" + kver
	if err := extractModules(kernelApk, modDir, order, cw); err != nil {
		return fmt.Errorf("modules: %w", err)
	}

	if err := cw.FileBytes("init", 0o755, initScript(modDir, order)); err != nil {
		return err
	}
	if overlay != "" {
		n, err := copyDirIntoCpio(overlay, cw)
		if err != nil {
			return fmt.Errorf("overlay: %w", err)
		}
		log.Printf("overlay: %d entries from %s", n, overlay)
	}
	if err := cw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	st, _ := out.Stat()
	log.Printf("wrote %s (%.1f MiB)", out.Name(), float64(st.Size())/(1<<20))
	return nil
}

// openApk returns a tar reader over an apk v2 package. The package is three
// concatenated gzip members (signature, control, data); the first two omit the
// tar end-of-archive marker precisely so that one tar reader sees them all.
func openApk(p string) (*tar.Reader, func() error, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return tar.NewReader(gz), f.Close, nil
}

func scanKernelApk(p, outDir string) (kver string, deps map[string][]string, err error) {
	tr, closeFn, err := openApk(p)
	if err != nil {
		return "", nil, err
	}
	defer closeFn()
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		switch {
		case name == "boot/vmlinuz-virt":
			raw, err := io.ReadAll(tr)
			if err != nil {
				return "", nil, err
			}
			img, how, err := rawImage(raw)
			if err != nil {
				return "", nil, fmt.Errorf("kernel: %w", err)
			}
			dst := filepath.Join(outDir, "vmlinux")
			if err := os.WriteFile(dst, img, 0o644); err != nil {
				return "", nil, err
			}
			log.Printf("kernel: %s (%.1f MiB on disk -> %.1f MiB raw Image, %s)", dst, float64(len(raw))/(1<<20), float64(len(img))/(1<<20), how)
		case strings.HasPrefix(name, "lib/modules/") && path.Base(name) == "modules.dep":
			kver = strings.Split(strings.TrimPrefix(name, "lib/modules/"), "/")[0]
			deps, err = parseModulesDep(tr)
			if err != nil {
				return "", nil, err
			}
		}
	}
	if kver == "" {
		return "", nil, fmt.Errorf("%s: no lib/modules/*/modules.dep", p)
	}
	return kver, deps, nil
}

// parseModulesDep reads "kernel/a/b.ko.gz: kernel/c.ko.gz kernel/d.ko.gz" lines.
func parseModulesDep(r io.Reader) (map[string][]string, error) {
	deps := map[string][]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		mod, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		deps[mod] = strings.Fields(rest)
	}
	return deps, sc.Err()
}

func modName(p string) string {
	b := path.Base(p)
	b = strings.TrimSuffix(b, ".gz")
	return strings.TrimSuffix(b, ".ko")
}

// resolveModules returns module paths (as in modules.dep) in a load order
// where every dependency precedes its dependents.
func resolveModules(deps map[string][]string, want []string) ([]string, error) {
	byName := map[string]string{}
	for p := range deps {
		byName[modName(p)] = p
	}
	var order []string
	seen := map[string]bool{}
	var visit func(p string)
	visit = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, d := range deps[p] {
			visit(d)
		}
		order = append(order, p)
	}
	for _, w := range want {
		p, ok := byName[w]
		if !ok {
			// Module names use '-' and '_' interchangeably.
			p, ok = byName[strings.ReplaceAll(w, "-", "_")]
			if !ok {
				return nil, fmt.Errorf("module %q not found in modules.dep", w)
			}
		}
		visit(p)
	}
	return order, nil
}

func extractModules(apk, modDir string, order []string, cw *cpio.Writer) error {
	want := map[string]bool{}
	for _, p := range order {
		want[p] = true
	}
	tr, closeFn, err := openApk(apk)
	if err != nil {
		return err
	}
	defer closeFn()
	found := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		rel := strings.TrimPrefix(name, modDir+"/")
		if !want[rel] {
			continue
		}
		gz, err := gzip.NewReader(tr)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		raw, err := io.ReadAll(gz)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := cw.FileBytes(path.Join(modDir, strings.TrimSuffix(rel, ".gz")), 0o644, raw); err != nil {
			return err
		}
		found++
	}
	if found != len(order) {
		return fmt.Errorf("found %d of %d modules", found, len(order))
	}
	return nil
}

func copyTarIntoCpio(p string, cw *cpio.Writer) (int, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" || name == "." {
			continue
		}
		perm := uint32(h.Mode) & 0o7777
		switch h.Typeflag {
		case tar.TypeDir:
			err = cw.Dir(name, perm)
		case tar.TypeSymlink:
			err = cw.Symlink(name, h.Linkname)
		case tar.TypeReg:
			err = cw.File(name, perm, h.Size, tr)
		default:
			return n, fmt.Errorf("%s: unsupported tar entry type %q", name, h.Typeflag)
		}
		if err != nil {
			return n, err
		}
		n++
	}
}

func copyDirIntoCpio(dir string, cw *cpio.Writer) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		perm := uint32(info.Mode().Perm())
		n++
		switch {
		case d.IsDir():
			return cw.Dir(rel, perm)
		case info.Mode()&os.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return cw.Symlink(rel, t)
		default:
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			return cw.File(rel, perm, info.Size(), f)
		}
	})
	return n, err
}

// rawImage turns whatever a distro calls "vmlinuz" into the raw arm64 Image
// that VZLinuxBootLoader requires (magic "ARM\x64" at offset 0x38).
//
// Alpine (and Fedora, and others since Linux 6.2) ship the kernel as an EFI
// "zboot" binary: a PE/COFF file whose header starts "MZ\0\0zimg", carrying a
// gzip (or zstd) compressed Image that the EFI stub inflates. A firmware-less
// loader like Virtualization.framework has no EFI stub to run, so it rejects
// the file with an opaque "Internal Virtualization error". We unwrap it here.
// Format: u32 LE payload offset at 8, u32 LE payload size at 12, compression
// name at 0x18 (see linux/drivers/firmware/efi/libstub/zboot-header.S).
func rawImage(b []byte) ([]byte, string, error) {
	const arm64Magic = "ARM\x64"
	if len(b) > 0x3c && string(b[0x38:0x3c]) == arm64Magic {
		return b, "already a raw Image", nil
	}
	if len(b) > 0x20 && string(b[4:8]) == "zimg" {
		off := binary.LittleEndian.Uint32(b[8:])
		size := binary.LittleEndian.Uint32(b[12:])
		comp := strings.TrimRight(string(b[0x18:0x20]), "\x00")
		if comp != "gzip" {
			return nil, "", fmt.Errorf("zboot payload is %q compressed; only gzip is supported", comp)
		}
		if uint64(off)+uint64(size) > uint64(len(b)) {
			return nil, "", fmt.Errorf("zboot payload out of range")
		}
		zr, err := gzip.NewReader(bytes.NewReader(b[off : off+size]))
		if err != nil {
			return nil, "", err
		}
		img, err := io.ReadAll(zr)
		if err != nil {
			return nil, "", err
		}
		if len(img) < 0x3c || string(img[0x38:0x3c]) != arm64Magic {
			return nil, "", fmt.Errorf("zboot payload is not an arm64 Image")
		}
		return img, "unwrapped from EFI zboot + gunzip", nil
	}
	if zr, err := gzip.NewReader(bytes.NewReader(b)); err == nil {
		img, err := io.ReadAll(zr)
		if err == nil && len(img) > 0x3c && string(img[0x38:0x3c]) == arm64Magic {
			return img, "gunzipped", nil
		}
	}
	return nil, "", fmt.Errorf("unrecognised kernel format (first bytes %q)", b[:8])
}

func writeFile(dst string, r io.Reader, perm os.FileMode) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// initScript is PID 1. It is deliberately a shell script rather than OpenRC:
// there is nothing to supervise yet, and every service we skip is boot time
// and resident memory we keep. The host looks for the THIMBLE_BOOT_OK line to
// stop the boot clock.
func initScript(modDir string, order []string) []byte {
	var b bytes.Buffer
	b.WriteString(`#!/bin/sh
# Generated by cmd/mkimage. PID 1 for the thimble guest.
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export HOME=/root TERM=xterm

mount -t proc  -o nosuid,nodev,noexec proc  /proc
mount -t sysfs -o nosuid,nodev,noexec sysfs /sys
mountpoint -q /dev || mount -t devtmpfs -o mode=0755,nosuid devtmpfs /dev
mkdir -p /dev/pts /dev/shm /run /tmp
mount -t devpts -o gid=5,mode=620,noexec,nosuid devpts /dev/pts
mount -t tmpfs  -o nosuid,nodev,noexec,mode=0755 tmpfs /run
mount -t tmpfs  -o nosuid,nodev tmpfs /tmp

`)
	fmt.Fprintf(&b, "for m in \\\n")
	for _, p := range order {
		fmt.Fprintf(&b, "    %s \\\n", strings.TrimSuffix(p, ".gz"))
	}
	fmt.Fprintf(&b, `    ; do
    insmod /%s/$m || echo "init: insmod $m failed" >&2
done

hostname thimble
ip link set lo up 2>/dev/null || ifconfig lo up

# busybox poweroff/reboot/halt signal PID 1; translate them into the syscall.
trap 'sync; poweroff -f' USR2
trap 'sync; reboot -f'   TERM
trap 'sync; halt -f'     USR1

# E2B's in-guest agent, if the overlay baked it in. Runs as root so it can
# switch uid per request (SysProcAttr.Credential). -isnotfc: do not poll
# Firecracker's MMDS for config; -no-cgroups: no cgroup v2 setup. vsockfwd
# makes its TCP port reachable from the host over vsock. Both are respawned by
# a loop rather than a supervisor; see NOTES.md "PID 1".
if [ -x /usr/bin/envd ]; then
    adduser -D -u 1000 -s /bin/sh -h /home/user user 2>/dev/null
    mkdir -p /home/user && chown user:user /home/user
    ( while :; do /usr/bin/envd -isnotfc -no-cgroups >>/run/envd.log 2>&1; sleep 1; done ) &
    ( while :; do /usr/bin/vsockfwd 49983 127.0.0.1:49983 >>/run/vsockfwd.log 2>&1; sleep 1; done ) &
fi

read up _ < /proc/uptime
echo "THIMBLE_BOOT_OK uptime=${up}s $(awk '/^(MemTotal|MemFree|MemAvailable)/{printf "%%s%%s ", $1, $2}' /proc/meminfo)kB"

# Interactive shell on the virtio console, respawned if it exits. Backgrounded
# so the traps above can fire while we wait.
while :; do
    setsid sh -c 'exec sh </dev/hvc0 >/dev/hvc0 2>&1' &
    wait $!
done
`, modDir)
	return b.Bytes()
}
