package vnet

import (
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
)

// AllTraffic is the selector E2B's SDKs use for "everything" (ctx.all_traffic).
const AllTraffic = "0.0.0.0/0"

// Verdict is what a policy says about one connection.
type Verdict int

const (
	Allow Verdict = iota
	Deny
	// NeedName means the destination IP is denied, but a domain allow rule
	// might still admit it, so the caller must look at the TLS SNI or HTTP
	// Host header and ask again with Decide.
	NeedName
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	}
	return "need-name"
}

// ruleset is an immutable parsed policy.
type ruleset struct {
	allowNets    []netip.Prefix
	allowDomains []string // lowercase; "*.x.com" kept as ".x.com"
	denyNets     []netip.Prefix
	spec         Spec
}

// Spec is a policy as the API states it, mirroring E2B's network config:
// allow wins over deny, deny takes CIDRs and IPs, allow also takes domains.
type Spec struct {
	AllowOut []string `json:"allowOut,omitempty"`
	DenyOut  []string `json:"denyOut,omitempty"`
}

// Policy is a sandbox's egress policy. It can be replaced while the sandbox
// runs; readers never block.
type Policy struct {
	rs atomic.Pointer[ruleset]
}

// NewPolicy parses spec. A nil or empty spec allows everything.
func NewPolicy(spec Spec) (*Policy, error) {
	p := &Policy{}
	if err := p.Set(spec); err != nil {
		return nil, err
	}
	return p, nil
}

// Set swaps in a new rule set, or leaves the old one if spec is invalid.
func (p *Policy) Set(spec Spec) error {
	rs := &ruleset{spec: spec}
	for _, a := range spec.AllowOut {
		a = strings.TrimSpace(a)
		if pfx, ok := parseNet(a); ok {
			rs.allowNets = append(rs.allowNets, pfx)
			continue
		}
		d, err := parseDomain(a)
		if err != nil {
			return fmt.Errorf("allowOut %q: %w", a, err)
		}
		rs.allowDomains = append(rs.allowDomains, d)
	}
	for _, d := range spec.DenyOut {
		d = strings.TrimSpace(d)
		pfx, ok := parseNet(d)
		if !ok {
			return fmt.Errorf("denyOut %q: only IPs and CIDR blocks are supported, not domain names", d)
		}
		rs.denyNets = append(rs.denyNets, pfx)
	}
	p.rs.Store(rs)
	return nil
}

// Spec returns the rules currently in force.
func (p *Policy) Spec() Spec { return p.rs.Load().spec }

// Enforcing reports whether any rule is set.
func (p *Policy) Enforcing() bool {
	rs := p.rs.Load()
	return len(rs.denyNets) > 0
}

func parseNet(s string) (netip.Prefix, bool) {
	if pfx, err := netip.ParsePrefix(s); err == nil {
		return pfx.Masked(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

// parseDomain accepts "example.com" and "*.example.com". The wildcard
// matches subdomains at any depth but not the apex, as in E2B.
func parseDomain(s string) (string, error) {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" || s == "*" {
		return "", fmt.Errorf("not a domain, IP or CIDR")
	}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		s = "." + rest
	}
	if strings.ContainsAny(s, "*/ :") || strings.HasPrefix(strings.TrimPrefix(s, "."), ".") {
		return "", fmt.Errorf("not a domain, IP or CIDR")
	}
	return s, nil
}

func (rs *ruleset) matchDomain(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, d := range rs.allowDomains {
		if strings.HasPrefix(d, ".") {
			if strings.HasSuffix(name, d) {
				return true
			}
		} else if name == d {
			return true
		}
	}
	return false
}

func matchNets(nets []netip.Prefix, ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Check decides on destination IP alone. It returns NeedName when only a
// domain allow rule could admit the connection.
func (p *Policy) Check(ip netip.Addr) (Verdict, string) {
	rs := p.rs.Load()
	if matchNets(rs.allowNets, ip) {
		return Allow, "allowOut ip"
	}
	if !matchNets(rs.denyNets, ip) {
		return Allow, "default"
	}
	if len(rs.allowDomains) > 0 {
		return NeedName, "denied ip, domain rules present"
	}
	return Deny, "denyOut ip"
}

// Decide is Check with the hostname the client asked for (TLS SNI or HTTP
// Host). An empty name never matches a domain rule.
func (p *Policy) Decide(ip netip.Addr, name string) (Verdict, string) {
	v, why := p.Check(ip)
	if v != NeedName {
		return v, why
	}
	if name != "" && p.rs.Load().matchDomain(name) {
		return Allow, "allowOut domain"
	}
	if name == "" {
		return Deny, "denyOut ip, no hostname to match"
	}
	return Deny, "denyOut ip, " + name + " not allowed"
}
