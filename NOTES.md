# thimble design notes

A macOS-native sandbox orchestrator in the spirit of E2B, for an 8 GB M2.
Guests are Linux/arm64 microVMs run through Apple's Virtualization.framework
from Go (`github.com/Code-Hex/vz/v3`). This file records every design decision,
what E2B does on Linux instead, and why thimble differs. Measurements are from
the machine described in the prompt: M2, 8 GB, macOS 26.5.

Reference trees read before designing:

- `../runtime` (E2B backend): `docs/ARCHITECTURE.md`,
  `packages/orchestrator/pkg/sandbox/`, `packages/envd/`.
- `../E2B` (SDKs): `spec/openapi.yml`, `spec/envd/`, `packages/python-sdk/`.

## The one big difference: who is the hypervisor

E2B runs Firecracker on Linux. Firecracker is a userspace VMM talking to KVM;
the orchestrator owns everything around it (memory backing, block devices,
network namespaces, snapshots). On an M2 there is no nested virtualization, so
a Linux VM cannot host Firecracker. Instead thimble *is* the orchestrator and
Virtualization.framework (VZ) is the VMM. VZ gives us, as framework objects,
most of what E2B assembles by hand:

| Need                        | E2B on Linux                                   | thimble on macOS                                   |
|-----------------------------|------------------------------------------------|----------------------------------------------------|
| Run a VM                    | Firecracker process per sandbox, in a netns    | `VZVirtualMachine`; VZ spawns one helper per VM    |
| Lazy memory restore         | userfaultfd server feeding the memfile         | `restoreMachineStateFromURL` (phase 3)             |
| Reclaim idle RAM            | virtio-balloon + free page reporting           | `VZVirtioTraditionalMemoryBalloonDevice` (phase 5) |
| Rootfs per sandbox          | COW block cache served over NBD                | APFS `clonefile` of a raw image + virtio-blk (ph 3)|
| Host <-> envd               | HTTP over veth/tap, per-sandbox netns          | vsock, see "Talking to envd" (phase 2)             |
| Serial console              | ttyS0 via Firecracker                          | virtio console (`hvc0`) bound to a file handle     |

## Phase 1: boot one guest

### Kernel and image: Alpine, not Buildroot

Chosen: Alpine 3.22's `linux-virt` kernel (6.12.111) plus the Alpine
minirootfs, assembled into an initramfs by `cmd/mkimage`.

Why not Buildroot: Buildroot produces a smaller kernel and rootfs, but it means
compiling a toolchain and kernel on this laptop (30 to 60 minutes per
iteration) and maintaining a defconfig. Alpine's virt kernel is prebuilt,
already configured for virtio-pci guests, and its packages are plain tarballs.
E2B builds its own kernels (`firecracker/fc-kernels/`, 6.1.x with custom
configs) because they control the whole fleet and shave every MiB. For a
learning project the stock kernel is the simple way. Cost: see "Memory" below.

Why an initramfs and not a disk image for phase 1: there is no `mkfs.ext4` on
macOS, and the initramfs is just a cpio archive, which `internal/cpio` writes
from Go. Phase 3 needs a block device (per-sandbox writable rootfs), and the
plan is to have the guest itself format it: boot this initramfs with a blank
raw disk attached, run `mkfs.ext4` and copy the rootfs in. No Docker needed.
E2B extracts an OCI image into ext4 on the Linux host (`pkg/template/build`).

### What the guest needs that Alpine makes optional

Alpine's virt kernel builds in virtio-pci and the virtio console, but these
are modules: `virtio_blk`, `virtio_net` (+`net_failover`, `failover`),
`virtio_balloon`, `virtio-rng`, the vsock stack (`vsock`,
`vmw_vsock_virtio_transport_common`, `vmw_vsock_virtio_transport`) and `ext4`
(+`jbd2`, `mbcache`, `crc16`). Modules are shipped gzip-compressed and the
kernel lacks `CONFIG_MODULE_DECOMPRESS`, so `mkimage` gunzips them, resolves
load order from `modules.dep`, and emits explicit `insmod` lines into `/init`.
No modprobe, no udev.

### PID 1 is a 40-line shell script

