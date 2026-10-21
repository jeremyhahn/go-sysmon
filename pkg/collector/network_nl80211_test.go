//go:build linux

package collector

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"golang.org/x/sys/unix"
)

// netlinkMessage builds one netlink message of the given type and sequence
// number around body, padded as the kernel pads it.
func netlinkMessage(msgType uint16, seq uint32, body []byte) []byte {
	length := unix.SizeofNlMsghdr + len(body)
	msg := binary.NativeEndian.AppendUint32(nil, uint32(length))
	msg = binary.NativeEndian.AppendUint16(msg, msgType)
	msg = binary.NativeEndian.AppendUint16(msg, 0)
	msg = binary.NativeEndian.AppendUint32(msg, seq)
	msg = binary.NativeEndian.AppendUint32(msg, 0)
	msg = append(msg, body...)
	for len(msg) < nlaAlign(length) {
		msg = append(msg, 0)
	}
	return msg
}

// genlReply is a generic-netlink data message: a genl header, then attrs.
func genlReply(family uint16, seq uint32, attrs ...[]byte) []byte {
	body := []byte{0, 0, 0, 0}
	for _, a := range attrs {
		body = append(body, a...)
	}
	return netlinkMessage(family, seq, body)
}

// statusMessage is an NLMSG_ERROR or NLMSG_DONE carrying code (0 or -errno).
func statusMessage(msgType uint16, seq uint32, code int32) []byte {
	return netlinkMessage(msgType, seq, binary.NativeEndian.AppendUint32(nil, uint32(code)))
}

func u32(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }

// bssAttr is an NL80211_ATTR_BSS block for one scan entry.
func bssAttr(mac []byte, status uint32, withStatus bool) []byte {
	inner := encodeAttr(unix.NL80211_BSS_BSSID, mac)
	if withStatus {
		inner = append(inner, encodeAttr(unix.NL80211_BSS_STATUS, u32(status))...)
	}
	return encodeAttr(unix.NL80211_ATTR_BSS|unix.NLA_F_NESTED, inner)
}

func TestEncodeAttrPadsToFourBytes(t *testing.T) {
	attr := encodeAttr(7, []byte("abcde"))
	if len(attr) != 12 {
		t.Fatalf("len = %d, want 12 (4 header + 5 data + 3 padding)", len(attr))
	}
	if got := binary.NativeEndian.Uint16(attr[0:2]); got != 9 {
		t.Errorf("length field = %d, want 9: padding is not part of the length", got)
	}
	attrs, err := parseAttrs(attr)
	if err != nil {
		t.Fatalf("parseAttrs: %v", err)
	}
	if string(attrs[7]) != "abcde" {
		t.Errorf("round trip = %q, want %q", attrs[7], "abcde")
	}
}

func TestEncodeGenlRequestHeader(t *testing.T) {
	attrs := encodeAttr(unix.CTRL_ATTR_FAMILY_NAME, []byte("nl80211\x00"))
	msg := encodeGenlRequest(unix.GENL_ID_CTRL, unix.NLM_F_REQUEST|unix.NLM_F_ACK, 42, unix.CTRL_CMD_GETFAMILY, ctrlVersion, attrs)

	if got := int(binary.NativeEndian.Uint32(msg[0:4])); got != len(msg) {
		t.Errorf("length field = %d, message is %d bytes", got, len(msg))
	}
	if got := binary.NativeEndian.Uint16(msg[4:6]); got != unix.GENL_ID_CTRL {
		t.Errorf("type = %d, want GENL_ID_CTRL", got)
	}
	if got := binary.NativeEndian.Uint32(msg[8:12]); got != 42 {
		t.Errorf("seq = %d, want 42", got)
	}
	if msg[16] != unix.CTRL_CMD_GETFAMILY || msg[17] != ctrlVersion {
		t.Errorf("genl header = %v, want cmd %d version %d", msg[16:18], unix.CTRL_CMD_GETFAMILY, ctrlVersion)
	}
}

func TestParseAttrsMalformed(t *testing.T) {
	tests := map[string][]byte{
		"short header":           {4, 0},
		"length below header":    {2, 0, 1, 0},
		"length beyond buffer":   {12, 0, 1, 0, 'x'},
		"second attribute short": append(encodeAttr(1, []byte{1, 2, 3, 4}), 9),
	}
	for name, buf := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseAttrs(buf)
			var malformed *NetlinkMalformedError
			if !errors.As(err, &malformed) {
				t.Fatalf("err = %v, want NetlinkMalformedError", err)
			}
		})
	}
}

func TestParseAttrsStripsNestedFlag(t *testing.T) {
	attrs, err := parseAttrs(encodeAttr(5|unix.NLA_F_NESTED, []byte{1, 2, 3, 4}))
	if err != nil {
		t.Fatalf("parseAttrs: %v", err)
	}
	if _, ok := attrs[5]; !ok {
		t.Errorf("attrs = %v, want type 5 with the nested flag stripped", attrs)
	}
}

