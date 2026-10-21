package config

import (
	"net/netip"
	"strings"
)

// ParseTrustedProxies parses server.trusted_proxies into the prefixes the
// server checks a request's immediate peer against. An empty list trusts no
// proxy, which is the default.
//
// Every entry must be a CIDR prefix; a lone address is refused rather than
// read as a single host, so "10.0.0.1" written where "10.0.0.0/8" was meant
// stops startup instead of trusting one host when a network was intended.
// Each prefix is masked, so "10.1.2.3/8" means 10.0.0.0/8.
func ParseTrustedProxies(cidrs []string) ([]netip.Prefix, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return nil, &TrustedProxyError{CIDR: cidr, Err: err}
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}
