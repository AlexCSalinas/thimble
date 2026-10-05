# thimble

Tiny Linux microVMs on Apple Silicon via Virtualization.framework, built as a
learning re-implementation of E2B's sandbox orchestrator for an 8 GB Mac.
Design notes and E2B comparisons live in [NOTES.md](NOTES.md).

## Phases

| phase | what                                                         | command         |
|-------|--------------------------------------------------------------|-----------------|
| 1     | boot one Alpine guest, measure boot time and host memory     | `make boot`, `make bench` |
| 2     | run a command through E2B's envd over vsock                  | `make run CMD='uname -a'` |
| 3     | snapshot a ready guest, create N sandboxes by restoring it   | `make snapshot`, `make restore` |
| 4     | E2B-compatible API + envd proxy; stock Python SDK works      | `make serve`, `make sdk-test` |
| 5     | pause/resume via the API, backed by save/restore             | part of `make serve` |

```sh
make serve &
E2B_API_KEY=x E2B_API_URL=http://localhost:3000 E2B_SANDBOX_URL=http://localhost:49983 \
  build/venv/bin/python -c 'from e2b import Sandbox; s=Sandbox.create("base"); print(s.commands.run("uname -a").stdout); s.kill()'
```

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
