"""Egress policy checks through the stock E2B SDK, modelled on the SDK's own
tests/async/sandbox_async/test_network.py. Needs a server started with -vnet."""
import json
import os
import sys
import time
import urllib.request

from e2b import ALL_TRAFFIC, Sandbox, SandboxNetworkOpts
from e2b.sandbox.commands.command_handle import CommandExitException

API = os.environ.get("E2B_API_URL", "http://localhost:3000")
fails = 0


def curl(sbx, url, extra=""):
    try:
        r = sbx.commands.run(f"curl -sS {extra} --connect-timeout 4 --max-time 8 -o /dev/null -w '%{{http_code}}' {url}")
        return r.stdout.strip()
    except CommandExitException:
        return "fail"


def check(name, got, want):
    global fails
    ok = got == want if not callable(want) else want(got)
    print(f"{'ok  ' if ok else 'FAIL'} {name:52} {got}")
    fails += not ok


def reachable(code):
    return code not in ("fail", "000", "")


def sandbox(**net):
    return Sandbox.create("base", network=SandboxNetworkOpts(**net), timeout=120)


def run():
    s = sandbox(deny_out=lambda c: [c.all_traffic], allow_out=["1.1.1.1"])
    check("deny all + allow ip: allowed ip works", curl(s, "https://1.1.1.1"), reachable)
    check("deny all + allow ip: other ip blocked", curl(s, "https://8.8.8.8"), "fail")
    s.kill()

    s = sandbox(deny_out=["8.8.8.8"])
    check("deny ip: denied ip blocked", curl(s, "https://8.8.8.8"), "fail")
    check("deny ip: other ip works", curl(s, "https://1.1.1.1"), reachable)
    s.kill()

    s = sandbox(deny_out=lambda c: [c.all_traffic])
    check("deny all: 1.1.1.1 blocked", curl(s, "https://1.1.1.1"), "fail")
    check("deny all: example.com blocked", curl(s, "https://example.com"), "fail")
    s.kill()

    s = sandbox(deny_out=lambda c: [c.all_traffic], allow_out=["1.1.1.1", "8.8.8.8"])
    check("allow beats deny: 1.1.1.1", curl(s, "https://1.1.1.1"), reachable)
    check("allow beats deny: 8.8.8.8", curl(s, "https://8.8.8.8"), reachable)
    s.kill()

    # Domains: the destination IP is denied, so the hostname in the TLS
    # ClientHello decides.
    s = sandbox(deny_out=lambda c: [c.all_traffic], allow_out=["example.com", "*.wikipedia.org"])
    check("domain allow: example.com (https)", curl(s, "https://example.com"), "200")
    check("domain allow: example.com (http, Host header)", curl(s, "http://example.com"), reachable)
    check("domain allow: en.wikipedia.org (wildcard)", curl(s, "https://en.wikipedia.org"), reachable)
    check("domain allow: github.com blocked", curl(s, "https://github.com"), "fail")
    check("domain allow: apex of wildcard blocked", curl(s, "https://wikipedia.org"), "fail")
    # Lying about the name: right SNI, wrong IP is the known gap; wrong SNI must fail.
    check("domain allow: SNI mismatch blocked",
          curl(s, "https://github.com", "--resolve github.com:443:93.184.216.34 -k"), "fail")

    ev = json.load(urllib.request.urlopen(urllib.request.Request(
        f"{API}/sandboxes/{s.sandbox_id}/network/events", headers={"X-API-Key": "local"})))
    verdicts = {(e.get("name") or e["dst"], e["verdict"]) for e in ev["events"]}
    check("events record the SNI of allowed traffic", ("example.com", "allow") in verdicts, True)
    check("events record denials", any(v == "deny" for _, v in verdicts), True)

    # Live update, then survive a pause/resume.
    s.update_network({"allow_out": ["github.com"], "deny_out": [ALL_TRAFFIC]})
    check("update_network: new domain allowed", curl(s, "https://github.com"), reachable)
    check("update_network: old domain now blocked", curl(s, "https://example.com"), "fail")
    s.pause()
    s = Sandbox.connect(s.sandbox_id)
    check("policy survives pause/resume: allowed", curl(s, "https://github.com"), reachable)
    check("policy survives pause/resume: blocked", curl(s, "https://example.com"), "fail")
    s.kill()

    s = Sandbox.create("base", allow_internet_access=False, timeout=120)
    check("allow_internet_access=False blocks everything", curl(s, "https://example.com"), "fail")
    s.kill()

    s = Sandbox.create("base", timeout=120)
    check("no policy: open internet", curl(s, "https://example.com"), "200")
    s.kill()


run()
print("\nall passed" if not fails else f"\n{fails} failed")
sys.exit(1 if fails else 0)
