"""End-to-end check of thimble as an agent code-execution sandbox.

Runs against a live `make serve` through the stock E2B Python SDK, the way an
agent framework would: create a sandbox, run Python, install a package, clone
a repository, hit the network, use node and sudo, write files, pause, resume,
check everything survived, kill. Exits non-zero on the first failure.

    make serve &
    make smoke
"""

import sys
import time

from e2b import Sandbox

FAILED = False


def step(name, cmd, sandbox, expect=None, user=None):
    global FAILED
    t0 = time.time()
    kwargs = {"timeout": 180}
    if user:
        kwargs["user"] = user
    r = sandbox.commands.run(cmd, **kwargs)
    out = (r.stdout + r.stderr).strip()
    ok = r.exit_code == 0 and (expect is None or expect in out)
    FAILED = FAILED or not ok
    mark = "ok  " if ok else "FAIL"
    print(f"{mark} {name:<28} {time.time() - t0:5.2f}s  {out.splitlines()[-1][:80] if out else ''}")
    return out


def main():
    t0 = time.time()
    sbx = Sandbox.create("base", timeout=300)
    print(f"ok   {'create':<28} {time.time() - t0:5.2f}s  {sbx.sandbox_id}")

    step("python", "python3 -c 'import sys, json; print(json.dumps({\"python\": sys.version.split()[0]}))'", sbx, '"python"')
    step("node", "node -e 'console.log(\"node\", process.version)'", sbx, "node v")
    step("git", "git --version", sbx, "git version")
    step("sudo", "sudo -n id -u", sbx, "0")
    step("root fs on disk", "awk '$2==\"/\"{print $1, $3}' /proc/mounts", sbx, "/dev/vda ext4")
    step("network: https", "curl -sS -o /dev/null -w '%{http_code}' https://example.com", sbx, "200")
    step("pip install", "pip install -q requests 2>/dev/null; python3 -c 'import requests; print(requests.__version__)'", sbx)
    step("python + network", "python3 -c 'import requests; print(requests.get(\"https://httpbin.org/get\").status_code)'", sbx, "200")
    step("git clone", "git clone -q https://github.com/octocat/Hello-World.git && cat Hello-World/README", sbx, "Hello World")
    step("sudo apk add", "sudo apk add -q --no-cache tree && tree --version | head -1", sbx, "tree v")
    sbx.files.write("/home/user/note.txt", "written from the host\n")
    step("files api", "cat /home/user/note.txt", sbx, "written from the host")
    step("write for later", "echo survived > /home/user/persist.txt", sbx)

    t = time.time()
    sbx.pause()
    print(f"ok   {'pause':<28} {time.time() - t:5.2f}s")
    t = time.time()
    sbx = Sandbox.connect(sbx.sandbox_id)
    print(f"ok   {'resume':<28} {time.time() - t:5.2f}s")

    step("file survived resume", "cat /home/user/persist.txt", sbx, "survived")
    step("pip pkg survived resume", "python3 -c 'import requests; print(\"requests ok\")'", sbx, "requests ok")
    step("network after resume", "curl -sS -o /dev/null -w '%{http_code}' https://example.com", sbx, "200")

    sbx.kill()
    print(f"{'FAIL' if FAILED else 'ok  '} {'total':<28} {time.time() - t0:5.2f}s")
    sys.exit(1 if FAILED else 0)


if __name__ == "__main__":
    main()
