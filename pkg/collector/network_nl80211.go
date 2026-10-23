//go:build linux

package collector

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"golang.org/x/sys/unix"
)

// The SSID and BSSID of a wifi link come only from nl80211, the kernel's
// generic-netlink wireless API: /proc/net/wireless carries the radio
// statistics but neither name. Both commands used here, GET_INTERFACE and a
// GET_SCAN dump, are unprivileged -- they are what `iw dev <if> link` runs as
// an ordinary user.

const (
	// genlHeaderLen is the size of struct genlmsghdr: cmd, version, reserved.
	genlHeaderLen = 4
	// ctrlVersion and nl80211Version are the generic-netlink header versions
	// the controller and nl80211 families accept.
	ctrlVersion    = 1
	nl80211Version = 0
	// netlinkRecvBuffer holds one recvfrom; the kernel never batches more than
	// 32 KiB of dump messages into one read.
	netlinkRecvBuffer = 64 * 1024
	// netlinkTimeout bounds every read, so a kernel that never answers cannot
	// stall a collection cycle.
	netlinkTimeout = time.Second
	// nlaTypeMask strips the nested and byte-order flags from an attribute type.
	nlaTypeMask = ^uint16(unix.NLA_F_NESTED | unix.NLA_F_NET_BYTEORDER)
)

// NetlinkError reports a netlink operation the kernel refused or that failed
// at the socket. Err is the errno, so errors.Is(err, unix.ENOENT) tells a
// missing family or interface apart from a transport failure.
type NetlinkError struct {
	Op  string
	Err error
}

func (e *NetlinkError) Error() string {
	return fmt.Sprintf("netlink %s: %v", e.Op, e.Err)
}

func (e *NetlinkError) Unwrap() error { return e.Err }

// NetlinkMalformedError reports a message or attribute whose length field
// disagrees with the bytes that carry it.
type NetlinkMalformedError struct {
	What   string
	Length int
	Have   int
}

func (e *NetlinkMalformedError) Error() string {
	return fmt.Sprintf("netlink: malformed %s: length %d, %d bytes available", e.What, e.Length, e.Have)
}

// NetlinkAttrMissingError reports a reply without an attribute it must carry.
type NetlinkAttrMissingError struct {
	Attr string
}

func (e *NetlinkAttrMissingError) Error() string {
	return fmt.Sprintf("netlink: reply has no %s attribute", e.Attr)
}

// wifiLink is the association of a wifi interface: empty strings when the
// radio is not associated.
type wifiLink struct {
	SSID  string
	BSSID string
	// FrequencyMHz is 0 when unknown.
	FrequencyMHz     uint32
	TxBitrateMbps    float64
	RxBitrateMbps    float64
	BitrateAvailable bool
}

// applyWifiLink adds the SSID and BSSID of interface ifIndex to w. A query
// that fails leaves both empty: a link that cannot be named is still a link,
// and its radio statistics are already in w.
func applyWifiLink(w *types.WirelessInfo, ifIndex int) {
	link, err := readWifiLink(ifIndex)
	if err != nil {
		slog.Debug("nl80211 link query failed", "ifindex", ifIndex, "err", err)
		return
	}
	applyLinkDetail(w, link)
}

// applyLinkDetail copies what nl80211 said about the link into w.
func applyLinkDetail(w *types.WirelessInfo, link wifiLink) {
	w.SSID = link.SSID
	w.BSSID = link.BSSID
	if link.FrequencyMHz != 0 {
		w.FrequencyMHz = link.FrequencyMHz
		w.Channel = frequencyToChannel(link.FrequencyMHz)
		w.FrequencyAvailable = true
	}
	w.TxBitrateMbps = link.TxBitrateMbps
	w.RxBitrateMbps = link.RxBitrateMbps
	w.BitrateAvailable = link.BitrateAvailable
}

