package evpn

import (
	"reflect"
	"strings"
	"testing"
)

func i64(value int64) *int64 { return &value }

func resolveOK(t *testing.T, spec Spec) Topology {
	t.Helper()
	topology, err := Resolve(spec)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return topology
}

func resolveErr(t *testing.T, spec Spec, fragment string) {
	t.Helper()
	_, err := Resolve(spec)
	if err == nil {
		t.Fatalf("Resolve succeeded, want error containing %q", fragment)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error %q does not contain %q", err, fragment)
	}
}

func TestResolveOffsetOnly(t *testing.T) {
	topology := resolveOK(t, Spec{
		VTEP: "172.31.0.11", ASN: i64(65000),
		Bridges: []BridgeSpec{{Name: "br0", VNIOffset: i64(10000), VLANs: []string{"100", "200", "300-302"}}},
	})
	want := []Segment{
		{VLAN: 100, VNI: 10100, RD: "172.31.0.11:100", RouteTarget: "65000:10100"},
		{VLAN: 200, VNI: 10200, RD: "172.31.0.11:200", RouteTarget: "65000:10200"},
		{VLAN: 300, VNI: 10300, RD: "172.31.0.11:300", RouteTarget: "65000:10300"},
		{VLAN: 301, VNI: 10301, RD: "172.31.0.11:301", RouteTarget: "65000:10301"},
		{VLAN: 302, VNI: 10302, RD: "172.31.0.11:302", RouteTarget: "65000:10302"},
	}
	if len(topology.Bridges) != 1 || topology.Bridges[0].Flat {
		t.Fatalf("unexpected bridges: %#v", topology.Bridges)
	}
	if !reflect.DeepEqual(topology.Bridges[0].Segments, want) {
		t.Fatalf("segments mismatch\ngot:  %#v\nwant: %#v", topology.Bridges[0].Segments, want)
	}
}

func TestResolveExplicitEntryWinsOverOffset(t *testing.T) {
	topology := resolveOK(t, Spec{
		VTEP: "172.31.0.11", ASN: i64(65000),
		Bridges: []BridgeSpec{{
			Name: "br0", VNIOffset: i64(10000), VLANs: []string{"100", "200"},
			VNIs: []VNISpec{
				{VLAN: i64(200), VNI: i64(5000), RouteTarget: "65000:777"},
				{VLAN: i64(400), VNI: i64(9400), RD: "65000:9400"},
			},
		}},
	})
	bridge := topology.Bridges[0]
	want := map[uint16]Segment{
		100: {VLAN: 100, VNI: 10100, RD: "172.31.0.11:100", RouteTarget: "65000:10100"},
		200: {VLAN: 200, VNI: 5000, RD: "172.31.0.11:200", RouteTarget: "65000:777"},
		400: {VLAN: 400, VNI: 9400, RD: "65000:9400", RouteTarget: "65000:9400"},
	}
	if len(bridge.Segments) != len(want) {
		t.Fatalf("got %d segments, want %d", len(bridge.Segments), len(want))
	}
	for vlan, expected := range want {
		got, ok := bridge.SegmentByVLAN(vlan)
		if !ok || got != expected {
			t.Fatalf("VLAN %d: got %#v (%v), want %#v", vlan, got, ok, expected)
		}
		byVNI, ok := bridge.SegmentByVNI(expected.VNI)
		if !ok || byVNI != expected {
			t.Fatalf("VNI %d lookup: got %#v (%v)", expected.VNI, byVNI, ok)
		}
	}
	if _, ok := bridge.SegmentByVNI(10200); ok {
		t.Fatal("offset VNI of an overridden VLAN must not be active")
	}
	if _, ok := bridge.SegmentByVLAN(300); ok {
		t.Fatal("unlisted VLAN must not be active")
	}
}

func TestResolveExplicitEntriesWithoutOffset(t *testing.T) {
	topology := resolveOK(t, Spec{
		VTEP: "172.31.0.11", ASN: i64(65000),
		Bridges: []BridgeSpec{{Name: "br0", VNIs: []VNISpec{{VLAN: i64(100), VNI: i64(777)}}}},
	})
	segment, ok := topology.Bridges[0].SegmentByVLAN(100)
	if !ok || segment.VNI != 777 {
		t.Fatalf("unexpected segment %#v", segment)
	}
}

