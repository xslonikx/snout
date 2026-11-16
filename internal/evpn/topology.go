package evpn

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

const (
	MaxVNI  = 16777215
	MaxVLAN = 4094
	MaxASN  = 4294967295
)

// Spec is the raw, unvalidated topology as written in the configuration.
// Optional numbers are pointers so that "absent" and "zero" differ.
type Spec struct {
	VTEP    string
	ASN     *int64
	Bridges []BridgeSpec
}

// BridgeSpec describes one [[bridge]] table.
type BridgeSpec struct {
	Name string

	// Flat mode.
	VNI         *int64
	RD          string
	RouteTarget string

	// VLAN-aware mode.
	VNIOffset *int64
	VLANs     []string
	VNIs      []VNISpec
}

// VNISpec describes one explicit [[bridge.vni]] entry.
type VNISpec struct {
	VLAN        *int64
	VNI         *int64
	RD          string
	RouteTarget string
}

// Segment is one active VNI on a bridge. VLAN is 0 on a flat bridge.
type Segment struct {
	VLAN        uint16
	VNI         uint32
	RD          string
	RouteTarget string
}

// Bridge is a validated bridge with its active segments.
type Bridge struct {
	Name     string
	Flat     bool
	Segments []Segment

	byVNI  map[uint32]int
	byVLAN map[uint16]int
}

// NewBridge validates the segment list of one bridge and indexes it.
func NewBridge(name string, flat bool, segments []Segment) (Bridge, error) {
	if name == "" {
		return Bridge{}, errors.New("bridge name cannot be empty")
	}
	if len(segments) == 0 {
		return Bridge{}, fmt.Errorf("bridge %q has no active VNI", name)
	}
	if flat && len(segments) != 1 {
		return Bridge{}, fmt.Errorf("flat bridge %q must have exactly one VNI", name)
	}
	sorted := append([]Segment(nil), segments...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].VLAN != sorted[j].VLAN {
			return sorted[i].VLAN < sorted[j].VLAN
		}
		return sorted[i].VNI < sorted[j].VNI
	})
	bridge := Bridge{Name: name, Flat: flat, Segments: sorted, byVNI: make(map[uint32]int), byVLAN: make(map[uint16]int)}
	for index, segment := range sorted {
		if segment.VNI == 0 || segment.VNI > MaxVNI {
			return Bridge{}, fmt.Errorf("bridge %q: VNI %d is outside 1-%d", name, segment.VNI, MaxVNI)
		}
		if flat && segment.VLAN != 0 {
			return Bridge{}, fmt.Errorf("flat bridge %q cannot use VLAN %d", name, segment.VLAN)
		}
		if !flat && (segment.VLAN == 0 || segment.VLAN > MaxVLAN) {
			return Bridge{}, fmt.Errorf("bridge %q: VLAN %d is outside 1-%d", name, segment.VLAN, MaxVLAN)
		}
		if _, err := ParseRD(segment.RD); err != nil {
			return Bridge{}, fmt.Errorf("bridge %q VNI %d: %w", name, segment.VNI, err)
		}
		if _, err := ParseRouteTarget(segment.RouteTarget); err != nil {
			return Bridge{}, fmt.Errorf("bridge %q VNI %d: %w", name, segment.VNI, err)
		}
		if _, exists := bridge.byVNI[segment.VNI]; exists {
			return Bridge{}, fmt.Errorf("bridge %q: duplicate VNI %d", name, segment.VNI)
		}
		if _, exists := bridge.byVLAN[segment.VLAN]; exists {
			return Bridge{}, fmt.Errorf("bridge %q: duplicate VLAN %d", name, segment.VLAN)
		}
		bridge.byVNI[segment.VNI] = index
		bridge.byVLAN[segment.VLAN] = index
	}
	return bridge, nil
}

// SegmentByVNI returns the active segment that carries vni.
func (b Bridge) SegmentByVNI(vni uint32) (Segment, bool) {
	index, ok := b.byVNI[vni]
	if !ok {
		return Segment{}, false
	}
	return b.Segments[index], true
}

// SegmentByVLAN returns the active segment mapped to vlan (0 on a flat bridge).
func (b Bridge) SegmentByVLAN(vlan uint16) (Segment, bool) {
	index, ok := b.byVLAN[vlan]
	if !ok {
		return Segment{}, false
	}
	return b.Segments[index], true
}

// Topology is the validated set of bridges served by one Snout instance.
type Topology struct {
	VTEP    string
	Bridges []Bridge
}

// BridgeForVNI returns the bridge and segment that own vni.
func (t Topology) BridgeForVNI(vni uint32) (Bridge, Segment, bool) {
	for _, bridge := range t.Bridges {
		if segment, ok := bridge.SegmentByVNI(vni); ok {
			return bridge, segment, true
		}
	}
	return Bridge{}, Segment{}, false
}