// frequencyToChannel maps a centre frequency to its IEEE 802.11 channel
// number, as the kernel's ieee80211_freq_khz_to_channel does. It returns 0
// for a frequency outside the 2.4, 4.9/5, 6 and 60 GHz bands.
func frequencyToChannel(mhz uint32) int {
	f := int(mhz)
	switch {
	case f == 2484:
		return 14
	case f >= 2412 && f < 2484:
		return (f - 2407) / 5
	case f >= 4910 && f <= 4980:
		return (f - 4000) / 5
	case f >= 5000 && f < 5925:
		return (f - 5000) / 5
	case f == 5935:
		return 2
	case f >= 5950 && f <= 7125:
		return (f - 5950) / 5
	case f >= 58320 && f <= 70200:
		return (f - 56160) / 2160
	}
	return 0
}

// readWifiLink asks nl80211 for the SSID and associated BSSID of interface
// ifIndex.
func readWifiLink(ifIndex int) (wifiLink, error) {
	conn, err := dialGenetlink()
	if err != nil {
		return wifiLink{}, err
	}
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Debug("close netlink socket", "err", err)
		}
	}()

	family, err := conn.resolveFamily("nl80211")
	if err != nil {
		return wifiLink{}, err
	}

	ifAttr := encodeAttr(unix.NL80211_ATTR_IFINDEX, binary.NativeEndian.AppendUint32(nil, uint32(ifIndex)))

	iface, err := conn.execute(family, nl80211Version, unix.NL80211_CMD_GET_INTERFACE, unix.NLM_F_ACK, ifAttr)
	if err != nil {
		return wifiLink{}, err
	}
	ssid, err := parseInterfaceSSID(iface)
	if err != nil {
		return wifiLink{}, err
	}

	scan, err := conn.execute(family, nl80211Version, unix.NL80211_CMD_GET_SCAN, unix.NLM_F_DUMP, ifAttr)
	if err != nil {
		return wifiLink{}, err
	}
	bss, err := parseAssociatedBSS(scan)
	if err != nil {
		return wifiLink{}, err
	}

	link := wifiLink{SSID: ssid, BSSID: bss.bssid, FrequencyMHz: bss.frequencyMHz}
	if bss.mac == nil {
		return link, nil
	}

	// The station entry for the access point holds the link's rates. A
	// failure here loses only the rates, not what is already known.
	query := slices.Concat(ifAttr, encodeAttr(unix.NL80211_ATTR_MAC, bss.mac))
	station, err := conn.execute(family, nl80211Version, unix.NL80211_CMD_GET_STATION, unix.NLM_F_ACK, query)
	if err != nil {
		slog.Debug("nl80211 station query failed", "ifindex", ifIndex, "err", err)
		return link, nil
	}
	rates, err := parseStationBitrates(station)
	if err != nil {
		slog.Debug("nl80211 station reply malformed", "ifindex", ifIndex, "err", err)
		return link, nil
	}
	link.TxBitrateMbps = rates.txMbps
	link.RxBitrateMbps = rates.rxMbps
	link.BitrateAvailable = rates.available
	return link, nil
}

// genetlinkConn is a generic-netlink socket that runs one request at a time.
type genetlinkConn struct {
	fd  int
	seq uint32
	buf []byte
}

// dialGenetlink opens and binds a generic-netlink socket.
func dialGenetlink() (*genetlinkConn, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_GENERIC)
	if err != nil {
		return nil, &NetlinkError{Op: "socket", Err: err}
	}
	conn := &genetlinkConn{fd: fd, buf: make([]byte, netlinkRecvBuffer)}

	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, errors.Join(&NetlinkError{Op: "bind", Err: err}, conn.Close())
	}
	tv := unix.NsecToTimeval(netlinkTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return nil, errors.Join(&NetlinkError{Op: "setsockopt", Err: err}, conn.Close())
	}
	return conn, nil
}

// Close releases the socket.
func (c *genetlinkConn) Close() error {
	if err := unix.Close(c.fd); err != nil {
		return &NetlinkError{Op: "close", Err: err}
	}
	return nil
}

