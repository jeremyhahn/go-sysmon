package server

import (
	"context"
	"net/http"
	"net/netip"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// maxForwardingHeaderLen bounds a forwarding header read from a trusted proxy,
// all of its lines together. A real chain of proxies writes a few dozen bytes
// per hop, so a longer header is not one, and it is ignored unparsed.
const maxForwardingHeaderLen = 4096

// trustedProxies are the prefixes of the reverse proxies whose forwarding
// headers are believed; see Config.TrustedProxies.
type trustedProxies []netip.Prefix

// trusts reports whether addr belongs to a trusted proxy.
func (t trustedProxies) trusts(addr netip.Addr) bool {
	for _, prefix := range t {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// pick walks a chain of hop addresses from the right -- each proxy appends the
// address it received the request from, so the right end is the nearest hop --
// past every trusted proxy, and returns the index of the first hop that is not
// one: the nearest party no trusted proxy vouches for, and so the client.
// Everything left of it was written by that party and may be invented. When
// every hop is trusted the client is the left-most.
func (t trustedProxies) pick(hops []netip.Addr) int {
	for i := len(hops) - 1; i > 0; i-- {
		if !hops[i].IsValid() || !t.trusts(hops[i]) {
			return i
		}
	}
	return 0
}

// forwardedClient is what a trusted proxy reported about the client whose
// request it forwarded.
type forwardedClient struct {
	// addr is the client's address, or empty when the proxy did not name one.
	addr string
	// secure reports that the client reached the proxy over HTTPS.
	secure bool
}

// forwardedClientKey is the request context key forwardedClient is stored
// under.
type forwardedClientKey struct{}

// forwardedHeaders believes the forwarding headers of a request whose
// immediate peer is a trusted proxy: Forwarded (RFC 7239) when it is present
// and well-formed, else X-Forwarded-For and X-Forwarded-Proto. What they
// report is stored in the request context for requestSecure and clientAddr,
// and is used for nothing else.
//
// Any other peer's forwarding headers are ignored: anyone can send them, and
// believing them would let a client choose the address it is logged under. A
// malformed or oversized header is ignored too, never answered with an error,
// so a proxy sending one costs the forwarded details and nothing more.
//
// With no trusted proxies, which is the default, it returns next itself.
func forwardedHeaders(trusted trustedProxies, next http.Handler) http.Handler {
	if len(trusted) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !trusted.trusts(canonicalAddr(peer.Addr())) {
			next.ServeHTTP(w, r)
			return
		}
		client, ok := fromForwarded(trusted, r.Header.Values("Forwarded"))
		if !ok {
			client, ok = fromXForwarded(trusted, r.Header.Values("X-Forwarded-For"), r.Header.Values("X-Forwarded-Proto"))
		}
		if ok {
			r = r.WithContext(context.WithValue(r.Context(), forwardedClientKey{}, client))
		}
		next.ServeHTTP(w, r)
	})
}

// requestSecure reports whether the client's request arrived over TLS: at this
// server, or at a trusted proxy that said so.
func requestSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	client, ok := r.Context().Value(forwardedClientKey{}).(forwardedClient)
	return ok && client.secure
}

// clientAddr is the client address logs name: the one a trusted proxy
// reported, else the immediate peer's.
func clientAddr(r *http.Request) string {
	if client, ok := r.Context().Value(forwardedClientKey{}).(forwardedClient); ok && client.addr != "" {
		return client.addr
	}
	return r.RemoteAddr
}

// canonicalAddr drops what would stop an address matching a prefix written
// the ordinary way: the IPv4-mapped form a dual-stack listener reports, and
// an IPv6 zone.
func canonicalAddr(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}

// joinedHeader joins a header's lines into the one list they stand for (RFC
// 9110 section 5.3), reporting false when there is none or it is too long.
func joinedHeader(lines []string) (string, bool) {
	header := strings.Join(lines, ",")
	return header, header != "" && len(header) <= maxForwardingHeaderLen
}

// fromForwarded reads a Forwarded header. Each element describes one hop: for
// is the party the request came from and proto the scheme it used, so the
// element pick chooses names the client and says how it connected.
func fromForwarded(trusted trustedProxies, lines []string) (forwardedClient, bool) {
	header, ok := joinedHeader(lines)
	if !ok {
		return forwardedClient{}, false
	}
	elements, ok := parseForwarded(header)
	if !ok || len(elements) == 0 {
		return forwardedClient{}, false
	}
	hops := make([]netip.Addr, len(elements))
	for i, element := range elements {
		hops[i] = element.node
	}
	chosen := elements[trusted.pick(hops)]
	client := forwardedClient{secure: strings.EqualFold(chosen.proto, "https")}
	if chosen.node.IsValid() {
		client.addr = chosen.node.String()
	}
	return client, true
}

// fromXForwarded reads X-Forwarded-For and X-Forwarded-Proto, each on its own:
// a malformed one is ignored without discarding the other.
//
// X-Forwarded-Proto is a list too when proxies append to it, and its entries
// carry no addresses to pair them with X-Forwarded-For's, so the right-most --
// the one the immediate peer, a trusted proxy, wrote -- is the one believed.
func fromXForwarded(trusted trustedProxies, forLines, protoLines []string) (forwardedClient, bool) {
	var client forwardedClient
	addrOK, protoOK := false, false
	if hops, ok := parseXForwardedFor(forLines); ok {
		client.addr = hops[trusted.pick(hops)].String()
		addrOK = true
	}
	if proto, ok := lastXForwardedProto(protoLines); ok {
		client.secure = strings.EqualFold(proto, "https")
		protoOK = true
	}
	return client, addrOK || protoOK
}

