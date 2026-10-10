package vnet

import (
	"net/netip"
	"testing"
)

func TestPolicy(t *testing.T) {
	ip := netip.MustParseAddr
	cases := []struct {
		name string
		spec Spec
		ip   string
		host string
		want Verdict
	}{
		{"empty allows", Spec{}, "8.8.8.8", "", Allow},
		{"deny all", Spec{DenyOut: []string{AllTraffic}}, "8.8.8.8", "", Deny},
		{"deny one ip", Spec{DenyOut: []string{"8.8.8.8"}}, "8.8.8.8", "", Deny},
		{"deny one ip, other ok", Spec{DenyOut: []string{"8.8.8.8"}}, "1.1.1.1", "", Allow},
		{"allow ip beats deny all", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"1.1.1.1"}}, "1.1.1.1", "", Allow},
		{"allow cidr", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"10.0.0.0/8"}}, "10.2.3.4", "", Allow},
		{"allow domain by sni", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"example.com"}}, "93.184.216.34", "example.com", Allow},
		{"other domain denied", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"example.com"}}, "93.184.216.34", "evil.com", Deny},
		{"no name denied", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"example.com"}}, "93.184.216.34", "", Deny},
		{"wildcard subdomain", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"*.example.com"}}, "1.2.3.4", "a.b.example.com", Allow},
		{"wildcard not apex", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"*.example.com"}}, "1.2.3.4", "example.com", Deny},
		{"suffix is not a match", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"example.com"}}, "1.2.3.4", "notexample.com", Deny},
		{"case insensitive", Spec{DenyOut: []string{AllTraffic}, AllowOut: []string{"Example.COM"}}, "1.2.3.4", "EXAMPLE.com.", Allow},
	}
	for _, c := range cases {
		p, err := NewPolicy(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, why := p.Decide(ip(c.ip), c.host)
		if got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, why, c.want)
		}
	}
	if _, err := NewPolicy(Spec{DenyOut: []string{"example.com"}}); err == nil {
		t.Error("domain in denyOut should be rejected")
	}
	if _, err := NewPolicy(Spec{AllowOut: []string{"*"}}); err == nil {
		t.Error("bare * should be rejected")
	}
}
