package gobgp

import (
	"context"
	"reflect"
	"testing"

	api "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xslonikx/snout/internal/event"
)

type deletePathClient struct {
	api.GoBgpServiceClient
	err error
}

func (c *deletePathClient) DeletePath(context.Context, *api.DeletePathRequest, ...grpc.CallOption) (*api.DeletePathResponse, error) {
	return nil, c.err
}

func TestBuildEVPNType2Path(t *testing.T) {
	client := &Client{paths: make(map[string][]byte)}
	path, err := client.buildPath(event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.10", RouteTarget: "65000:100"}, true)
	if err != nil {
		t.Fatal(err)
	}
	macAdvertisement := path.GetNlri().GetEvpnMacadv()
	if macAdvertisement == nil || macAdvertisement.GetEthernetTag() != 100 || macAdvertisement.GetMacAddress() != "02:00:00:00:00:01" {
		t.Fatalf("unexpected EVPN NLRI: %#v", macAdvertisement)
	}
	if len(path.GetPattrs()) != 3 || path.GetPattrs()[1].GetMpReach().GetNextHops()[0] != "192.0.2.10" || len(path.GetPattrs()[1].GetMpReach().GetNlris()) != 1 {
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

func TestBuildEVPNType3Path(t *testing.T) {
	client := &Client{paths: make(map[string][]byte)}
	path, err := client.buildIMETPath(event.IMETRoute{RD: "65000:100", VNI: 100, VTEP: "192.0.2.10", RouteTarget: "65000:100"}, true)
	if err != nil {
		t.Fatal(err)
	}
	imet := path.GetNlri().GetEvpnMulticast()
	if imet == nil || imet.GetEthernetTag() != 100 || imet.GetIpAddress() != "192.0.2.10" {
		t.Fatalf("unexpected Type-3 NLRI: %#v", imet)
	}
	pmsi := path.GetPattrs()[3].GetPmsiTunnel()
	if pmsi.GetType() != pmsiTunnelIngressReplication || pmsi.GetLabel() != 100 || !reflect.DeepEqual(pmsi.GetId(), []byte{192, 0, 2, 10}) {
		t.Fatalf("unexpected PMSI attribute: %#v", pmsi)
	}
	if len(path.GetPattrs()[1].GetMpReach().GetNlris()) != 1 || path.GetPattrs()[1].GetMpReach().GetNlris()[0].GetEvpnMulticast() == nil {
		t.Fatalf("MP_REACH does not contain the Type-3 NLRI: %#v", path.GetPattrs()[1].GetMpReach())
	}
}

func TestWithdrawTreatsMissingPathAsSuccess(t *testing.T) {
	route := event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.10"}
	client := &Client{
		client: &deletePathClient{err: status.Error(codes.NotFound, "missing")},
		paths:  map[string][]byte{publishedRouteKey(route): {1, 2, 3}},
	}
	if err := client.Withdraw(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if _, exists := client.paths[publishedRouteKey(route)]; exists {
		t.Fatal("missing path was not removed from local ownership state")
	}
}

func TestRouteTargetIsTakenPerRoute(t *testing.T) {
	client := &Client{paths: make(map[string][]byte)}
	for _, test := range []struct {
		vni       uint32
		rt        string
		wantASN   uint32
		wantLocal uint32
	}{
		{10100, "65000:10100", 65000, 10100},
		{10200, "65001:777", 65001, 777},
	} {
		path, err := client.buildPath(event.EVPNRoute{RD: "172.31.0.11:100", VNI: test.vni, MAC: "02:00:00:00:00:01", VTEP: "172.31.0.11", RouteTarget: test.rt}, true)
		if err != nil {
			t.Fatal(err)
		}
		rt := path.GetPattrs()[2].GetExtendedCommunities().GetCommunities()[0].GetTwoOctetAsSpecific()
		if rt.GetAsn() != test.wantASN || rt.GetLocalAdmin() != test.wantLocal {
			t.Fatalf("VNI %d: got RT %d:%d, want %s", test.vni, rt.GetAsn(), rt.GetLocalAdmin(), test.rt)
		}
		imet, err := client.buildIMETPath(event.IMETRoute{RD: "172.31.0.11:100", VNI: test.vni, VTEP: "172.31.0.11", RouteTarget: test.rt}, true)
		if err != nil {
			t.Fatal(err)
		}
		imetRT := imet.GetPattrs()[2].GetExtendedCommunities().GetCommunities()[0].GetTwoOctetAsSpecific()
		if imetRT.GetAsn() != test.wantASN || imetRT.GetLocalAdmin() != test.wantLocal {
			t.Fatalf("Type-3 VNI %d: got RT %d:%d, want %s", test.vni, imetRT.GetAsn(), imetRT.GetLocalAdmin(), test.rt)
		}
	}
}

func TestAnnouncementRequiresRouteTarget(t *testing.T) {
	client := &Client{paths: make(map[string][]byte)}
	if _, err := client.buildPath(event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.10"}, true); err == nil {
		t.Fatal("Type-2 announcement without route target was built")
	}
	if _, err := client.buildIMETPath(event.IMETRoute{RD: "65000:100", VNI: 100, VTEP: "192.0.2.10"}, true); err == nil {
		t.Fatal("Type-3 announcement without route target was built")
	}
	path, err := client.buildPath(event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.10"}, false)
	if err != nil {
		t.Fatal(err)
	}
	communities := path.GetPattrs()[2].GetExtendedCommunities().GetCommunities()
	if len(communities) != 1 || communities[0].GetEncap() == nil {
		t.Fatalf("withdrawal path must carry only the encapsulation community: %#v", communities)
	}
}

func TestFourOctetRouteTargetAndRD(t *testing.T) {
	rt, err := parseRouteTarget("4200000000:100")
	if err != nil {
		t.Fatal(err)
	}
	if rt.GetFourOctetAsSpecific().GetAsn() != 4200000000 || rt.GetFourOctetAsSpecific().GetLocalAdmin() != 100 {
		t.Fatalf("unexpected route target: %#v", rt)
	}
	rd, err := parseRD("4200000000:7")
	if err != nil {
		t.Fatal(err)
	}
	if rd.GetFourOctetAsn().GetAdmin() != 4200000000 || rd.GetFourOctetAsn().GetAssigned() != 7 {
		t.Fatalf("unexpected route distinguisher: %#v", rd)
	}
	rd, err = parseRD("65000:70000")
	if err != nil {
		t.Fatal(err)
	}
	if rd.GetTwoOctetAsn().GetAdmin() != 65000 || rd.GetTwoOctetAsn().GetAssigned() != 70000 {
		t.Fatalf("unexpected route distinguisher: %#v", rd)
	}
}