func TestResolveFlatBridge(t *testing.T) {
	topology := resolveOK(t, Spec{
		VTEP: "172.31.0.11", ASN: i64(65000),
		Bridges: []BridgeSpec{{Name: "br-flat", VNI: i64(100)}},
	})
	bridge := topology.Bridges[0]
	if !bridge.Flat {
		t.Fatal("bridge with vni must be flat")
	}
	want := Segment{VLAN: 0, VNI: 100, RD: "172.31.0.11:100", RouteTarget: "65000:100"}
	if got, ok := bridge.SegmentByVLAN(0); !ok || got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestResolveFlatAndVLANBridgesTogether(t *testing.T) {
	topology := resolveOK(t, Spec{
		VTEP: "172.31.0.11", ASN: i64(65000),
		Bridges: []BridgeSpec{
			{Name: "br-flat", VNI: i64(50)},
			{Name: "br0", VNIOffset: i64(10000), VLANs: []string{"100"}},
		},
	})
	bridge, segment, ok := topology.BridgeForVNI(10100)
	if !ok || bridge.Name != "br0" || segment.VLAN != 100 {
		t.Fatalf("lookup of VNI 10100 failed: %q %#v %v", bridge.Name, segment, ok)
	}
	bridge, segment, ok = topology.BridgeForVNI(50)
	if !ok || bridge.Name != "br-flat" || segment.VLAN != 0 {
		t.Fatalf("lookup of VNI 50 failed: %q %#v %v", bridge.Name, segment, ok)
	}
	if _, _, ok := topology.BridgeForVNI(9999); ok {
		t.Fatal("unknown VNI was found")
	}
}

func TestResolveFlatRequiresExplicitRDForLargeVNI(t *testing.T) {
	resolveErr(t, Spec{VTEP: "172.31.0.11", ASN: i64(65000), Bridges: []BridgeSpec{{Name: "br-flat", VNI: i64(70000)}}}, "explicit rd")
	resolveOK(t, Spec{VTEP: "172.31.0.11", ASN: i64(65000), Bridges: []BridgeSpec{{Name: "br-flat", VNI: i64(70000), RD: "172.31.0.11:7"}}})
}

func TestResolveRouteTargetRules(t *testing.T) {
	resolveErr(t, Spec{VTEP: "172.31.0.11", Bridges: []BridgeSpec{{Name: "br0", VNIOffset: i64(0), VLANs: []string{"100"}}}}, "asn is not set")
	resolveErr(t, Spec{VTEP: "172.31.0.11", ASN: i64(4200000000), Bridges: []BridgeSpec{{Name: "br0", VNIOffset: i64(0), VLANs: []string{"100"}}}}, "above 65535")
	topology := resolveOK(t, Spec{VTEP: "172.31.0.11", ASN: i64(4200000000), Bridges: []BridgeSpec{{
		Name: "br0", VNIs: []VNISpec{{VLAN: i64(100), VNI: i64(100), RouteTarget: "4200000000:100"}},
	}}})
	if got := topology.Bridges[0].Segments[0].RouteTarget; got != "4200000000:100" {
		t.Fatalf("route target %q", got)
	}
	// Explicit route targets everywhere make asn optional.
	resolveOK(t, Spec{VTEP: "172.31.0.11", Bridges: []BridgeSpec{{Name: "br-flat", VNI: i64(5), RouteTarget: "65000:5"}}})
}

func TestResolveRDRequiresIPv4VTEPForDefaults(t *testing.T) {
	resolveErr(t, Spec{VTEP: "2001:db8::1", ASN: i64(65000), Bridges: []BridgeSpec{{Name: "br0", VNIOffset: i64(0), VLANs: []string{"100"}}}}, "not IPv4")
	resolveOK(t, Spec{VTEP: "2001:db8::1", ASN: i64(65000), Bridges: []BridgeSpec{{
		Name: "br0", VNIs: []VNISpec{{VLAN: i64(100), VNI: i64(100), RD: "65000:100"}},
	}}})
}

func TestResolveRejectsInvalidSpecs(t *testing.T) {
	base := func(bridges ...BridgeSpec) Spec { return Spec{VTEP: "172.31.0.11", ASN: i64(65000), Bridges: bridges} }
	tests := []struct {
		name     string
		spec     Spec
		fragment string
	}{
		{"missing vtep", Spec{ASN: i64(65000), Bridges: []BridgeSpec{{Name: "br0", VNI: i64(1)}}}, "vtep"},
		{"bad asn", Spec{VTEP: "172.31.0.11", ASN: i64(0), Bridges: []BridgeSpec{{Name: "br0", VNI: i64(1)}}}, "asn"},
		{"no bridges", Spec{VTEP: "172.31.0.11", ASN: i64(65000)}, "at least one"},
		{"empty name", base(BridgeSpec{VNI: i64(1)}), "name is required"},
		{"duplicate bridge", base(BridgeSpec{Name: "br0", VNI: i64(1)}, BridgeSpec{Name: "br0", VNI: i64(2)}), "more than once"},
		{"no mode", base(BridgeSpec{Name: "br0"}), "set vni"},
		{"flat with offset", base(BridgeSpec{Name: "br0", VNI: i64(1), VNIOffset: i64(0)}), "cannot be combined"},
		{"flat with vlans", base(BridgeSpec{Name: "br0", VNI: i64(1), VLANs: []string{"1"}}), "cannot be combined"},
		{"flat with entries", base(BridgeSpec{Name: "br0", VNI: i64(1), VNIs: []VNISpec{{VLAN: i64(1), VNI: i64(2)}}}), "cannot be combined"},
		{"flat vni zero", base(BridgeSpec{Name: "br0", VNI: i64(0)}), "vni"},
		{"flat vni too large", base(BridgeSpec{Name: "br0", VNI: i64(MaxVNI + 1), RD: "65000:1"}), "vni"},
		{"vlan rd at bridge level", base(BridgeSpec{Name: "br0", VNIOffset: i64(0), VLANs: []string{"1"}, RD: "65000:1"}), "only on a flat bridge"},
		{"vlans without offset", base(BridgeSpec{Name: "br0", VLANs: []string{"100"}}), "requires vni_offset"},
		{"negative offset", base(BridgeSpec{Name: "br0", VNIOffset: i64(-1), VLANs: []string{"100"}}), "vni_offset"},
		{"vlan zero", base(BridgeSpec{Name: "br0", VNIOffset: i64(0), VLANs: []string{"0"}}), "1-4094"},
		{"vlan too large", base(BridgeSpec{Name: "br0", VNIOffset: i64(0), VLANs: []string{"4095"}}), "1-4094"},
		{"vlan not a number", base(BridgeSpec{Name: "br0", VNIOffset: i64(0), VLANs: []string{"abc"}}), "not a number"},
		{"reversed range", base(BridgeSpec{Name: "br0", VNIOffset: i64(0), VLANs: []string{"310-300"}}), "end is below start"},
		{"duplicate vlan", base(BridgeSpec{Name: "br0", VNIOffset: i64(0), VLANs: []string{"100", "90-110"}}), "more than once"},
		{"offset overflow", base(BridgeSpec{Name: "br0", VNIOffset: i64(MaxVNI - 10), VLANs: []string{"100"}}), "exceeds"},
		{"entry without vlan", base(BridgeSpec{Name: "br0", VNIs: []VNISpec{{VNI: i64(1)}}}), "vlan is required"},
		{"entry without vni", base(BridgeSpec{Name: "br0", VNIs: []VNISpec{{VLAN: i64(1)}}}), "vni is required"},
		{"duplicate entry", base(BridgeSpec{Name: "br0", VNIs: []VNISpec{{VLAN: i64(1), VNI: i64(1)}, {VLAN: i64(1), VNI: i64(2)}}}), "more than one"},
		{"bad entry rd", base(BridgeSpec{Name: "br0", VNIs: []VNISpec{{VLAN: i64(1), VNI: i64(1), RD: "nope"}}}), "rd"},
		{"bad entry rt", base(BridgeSpec{Name: "br0", VNIs: []VNISpec{{VLAN: i64(1), VNI: i64(1), RouteTarget: "1.2.3.4:5"}}}), "route_target"},
		{"calculated vs explicit vni collision", base(BridgeSpec{Name: "br0", VNIOffset: i64(10000), VLANs: []string{"100"}, VNIs: []VNISpec{{VLAN: i64(200), VNI: i64(10100)}}}), "duplicate VNI"},
		{"vni collision across bridges", base(BridgeSpec{Name: "br0", VNIOffset: i64(0), VLANs: []string{"100"}}, BridgeSpec{Name: "br1", VNI: i64(100), RD: "65000:1"}), "VNI 100 is used by both"},
		{"rd collision same vlan on two bridges", base(BridgeSpec{Name: "br0", VNIOffset: i64(0), VLANs: []string{"100"}}, BridgeSpec{Name: "br1", VNIOffset: i64(1000), VLANs: []string{"100"}}), "set an explicit rd"},
		{"rd collision flat vni equals vlan", base(BridgeSpec{Name: "br0", VNIOffset: i64(1000), VLANs: []string{"100"}}, BridgeSpec{Name: "br-flat", VNI: i64(100)}), "set an explicit rd"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolveErr(t, test.spec, test.fragment)
		})
	}
}

