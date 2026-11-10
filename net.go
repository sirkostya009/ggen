package ggen

import (
	"encoding/binary"
	"errors"
	"math/bits"
	"net/netip"
	"strconv"
	"strings"
)

// The writers and parsers below take and return the fields of net.IPNet,
// net.TCPAddr and net.UDPAddr as plain parts. net.IP and net.IPMask are named
// []byte types, so generated code passes and assigns the fields directly.
// That keeps package net, and the cgo resolver it links, out of every program
// that imports the runtime.

var (
	errIPNet       = errors.New("ggen: unsupported value: net.IPNet has no CIDR form")
	errNetAddrPort = errors.New("ggen: unsupported value: port outside of range [0,65535]")
	errNetAddrIP   = errors.New("ggen: unsupported value: IP of invalid length")
	errNetAddrZone = errors.New("ggen: unsupported value: zone without an IPv6 address")
)

// AddrError reports text that is not the string form of a net type. Type
// names the form: "IP address", "CIDR address" or "ip:port address". Text is
// a copy of the rejected text, so it stays valid when the input buffer is
// reused. Error quotes it, so control bytes from a payload never reach a log
// line raw.
type AddrError struct {
	Type string
	Text string
}

func (e *AddrError) Error() string {
	return "invalid " + e.Type + ": " + strconv.Quote(e.Text)
}

// ParseIPNet parses CIDR text such as "10.0.0.0/8" into the IP and Mask of a
// net.IPNet. "" is the zero IPNet, which is what AppendIPNet writes for it.
// The grammar is netip.ParsePrefix's. Host bits are masked off, as
// net.ParseCIDR does. IP and Mask share one allocation, and IP's capacity
// ends where Mask starts, so appending to IP cannot overwrite Mask.
func ParseIPNet(s string) (ip, mask []byte, err error) {
	if s == "" {
		return nil, nil, nil
	}
	// A prefix never has a zone, and netip would intern one before refusing
	// it.
	if strings.IndexByte(s, '%') >= 0 {
		return nil, nil, &AddrError{Type: "CIDR address", Text: strings.Clone(s)}
	}
	p, perr := netip.ParsePrefix(s)
	if perr != nil {
		return nil, nil, &AddrError{Type: "CIDR address", Text: strings.Clone(s)}
	}
	p = p.Masked()
	a, n := p.Addr(), uint(p.Bits())
	if a.Is4() {
		b := make([]byte, 8)
		*(*[4]byte)(b) = a.As4()
		binary.BigEndian.PutUint32(b[4:], ^uint32(0)<<(32-n))
		return b[:4:4], b[4:], nil
	}
	b := make([]byte, 32)
	*(*[16]byte)(b) = a.As16()
	binary.BigEndian.PutUint64(b[16:], ^uint64(0)<<(64-min(n, 64)))
	binary.BigEndian.PutUint64(b[24:], ^uint64(0)<<(128-max(n, 64)))
	return b[:16:16], b[16:], nil
}

// AppendIPNet appends the CIDR text of the net.IPNet with the given IP and
// Mask, then the closing '"'. The caller writes the opening '"'. The text is
// what (*net.IPNet).String returns, "" for an IPNet with neither part. A
// value String would print without a prefix length (a non-canonical mask, or
// an IP and mask that do not match) is an error, since ParseIPNet could not
// read it back.
func AppendIPNet(dst, ip, mask []byte) ([]byte, error) {
	if len(ip) == 0 && len(mask) == 0 {
		return append(dst, '"'), nil
	}
	if v4 := ipv4(ip); v4 != nil {
		ip = v4
	} else if len(ip) != 16 {
		return dst, errIPNet
	}
	switch len(mask) {
	case 4:
		if len(ip) != 4 {
			return dst, errIPNet
		}
	case 16:
		if len(ip) == 4 {
			mask = mask[12:]
		}
	default:
		return dst, errIPNet
	}
	ones, ok := maskOnes(mask)
	if !ok {
		return dst, errIPNet
	}
	a, _ := netip.AddrFromSlice(ip)
	dst = a.AppendTo(dst)
	dst = append(dst, '/')
	dst = strconv.AppendInt(dst, int64(ones), 10)
	return append(dst, '"'), nil
}

