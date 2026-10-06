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

## Phase 4: the E2B control plane, enough of it

`thimble serve` runs two listeners: the control plane on `:3000` and an envd
proxy on `:49983`. The stock Python SDK (`e2b` 2.51.0, unmodified) is pointed
at it with three environment variables and nothing else:

```sh
E2B_API_KEY=anything            # required by the SDK; thimble accepts any non-empty key unless -api-key is set
E2B_API_URL=http://localhost:3000
E2B_SANDBOX_URL=http://localhost:49983
```

### What the SDK actually needs

Read from `spec/openapi.yml` and the SDK's generated client:

| SDK call               | HTTP                                   | thimble                                   |
|------------------------|----------------------------------------|-------------------------------------------|
| `Sandbox.create`       | `POST /v2/sandboxes` -> 201 `Sandbox`  | restore snapshot, `/init` with fresh token|
| `Sandbox.connect`      | `POST /v2/sandboxes/{id}/connect`      | 200 if running (TTL only extended), 201 if resumed |
| `kill`                 | `DELETE /sandboxes/{id}` -> 204/404    | stop VM, forget                           |
| `Sandbox.list`         | `GET /v2/sandboxes?metadata=&state=`   | filter the in-memory table                |
| `set_timeout`          | `POST /sandboxes/{id}/timeout` -> 204  | reset a `time.AfterFunc`                  |
| `get_info`             | `GET /sandboxes/{id}` -> `SandboxDetail`| `endAt`, `state`, `metadata`, sizes      |
| `pause`                | `POST /sandboxes/{id}/pause` -> 204    | phase 5                                   |
| `is_running`           | `GET /health` on the **sandbox** URL   | proxied to envd; 502 when not running     |

Auth is the `X-API-Key` header. `clientID` is deprecated in the spec and
returned as a constant. `envdAccessToken` is generated per sandbox and handed
to envd in `/init`; envd itself then rejects requests without the matching
`X-Access-Token`, so the proxy does no auth of its own.

### Routing without hostnames

E2B reaches a sandbox at `https://49983-<id>.<domain>` (or the stable
`https://sandbox.<domain>` with routing headers). Both need DNS and TLS,
which a laptop does not have. `E2B_SANDBOX_URL` makes the SDK send all envd
traffic to one base URL, and because the SDK attaches `E2b-Sandbox-Id` to
every envd request (`sandbox_sync/main.py`), the proxy can route on that
header to the right VM's vsock. Each sandbox owns an `http.Transport` whose
dialer is its `VirtioSocketDevice.Connect`. `httputil.ReverseProxy` with
`FlushInterval: -1` carries Connect streams unbuffered. Header-less requests
(signed download URLs opened with `urllib`) fall back to "the only running
sandbox", which is the one thing a single shared URL cannot express.

### Two things the guest image had to grow

- `/bin/bash`: the SDK runs every command as `/bin/bash -l -c`. Alpine's
  bash plus readline, libncursesw and terminfo are unpacked from their apk
  tarballs by `mkimage -apks`, no package manager involved.
- `chmod 0755 /home/user`: busybox `adduser` creates the home with mode
  `02755`; every directory created under it inherited setgid and envd's
  `ListDir` reported `dgrwxr-xr-x`, failing two files tests.

### A killed sandbox mid-stream

When a sandbox is killed while a command is streaming, the upstream vsock
connection dies and `ReverseProxy` would reset the client. E2B's proxy
instead emits a Connect end-of-stream envelope with code `unavailable` at
the next frame boundary, which the SDK maps to a `TimeoutException`
explaining the sandbox is gone. `eofOnError` in `internal/api` does the
same.

### Test results (`make sdk-test`)

Against `tests/sync/sandbox_sync/` and `tests/async/sandbox_async/`:

| suite                              | passed | failed | why the failures are not fixable here |
|------------------------------------|--------|--------|---------------------------------------|
| sync test_create/connect/kill/timeout | 27  | 2      | see below                             |
| async test_create/connect/kill/timeout| 27  | 2      | same two                              |
| sync commands/ + files/            | 82     | 3      | guest has no `sudo`, no `python3`     |

Skipped / failing, and why:

- `test_auto_pause_filesystem_only_reboots`: `keep_memory=False` means "drop
  memory, cold-boot from disk on resume". thimble has no per-sandbox disk;
  the rootfs is an initramfs, so a cold boot would lose the files the test
  checks. Needs the virtio-blk + clonefile design.
- `test_auto_resume_wakes_on_http_request`: expects a public
  `https://8000-<id>.e2b.app` URL to wake a paused sandbox. Needs an ingress
  with DNS and TLS that resumes on first packet. Out of scope.
- `test_bash_command_scoped_env_vars`, `test_python_command_scoped_env_vars`,
  `test_metadata_set_via_xattrs_surfaced_in_get_info`: run `sudo` or
  `python3`. `sudo` is in Alpine's community repo and needs sudoers setup;
  `python3` is 35 MiB that would sit in guest RAM for every sandbox with the
  initramfs design. Both are `mkimage -apks` additions once there is a disk.