// parseXForwardedFor parses X-Forwarded-For, a comma-separated list of
// addresses. An entry carrying a port, as a few proxies write, is accepted.
// One entry that is not an address makes the whole header unusable.
func parseXForwardedFor(lines []string) ([]netip.Addr, bool) {
	header, ok := joinedHeader(lines)
	if !ok {
		return nil, false
	}
	var hops []netip.Addr
	for _, entry := range strings.Split(header, ",") {
		entry = strings.Trim(entry, " \t")
		if entry == "" {
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			addrPort, portErr := netip.ParseAddrPort(entry)
			if portErr != nil {
				return nil, false
			}
			addr = addrPort.Addr()
		}
		hops = append(hops, canonicalAddr(addr))
	}
	return hops, len(hops) > 0
}

// lastXForwardedProto returns the right-most X-Forwarded-Proto entry, which
// must be a token.
func lastXForwardedProto(lines []string) (string, bool) {
	header, ok := joinedHeader(lines)
	if !ok {
		return "", false
	}
	entries := strings.Split(header, ",")
	proto := strings.Trim(entries[len(entries)-1], " \t")
	return proto, isToken(proto)
}

// forwardedElement is one element of a Forwarded header: one hop.
type forwardedElement struct {
	// node is the for parameter's address, or the zero Addr when it names
	// none -- absent, "unknown", or an obfuscated identifier.
	node  netip.Addr
	proto string
}

// parseForwarded parses a Forwarded header (RFC 7239 section 4):
//
//	Forwarded   = 1#forwarded-element
//	forwarded-element = [ forwarded-pair ] *( ";" [ forwarded-pair ] )
//	forwarded-pair    = token "=" value
//	value             = token / quoted-string
//
// Only for and proto are kept. Any syntax error, or a parameter named twice in
// one element, makes the whole header unusable: it reports false.
func parseForwarded(header string) ([]forwardedElement, bool) {
	rawElements, ok := splitOutsideQuotes(header, ',')
	if !ok {
		return nil, false
	}
	var elements []forwardedElement
	for _, raw := range rawElements {
		if strings.Trim(raw, " \t") == "" {
			continue
		}
		element, ok := parseForwardedElement(raw)
		if !ok {
			return nil, false
		}
		elements = append(elements, element)
	}
	return elements, true
}

// parseForwardedElement parses the pairs of one element.
func parseForwardedElement(raw string) (forwardedElement, bool) {
	pairs, ok := splitOutsideQuotes(raw, ';')
	if !ok {
		return forwardedElement{}, false
	}
	var element forwardedElement
	seen := make(map[string]bool, len(pairs))
	for _, pair := range pairs {
		pair = strings.Trim(pair, " \t")
		if pair == "" {
			continue
		}
		name, value, found := strings.Cut(pair, "=")
		if !found || !isToken(name) {
			return forwardedElement{}, false
		}
		value, ok := unquoteForwardedValue(value)
		if !ok {
			return forwardedElement{}, false
		}
		name = strings.ToLower(name)
		if seen[name] {
			return forwardedElement{}, false
		}
		seen[name] = true
		switch name {
		case "for":
			element.node = parseForwardedNode(value)
		case "proto":
			element.proto = value
		}
	}
	return element, true
}

// splitOutsideQuotes splits s at every sep that is not inside a quoted
// string, honouring backslash escapes there. It reports false for a quoted
// string left open.
func splitOutsideQuotes(s string, sep byte) ([]string, bool) {
	var parts []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch {
		case quoted && s[i] == '\\':
			i++
		case s[i] == '"':
			quoted = !quoted
		case !quoted && s[i] == sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	if quoted {
		return nil, false
	}
	return append(parts, s[start:]), true
}

// unquoteForwardedValue returns a pair's value: a token as it stands, or a
// quoted string with its quotes and escapes removed.
func unquoteForwardedValue(value string) (string, bool) {
	if !strings.HasPrefix(value, `"`) {
		return value, isToken(value)
	}
	if len(value) < 2 || !strings.HasSuffix(value, `"`) {
		return "", false
	}
	var b strings.Builder
	for i := 1; i < len(value)-1; i++ {
		c := value[i]
		switch c {
		case '\\':
			i++
			if i == len(value)-1 {
				return "", false
			}
			c = value[i]
		case '"':
			return "", false
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

// parseForwardedNode returns the address a for parameter names (RFC 7239
// section 6): an IPv4 address or a bracketed IPv6 address, either with an
// optional port. "unknown", an obfuscated identifier or anything else names no
// address and returns the zero Addr.
func parseForwardedNode(node string) netip.Addr {
	if rest, bracketed := strings.CutPrefix(node, "["); bracketed {
		host, port, found := strings.Cut(rest, "]")
		if !found || (port != "" && !strings.HasPrefix(port, ":")) {
			return netip.Addr{}
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() {
			return netip.Addr{}
		}
		return canonicalAddr(addr)
	}
	host, _, _ := strings.Cut(node, ":")
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Is4() {
		return netip.Addr{}
	}
	return addr
}

// isToken reports whether s is an RFC 9110 token, which is exactly what a
// valid header field name is.
func isToken(s string) bool {
	return httpguts.ValidHeaderFieldName(s)
}
