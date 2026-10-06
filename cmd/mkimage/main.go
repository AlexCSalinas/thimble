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
	"af_packet",                  // udhcpc needs AF_PACKET raw sockets
	"virtio_balloon",             // memory reclaim from idle sandboxes
	"virtio-rng",                 // entropy, keeps getrandom() from blocking at boot
	"vmw_vsock_virtio_transport", // host <-> envd channel
	"ext4",                       // rootfs disk filesystem (phase 3)
	"crc32c_generic",             // ext4 metadata checksums; mount fails with "Cannot load crc32c driver" without it
	"libcrc32c",
}

func main() {
	log.SetFlags(0)
	var (
		rootfsTar = flag.String("rootfs", "", "path to alpine-minirootfs-*.tar.gz")
		kernelApk = flag.String("kernel-apk", "", "path to linux-virt-*.apk")
		outDir    = flag.String("out", "build/guest", "output directory")
		extraDir  = flag.String("overlay", "", "optional directory copied over the rootfs (files become root-owned, mode preserved)")
		apks      = flag.String("apks", "", "comma-separated extra Alpine .apk files to unpack into the rootfs (no install scripts run)")
	)
	flag.Parse()
	if *rootfsTar == "" || *kernelApk == "" {
		flag.Usage()
		os.Exit(2)
	}
	var extra []string
	if *apks != "" {
		extra = strings.Split(*apks, ",")
	}
	if err := run(*rootfsTar, *kernelApk, *outDir, *extraDir, extra); err != nil {
		log.Fatal(err)
	}
}

func run(rootfsTar, kernelApk, outDir, overlay string, apks []string) error {
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
	modDir := "lib/modules/" + kver

	// Two images from the same inputs:
	//
	//   initramfs.cpio.gz  everything: minirootfs + modules + extra apks +
	//                      overlay (envd, vsockfwd). A complete guest in RAM.
	//                      `thimble boot/run/snapshot` use it, and `mkdisk`
	//                      boots it to copy itself onto a disk image.
	//   boot.cpio.gz       minirootfs + modules only. Loads the virtio and
	//                      ext4 modules, mounts /dev/vda and switch_roots to
	//                      it. ~5 MiB, so a disk-backed sandbox does not pay
	//                      to unpack envd and bash into RAM on every boot.
	full := func(cw *cpio.Writer) error {
		for _, a := range apks {
			n, err := unpackApk(a, cw)
			if err != nil {
				return fmt.Errorf("%s: %w", a, err)
			}
			log.Printf("apk: %d entries from %s", n, filepath.Base(a))
		}
		if overlay != "" {
			n, err := copyDirIntoCpio(overlay, cw)
			if err != nil {
				return fmt.Errorf("overlay: %w", err)
			}
			log.Printf("overlay: %d entries from %s", n, overlay)
		}
		return nil
	}
	if err := writeImage(filepath.Join(outDir, "initramfs.cpio.gz"), rootfsTar, kernelApk, modDir, order, full); err != nil {
		return err
	}
	return writeImage(filepath.Join(outDir, "boot.cpio.gz"), rootfsTar, kernelApk, modDir, order, nil)
}

