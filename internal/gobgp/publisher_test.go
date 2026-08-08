package gobgp

import (
	"testing"

	"github.com/xslonikx/snout/internal/event"
)

func TestBuildEVPNType2Path(t *testing.T) {
	client := &Client{paths: make(map[string][]byte), publisherConfig: PublisherConfig{RouteTarget: "65000:100"}}
	path, err := client.buildPath(event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	macAdvertisement := path.GetNlri().GetEvpnMacadv()
	if macAdvertisement == nil || macAdvertisement.GetEthernetTag() != 100 || macAdvertisement.GetMacAddress() != "02:00:00:00:00:01" {
		t.Fatalf("unexpected EVPN NLRI: %#v", macAdvertisement)
	}
	if len(path.GetPattrs()) != 3 || path.GetPattrs()[1].GetMpReach().GetNextHops()[0] != "192.0.2.10" {
		t.Fatalf("unexpected path attributes: %#v", path.GetPattrs())
	}
	communities := path.GetPattrs()[2].GetExtendedCommunities().GetCommunities()
	if len(communities) != 2 || communities[0].GetTwoOctetAsSpecific().GetLocalAdmin() != 100 || communities[1].GetEncap().GetTunnelType() != tunnelTypeVXLAN {
		t.Fatalf("unexpected extended communities: %#v", communities)
	}
}

func TestParseIPv4RouteDistinguisher(t *testing.T) {
	rd, err := parseRD("192.0.2.10:100")
	if err != nil {
		t.Fatal(err)
	}
	if rd.GetIpAddress().GetAdmin() != "192.0.2.10" || rd.GetIpAddress().GetAssigned() != 100 {
		t.Fatalf("unexpected route distinguisher: %#v", rd)
	}
}