// ParseNetAddr parses "ip:port" text into the IP, Port and Zone of a
// net.TCPAddr or net.UDPAddr: "1.2.3.4:80", "[::1]:80", "[fe80::1%eth0]:80",
// or ":80" for an address with no IP. "" is the zero value, which is what
// AppendNetAddr writes for it. Otherwise the grammar is
// netip.ParseAddrPort's: both parts are numeric, and no name or service
// lookup is done. An IPv4-mapped IPv6 address is read as IPv4, as net.IP
// treats it, so a zone on one is an error, since AppendNetAddr could not
// write it back. ip and zone never alias s.
func ParseNetAddr(s string) (ip []byte, port int, zone string, err error) {
	if s == "" {
		return nil, 0, "", nil
	}
	host, digits, ok := strings.CutLast(s, ":")
	if !ok {
		return nil, 0, "", netAddrError(s)
	}
	p, perr := strconv.ParseUint(digits, 10, 16)
	if perr != nil {
		return nil, 0, "", netAddrError(s)
	}
	if host == "" {
		return nil, int(p), "", nil
	}
	// The zone is split off here, because netip would intern it through
	// package unique, and a TCPAddr keeps it as a plain string.
	var a netip.Addr
	if host[0] == '[' {
		if len(host) < 2 || host[len(host)-1] != ']' {
			return nil, 0, "", netAddrError(s)
		}
		host = host[1 : len(host)-1]
		if z := strings.IndexByte(host, '%'); z >= 0 {
			if host, zone = host[:z], host[z+1:]; zone == "" {
				return nil, 0, "", netAddrError(s)
			}
		}
		// Brackets hold an IPv6 address. A mapped one is read as IPv4, which
		// cannot carry the zone.
		a, err = netip.ParseAddr(host)
		if err != nil || !a.Is6() || zone != "" && a.Is4In6() {
			return nil, 0, "", netAddrError(s)
		}
		zone = strings.Clone(zone)
	} else {
		if strings.IndexByte(host, '%') >= 0 {
			return nil, 0, "", netAddrError(s)
		}
		if a, err = netip.ParseAddr(host); err != nil || !a.Is4() {
			return nil, 0, "", netAddrError(s)
		}
	}
	return a.Unmap().AsSlice(), int(p), zone, nil
}

func netAddrError(s string) error {
	return &AddrError{Type: "ip:port address", Text: strings.Clone(s)}
}

// AppendNetAddr appends the "ip:port" text of the net.TCPAddr or net.UDPAddr
// with the given IP, Port and Zone, then the closing '"'. The caller writes
// the opening '"'. The text is what the address's String method returns, ""
// for the zero value. A port outside [0,65535], an IP that is neither 4 nor
// 16 bytes, and a zone on a value without an IPv6 address are errors, since
// ParseNetAddr could not read them back. A zone can hold any bytes, so text
// with one goes through the JSON escape check.
func AppendNetAddr(dst, ip []byte, port int, zone string) ([]byte, error) {
	from := len(dst)
	dst, err := appendNetAddrRaw(dst, ip, port, zone)
	if err != nil || zone == "" {
		return dst, err
	}
	return closeJSONString(dst[:len(dst)-1], from), nil
}

// AppendNetAddrHTML is AppendNetAddr with HTML-safe escaping for text with a
// zone. Text without a zone has no <, > or & to escape.
func AppendNetAddrHTML(dst, ip []byte, port int, zone string) ([]byte, error) {
	from := len(dst)
	dst, err := appendNetAddrRaw(dst, ip, port, zone)
	if err != nil || zone == "" {
		return dst, err
	}
	return CloseJSONStringHTML(dst[:len(dst)-1], from), nil
}

func appendNetAddrRaw(dst, ip []byte, port int, zone string) ([]byte, error) {
	if port < 0 || port > 65535 {
		return dst, errNetAddrPort
	}
	if len(ip) == 0 {
		if zone != "" {
			return dst, errNetAddrZone
		}
		if port == 0 {
			return append(dst, '"'), nil
		}
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(port), 10)
		return append(dst, '"'), nil
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return dst, errNetAddrIP
	}
	// The zone is appended by hand: netip.Addr.WithZone interns it on every
	// call.
	if a = a.Unmap(); a.Is4() {
		if zone != "" {
			return dst, errNetAddrZone
		}
		dst = a.AppendTo(dst)
	} else {
		dst = append(dst, '[')
		dst = a.AppendTo(dst)
		if zone != "" {
			dst = append(dst, '%')
			dst = append(dst, zone...)
		}
		dst = append(dst, ']')
	}
	dst = append(dst, ':')
	dst = strconv.AppendInt(dst, int64(port), 10)
	return append(dst, '"'), nil
}

var v4InV6Prefix = [12]byte{10: 0xff, 11: 0xff}

// ipv4 returns the 4-byte form of an IPv4 or IPv4-mapped IP, as net.IP.To4
// does, and nil for anything else.
func ipv4(ip []byte) []byte {
	switch len(ip) {
	case 4:
		return ip
	case 16:
		if [12]byte(ip[:12]) == v4InV6Prefix {
			return ip[12:]
		}
	}
	return nil
}

// maskOnes returns the number of leading one bits of a canonical mask, ones
// followed by zeros, as net.IPMask.Size counts them. ok is false for any
// other mask.
func maskOnes(m []byte) (ones int, ok bool) {
	for i, b := range m {
		if b == 0xff {
			ones += 8
			continue
		}
		n := bits.LeadingZeros8(^b)
		if b<<n != 0 {
			return 0, false
		}
		for _, z := range m[i+1:] {
			if z != 0 {
				return 0, false
			}
		}
		return ones + n, true
	}
	return ones, true
}