func TestParseNetlinkRepliesCollectsUntilAck(t *testing.T) {
	buf := append(genlReply(30, 3, encodeAttr(1, []byte("one"))), statusMessage(unix.NLMSG_ERROR, 3, 0)...)

	payloads, done, err := parseNetlinkReplies(buf, 3)
	if err != nil {
		t.Fatalf("parseNetlinkReplies: %v", err)
	}
	if !done {
		t.Error("done = false, want true after the acknowledgement")
	}
	if len(payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(payloads))
	}
	attrs, err := parseAttrs(payloads[0])
	if err != nil || string(attrs[1]) != "one" {
		t.Errorf("payload attrs = %v, %v; want the genl header removed", attrs, err)
	}
}

func TestParseNetlinkRepliesDumpContinues(t *testing.T) {
	payloads, done, err := parseNetlinkReplies(genlReply(30, 5, encodeAttr(1, []byte{1, 0, 0, 0})), 5)
	if err != nil {
		t.Fatalf("parseNetlinkReplies: %v", err)
	}
	if done || len(payloads) != 1 {
		t.Errorf("done = %v, payloads = %d; want a dump part read with more to come", done, len(payloads))
	}

	_, done, err = parseNetlinkReplies(statusMessage(unix.NLMSG_DONE, 5, 0), 5)
	if err != nil || !done {
		t.Errorf("NLMSG_DONE: done = %v, err = %v; want done", done, err)
	}
}

func TestParseNetlinkRepliesSkipsOtherSequence(t *testing.T) {
	buf := append(genlReply(30, 1, encodeAttr(1, []byte("stale"))), statusMessage(unix.NLMSG_ERROR, 2, 0)...)

	payloads, done, err := parseNetlinkReplies(buf, 2)
	if err != nil || !done || len(payloads) != 0 {
		t.Errorf("payloads = %d, done = %v, err = %v; want the stale reply skipped", len(payloads), done, err)
	}
}

func TestParseNetlinkRepliesKernelError(t *testing.T) {
	for _, msgType := range []uint16{unix.NLMSG_ERROR, unix.NLMSG_DONE} {
		_, _, err := parseNetlinkReplies(statusMessage(msgType, 9, -int32(unix.ENODEV)), 9)
		var nlErr *NetlinkError
		if !errors.As(err, &nlErr) || !errors.Is(err, unix.ENODEV) {
			t.Errorf("type %d: err = %v, want NetlinkError wrapping ENODEV", msgType, err)
		}
	}
}

func TestParseNetlinkRepliesMalformed(t *testing.T) {
	tests := map[string][]byte{
		"short header":     make([]byte, 8),
		"status too short": netlinkMessage(unix.NLMSG_ERROR, 1, []byte{0, 0}),
		"no genl header":   netlinkMessage(30, 1, []byte{1, 2}),
	}
	// A header claiming more bytes than the read holds.
	long := netlinkMessage(30, 1, nil)
	binary.NativeEndian.PutUint32(long[0:4], 64)
	tests["length beyond read"] = long

	for name, buf := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseNetlinkReplies(buf, 1)
			var malformed *NetlinkMalformedError
			if !errors.As(err, &malformed) {
				t.Fatalf("err = %v, want NetlinkMalformedError", err)
			}
		})
	}
}

func TestParseFamilyID(t *testing.T) {
	reply := append(encodeAttr(unix.CTRL_ATTR_FAMILY_NAME, []byte("nl80211\x00")),
		encodeAttr(unix.CTRL_ATTR_FAMILY_ID, binary.NativeEndian.AppendUint16(nil, 0x1c))...)

	id, err := parseFamilyID([][]byte{reply})
	if err != nil {
		t.Fatalf("parseFamilyID: %v", err)
	}
	if id != 0x1c {
		t.Errorf("id = %#x, want 0x1c", id)
	}
}

func TestParseFamilyIDMissing(t *testing.T) {
	_, err := parseFamilyID([][]byte{encodeAttr(unix.CTRL_ATTR_FAMILY_NAME, []byte("x\x00"))})
	var missing *NetlinkAttrMissingError
	if !errors.As(err, &missing) {
		t.Errorf("err = %v, want NetlinkAttrMissingError", err)
	}
}

func TestParseInterfaceSSID(t *testing.T) {
	reply := append(encodeAttr(unix.NL80211_ATTR_IFINDEX, u32(3)), encodeAttr(unix.NL80211_ATTR_SSID, []byte("home-net"))...)

	ssid, err := parseInterfaceSSID([][]byte{reply})
	if err != nil || ssid != "home-net" {
		t.Errorf("ssid = %q, err = %v; want home-net", ssid, err)
	}
}

func TestParseInterfaceSSIDNotAssociated(t *testing.T) {
	ssid, err := parseInterfaceSSID([][]byte{encodeAttr(unix.NL80211_ATTR_IFINDEX, u32(3))})
	if err != nil || ssid != "" {
		t.Errorf("ssid = %q, err = %v; want empty without error", ssid, err)
	}
}

