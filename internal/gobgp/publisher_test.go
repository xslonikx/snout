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
	client := &Client{paths: make(map[string][]byte), publisherConfig: PublisherConfig{RouteTarget: "65000:100"}}
	path, err := client.buildPath(event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.10"})
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
	client := &Client{paths: make(map[string][]byte), publisherConfig: PublisherConfig{RouteTarget: "65000:100"}}
	path, err := client.buildIMETPath(event.IMETRoute{RD: "65000:100", VNI: 100, VTEP: "192.0.2.10"})
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
		client:          &deletePathClient{err: status.Error(codes.NotFound, "missing")},
		paths:           map[string][]byte{publishedRouteKey(route): {1, 2, 3}},
		publisherConfig: PublisherConfig{RouteTarget: "65000:100"},
	}
	if err := client.Withdraw(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if _, exists := client.paths[publishedRouteKey(route)]; exists {
		t.Fatal("missing path was not removed from local ownership state")
	}
}