- `test_host.py` (`get_host(port)` to a `https://8001-<id>.<domain>` URL):
  not run, same ingress reason.

### What E2B has that this does not

Templates (every create restores the one snapshot; `templateID` is
recorded, not resolved), teams and auth, Postgres/Redis state, metrics,
volumes, MCP gateway, network policies, the orchestrator/API split over
gRPC, and the proxies that make `49983-<id>.e2b.app` resolve to the right
host. The shape is the same; the table is a Go map.

## Phase 5: pause and resume

`POST /sandboxes/{id}/pause` does: stop the TTL timer, `Pause` the VM,
`SaveMachineStateToPath` into `build/snap/paused/<id>/`, `Stop` the VM. The
helper process exits, so a paused sandbox costs zero host memory, not "less".
`connect` on a paused sandbox restores that file into a fresh VM (same MAC
and machine id, recorded on the sandbox), resumes, waits for envd, re-posts
`/init` with the same access token (envd accepts a matching token), re-arms
the TTL, and answers 201. `autoPause` on create makes TTL expiry pause
instead of kill, which is how the SDK's `lifecycle={"on_timeout": "pause"}`
tests passed, including the one that checks `boot_id` is unchanged across
the pause (it is: this is a memory restore, not a reboot).

From the server log for one sandbox:

```
create  took 354 ms  footprint 356 MiB
pause   footprint 300 MiB -> 0 (helper exited), state file 24 MiB, took 331 ms
resume  took 249 ms  footprint 353 MiB
```

Where the balloon comes in: the sandbox has been running with the balloon
inflated to `-idle` since it was created, so at pause time the guest's free
pages are already with the host and the state file holds only what is in
use (24 MiB for a 256 MiB guest). After resume the balloon is inflated again
before the sandbox is handed back. E2B's pause (`sandbox.go Pause`) is a
much longer sequence for the same idea: freeze cgroups, `fstrim`, `sync`,
drop caches, drain the free-page-hinting balloon, then snapshot, then diff
the memory file against the template so only changed pages are uploaded. It
has to be, because its snapshots go to object storage and are restored on
another machine; thimble's go to a local directory and come back on the
same one.

## Phase 6: a sandbox an agent can use

Phases 1 to 5 proved the mechanics on a 20 MiB initramfs with no network.
An agent needs `pip install`, `git clone`, `curl`, node, and a disk that
keeps what it writes. This phase adds those and, in doing so, changes what
"create sandbox" means on this host.

### Networking: one module and one hard limit

Outbound networking was a single missing kernel module. Alpine's virt
kernel builds `CONFIG_PACKET` as a module and udhcpc needs an `AF_PACKET`
socket to broadcast DHCP DISCOVER; without `af_packet.ko` it fails with
"Address family not supported by protocol". With it, the framework's NAT
hands out a lease from `192.168.64.1` in ~50 ms and TCP, UDP, HTTPS all
work. `/init` starts `udhcpc -b` in the background so envd is not held up.

Then the finding that shaped the rest of the phase. Two experiments:

1. A restore rejects any configuration change, MAC included (`VZErrorDomain
   Code=12 "invalid argument"` with a different MAC in `snapshot.json`).
2. The NAT delivers frames only to the MAC the device was configured with.
   `ip link set eth0 address` inside the guest kills its connectivity, and
   two sandboxes restored from one snapshot get the same lease
   (`192.168.64.6` for both) and only one of them can reach the network.

So N sandboxes restored from one snapshot cannot all have networking. E2B
does not have this problem: every Firecracker sandbox lives in its own
network namespace with the same private IP, NATed to a per-sandbox slot
address by the host's iptables. vmnet offers no equivalent.

Two ways out: a pool of per-MAC snapshots (K slots, each snapshotted with
its own MAC and its own base disk; create picks a free slot), or a cold boot
per create with a fresh random MAC. Measured against the disk template:

| create strategy        | create latency | host footprint after create |
|------------------------|----------------|-----------------------------|
| restore (phase 3)      | ~250 ms        | ~360 MiB (every page written) |
| cold boot from disk    | ~600 ms        | ~140 MiB (only touched pages) |

Cold boot won: 350 ms slower, 2.5x cheaper in RAM, no slot bookkeeping,
no stale DHCP leases inside snapshots. Pause and resume are unchanged, per
sandbox, with that sandbox's own MAC. The slot pool is the way back to
restore-based create if latency ever matters more than memory.

### The disk: the guest formats it

macOS has no `mkfs.ext4`. `thimble mkdisk` creates a sparse raw file, boots
the full initramfs with it attached and `thimble.mkdisk=1 thimble.pkgs=...`
on the kernel command line, and `/init` does the rest: `apk add e2fsprogs`
into the RAM root, `mkfs.ext4`, copy the initramfs root onto the disk,
`chroot` + `apk add` the package list from the Alpine mirror, create the
`user` account with passwordless sudo, delete python's `EXTERNALLY-MANAGED`
marker so `pip install` works, power off. ~10 s, 220 MiB allocated of a 4
GiB sparse image. E2B builds templates by extracting an OCI image into
ext4 on the Linux host (`pkg/template/build`); here the only Linux around
is the guest.

