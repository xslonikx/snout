package evpn

import "testing"

func TestParseRD(t *testing.T) {
	tests := []struct {
		value string
		want  RouteDistinguisher
	}{
		{"65000:100", RouteDistinguisher{Type: RDTwoOctetASN, ASN: 65000, Assigned: 100}},
		{"65000:4294967295", RouteDistinguisher{Type: RDTwoOctetASN, ASN: 65000, Assigned: 4294967295}},
		{"172.31.0.11:100", RouteDistinguisher{Type: RDIPv4, IP: "172.31.0.11", Assigned: 100}},
		{"4200000000:7", RouteDistinguisher{Type: RDFourOctetASN, ASN: 4200000000, Assigned: 7}},
	}
	for _, test := range tests {
		got, err := ParseRD(test.value)
		if err != nil {
			t.Fatalf("ParseRD(%q): %v", test.value, err)
		}
		if got != test.want {
			t.Fatalf("ParseRD(%q) = %#v, want %#v", test.value, got, test.want)
		}
		if got.String() != test.value {
			t.Fatalf("String() = %q, want %q", got.String(), test.value)
		}
	}
}

func TestParseRDRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "65000", ":1", "65000:", "172.31.0.11:65536", "4200000000:65536", "2001:db8::1:1", "abc:1", "65000:x", "65000:-1"} {
		if _, err := ParseRD(value); err == nil {
			t.Fatalf("ParseRD(%q) succeeded", value)
		}
	}
}

func TestParseRouteTarget(t *testing.T) {
	tests := []struct {
		value string
		want  RouteTarget
	}{
		{"65000:10100", RouteTarget{ASN: 65000, Assigned: 10100}},
		{"65000:16777215", RouteTarget{ASN: 65000, Assigned: 16777215}},
		{"4200000000:100", RouteTarget{FourOctet: true, ASN: 4200000000, Assigned: 100}},
	}
	for _, test := range tests {
		got, err := ParseRouteTarget(test.value)
		if err != nil {
			t.Fatalf("ParseRouteTarget(%q): %v", test.value, err)
		}
		if got != test.want {
			t.Fatalf("ParseRouteTarget(%q) = %#v, want %#v", test.value, got, test.want)
		}
		if got.String() != test.value {
			t.Fatalf("String() = %q, want %q", got.String(), test.value)
		}
	}
}

func TestParseRouteTargetRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "65000", "172.31.0.11:1", "4200000000:65536", "65000:x", ":5"} {
		if _, err := ParseRouteTarget(value); err == nil {
			t.Fatalf("ParseRouteTarget(%q) succeeded", value)
		}
	}
}

func TestRDStringIsCanonical(t *testing.T) {
	rd, err := ParseRD("065000:0100")
	if err != nil {
		t.Fatal(err)
	}
	if rd.String() != "65000:100" {
		t.Fatalf("non-canonical RD %q", rd.String())
	}
}
