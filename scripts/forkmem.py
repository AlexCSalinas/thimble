"""Host memory cost of fork on macOS: per-helper phys_footprint and RSS, plus
system compressor and swap, at each stage. Run against a server started with
-vnet:  build/venv/bin/python scripts/forkmem.py 5"""
import json, os, re, subprocess, sys, time, urllib.request
from e2b import Sandbox

API = os.environ["E2B_API_URL"]
N = int(sys.argv[1])

def helpers():
    out = subprocess.run(["pgrep", "-f", "com.apple.Virtualization.VirtualMachine"], capture_output=True, text=True).stdout.split()
    return [int(p) for p in out]

def fp(pid):
    # phys_footprint in MiB via footprint(1)
    r = subprocess.run(["footprint", str(pid)], capture_output=True, text=True).stdout
    m = re.search(r"Footprint:\s+([\d.]+)\s*(KB|MB|GB)", r)
    if not m: return None
    v = float(m.group(1)); return v / 1024 if m.group(2) == "KB" else v * 1024 if m.group(2) == "GB" else v

def rss(pid):
    return int(subprocess.run(["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True).stdout.strip() or 0) / 1024

def sysmem():
    vs = subprocess.run(["vm_stat"], capture_output=True, text=True).stdout
    page = int(re.search(r"page size of (\d+)", vs).group(1))
    g = lambda k: int(re.search(k + r":\s+(\d+)", vs).group(1)) * page / 2**20
    sw = subprocess.run(["sysctl", "-n", "vm.swapusage"], capture_output=True, text=True).stdout
    used = re.search(r"used = ([\d.]+)M", sw)
    return dict(compressor=g("Pages occupied by compressor"), swap=float(used.group(1)) if used else 0, free=g("Pages free"))

def snap(label):
    time.sleep(6)  # let balloons and the compressor settle
    hs = helpers()
    f = [fp(p) or 0 for p in hs]; r = [rss(p) for p in hs]
    s = sysmem()
    print(f"{label:34} VMs={len(hs):2}  footprint={sum(f):7.0f} MiB (avg {sum(f)/max(len(hs),1):5.0f})  rss={sum(r):6.0f} MiB  "
          f"compressor={s['compressor']:6.0f}  swap={s['swap']:6.0f}  free={s['free']:5.0f}", flush=True)

snap("baseline (no sandboxes)")
p = Sandbox.create("base", timeout=600)
p.commands.run("python3 -c 'print(1)'")
snap("1 sandbox, idle")
req = urllib.request.Request(f"{API}/sandboxes/{p.sandbox_id}/fork", method="POST", data=json.dumps({"count": N}).encode(),
                             headers={"X-API-Key": "local", "Content-Type": "application/json"})
t = time.time(); kids = json.load(urllib.request.urlopen(req, timeout=180)); dt = time.time() - t
print(f"fork x{N} took {dt:.1f}s")
snap(f"parent + {N} forks, idle")
# every child dirties 100 MiB, as a real workload would
for k in kids:
    Sandbox.connect(k["sandboxID"]).commands.run("python3 -c \"b=bytearray(100*1024*1024); b[::4096]=b'x'*len(b[::4096])\"")
snap(f"parent + {N} forks, 100 MiB each touched")
for k in kids: Sandbox.connect(k["sandboxID"]).kill()
p.kill()