// resolveFamily returns the numeric id of the generic-netlink family name. An
// unknown family -- nl80211 on a host without cfg80211 -- is a NetlinkError
// wrapping ENOENT.
func (c *genetlinkConn) resolveFamily(name string) (uint16, error) {
	attr := encodeAttr(unix.CTRL_ATTR_FAMILY_NAME, append([]byte(name), 0))
	replies, err := c.execute(unix.GENL_ID_CTRL, ctrlVersion, unix.CTRL_CMD_GETFAMILY, unix.NLM_F_ACK, attr)
	if err != nil {
		return 0, err
	}
	return parseFamilyID(replies)
}

// execute sends one request and returns the attribute block of every reply,
// reading until the kernel's acknowledgement or the end of the dump. flags is
// NLM_F_ACK for a single request or NLM_F_DUMP for a dump; either way the
// kernel ends with a message execute recognises as the last.
func (c *genetlinkConn) execute(family uint16, version, cmd uint8, flags uint16, attrs []byte) ([][]byte, error) {
	c.seq++
	req := encodeGenlRequest(family, unix.NLM_F_REQUEST|flags, c.seq, cmd, version, attrs)
	if err := unix.Sendto(c.fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, &NetlinkError{Op: "send", Err: err}
	}

	var replies [][]byte
	for {
		n, _, err := unix.Recvfrom(c.fd, c.buf, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, &NetlinkError{Op: "receive", Err: err}
		}
		payloads, done, err := parseNetlinkReplies(c.buf[:n], c.seq)
		if err != nil {
			return nil, err
		}
		// The payloads point into c.buf, which the next read overwrites.
		for _, p := range payloads {
			replies = append(replies, bytes.Clone(p))
		}
		if done {
			return replies, nil
		}
	}
}

// encodeGenlRequest builds a netlink message carrying a generic-netlink
// header and attrs.
func encodeGenlRequest(family, flags uint16, seq uint32, cmd, version uint8, attrs []byte) []byte {
	length := unix.SizeofNlMsghdr + genlHeaderLen + len(attrs)
	msg := make([]byte, 0, length)
	msg = binary.NativeEndian.AppendUint32(msg, uint32(length))
	msg = binary.NativeEndian.AppendUint16(msg, family)
	msg = binary.NativeEndian.AppendUint16(msg, flags)
	msg = binary.NativeEndian.AppendUint32(msg, seq)
	msg = binary.NativeEndian.AppendUint32(msg, 0) // port id: the kernel assigns it
	msg = append(msg, cmd, version, 0, 0)
	return append(msg, attrs...)
}

// encodeAttr encodes one netlink attribute, padded to the 4-byte alignment
// the kernel expects of the next one.
func encodeAttr(typ uint16, data []byte) []byte {
	length := unix.SizeofNlAttr + len(data)
	attr := make([]byte, 0, nlaAlign(length))
	attr = binary.NativeEndian.AppendUint16(attr, uint16(length))
	attr = binary.NativeEndian.AppendUint16(attr, typ)
	attr = append(attr, data...)
	for len(attr) < nlaAlign(length) {
		attr = append(attr, 0)
	}
	return attr
}

// nlaAlign rounds n up to netlink's 4-byte alignment.
func nlaAlign(n int) int {
	return (n + unix.NLA_ALIGNTO - 1) &^ (unix.NLA_ALIGNTO - 1)
}

