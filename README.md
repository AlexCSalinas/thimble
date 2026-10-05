# thimble

Tiny Linux microVMs on Apple Silicon via Virtualization.framework, built as a
learning re-implementation of E2B's sandbox orchestrator for an 8 GB Mac.
Design notes and E2B comparisons live in [NOTES.md](NOTES.md).

## Phase 1: boot a guest

```sh
make fetch   # downloads Alpine 3.22 minirootfs + linux-virt apk into build/alpine (~40 MB)
make image   # builds build/guest/{vmlinux,initramfs.cpio.gz} with the Go-only cmd/mkimage
make build   # go build + ad-hoc codesign with the virtualization entitlement
make boot    # interactive serial console; Ctrl-] detaches and kills the VM
make bench   # non-interactive: boot, report latency and host memory, stop
```

Scripted sessions work too:

```sh
printf 'uname -a\nlsmod\npoweroff\n' | ./bin/thimble boot
```

Requirements: macOS 14+, Apple Silicon, Go 1.25+, Xcode Command Line Tools
(for cgo and `codesign`). No Docker, no developer account.
