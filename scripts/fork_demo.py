"""Fork a running sandbox into copies that continue from its exact state.
Needs a server started with -vnet. thimble's fork endpoint is an extension;
the children are ordinary sandboxes the stock SDK can connect to."""
import json
import os
import sys
import time
import urllib.request

from e2b import Sandbox

API = os.environ.get("E2B_API_URL", "http://localhost:3000")
N = int(sys.argv[1]) if len(sys.argv) > 1 else 3
fails = 0


def fork(sandbox_id, count):
    req = urllib.request.Request(f"{API}/sandboxes/{sandbox_id}/fork", method="POST",
                                 data=json.dumps({"count": count}).encode(),
                                 headers={"X-API-Key": "local", "Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=120))


def sh(sbx, cmd):
    return sbx.commands.run(cmd).stdout.strip()


def check(name, got, want):
    global fails
    ok = want(got) if callable(want) else got == want
    print(f"{'ok  ' if ok else 'FAIL'} {name:50} {got}")
    fails += not ok


parent = Sandbox.create("base", timeout=300)
# State that lives only in memory and in a running process, not on disk.
sh(parent, "sudo mkdir -p /mnt/ram && sudo mount -t tmpfs tmpfs /mnt/ram && sudo chmod 777 /mnt/ram")
sh(parent, "echo parent-memory > /mnt/ram/secret")
pid = sh(parent, "nohup sh -c 'while :; do sleep 1; done' >/dev/null 2>&1 & echo $!")
sh(parent, "echo on-disk-before-fork > /home/user/before.txt")
parent_rand = sh(parent, "head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \\n'")

t0 = time.time()
kids = [Sandbox.connect(k["sandboxID"]) for k in fork(parent.sandbox_id, N)]
print(f"forked x{N} in {time.time() - t0:.2f}s\n")

for i, k in enumerate(kids):
    check(f"child {i}: in-memory file inherited", sh(k, "cat /mnt/ram/secret"), "parent-memory")
    check(f"child {i}: running process inherited (pid {pid})", sh(k, f"kill -0 {pid} && echo alive"), "alive")
    check(f"child {i}: disk file inherited", sh(k, "cat /home/user/before.txt"), "on-disk-before-fork")
    check(f"child {i}: network works", sh(k, "curl -sS -m8 -o /dev/null -w '%{http_code}' https://example.com"), "200")

# Divergence: each writes its own file; nobody sees anyone else's.
for i, k in enumerate(kids):
    sh(k, f"echo child-{i} > /home/user/me.txt; echo child-{i} > /mnt/ram/secret")
sh(parent, "echo parent-after > /home/user/me.txt")
for i, k in enumerate(kids):
    check(f"child {i}: sees only its own writes", sh(k, "cat /home/user/me.txt /mnt/ram/secret").replace("\n", " "), f"child-{i} child-{i}")
check("parent: untouched by children", sh(parent, "cat /home/user/me.txt /mnt/ram/secret").replace("\n", " "), "parent-after parent-memory")

rands = {sh(k, "head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \\n'") for k in kids} | {parent_rand}
check("siblings draw different random numbers", len(rands), N + 1)

for s in kids + [parent]:
    s.kill()
print("\nall passed" if not fails else f"\n{fails} failed")
sys.exit(1 if fails else 0)