// writeImage assembles one gzip'd cpio: minirootfs, modules, /init, then
// whatever extra adds.
func writeImage(path, rootfsTar, kernelApk, modDir string, order []string, extra func(*cpio.Writer) error) error {
	out, err := os.Create(path)
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
	if err := extractModules(kernelApk, modDir, order, cw); err != nil {
		return fmt.Errorf("modules: %w", err)
	}
	if err := cw.FileBytes("init", 0o755, initScript(modDir, order)); err != nil {
		return err
	}
	// DNS goes to public resolvers, not the host's. See resolvConf.
	if err := cw.FileBytes("etc/udhcpc/udhcpc.conf", 0o644, []byte(udhcpcConf)); err != nil {
		return err
	}
	if err := cw.FileBytes("etc/resolv.conf", 0o644, []byte(resolvConf)); err != nil {
		return err
	}
	if extra != nil {
		if err := extra(cw); err != nil {
			return err
		}
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

// unpackApk adds a package's files to the archive, skipping apk's own
// metadata (.SIGN.*, .PKGINFO, .pre-install and friends at the root).
func unpackApk(p string, cw *cpio.Writer) (int, error) {
	tr, closeFn, err := openApk(p)
	if err != nil {
		return 0, err
	}
	defer closeFn()
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
		if name == "" || name == "." || strings.HasPrefix(name, ".") {
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
			continue
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
//
// The same script is PID 1 in three situations, told apart by the kernel
// command line and by what / is:
//
//   - plain initramfs boot (phases 1-3): run from RAM.
//   - thimble.root=/dev/vda: mount the disk and switch_root to it; the copy
//     of this script on the disk then runs again with / on ext4.
//   - thimble.mkdisk=1: format /dev/vda, copy this root filesystem onto it,
//     `apk add` the packages named by thimble.pkgs= into it, power off.
func initScript(modDir string, order []string) []byte {
	var b bytes.Buffer
	b.WriteString(`#!/bin/sh
# Generated by cmd/mkimage. PID 1 for the thimble guest.
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export HOME=/root TERM=xterm

# Guards everywhere: after switch_root, /dev /proc /sys /run arrive already
# mounted (busybox switch_root moves them) and the modules are loaded.
mountpoint -q /proc || mount -t proc  -o nosuid,nodev,noexec proc  /proc
mountpoint -q /sys  || mount -t sysfs -o nosuid,nodev,noexec sysfs /sys
mountpoint -q /dev  || mount -t devtmpfs -o mode=0755,nosuid devtmpfs /dev
mkdir -p /dev/pts /dev/shm /run /tmp
mountpoint -q /dev/pts || mount -t devpts -o gid=5,mode=620,noexec,nosuid devpts /dev/pts
mountpoint -q /run     || mount -t tmpfs  -o nosuid,nodev,noexec,mode=0755 tmpfs /run
mountpoint -q /tmp     || mount -t tmpfs  -o nosuid,nodev tmpfs /tmp

`)
	fmt.Fprintf(&b, "for m in \\\n")
	for _, p := range order {
		fmt.Fprintf(&b, "    %s \\\n", strings.TrimSuffix(p, ".gz"))
	}
	fmt.Fprintf(&b, `    ; do
    n=$(basename $m .ko | tr - _)
    [ -d /sys/module/$n ] || insmod /%s/$m || echo "init: insmod $m failed" >&2
done

ROOT= MKDISK= PKGS=
for a in $(cat /proc/cmdline); do
    case $a in
        thimble.root=*)   ROOT=${a#*=} ;;
        thimble.mkdisk=*) MKDISK=1 ;;
        thimble.pkgs=*)   PKGS=$(echo ${a#*=} | tr , ' ') ;;
    esac
done

# Disk-backed guest: hand over to the copy of this script on /dev/vda.
if [ -n "$ROOT" ] && ! grep -q ' / ext4 ' /proc/mounts; then
    i=0; while [ ! -b "$ROOT" ] && [ $i -lt 100 ]; do sleep 0.02; i=$((i+1)); done
    mkdir -p /newroot
    if mount -t ext4 -o noatime "$ROOT" /newroot; then
        exec switch_root /newroot /init
    fi
    echo "init: mounting $ROOT failed, staying in the initramfs" >&2
fi

hostname thimble
ip link set lo up 2>/dev/null || ifconfig lo up

# Outbound networking over the framework's NAT device: DHCP from the host's
# bootpd (192.168.64.1), which also serves DNS. udhcpc stays resident to renew
# the lease. Backgrounded so envd is not held up by the ~50 ms exchange.
if [ -e /sys/class/net/eth0 ]; then
    ip link set eth0 up
    udhcpc -i eth0 -b -t 10 -A 1 -S >/dev/null 2>&1 &
fi

# busybox poweroff/reboot/halt signal PID 1; translate them into the syscall.
trap 'sync; poweroff -f' USR2
trap 'sync; reboot -f'   TERM
trap 'sync; halt -f'     USR1

# Template build: this root filesystem, plus packages from the Alpine mirror,
# onto a blank disk. Runs from the full initramfs; see cmd/thimble/mkdisk.go.
if [ -n "$MKDISK" ]; then
    # A standalone subshell: inside "( ... ) && x || y" the shell ignores
    # set -e, so the result is checked through $? instead.
    (
    set -e
    for i in $(seq 1 100); do [ -f /etc/resolv.conf ] && grep -q nameserver /etc/resolv.conf && break; sleep 0.1; done
    echo "mkdisk: installing e2fsprogs into the initramfs"
    apk add --no-cache -q e2fsprogs
    echo "mkdisk: mkfs.ext4 /dev/vda"
    mkfs.ext4 -q -L thimble -E lazy_itable_init=0,lazy_journal_init=0 /dev/vda
    mkdir -p /mnt
    mount -t ext4 /dev/vda /mnt
    mountpoint -q /mnt
    echo "mkdisk: copying root filesystem"
    for d in /*; do
        case $d in /proc|/sys|/dev|/run|/tmp|/mnt|/newroot) ;; *) cp -a $d /mnt/ ;; esac
    done
    mkdir -p /mnt/proc /mnt/sys /mnt/dev /mnt/run /mnt/tmp /mnt/mnt
    chmod 1777 /mnt/tmp
    mount -t proc proc /mnt/proc; mount --bind /dev /mnt/dev; mount -t sysfs sysfs /mnt/sys
    cp /etc/resolv.conf /mnt/etc/resolv.conf
    echo "127.0.0.1 thimble" >> /mnt/etc/hosts
    if [ -n "$PKGS" ]; then
        echo "mkdisk: apk add $PKGS"
        chroot /mnt apk add --no-cache $PKGS
    fi
    # The sandbox user: uid 1000, passwordless sudo, as in E2B's templates.
    chroot /mnt sh -c 'adduser -D -u 1000 -s /bin/bash -h /home/user user 2>/dev/null || adduser -D -u 1000 -s /bin/sh -h /home/user user; chmod 0755 /home/user; chmod g-s /home/user'
    if [ -x /mnt/usr/bin/sudo ]; then
        mkdir -p /mnt/etc/sudoers.d
        echo "user ALL=(ALL) NOPASSWD: ALL" > /mnt/etc/sudoers.d/user
        chmod 0440 /mnt/etc/sudoers.d/user
    fi
    # PEP 668 marker: Alpine's python3 refuses "pip install" without it. An
    # agent's sandbox is exactly the place for pip to write into site-packages.
    rm -f /mnt/usr/lib/python3*/EXTERNALLY-MANAGED
    umount /mnt/proc /mnt/sys /mnt/dev
    umount /mnt
    sync
    echo "mkdisk: done, $(($(cat /sys/block/vda/size) / 2048)) MiB disk"
    )
    if [ $? -eq 0 ]; then echo THIMBLE_MKDISK_OK; else echo THIMBLE_MKDISK_FAIL; fi
    sync
    poweroff -f
fi

# E2B's in-guest agent, if present. Runs as root so it can switch uid per
# request (SysProcAttr.Credential). -isnotfc: do not poll Firecracker's MMDS
# for config; -no-cgroups: no cgroup v2 setup. vsockfwd makes its TCP port
# reachable from the host over vsock. Both are respawned by a loop rather
# than a supervisor; see NOTES.md "PID 1".
if [ -x /usr/bin/envd ]; then
    if ! id user >/dev/null 2>&1; then
        adduser -D -u 1000 -s /bin/sh -h /home/user user 2>/dev/null
        mkdir -p /home/user && chown user:user /home/user
        chmod 0755 /home/user; chmod g-s /home/user   # busybox adduser sets 02755; GNU chmod keeps a directory's setgid bit even for "0755"
    fi
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

// The host's own resolver (the NAT gateway, 192.168.64.1, served by
// mDNSResponder) ignores UDP from a sandbox for about two seconds after the
// VM's network interface appears: always after a resume from pause, sometimes
// right after boot. Everything else (TCP, ICMP, UDP to the internet) works at
// once. curl's c-ares waits 2 s before trying the next server, so the first
// lookup in a sandbox stalled. Public resolvers answer in ~10 ms from the
// first packet, so the guest uses those and udhcpc is told to leave
// /etc/resolv.conf alone. To use the host's DNS instead (a VPN, say), delete
// the RESOLV_CONF line from /etc/udhcpc/udhcpc.conf in the template.
const udhcpcConf = `# Generated by cmd/mkimage. See NOTES.md, phase 6, "DNS".
RESOLV_CONF="no"
`

const resolvConf = `# Generated by cmd/mkimage. See NOTES.md, phase 6, "DNS".
nameserver 1.1.1.1
nameserver 8.8.8.8
`
