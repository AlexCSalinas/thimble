# thimble

thimble is a sandbox orchestrator for macOS on Apple Silicon. It runs Linux
microVMs through Virtualization.framework and exposes the E2B control plane
API, so the unmodified E2B SDKs can create sandboxes on a laptop and run code
in them. It was written as a study of E2B's architecture and what changes
when the hypervisor is Apple's instead of Firecracker on KVM.

Each sandbox is a full Linux VM: its own kernel, memory, root disk and
network address. Creating one takes about 0.6 s and costs about 140 MiB of
host memory.

## Quick start

```sh
make all     # fetch Alpine, build the guest images and the template disk (needs network)
make serve   # API on 127.0.0.1:3000, sandbox proxy on 127.0.0.1:49983
```

From another shell, with `pip install e2b`:

```sh
export E2B_API_KEY=local
export E2B_API_URL=http://localhost:3000
export E2B_SANDBOX_URL=http://localhost:49983
```

```python
from e2b import Sandbox

sbx = Sandbox.create("base")
print(sbx.commands.run("python3 -c 'print(2**100)'").stdout)
sbx.commands.run("pip install requests")
sbx.files.write("/home/user/notes.txt", "kept on the sandbox's disk")

sbx.pause()                           # memory and disk saved, host memory freed
sbx = Sandbox.connect(sbx.sandbox_id) # resumed where it left off
print(sbx.files.read("/home/user/notes.txt"))
sbx.kill()
```

`make sdk-venv && make smoke` runs a representative workload against a
running server: Python, pip, git clone, HTTPS, node, sudo, the files API,
pause and resume. If port 3000 is in use, run `make serve API_PORT=3100`
and set `E2B_API_URL` accordingly (`make smoke API_PORT=3100`).

## What you get

| | |
|---|---|
| Guest | Alpine Linux 3.22, kernel 6.12 (`linux-virt`), arm64 |
| Resources | 1 vCPU, 512 MiB RAM ceiling per sandbox; only touched pages cost host memory |
| Root disk | 4 GiB ext4, an APFS clone of the template image; writes persist for the sandbox's life |
| Network | outbound through the framework's NAT; DNS via 1.1.1.1 and 8.8.8.8; no inbound ports |
| Toolset | bash, coreutils, python3 + pip, node + npm, git, curl, wget, jq, make, openssh-client, sudo |
| User | commands run as `user` in `/home/user` with passwordless sudo, as on E2B |
| Agent | E2B's `envd`, unmodified, reached over vsock |
| Lifecycle | create, connect, list, kill, timeouts, auto-pause, pause and resume |

The package list is `GUEST_PKGS` in the Makefile. Change it, delete
`build/guest/rootfs.img`, and run `make disk`.

## How it works

`thimble serve` is E2B's API server, orchestrator and client proxy in one
process. The control plane keeps an in-memory table of sandboxes; the proxy
on port 49983 routes SDK traffic to the right VM using the `E2b-Sandbox-Id`
header the SDK sends, or the URL signature for signed file downloads.

Creating a sandbox clones the template disk with `clonefile(2)`, builds a
`VZVirtualMachine` with a virtio console, block device, NAT network device,
vsock, balloon and entropy device, and boots it. A 5 MiB initramfs loads the
virtio and ext4 modules, mounts the disk and `switch_root`s to it. PID 1 is
a shell script that brings up networking with `udhcpc` and starts `envd`.
The host waits for `envd` to answer over vsock, posts `/init` with a fresh
access token, and returns the sandbox to the SDK. About 600 ms in total.

Pausing a sandbox saves the VM's memory and device state next to its disk
and stops the VM, so a paused sandbox uses no host memory. Connecting to it
restores that state into a new VM with the same MAC and machine identifier.

The template disk is built by the guest itself, since macOS has no
`mkfs.ext4`: `thimble mkdisk` boots a full initramfs with a blank image
attached, and the guest formats it, copies its root filesystem in, and runs
`apk add` for the package list.

Design decisions, measurements and comparisons with E2B's Linux
implementation are in [NOTES.md](NOTES.md). Two findings shaped the
current design:

- Virtualization.framework restores a saved VM only with the exact
  configuration it was saved with, MAC address included, and its NAT
  delivers frames only to that MAC. Several sandboxes restored from one
  snapshot therefore share a MAC and a DHCP lease, and only one of them has
  a working network. Create is a cold boot from disk for that reason; the
  snapshot-restore path from phase 3 remains as a benchmark.
- A restored VM has every guest page materialised on the host, roughly 2.5x
  the footprint of a booted one. The balloon device can reclaim it, but
  deflating later costs more than never inflating.

## Commands

| target | what it does |
|---|---|
| `make all` | fetch, image, build, disk |
| `make serve` | run the API and proxy |
| `make smoke` | end-to-end workload against a running server |
| `make sdk-test` | E2B's Python SDK test suite against a running server (112 of 114 pass; the rest need an HTTPS ingress) |
| `make boot` | one guest with its serial console on the terminal, Ctrl-] to exit |
| `make run CMD='uname -a'` | boot a guest, run one command through envd, print timings |
| `make snapshot`, `make restore N=1,5,10` | the snapshot-restore benchmark from phase 3 |
| `make disk` | rebuild the template disk |

`bin/thimble` has the same subcommands with flags; `bin/thimble serve -h`
lists the server's (memory and CPU per sandbox, data directory, maximum
sandbox count, API key).

## Requirements

- macOS 14 or later on Apple Silicon.
- Go 1.25 or later and the Xcode Command Line Tools, for cgo and `codesign`.
  The binary is signed ad hoc with the virtualization entitlement; no
  developer account is needed, but `go run` cannot be used.
- E2B's [`infra`](https://github.com/e2b-dev/infra) repository checked out
  at `../runtime`, to cross-compile `envd` for the guest. The
  [`E2B`](https://github.com/e2b-dev/E2B) repository at `../E2B` is needed
  only for `make sdk-venv` and `make sdk-test`.
- No Docker.

## Limitations

- One template. `templateID` is accepted and recorded but every sandbox
  boots the same image.
- No inbound connections. `sandbox.get_host(port)` has no ingress to point
  at, and the code-interpreter SDK's `run_code` needs a Jupyter kernel that
  the template does not include.
- State lives in the server process. Restarting `thimble serve` loses all
  sandboxes, running or paused.
- Capacity is whatever the machine has. On an 8 GB machine expect 10 to 15
  concurrent sandboxes; `thimble serve -max N` enforces a limit.
- DNS inside a sandbox goes to public resolvers, not the host's. Private
  names behind a VPN do not resolve from inside a sandbox.

## Layout

```
cmd/thimble      the CLI: boot, run, snapshot, restore, mkdisk, serve
cmd/mkimage      builds the kernel image, initramfs files and PID 1 script from Alpine packages
cmd/vsockfwd     guest-side forwarder from vsock to envd's TCP port
internal/vm      Virtualization.framework wrapper (github.com/Code-Hex/vz)
internal/sandbox the sandbox table and lifecycle: create, kill, pause, resume, timeouts
internal/api     the E2B control plane endpoints and the envd proxy
internal/envd    a minimal client for envd's REST and Connect RPC endpoints
internal/snapshot, internal/cpio, internal/hostmem, internal/rawterm
scripts/smoke.py the end-to-end check
```