// parseNetlinkReplies splits one read into messages answering request seq and
// returns their attribute blocks, the generic-netlink header removed. done
// reports the acknowledgement or end-of-dump message; a kernel error is a
// NetlinkError carrying its errno. Messages for another sequence number are
// stale replies and are skipped.
func parseNetlinkReplies(buf []byte, seq uint32) (payloads [][]byte, done bool, err error) {
	for len(buf) > 0 {
		if len(buf) < unix.SizeofNlMsghdr {
			return nil, false, &NetlinkMalformedError{What: "message header", Length: unix.SizeofNlMsghdr, Have: len(buf)}
		}
		length := int(binary.NativeEndian.Uint32(buf[0:4]))
		if length < unix.SizeofNlMsghdr || length > len(buf) {
			return nil, false, &NetlinkMalformedError{What: "message", Length: length, Have: len(buf)}
		}
		msgType := binary.NativeEndian.Uint16(buf[4:6])
		msgSeq := binary.NativeEndian.Uint32(buf[8:12])
		body := buf[unix.SizeofNlMsghdr:length]
		buf = buf[min(nlaAlign(length), len(buf)):]

		if msgSeq != seq {
			continue
		}
		switch msgType {
		case unix.NLMSG_ERROR, unix.NLMSG_DONE:
			// Both carry a leading int32: an errno, negated, or 0 for success.
			if len(body) < 4 {
				return nil, false, &NetlinkMalformedError{What: "status", Length: 4, Have: len(body)}
			}
			if code := int32(binary.NativeEndian.Uint32(body[:4])); code != 0 {
				return nil, false, &NetlinkError{Op: "request", Err: unix.Errno(-code)}
			}
			return payloads, true, nil
		default:
			if len(body) < genlHeaderLen {
				return nil, false, &NetlinkMalformedError{What: "generic netlink header", Length: genlHeaderLen, Have: len(body)}
			}
			payloads = append(payloads, body[genlHeaderLen:])
		}
	}
	return payloads, false, nil
}

// parseAttrs indexes an attribute block by type. Later duplicates win, which
// nl80211 never sends.
func parseAttrs(buf []byte) (map[uint16][]byte, error) {
	attrs := make(map[uint16][]byte)
	for len(buf) > 0 {
		if len(buf) < unix.SizeofNlAttr {
			return nil, &NetlinkMalformedError{What: "attribute header", Length: unix.SizeofNlAttr, Have: len(buf)}
		}
		length := int(binary.NativeEndian.Uint16(buf[0:2]))
		if length < unix.SizeofNlAttr || length > len(buf) {
			return nil, &NetlinkMalformedError{What: "attribute", Length: length, Have: len(buf)}
		}
		typ := binary.NativeEndian.Uint16(buf[2:4]) & nlaTypeMask
		attrs[typ] = buf[unix.SizeofNlAttr:length]
		buf = buf[min(nlaAlign(length), len(buf)):]
	}
	return attrs, nil
}

// parseFamilyID reads CTRL_ATTR_FAMILY_ID from a CTRL_CMD_GETFAMILY reply.
func parseFamilyID(replies [][]byte) (uint16, error) {
	for _, reply := range replies {
		attrs, err := parseAttrs(reply)
		if err != nil {
			return 0, err
		}
		if id, ok := attrs[unix.CTRL_ATTR_FAMILY_ID]; ok && len(id) >= 2 {
			return binary.NativeEndian.Uint16(id[:2]), nil
		}
	}
	return 0, &NetlinkAttrMissingError{Attr: "CTRL_ATTR_FAMILY_ID"}
}

// parseInterfaceSSID reads NL80211_ATTR_SSID from a GET_INTERFACE reply. The
// kernel includes it only while a station is associated, so its absence is
// an empty SSID rather than an error.
func parseInterfaceSSID(replies [][]byte) (string, error) {
	for _, reply := range replies {
		attrs, err := parseAttrs(reply)
		if err != nil {
			return "", err
		}
		if ssid, ok := attrs[unix.NL80211_ATTR_SSID]; ok {
			return displaySSID(ssid), nil
		}
	}
	return "", nil
}

// associatedBSS is the scan entry the interface is associated with.
type associatedBSS struct {
	bssid        string // formatted MAC; empty when there is no entry
	mac          []byte // raw MAC, nil when there is no entry
	frequencyMHz uint32 // 0 when the entry does not carry it
}