// Resolve validates spec, applies the vni_offset and RD/RT defaults, and
// returns the resulting topology.
func Resolve(spec Spec) (Topology, error) {
	vtep := net.ParseIP(strings.TrimSpace(spec.VTEP))
	if vtep == nil {
		return Topology{}, fmt.Errorf("vtep must be a valid IP address, got %q", spec.VTEP)
	}
	if spec.ASN != nil && (*spec.ASN < 1 || *spec.ASN > MaxASN) {
		return Topology{}, fmt.Errorf("asn must be 1-%d, got %d", int64(MaxASN), *spec.ASN)
	}
	if len(spec.Bridges) == 0 {
		return Topology{}, errors.New("at least one [[bridge]] is required")
	}
	defaults := defaulter{vtep: vtep, asn: spec.ASN}
	topology := Topology{VTEP: vtep.String()}
	bridgeNames := make(map[string]struct{})
	vniOwners := make(map[uint32]string)
	rdOwners := make(map[string]string)
	for index, bridgeSpec := range spec.Bridges {
		name := strings.TrimSpace(bridgeSpec.Name)
		if name == "" {
			return Topology{}, fmt.Errorf("bridge[%d]: name is required", index)
		}
		if _, exists := bridgeNames[name]; exists {
			return Topology{}, fmt.Errorf("bridge %q is defined more than once", name)
		}
		bridgeNames[name] = struct{}{}

		var bridge Bridge
		var err error
		if bridgeSpec.VNI != nil {
			bridge, err = resolveFlat(name, bridgeSpec, defaults)
		} else {
			bridge, err = resolveVLANAware(name, bridgeSpec, defaults)
		}
		if err != nil {
			return Topology{}, err
		}
		for _, segment := range bridge.Segments {
			owner := fmt.Sprintf("bridge %q VLAN %d", name, segment.VLAN)
			if bridge.Flat {
				owner = fmt.Sprintf("flat bridge %q", name)
			}
			if previous, exists := vniOwners[segment.VNI]; exists {
				return Topology{}, fmt.Errorf("VNI %d is used by both %s and %s", segment.VNI, previous, owner)
			}
			vniOwners[segment.VNI] = owner
			rd, _ := ParseRD(segment.RD)
			canonical := rd.String()
			if previous, exists := rdOwners[canonical]; exists {
				return Topology{}, fmt.Errorf("route distinguisher %s is used by both %s and %s; set an explicit rd", canonical, previous, owner)
			}
			rdOwners[canonical] = owner
		}
		topology.Bridges = append(topology.Bridges, bridge)
	}
	return topology, nil
}

func resolveFlat(name string, spec BridgeSpec, defaults defaulter) (Bridge, error) {
	if spec.VNIOffset != nil || len(spec.VLANs) != 0 || len(spec.VNIs) != 0 {
		return Bridge{}, fmt.Errorf("bridge %q: vni (flat mode) cannot be combined with vni_offset, vlans, or [[bridge.vni]]", name)
	}
	vni, err := checkVNI(*spec.VNI)
	if err != nil {
		return Bridge{}, fmt.Errorf("bridge %q: vni: %w", name, err)
	}
	segment := Segment{VLAN: 0, VNI: vni}
	where := fmt.Sprintf("flat bridge %q", name)
	if segment.RD, err = defaults.rd(spec.RD, uint64(vni), where); err != nil {
		return Bridge{}, err
	}
	if segment.RouteTarget, err = defaults.routeTarget(spec.RouteTarget, vni, where); err != nil {
		return Bridge{}, err
	}
	return NewBridge(name, true, []Segment{segment})
}

func resolveVLANAware(name string, spec BridgeSpec, defaults defaulter) (Bridge, error) {
	if spec.RD != "" || spec.RouteTarget != "" {
		return Bridge{}, fmt.Errorf("bridge %q: rd and route_target are allowed only on a flat bridge or in [[bridge.vni]]", name)
	}
	if spec.VNIOffset == nil && len(spec.VLANs) == 0 && len(spec.VNIs) == 0 {
		return Bridge{}, fmt.Errorf("bridge %q: set vni (flat mode) or vlans/[[bridge.vni]] (VLAN-aware mode)", name)
	}
	var offset int64
	if spec.VNIOffset != nil {
		offset = *spec.VNIOffset
		if offset < 0 || offset >= MaxVNI {
			return Bridge{}, fmt.Errorf("bridge %q: vni_offset must be 0-%d, got %d", name, MaxVNI-1, offset)
		}
	}
	if len(spec.VLANs) != 0 && spec.VNIOffset == nil {
		return Bridge{}, fmt.Errorf("bridge %q: vlans requires vni_offset", name)
	}

	listed, err := parseVLANList(spec.VLANs)
	if err != nil {
		return Bridge{}, fmt.Errorf("bridge %q: vlans: %w", name, err)
	}
	explicit := make(map[uint16]Segment)
	for index, entry := range spec.VNIs {
		where := fmt.Sprintf("bridge %q vni[%d]", name, index)
		if entry.VLAN == nil {
			return Bridge{}, fmt.Errorf("%s: vlan is required", where)
		}
		if entry.VNI == nil {
			return Bridge{}, fmt.Errorf("%s: vni is required", where)
		}
		vlan, err := checkVLAN(*entry.VLAN)
		if err != nil {
			return Bridge{}, fmt.Errorf("%s: vlan: %w", where, err)
		}
		vni, err := checkVNI(*entry.VNI)
		if err != nil {
			return Bridge{}, fmt.Errorf("%s: vni: %w", where, err)
		}
		if _, exists := explicit[vlan]; exists {
			return Bridge{}, fmt.Errorf("%s: VLAN %d has more than one [[bridge.vni]] entry", where, vlan)
		}
		segment := Segment{VLAN: vlan, VNI: vni}
		where = fmt.Sprintf("bridge %q VLAN %d", name, vlan)
		if segment.RD, err = defaults.rd(entry.RD, uint64(vlan), where); err != nil {
			return Bridge{}, err
		}
		if segment.RouteTarget, err = defaults.routeTarget(entry.RouteTarget, vni, where); err != nil {
			return Bridge{}, err
		}
		explicit[vlan] = segment
	}

	segments := make([]Segment, 0, len(listed)+len(explicit))
	for _, segment := range explicit {
		segments = append(segments, segment)
	}
	for _, vlan := range listed {
		if _, overridden := explicit[vlan]; overridden {
			continue
		}
		vni := int64(vlan) + offset
		if vni > MaxVNI {
			return Bridge{}, fmt.Errorf("bridge %q VLAN %d: VNI %d (vlan + vni_offset) exceeds %d", name, vlan, vni, MaxVNI)
		}
		segment := Segment{VLAN: vlan, VNI: uint32(vni)}
		where := fmt.Sprintf("bridge %q VLAN %d", name, vlan)
		if segment.RD, err = defaults.rd("", uint64(vlan), where); err != nil {
			return Bridge{}, err
		}
		if segment.RouteTarget, err = defaults.routeTarget("", segment.VNI, where); err != nil {
			return Bridge{}, err
		}
		segments = append(segments, segment)
	}
	return NewBridge(name, false, segments)
}