`/init` mounts proc/sys/dev, loads the 14 modules, prints a ready marker, and
respawns a shell on `hvc0`. busybox `poweroff`/`reboot` signal PID 1 with
SIGUSR2/SIGTERM; the script traps those and calls `poweroff -f`. The console
shell runs in the background with `wait $!` so traps can fire while a shell is
up.

E2B uses systemd (`envd.service`, `Nice=-20`, `OOMScoreAdjust=-1000`) for the
runtime image and a baked busybox init only during template provisioning.
systemd would cost a few hundred ms and tens of MiB per sandbox. In phase 2
envd is started directly from this script; respawn-on-exit is a loop.

### The gotcha that cost the most time: EFI zboot kernels

Alpine's `vmlinuz-virt` for aarch64 is not an arm64 `Image`. Since Linux 6.2
distros ship an EFI "zboot" binary: a PE/COFF wrapper (`MZ\0\0zimg` header)
around a gzip-compressed Image that the kernel's own EFI stub inflates at
boot. `file` reports it as "PE32+ executable (EFI application)", which looks
right and is not. `VZLinuxBootLoader` has no EFI stub to run; it wants the raw
Image with `ARM\x64` at offset 0x38, and it fails with an opaque
`VZErrorDomain Code=1 "Internal Virtualization error"` otherwise. The helper
process logs only a breadcrumb hex code. `mkimage.rawImage` detects the zboot
header (payload offset at byte 8, size at 12, compression name at 0x18) and
inflates it. The 9.2 MiB file becomes a 33.1 MiB Image.

(First suspect was the Claude Code command sandbox blocking the hypervisor
XPC service. It was not: the helper started fine and died on the kernel.)

### Codesigning

VZ refuses to start a VM from a binary without the
`com.apple.security.virtualization` entitlement. `go build` output is unsigned,
so the Makefile runs
`codesign --force --sign - --entitlements entitlements.plist bin/thimble`.
An ad-hoc signature (`-s -`) is enough; no Apple developer account. Consequence:
`go run ./cmd/thimble` can never work, and every rebuild re-signs.

### Where the guest's memory actually lives

Since macOS 13, VZ does not run the guest inside the calling process. Each
`VZVirtualMachine` gets its own XPC helper,
`com.apple.Virtualization.VirtualMachine`, launched by launchd (parent pid 1,
not us). Guest RAM is in that helper. Measuring our own RSS reports a
meaningless 14 MiB. `internal/hostmem` snapshots the set of helper pids before
and after `Start` to find ours, then reads `proc_pid_rusage` for
`ri_resident_size` and `ri_phys_footprint`.

For the orchestrator this matters beyond measurement: a sandbox is a separate
process the kernel can account, pressure and kill independently, which is
closer to Firecracker's one-process-per-VM model than it first appears.

### Measurements (phase 1, `make bench`, 3 runs each)

| guest RAM | vm.Start to /init | kernel's own clock at /init | helper resident | helper phys_footprint | guest MemTotal / MemAvailable |
|-----------|-------------------|-----------------------------|-----------------|-----------------------|-------------------------------|
| 128 MiB   | 227 to 229 ms     | 90 ms                       | 154 MiB         | 112 MiB               | 94 MiB / 63 MiB               |
| 256 MiB   | 228 to 230 ms     | 90 ms                       | 156 MiB         | 115 MiB               | 222 MiB / 190 MiB             |

Things to notice:

- The host cost is the same at 128 and 256 MiB. VZ backs guest RAM lazily;
  what you pay for is pages the guest has touched, not the configured size.
  Configured size is a ceiling, not a cost. This is the same reason E2B can
  overcommit.
- The guest reports 18 MiB used, the host sees 112 MiB. The difference is
  pages the guest touched once and freed: the compressed initramfs the loader
  placed in RAM, decompression buffers, the kernel's early allocations. Linux
  has no reason to tell the host those pages are free. E2B closes this gap with
  the balloon in "free page reporting" mode plus a pre-pause hint drain
  (`pkg/sandbox/fph.go`). Phase 5 uses the traditional balloon to the same end.
- The 33 MiB kernel Image is the largest single fixed cost, and the kernel
  reserves another chunk for itself (MemTotal is 34 MiB below configured). A
  tailored kernel config would roughly halve this. Not doing it yet: simple
  way first.