// parseAssociatedBSS returns the scan entry the interface is associated
// with, or joined to for an ad-hoc network: the zero value when there is
// none.
func parseAssociatedBSS(replies [][]byte) (associatedBSS, error) {
	for _, reply := range replies {
		attrs, err := parseAttrs(reply)
		if err != nil {
			return associatedBSS{}, err
		}
		nested, ok := attrs[unix.NL80211_ATTR_BSS]
		if !ok {
			continue
		}
		bss, err := parseAttrs(nested)
		if err != nil {
			return associatedBSS{}, err
		}
		status, ok := bss[unix.NL80211_BSS_STATUS]
		if !ok || len(status) < 4 {
			continue
		}
		switch binary.NativeEndian.Uint32(status[:4]) {
		case unix.NL80211_BSS_STATUS_ASSOCIATED, unix.NL80211_BSS_STATUS_IBSS_JOINED:
			mac := bss[unix.NL80211_BSS_BSSID]
			if len(mac) != 6 {
				continue
			}
			entry := associatedBSS{bssid: net.HardwareAddr(mac).String(), mac: mac}
			if freq := bss[unix.NL80211_BSS_FREQUENCY]; len(freq) >= 4 {
				entry.frequencyMHz = binary.NativeEndian.Uint32(freq[:4])
			}
			return entry, nil
		}
	}
	return associatedBSS{}, nil
}

// stationBitrates is the link rate to one station, in Mbit/s.
type stationBitrates struct {
	txMbps, rxMbps float64
	// available is true when either direction was reported.
	available bool
}

// parseStationBitrates reads the transmit and receive rates from a
// NL80211_CMD_GET_STATION reply.
func parseStationBitrates(replies [][]byte) (stationBitrates, error) {
	for _, reply := range replies {
		attrs, err := parseAttrs(reply)
		if err != nil {
			return stationBitrates{}, err
		}
		nested, ok := attrs[unix.NL80211_ATTR_STA_INFO]
		if !ok {
			continue
		}
		info, err := parseAttrs(nested)
		if err != nil {
			return stationBitrates{}, err
		}
		var rates stationBitrates
		var okTx, okRx bool
		if rates.txMbps, okTx, err = parseRateInfo(info[unix.NL80211_STA_INFO_TX_BITRATE]); err != nil {
			return stationBitrates{}, err
		}
		if rates.rxMbps, okRx, err = parseRateInfo(info[unix.NL80211_STA_INFO_RX_BITRATE]); err != nil {
			return stationBitrates{}, err
		}
		rates.available = okTx || okRx
		return rates, nil
	}
	return stationBitrates{}, nil
}

// parseRateInfo reads a nested NL80211_RATE_INFO block's bitrate, which the
// kernel gives in units of 100 kbit/s: the 32-bit attribute when present,
// else the older 16-bit one. It reports false for an absent block or one
// with neither attribute.
func parseRateInfo(nested []byte) (float64, bool, error) {
	if nested == nil {
		return 0, false, nil
	}
	rate, err := parseAttrs(nested)
	if err != nil {
		return 0, false, err
	}
	if v := rate[unix.NL80211_RATE_INFO_BITRATE32]; len(v) >= 4 {
		return float64(binary.NativeEndian.Uint32(v[:4])) / 10, true, nil
	}
	if v := rate[unix.NL80211_RATE_INFO_BITRATE]; len(v) >= 2 {
		return float64(binary.NativeEndian.Uint16(v[:2])) / 10, true, nil
	}
	return 0, false, nil
}

// displaySSID renders an SSID, which is up to 32 arbitrary octets rather than
// text. A printable UTF-8 name is shown as it is; anything else is shown in
// hex, so a crafted name cannot carry terminal escape sequences into the CLI.
func displaySSID(raw []byte) string {
	s := string(raw)
	if utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return s
	}
	return fmt.Sprintf("0x%x", raw)
}