Two gotchas: ext4's mount needs `crc32c` (`crc32c_generic` + `libcrc32c`
modules; the error is "Cannot load crc32c driver" and the mount fails with
ENOENT), and `( set -e; ... ) && ok || fail` silently disables `set -e`
inside the subshell, so the first build reported success with an empty
disk. The result is checked through `$?` now.

Each sandbox boots `boot.cpio.gz` (minirootfs + modules, 5 MiB; the 11 MiB
full image is only for `boot`/`run`/`snapshot`/`mkdisk`) with
`thimble.root=/dev/vda`. `/init` loads the modules, mounts the disk and
`switch_root`s to the same script on the disk, which then runs its normal
path with guards for the mounts and modules that already exist.

Per-sandbox disks are APFS clones (`clonefile(2)`) of the template image:
instant, copy-on-write, and the framework locks each open image, which is
why two sandboxes cannot share one file. `mkfs` runs with
`lazy_itable_init=0` so the kernel's `ext4lazyinit` thread does not spend
every sandbox's first minutes dirtying COW blocks. Kill deletes the clone;
pause keeps it and writes the memory state next to it, so a paused sandbox
is a directory, not a process.

### DNS

After a resume the host's own resolver (the NAT gateway, mDNSResponder on
`192.168.64.1`) ignores UDP from the sandbox for about two seconds; TCP,
ICMP and UDP to the internet work from the first packet, and the same stall
sometimes appears right after a cold boot. Probed by sending raw DNS
queries at 150 ms intervals after resume: host resolver dead until t+2.2 s,
`1.1.1.1` answering at t+0. Disabling checksum offload changed nothing.
musl's resolver queries every nameserver in parallel and would hide it, but
curl uses c-ares, which waits 2 s before trying the next server, so the
first `curl` in a sandbox stalled. The guest now uses `1.1.1.1` and
`8.8.8.8` directly (`RESOLV_CONF="no"` in `/etc/udhcpc/udhcpc.conf`), at
~10 ms per lookup instead of 2 ms, and a VPN's private names are not
resolvable from inside a sandbox. Deleting that line in the template brings
the host's DNS back.

### Two small things the SDK test suite caught

- Signed download URLs (`sandbox.download_url(path)`) carry no
  `E2b-Sandbox-Id` header and are opened by `urllib`. Phase 4 routed them to
  "the only running sandbox", which stops working as soon as two are up. The
  signature is `sha256("<path>:<read|write>:<user>:<envd access token>[:<exp>]")`
  (`e2b/sandbox/signature.py`), and the proxy knows every sandbox's token, so
  it recomputes the signature per running sandbox and routes to the match.
  E2B never needs this: its hostnames carry the sandbox id.
- `coreutils` replaced busybox's `chmod`, and GNU `chmod 0755` on a directory
  keeps an existing setgid bit (a documented POSIX allowance). busybox
  `adduser` creates `/home/user` as `02755`, so every subdirectory came back
  from envd as `dgrwxr-xr-x` again. `chmod g-s,0755` clears it in both
  implementations.

### Measurements (`make smoke`, M2 8 GB)

```
create                        0.6 s    footprint 139 MiB
python3 -c ...                0.08 s
curl https://example.com      0.12 s   (first lookup in the sandbox included)
pip install requests          2.1 s
git clone (small repo)        0.45 s
pause                         0.6 s    state file 68 MiB, footprint -> 0
resume (Sandbox.connect)      0.4 s    footprint 625 MiB (restore writes every page of the 512 MiB guest)
whole smoke run               6 s
```

The resume footprint is the phase 3 problem again: a restore materialises
the whole guest. `serve -idle N` inflates the balloon after a resume, but a
guest capped at N MiB thereafter is a bad trade for an agent that may
`pip install` something large, so the default is off and macOS's compressor
is left to reclaim the zero pages over the following seconds.

### SDK test results (`make sdk-test`, sync create/connect/kill/timeout + commands + files)

112 passed, 2 failed. The phase 4 failures for `sudo` and `python3` are gone
(both are in the template now), and so is the xattr metadata one. The two
left are the same two as before: `test_auto_pause_filesystem_only_reboots`
(`keep_memory=False` means "drop memory, reboot from disk on resume"; the
disk exists now, so this is implementable: a resume with no state file is a
cold boot of the sandbox's own clone) and
`test_auto_resume_wakes_on_http_request` (needs an ingress that resumes a
paused sandbox on the first packet to `https://8000-<id>.<domain>`).

### What this is not

One template (`templateID` is accepted, not resolved), one sandbox size,
no inbound ports (`sandbox.get_host(port)` has no ingress to point at), no
persistence across a `thimble serve` restart (the sandbox table is in
memory; stale directories are deleted at startup), and no resource limits
beyond `-max`. The code-interpreter SDK (`run_code`) needs a Jupyter kernel
in the template; the plain `e2b` SDK's `commands.run("python3 ...")` is
what works today.