- 90 ms of the 228 ms is Linux. The remaining ~140 ms is VZ creating the
  helper, mapping a 33 MiB kernel and a 5 MiB initramfs, and starting the vCPU.
  Phase 3's snapshot restore skips the Linux part entirely and is the number
  that matters for "create sandbox".
- Budget on this machine, idle: about 115 MiB per sandbox, so roughly 35
  idle sandboxes in 4 GB before any balloon work. envd will add to that.

### Not done in phase 1, deliberately

- Networking is a NAT device the guest has not configured (`eth0` is DOWN, no
  DHCP client run). Phase 2 or 4 brings it up with `udhcpc` for outbound
  access; host-to-guest traffic goes over vsock.
- No block device attached. Phase 3.
- The busybox shell emits an `ESC[6n` cursor-position query on the console. It
  is harmless and disappears once the console is a real terminal.

## Talking to envd (decision for phase 2, recorded now)

The prompt asks for vsock. E2B does not use vsock at all: the orchestrator
speaks HTTP/1.1 to `http://<slot host ip>:49983` over a veth pair into the
sandbox's netns (`pkg/sandbox/envd.go envdServerURL`, `consts.DefaultEnvdServerPort = 49983`).
envd listens on `0.0.0.0:49983` TCP and knows nothing about `AF_VSOCK`.

Reusing envd unchanged while keeping vsock as the transport therefore needs a
tiny forwarder inside the guest: listen on vsock port 49983, dial
`127.0.0.1:49983`, splice. Plan: a ~60-line static Go binary built into the
image alongside envd, started by `/init`. On the host, `VirtioSocketDevice.Connect(49983)`
returns a `net.Conn`, and a `net/http` client with a custom `DialContext` uses
it. Alternative considered: skip vsock and talk TCP to the guest's NAT address
like E2B does. Rejected for now because NAT addresses are assigned by the
host's DHCP and would need discovery, while vsock is addressable the instant
the VM exists, which is what "create sandbox is a restore" needs.

Other envd facts that shape phase 2:

- Build: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath` from
  `packages/envd` with `-X pkg.Version=...`. Static, so it runs on musl.
- Run with `-isnotfc`, otherwise envd polls Firecracker's MMDS at
  `169.254.169.254` for its config.
- Handshake: `POST /init` with `envVars`, `accessToken`, `defaultUser`,
  `defaultWorkdir`, `timestamp`; expects 204. `GET /health` expects 204.
- Services are Connect RPC with JSON codec: `/process.Process/Start`
  (server stream), `/filesystem.Filesystem/*`.

## Phase 2: envd over vsock

Measured (3 runs): userspace ready 480 to 590 ms after `Start`, envd
healthy 15 to 30 ms after that, `echo hello` round trip through
`process.Process/Start` 1.9 ms, helper footprint 132 MiB. Boot got slower than
phase 1 because the initramfs doubled (envd is a 10 MiB static binary) and the
guest's own clock went from 90 to 240 ms; the kernel inflates and unpacks the
cpio before `/init` runs. A block-device rootfs would avoid that (phase 3+).

### Reusing envd unchanged

envd is cross-compiled from `../runtime/packages/envd` with
`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`, the same recipe as E2B's Makefile. It
runs as root (it switches uid per request via `SysProcAttr.Credential`) with
`-isnotfc` (do not poll Firecracker's MMDS for config) and `-no-cgroups`. The
guest gets a `user` account at boot, and `POST /init` sets `defaultUser=user`,
`defaultWorkdir=/home/user`, which is what E2B's orchestrator sends.

### vsock, with a forwarder

E2B never uses vsock. The orchestrator talks plain HTTP to
`http://<slot ip>:49983` over a veth pair into the sandbox's network
namespace. Here the host has no such address (NAT assigns one by DHCP, which
would need discovery), while vsock is addressable the moment the VM exists.
envd only listens on TCP, so `cmd/vsockfwd` runs in the guest: it binds
`AF_VSOCK` port 49983 and splices each connection to `127.0.0.1:49983`. It is
written against raw syscalls because `net` has no vsock support and `syscall`
has no `SockaddrVM` (the struct is 16 bytes, easy to hand-roll). On the host,
`internal/envd` is an `http.Client` whose `DialContext` returns
`VirtioSocketDevice.Connect(49983)`.

### Connect RPC by hand

envd's services use the Connect protocol. With the JSON codec a server-stream
call is just `POST /process.Process/Start` with
`Content-Type: application/connect+json`, a request body of one envelope
(1 flag byte, 4-byte big-endian length, JSON), and a chunked response of
envelopes where flag `0x02` marks the trailer carrying `{"error": ...}`. That
is ~60 lines in `internal/envd/client.go` and avoids pulling in
`connectrpc.com/connect` and protobuf. Event JSON is proto3 JSON: `exitCode`,
bytes as base64, which `encoding/json` decodes into `[]byte` directly.

## Phase 3: snapshot and restore

"Create sandbox" is now: build a VM with the saved configuration, restore the
state file, resume, reconnect to envd, re-run `/init` so the guest clock is
corrected. That is E2B's model too: `ResumeSandbox` loads a Firecracker
snapshot and then `WaitForEnvd` posts `/init`.

### What must match

`restoreMachineStateFromURL` rejects the file with a bare "invalid argument"
unless the VM configuration is identical to the one saved. Two things are
random per configuration by default and therefore have to be recorded in
`snapshot.json`: the network device's MAC and the generic platform's machine
identifier (`VZGenericMachineIdentifier`, opaque bytes). The state file also
cannot be overwritten; `snapshot` removes the old one first.

### Measurements (`make restore`, `-idle 96`)

| N  | restore (first .. last) | envd healthy after restore | total helper footprint | per sandbox |
|----|-------------------------|----------------------------|------------------------|-------------|
| 1  | 240 to 360 ms           | +3 ms                      | 206 MiB                | 206 MiB     |
| 5  | 244 to 318 ms           | +3 ms                      | 1035 MiB               | 207 MiB     |
| 10 | 242 to 520 ms           | +3 to +7 ms                | 1471 MiB               | 147 MiB     |

The state file is 23 MiB: the framework stores touched pages, compressed.
Restore beats cold boot by about 2x (260 vs 550 ms to a healthy envd) and the
guest does no work at all: envd answers 3 ms after `Resume`. Restore latency
climbs with N as host memory fills; the tenth restore takes twice the first.

### Why a restored sandbox costs 2.5x a booted one, and the balloon

A cold-booted guest costs 132 MiB because the host only backs pages the guest
has touched. A restored guest costs 355 to 367 MiB: restore writes every one
of the 256 MiB guest pages, zero or not, and macOS has no equivalent of the
lazy page-in E2B gets from userfaultfd (`pkg/sandbox/uffd`), where a page is
only materialised when the guest faults on it.

The only lever on macOS is the virtio balloon. Experiment, one restored
sandbox, footprint sampled for 6 s:

| strategy                                   | footprint |
|--------------------------------------------|-----------|
| restore only                               | 367 MiB   |
| inflate balloon to 64 MiB, keep inflated   | 211 MiB   |
| inflate to 64 MiB, then deflate to 256 MiB | 537 MiB   |

Inflating works: the guest hands its free pages to the balloon, the host
unmaps them. Deflating is worse than never inflating, by 170 MiB; the
framework appears to repopulate the returned range eagerly. So the policy is
"inflate after restore and stay inflated" (`restore -idle 96`, default).
`-idle` is the memory the guest keeps, not the balloon size. 96 MiB leaves
envd room; raise it per sandbox when a workload needs more and accept the
one-time cost. E2B avoids this dance with free page reporting
(`pkg/sandbox/balloon_mode.go`), a newer balloon feature where the guest
reports freed pages continuously; Apple's "traditional" balloon device does
not offer it.

At N=10 the per-sandbox number drops to 147 MiB because macOS starts
compressing the helpers' idle pages; `phys_footprint` counts compressed pages
at their compressed size.

### Still an initramfs

Every restored sandbox shares the one state file and has its rootfs in RAM.
No per-sandbox disk, no COW, no NBD. E2B's rootfs path (`pkg/sandbox/rootfs`,
`pkg/sandbox/nbd`) exists because templates are gigabytes and sandboxes write
to them. Here the rootfs is 20 MiB and writes land in tmpfs, counted against
the sandbox's memory. The APFS `clonefile` design from phase 1 is still the
plan if a disk is ever needed; it was not needed for any phase.