func TestNewBridgeValidatesSegments(t *testing.T) {
	ok := Segment{VLAN: 100, VNI: 100, RD: "65000:100", RouteTarget: "65000:100"}
	if _, err := NewBridge("br0", false, []Segment{ok}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		flat     bool
		segments []Segment
	}{
		"empty":            {false, nil},
		"flat with vlan":   {true, []Segment{ok}},
		"flat with two":    {true, []Segment{{VNI: 1, RD: "65000:1", RouteTarget: "65000:1"}, {VNI: 2, RD: "65000:2", RouteTarget: "65000:2"}}},
		"vlan zero":        {false, []Segment{{VLAN: 0, VNI: 1, RD: "65000:1", RouteTarget: "65000:1"}}},
		"bad rd":           {false, []Segment{{VLAN: 1, VNI: 1, RD: "x", RouteTarget: "65000:1"}}},
		"duplicate vni":    {false, []Segment{ok, {VLAN: 101, VNI: 100, RD: "65000:101", RouteTarget: "65000:100"}}},
		"duplicate vlan":   {false, []Segment{ok, {VLAN: 100, VNI: 101, RD: "65000:101", RouteTarget: "65000:101"}}},
		"vni out of range": {false, []Segment{{VLAN: 1, VNI: MaxVNI + 1, RD: "65000:1", RouteTarget: "65000:1"}}},
	}
	for name, test := range cases {
		if _, err := NewBridge("br0", test.flat, test.segments); err == nil {
			t.Fatalf("%s: NewBridge succeeded", name)
		}
	}
	if _, err := NewBridge("", false, []Segment{ok}); err == nil {
		t.Fatal("empty bridge name was accepted")
	}
}