func TestParseInterfaceSSIDMalformed(t *testing.T) {
	_, err := parseInterfaceSSID([][]byte{{9, 0, 1}})
	var malformed *NetlinkMalformedError
	if !errors.As(err, &malformed) {
		t.Errorf("err = %v, want NetlinkMalformedError", err)
	}
}

func TestParseAssociatedBSSID(t *testing.T) {
	seen := bssAttr([]byte{0x02, 0, 0, 0, 0, 0x01}, 0, false)
	associated := bssAttr([]byte{0x02, 0, 0, 0, 0, 0x02}, unix.NL80211_BSS_STATUS_ASSOCIATED, true)

	bssid, err := parseAssociatedBSSID([][]byte{seen, associated})
	if err != nil {
		t.Fatalf("parseAssociatedBSSID: %v", err)
	}
	if bssid != "02:00:00:00:00:02" {
		t.Errorf("bssid = %q, want the associated entry 02:00:00:00:00:02", bssid)
	}
}

func TestParseAssociatedBSSIDAdHoc(t *testing.T) {
	joined := bssAttr([]byte{0x06, 1, 2, 3, 4, 5}, unix.NL80211_BSS_STATUS_IBSS_JOINED, true)

	bssid, err := parseAssociatedBSSID([][]byte{joined})
	if err != nil || bssid != "06:01:02:03:04:05" {
		t.Errorf("bssid = %q, err = %v; want the joined ad-hoc cell", bssid, err)
	}
}

func TestParseAssociatedBSSIDNone(t *testing.T) {
	authenticated := bssAttr([]byte{0x02, 0, 0, 0, 0, 0x03}, unix.NL80211_BSS_STATUS_AUTHENTICATED, true)

	bssid, err := parseAssociatedBSSID([][]byte{authenticated, encodeAttr(unix.NL80211_ATTR_IFINDEX, u32(3))})
	if err != nil || bssid != "" {
		t.Errorf("bssid = %q, err = %v; want empty: nothing is associated", bssid, err)
	}
}

func TestParseAssociatedBSSIDMalformedNested(t *testing.T) {
	_, err := parseAssociatedBSSID([][]byte{encodeAttr(unix.NL80211_ATTR_BSS, []byte{8, 0, 1, 0})})
	var malformed *NetlinkMalformedError
	if !errors.As(err, &malformed) {
		t.Errorf("err = %v, want NetlinkMalformedError", err)
	}
}

func TestDisplaySSID(t *testing.T) {
	tests := map[string]struct {
		raw  []byte
		want string
	}{
		"ascii":          {[]byte("cafe wifi"), "cafe wifi"},
		"utf-8":          {[]byte("café"), "café"},
		"invalid utf-8":  {[]byte{0xff, 0xfe}, "0xfffe"},
		"escape":         {[]byte("x\x1b[2Jy"), "0x781b5b324a79"},
		"embedded nul":   {[]byte("a\x00"), "0x6100"},
		"empty (hidden)": {[]byte{}, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := displaySSID(tc.raw); got != tc.want {
				t.Errorf("displaySSID(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The controller family is part of every Linux kernel with generic netlink, so
// resolving it exercises the real socket path without wifi hardware. The
// query is read-only.
func TestResolveFamilyController(t *testing.T) {
	conn, err := dialGenetlink()
	if err != nil {
		t.Fatalf("dialGenetlink: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	id, err := conn.resolveFamily("nlctrl")
	if err != nil {
		t.Fatalf("resolveFamily(nlctrl): %v", err)
	}
	if id != unix.GENL_ID_CTRL {
		t.Errorf("id = %d, want GENL_ID_CTRL (%d)", id, unix.GENL_ID_CTRL)
	}
}

func TestResolveFamilyUnknown(t *testing.T) {
	conn, err := dialGenetlink()
	if err != nil {
		t.Fatalf("dialGenetlink: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// Family names are at most 15 bytes; a longer one is EINVAL, not ENOENT.
	_, err = conn.resolveFamily("sysmon-nofam")
	if !errors.Is(err, unix.ENOENT) {
		t.Errorf("err = %v, want ENOENT", err)
	}
}

// Interface index 0 is never a real interface, so the query fails whether or
// not the host has nl80211: with ENOENT for the family, or an error for the
// interface.
func TestReadWifiLinkNoSuchInterface(t *testing.T) {
	if _, err := readWifiLink(0); err == nil {
		t.Error("readWifiLink(0) succeeded, want an error")
	}
}

func TestApplyWifiLinkKeepsStatisticsOnFailure(t *testing.T) {
	w := &types.WirelessInfo{LinkQuality: 60, SignalLevelDBm: -50}
	applyWifiLink(w, 0)

	if w.SSID != "" || w.BSSID != "" {
		t.Errorf("SSID %q, BSSID %q; want empty after a failed query", w.SSID, w.BSSID)
	}
	if w.LinkQuality != 60 || w.SignalLevelDBm != -50 {
		t.Errorf("statistics changed to %+v", *w)
	}
}