type defaulter struct {
	vtep net.IP
	asn  *int64
}

func (d defaulter) rd(explicit string, assigned uint64, where string) (string, error) {
	if explicit != "" {
		rd, err := ParseRD(explicit)
		if err != nil {
			return "", fmt.Errorf("%s: rd: %w", where, err)
		}
		return rd.String(), nil
	}
	ipv4 := d.vtep.To4()
	if ipv4 == nil {
		return "", fmt.Errorf("%s: an explicit rd is required because vtep is not IPv4", where)
	}
	if assigned > 65535 {
		return "", fmt.Errorf("%s: default rd %s:%d exceeds 65535; set an explicit rd", where, ipv4, assigned)
	}
	return fmt.Sprintf("%s:%d", ipv4, assigned), nil
}

func (d defaulter) routeTarget(explicit string, vni uint32, where string) (string, error) {
	if explicit != "" {
		rt, err := ParseRouteTarget(explicit)
		if err != nil {
			return "", fmt.Errorf("%s: route_target: %w", where, err)
		}
		return rt.String(), nil
	}
	if d.asn == nil {
		return "", fmt.Errorf("%s: route_target is required when asn is not set", where)
	}
	if *d.asn > 65535 {
		return "", fmt.Errorf("%s: route_target is required because asn %d is above 65535", where, *d.asn)
	}
	return fmt.Sprintf("%d:%d", *d.asn, vni), nil
}

func checkVNI(value int64) (uint32, error) {
	if value < 1 || value > MaxVNI {
		return 0, fmt.Errorf("must be 1-%d, got %d", MaxVNI, value)
	}
	return uint32(value), nil
}

func checkVLAN(value int64) (uint16, error) {
	if value < 1 || value > MaxVLAN {
		return 0, fmt.Errorf("must be 1-%d, got %d", MaxVLAN, value)
	}
	return uint16(value), nil
}

// parseVLANList expands items such as "100" and "300-310" and rejects
// duplicates.
func parseVLANList(items []string) ([]uint16, error) {
	seen := make(map[uint16]struct{})
	var vlans []uint16
	add := func(vlan uint16, item string) error {
		if _, exists := seen[vlan]; exists {
			return fmt.Errorf("VLAN %d is listed more than once (%q)", vlan, item)
		}
		seen[vlan] = struct{}{}
		vlans = append(vlans, vlan)
		return nil
	}
	for _, raw := range items {
		item := strings.TrimSpace(raw)
		low, high, isRange := strings.Cut(item, "-")
		first, err := parseVLAN(strings.TrimSpace(low))
		if err != nil {
			return nil, fmt.Errorf("invalid item %q: %w", raw, err)
		}
		if !isRange {
			if err := add(first, raw); err != nil {
				return nil, err
			}
			continue
		}
		last, err := parseVLAN(strings.TrimSpace(high))
		if err != nil {
			return nil, fmt.Errorf("invalid item %q: %w", raw, err)
		}
		if last < first {
			return nil, fmt.Errorf("invalid range %q: end is below start", raw)
		}
		for vlan := int(first); vlan <= int(last); vlan++ {
			if err := add(uint16(vlan), raw); err != nil {
				return nil, err
			}
		}
	}
	return vlans, nil
}

func parseVLAN(value string) (uint16, error) {
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", value)
	}
	return checkVLAN(number)
}
