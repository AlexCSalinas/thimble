# thimble

E2B sandboxes on your laptop. Real Linux microVMs on Apple Silicon, same API,
no cloud.

```python
from e2b import Sandbox

sbx = Sandbox.create("base")                    # 0.6 s, 140 MiB
sbx.commands.run("pip install requests")
sbx.pause()                                     # RAM back to the host
sbx = Sandbox.connect(sbx.sandbox_id)           # exactly where you left off
```

The stock E2B SDK talks to it unmodified. Every sandbox is its own kernel,
its own disk, its own network.

## why

I wanted to study how E2B works and my laptop is an 8 GB M2. No KVM, no
nested virtualization, no room for Docker plus a Linux VM. So I rebuilt the
sandbox layer on Apple's hypervisor instead, and it turned out to fit: a
sandbox only costs the pages it touches.

## run it

```sh
make all
make serve                # API :3000, proxy :49983
```

```sh
pip install e2b
export E2B_API_KEY=local E2B_API_URL=http://localhost:3000 E2B_SANDBOX_URL=http://localhost:49983
```

Port 3000 busy: `make serve API_PORT=3100`. Check it works: `make sdk-venv && make smoke`.

## network

By default a sandbox goes out through the framework's NAT. With `-vnet` it
doesn't:

```sh
bin/thimble serve -vnet
```

Each VM gets a private network that lives inside the thimble process. Every
frame it sends lands in a userspace TCP/IP stack on the host, so there is no
other way out, and every sandbox can have the same IP, which the NAT cannot
do. That makes egress policy real, using E2B's own options:

```python
sbx = Sandbox.create("base", network={
    "deny_out": [ALL_TRAFFIC],
    "allow_out": ["api.github.com", "*.pypi.org", "1.1.1.1"],
})
sbx.update_network({"allow_out": ["example.com"], "deny_out": [ALL_TRAFFIC]})
```

IPs and CIDRs are matched on the destination. Domains are matched on the TLS
SNI or HTTP Host the client sends, so `https://github.com` is refused before a
byte reaches it. Allow beats deny. Rules apply to the next connection, and
survive pause and resume.

Every connection and its verdict is kept per sandbox:

```sh
curl -H 'X-API-Key: local' localhost:3000/sandboxes/$ID/network/events
bin/thimble serve -vnet -netlog      # also log allowed connections
```

`make sdk-venv && build/venv/bin/python scripts/netpolicy.py` runs the checks.
Not done: request transforms (`rules`, secret injection) and `egressProxy`,
which need TLS interception. A guest that sends an allowed SNI to a different
IP gets through, and DNS is not filtered.

## inside

| | |
|---|---|
| guest | Alpine 3.22, Linux 6.12, arm64 |
| per sandbox | 1 vCPU, 512 MiB ceiling, 4 GiB disk (APFS clone) |
| tools | python, node, git, curl, sudo, the usual |
| agent | E2B's `envd`, unmodified, over vsock |
| lifecycle | create, connect, list, kill, timeouts, pause, resume |

Not here yet: inbound ports, multiple templates, state across server restarts.

How it works and why, with measurements, is in [NOTES.md](NOTES.md).

## requirements

macOS 14+, Apple Silicon, Go, Xcode command line tools. No Docker. The build
expects E2B's [`infra`](https://github.com/e2b-dev/infra) at `../runtime` for
`envd`, and [`E2B`](https://github.com/e2b-dev/E2B) at `../E2B` for the SDK tests.

## layout

```
cmd/thimble      boot, run, snapshot, mkdisk, serve
cmd/mkimage      guest image and PID 1
internal/vm      Virtualization.framework
internal/sandbox lifecycle
internal/vnet    per-sandbox userspace network, egress policy
internal/api     E2B control plane and envd proxy
```

## license

MIT. `envd` is E2B's and keeps its own license.
