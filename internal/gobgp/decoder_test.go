package gobgp

import (
	"testing"

	api "github.com/osrg/gobgp/v4/api"

	"github.com/xslonikx/snout/internal/event"
)

func TestDecodeIMET(t *testing.T) {
	path := &api.Path{
		Nlri: &api.NLRI{Nlri: &api.NLRI_EvpnMulticast{EvpnMulticast: &api.EVPNInclusiveMulticastEthernetTagRoute{
			Rd:          &api.RouteDistinguisher{Rd: &api.RouteDistinguisher_TwoOctetAsn{TwoOctetAsn: &api.RouteDistinguisherTwoOctetASN{Admin: 65000, Assigned: 100}}},
			EthernetTag: 100,
			IpAddress:   "192.0.2.20",
		}}},
		IsWithdraw: true,
	}
	imetEvent, err := DecodeIMET(path)
	if err != nil {
		t.Fatal(err)
	}
	want := event.IMETEvent{Action: event.RouteDelete, Route: event.IMETRoute{RD: "65000:100", VNI: 100, VTEP: "192.0.2.20"}}
	if *imetEvent != want {
		t.Fatalf("decoded event mismatch: got %#v, want %#v", *imetEvent, want)
	}
}
