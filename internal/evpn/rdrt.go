// Package evpn holds EVPN identifiers and the bridge/VLAN/VNI topology used
// by Snout. It has no dependencies outside the standard library so that both
// the configuration loader and the GoBGP publisher can share one set of rules.
package evpn

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// RDType is the encoding of a route distinguisher (RFC 4364, section 4.2).
type RDType int

const (
	// RDTwoOctetASN is type 0: 2-byte ASN : 4-byte assigned number.
	RDTwoOctetASN RDType = iota
	// RDIPv4 is type 1: IPv4 address : 2-byte assigned number.
	RDIPv4
	// RDFourOctetASN is type 2: 4-byte ASN : 2-byte assigned number.
	RDFourOctetASN
)

// RouteDistinguisher is a parsed route distinguisher.
type RouteDistinguisher struct {
	Type RDType
	// IP is set for RDIPv4.
	IP string
	// ASN is set for RDTwoOctetASN and RDFourOctetASN.
	ASN      uint32
	Assigned uint32
}

// String returns the canonical administrator:assigned form.
func (rd RouteDistinguisher) String() string {
	if rd.Type == RDIPv4 {
		return fmt.Sprintf("%s:%d", rd.IP, rd.Assigned)
	}
	return fmt.Sprintf("%d:%d", rd.ASN, rd.Assigned)
}

// ParseRD parses "ASN:number" or "IPv4:number".
func ParseRD(value string) (RouteDistinguisher, error) {
	admin, assigned, err := splitAdminAssigned("route distinguisher", value)
	if err != nil {
		return RouteDistinguisher{}, err
	}
	if ip := net.ParseIP(admin); ip != nil {
		ipv4 := ip.To4()
		if ipv4 == nil {
			return RouteDistinguisher{}, fmt.Errorf("route distinguisher %q: IPv6 administrator is not supported", value)
		}
		if assigned > 65535 {
			return RouteDistinguisher{}, fmt.Errorf("route distinguisher %q: IPv4 assigned value exceeds 65535", value)
		}
		return RouteDistinguisher{Type: RDIPv4, IP: ipv4.String(), Assigned: uint32(assigned)}, nil
	}
	asn, err := strconv.ParseUint(admin, 10, 32)
	if err != nil {
		return RouteDistinguisher{}, fmt.Errorf("route distinguisher %q: invalid administrator %q", value, admin)
	}
	if asn <= 65535 {
		return RouteDistinguisher{Type: RDTwoOctetASN, ASN: uint32(asn), Assigned: uint32(assigned)}, nil
	}
	if assigned > 65535 {
		return RouteDistinguisher{}, fmt.Errorf("route distinguisher %q: four-octet ASN assigned value exceeds 65535", value)
	}
	return RouteDistinguisher{Type: RDFourOctetASN, ASN: uint32(asn), Assigned: uint32(assigned)}, nil
}

// RouteTarget is a parsed ASN-based route target extended community.
type RouteTarget struct {
	// FourOctet selects the four-octet AS specific encoding (ASN > 65535).
	FourOctet bool
	ASN       uint32
	Assigned  uint32
}

// String returns the canonical ASN:assigned form.
func (rt RouteTarget) String() string {
	return fmt.Sprintf("%d:%d", rt.ASN, rt.Assigned)
}

// ParseRouteTarget parses "ASN:number".
func ParseRouteTarget(value string) (RouteTarget, error) {
	admin, assigned, err := splitAdminAssigned("route target", value)
	if err != nil {
		return RouteTarget{}, err
	}
	asn, err := strconv.ParseUint(admin, 10, 32)
	if err != nil {
		return RouteTarget{}, fmt.Errorf("route target %q must use ASN:assigned form", value)
	}
	if asn <= 65535 {
		return RouteTarget{ASN: uint32(asn), Assigned: uint32(assigned)}, nil
	}
	if assigned > 65535 {
		return RouteTarget{}, fmt.Errorf("route target %q: four-octet ASN assigned value exceeds 65535", value)
	}
	return RouteTarget{FourOctet: true, ASN: uint32(asn), Assigned: uint32(assigned)}, nil
}

func splitAdminAssigned(name, value string) (string, uint64, error) {
	separator := strings.LastIndex(value, ":")
	if separator < 1 || separator == len(value)-1 {
		return "", 0, fmt.Errorf("%s %q must have administrator:assigned form", name, value)
	}
	assigned, err := strconv.ParseUint(value[separator+1:], 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("%s %q: invalid assigned value", name, value)
	}
	return value[:separator], assigned, nil
}
