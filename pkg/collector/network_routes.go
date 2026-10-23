package collector

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"golang.org/x/sys/unix"
)

const (
	familyIPv4 = "ipv4"
	familyIPv6 = "ipv6"
)

// route is one usable entry of a kernel routing table: up, and not a reject
// route.
type route struct {
	iface     string
	gateway   netip.Addr // the zero Addr when the destination is on-link
	isDefault bool
	metric    uint32
}

// familyRoutes is what one family's routing table says about each interface.
type familyRoutes struct {
	// gateways holds each interface's preferred next hop: that of its
	// lowest-metric default route, else of its lowest-metric gatewayed route.
	gateways map[string]netip.Addr
	// defaultIface carries the lowest-metric default route; empty when the
	// table has none.
	defaultIface string
}

// routeTables is both families' routing state for one collection.
type routeTables struct {
	v4, v6 familyRoutes
	// available is false when /proc/net/route could not be read. A missing
	// /proc/net/ipv6_route is not a failure: the kernel omits it when IPv6
	// is disabled, and there are then no IPv6 routes.
	available bool
}

// readRouteTables reads both routing tables under procRoot. A table that
// cannot be read yields no routes; the IPv4 table's failure also clears
// available, so the interface fields read as unknown.
func readRouteTables(logger *slog.Logger) routeTables {
	var tables routeTables

	data, err := os.ReadFile(filepath.Join(procRoot, "net", "route"))
	if err != nil {
		warnOnce(logger, "network:route", "network: could not read the IPv4 routing table", "error", err)
	} else {
		tables.available = true
		tables.v4 = summarizeRoutes(parseIPv4Routes(data))
	}

	data, err = os.ReadFile(filepath.Join(procRoot, "net", "ipv6_route"))
	switch {
	case err == nil:
		tables.v6 = summarizeRoutes(parseIPv6Routes(data))
	case !errors.Is(err, fs.ErrNotExist):
		warnOnce(logger, "network:ipv6_route", "network: could not read the IPv6 routing table", "error", err)
	}
	return tables
}

// applyRoutes sets info's routing fields from tables.
func applyRoutes(info *types.NetworkInfo, tables routeTables) {
	info.RoutesAvailable = tables.available
	if gw, ok := tables.v4.gateways[info.Name]; ok {
		info.GatewayIPv4 = gw.String()
	}
	if gw, ok := tables.v6.gateways[info.Name]; ok {
		info.GatewayIPv6 = gw.String()
	}
	info.DefaultRouteIPv4 = info.Name != "" && tables.v4.defaultIface == info.Name
	info.DefaultRouteIPv6 = info.Name != "" && tables.v6.defaultIface == info.Name
}

// summarizeRoutes picks each interface's gateway and the default interface.
// Ties keep the route listed first, as the kernel does.
func summarizeRoutes(routes []route) familyRoutes {
	summary := familyRoutes{gateways: make(map[string]netip.Addr)}
	best := make(map[string]route)
	var defaultMetric uint32

	for _, r := range routes {
		if r.isDefault && (summary.defaultIface == "" || r.metric < defaultMetric) {
			summary.defaultIface = r.iface
			defaultMetric = r.metric
		}
		if !r.gateway.IsValid() {
			continue
		}
		if current, ok := best[r.iface]; !ok || preferRoute(r, current) {
			best[r.iface] = r
		}
	}

	for iface, r := range best {
		summary.gateways[iface] = r.gateway
	}
	return summary
}

// preferRoute reports whether candidate is a better source of an interface's
// gateway than current: a default route beats any other, then the lower
// metric wins.
func preferRoute(candidate, current route) bool {
	if candidate.isDefault != current.isDefault {
		return candidate.isDefault
	}
	return candidate.metric < current.metric
}

// parseIPv4Routes parses /proc/net/route. After a header line, each line is
//
//	Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
//
// with the addresses as the hex of a 32-bit word in host byte order. A
// malformed line is skipped.
func parseIPv4Routes(data []byte) []route {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return nil
	}

	var routes []route
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		dest, okDest := parseIPv4Hex(fields[1])
		gateway, okGateway := parseIPv4Hex(fields[2])
		flags, errFlags := strconv.ParseUint(fields[3], 16, 32)
		metric, errMetric := strconv.ParseUint(fields[6], 10, 32)
		mask, okMask := parseIPv4Hex(fields[7])
		if !okDest || !okGateway || !okMask || errFlags != nil || errMetric != nil {
			continue
		}
		r, ok := newRoute(fields[0], flags, uint32(metric), gateway)
		if !ok {
			continue
		}
		r.isDefault = dest.IsUnspecified() && mask.IsUnspecified()
		routes = append(routes, r)
	}
	return routes
}

// parseIPv6Routes parses /proc/net/ipv6_route. Each line is
//
//	Destination DestPrefixLen Source SourcePrefixLen NextHop Metric RefCnt Use Flags Iface
//
// with the addresses as 32 hex digits in network byte order and the numbers
// in hex. A malformed line is skipped.
func parseIPv6Routes(data []byte) []route {
	var routes []route
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		_, okDest := parseIPv6Hex(fields[0])
		prefixLen, errPrefix := strconv.ParseUint(fields[1], 16, 8)
		nextHop, okNextHop := parseIPv6Hex(fields[4])
		metric, errMetric := strconv.ParseUint(fields[5], 16, 32)
		flags, errFlags := strconv.ParseUint(fields[8], 16, 32)
		if !okDest || !okNextHop || errPrefix != nil || errMetric != nil || errFlags != nil {
			continue
		}
		r, ok := newRoute(fields[9], flags, uint32(metric), nextHop)
		if !ok {
			continue
		}
		// Only a ::/0 destination has a zero prefix length.
		r.isDefault = prefixLen == 0
		routes = append(routes, r)
	}
	return routes
}

// newRoute builds a route from the fields both tables share. It reports
// false for a route that is down or rejects its traffic, which leads nowhere.
// The next hop is kept only when the route is flagged as gatewayed.
func newRoute(iface string, flags uint64, metric uint32, nextHop netip.Addr) (route, bool) {
	if flags&unix.RTF_UP == 0 || flags&unix.RTF_REJECT != 0 {
		return route{}, false
	}
	r := route{iface: iface, metric: metric}
	if flags&unix.RTF_GATEWAY != 0 && !nextHop.IsUnspecified() {
		r.gateway = nextHop
	}
	return r, true
}

// parseIPv4Hex decodes /proc/net/route's address form: the hex of the 32-bit
// word holding the address, printed in host byte order.
func parseIPv4Hex(s string) (netip.Addr, bool) {
	word, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return netip.Addr{}, false
	}
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], uint32(word))
	return netip.AddrFrom4(b), true
}

// parseIPv6Hex decodes /proc/net/ipv6_route's address form: 32 hex digits in
// network byte order.
func parseIPv6Hex(s string) (netip.Addr, bool) {
	if len(s) != 32 {
		return netip.Addr{}, false
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return netip.AddrFrom16([16]byte(raw)), true
}

// parseAddress turns one of an interface's CIDR strings into its structured
// form. It reports false for a string that is not CIDR.
func parseAddress(cidr string) (types.AddressInfo, bool) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return types.AddressInfo{}, false
	}
	addr := prefix.Addr()
	info := types.AddressInfo{
		Address:      addr.String(),
		PrefixLength: prefix.Bits(),
		Family:       familyIPv6,
	}
	if addr.Is4() {
		info.Family = familyIPv4
		info.Netmask = net.IP(net.CIDRMask(prefix.Bits(), 32)).String()
	}
	return info, true
}
