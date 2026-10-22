package ggen

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

var (
	errIPNet       = errors.New("ggen: unsupported value: net.IPNet has no CIDR form")
	errNetAddrPort = errors.New("ggen: unsupported value: port outside of range [0,65535]")
	errNetAddrIP   = errors.New("ggen: unsupported value: IP of invalid length")
	errNetAddrZone = errors.New("ggen: unsupported value: zone without an IPv6 address")
)

// ParseIPNet parses CIDR text such as "10.0.0.0/8" into a net.IPNet. "" is
// the zero IPNet, which is what AppendIPNet writes for it. Host bits are
// masked off, as net.ParseCIDR does. The grammar is netip.ParsePrefix's.
// The error never retains s, so s may alias a buffer that is reused.
func ParseIPNet(s string) (net.IPNet, error) {
	if s == "" {
		return net.IPNet{}, nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return net.IPNet{}, &net.ParseError{Type: "CIDR address", Text: strings.Clone(s)}
	}
	p = p.Masked()
	a := p.Addr()
	return net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(p.Bits(), a.BitLen())}, nil
}

// AppendIPNet appends the CIDR text of n and the closing '"'. The caller
// writes the opening '"'. The text is what (*net.IPNet).String returns, ""
// for the zero IPNet. A value String would print without a prefix length
// (a non-canonical mask, or an IP and mask that do not match) is an error,
// since ParseIPNet could not read it back.
func AppendIPNet(dst []byte, n net.IPNet) ([]byte, error) {
	if len(n.IP) == 0 && len(n.Mask) == 0 {
		return append(dst, '"'), nil
	}
	ip := n.IP.To4()
	if ip == nil {
		ip = n.IP
		if len(ip) != net.IPv6len {
			return dst, errIPNet
		}
	}
	m := n.Mask
	switch len(m) {
	case net.IPv4len:
		if len(ip) != net.IPv4len {
			return dst, errIPNet
		}
	case net.IPv6len:
		if len(ip) == net.IPv4len {
			m = m[12:]
		}
	default:
		return dst, errIPNet
	}
	ones, bits := m.Size()
	if ones == 0 && bits == 0 {
		return dst, errIPNet
	}
	a, _ := netip.AddrFromSlice(ip)
	dst, _ = a.AppendText(dst)
	dst = append(dst, '/')
	dst = strconv.AppendInt(dst, int64(ones), 10)
	return append(dst, '"'), nil
}

// ParseNetAddr parses "ip:port" text into a net.TCPAddr or net.UDPAddr:
// "1.2.3.4:80", "[::1]:80", "[fe80::1%eth0]:80", or ":80" for a value
// with no IP. "" is the zero value, which is what AppendNetAddr writes for
// it. Both parts must be numeric. No name or service lookup is done. An
// IPv4-mapped IPv6 address is read as IPv4, as net.IP treats it. The error
// never retains s, so s may alias a buffer that is reused.
func ParseNetAddr[T net.TCPAddr | net.UDPAddr](s string) (T, error) {
	if s == "" {
		return T{}, nil
	}
	if s[0] == ':' {
		port, err := strconv.ParseUint(s[1:], 10, 16)
		if err != nil {
			return T{}, &net.ParseError{Type: "ip:port address", Text: strings.Clone(s)}
		}
		return T(net.TCPAddr{Port: int(port)}), nil
	}
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return T{}, &net.ParseError{Type: "ip:port address", Text: strings.Clone(s)}
	}
	a := ap.Addr()
	zone := a.Zone()
	return T(net.TCPAddr{IP: a.WithZone("").Unmap().AsSlice(), Port: int(ap.Port()), Zone: zone}), nil
}

// AppendNetAddr appends the "ip:port" text of a and the closing '"'. The
// caller writes the opening '"'. The text is what a.String returns, ""
// for the zero value. A port outside [0,65535], an IP that is neither 4 nor
// 16 bytes, and a zone on a value without an IPv6 address are errors, since
// ParseNetAddr could not read them back. A zone can hold any bytes, so text
// with one goes through the JSON escape check.
func AppendNetAddr[T net.TCPAddr | net.UDPAddr](dst []byte, a T) ([]byte, error) {
	from := len(dst)
	dst, err := appendNetAddrRaw(dst, net.TCPAddr(a))
	if err != nil || net.TCPAddr(a).Zone == "" {
		return dst, err
	}
	return closeJSONString(dst[:len(dst)-1], from), nil
}

// AppendNetAddrHTML is AppendNetAddr with HTML-safe escaping for text with a
// zone. Text without a zone has no <, > or & to escape.
func AppendNetAddrHTML[T net.TCPAddr | net.UDPAddr](dst []byte, a T) ([]byte, error) {
	from := len(dst)
	dst, err := appendNetAddrRaw(dst, net.TCPAddr(a))
	if err != nil || net.TCPAddr(a).Zone == "" {
		return dst, err
	}
	return CloseJSONStringHTML(dst[:len(dst)-1], from), nil
}

func appendNetAddrRaw(dst []byte, a net.TCPAddr) ([]byte, error) {
	if a.Port < 0 || a.Port > 65535 {
		return dst, errNetAddrPort
	}
	if len(a.IP) == 0 {
		if a.Zone != "" {
			return dst, errNetAddrZone
		}
		if a.Port == 0 {
			return append(dst, '"'), nil
		}
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(a.Port), 10)
		return append(dst, '"'), nil
	}
	ip, ok := netip.AddrFromSlice(a.IP)
	if !ok {
		return dst, errNetAddrIP
	}
	ip = ip.Unmap()
	if a.Zone != "" {
		if ip.Is4() {
			return dst, errNetAddrZone
		}
		ip = ip.WithZone(a.Zone)
	}
	dst, _ = netip.AddrPortFrom(ip, uint16(a.Port)).AppendText(dst)
	return append(dst, '"'), nil
}
