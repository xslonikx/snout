package gobgp

import (
	"context"
	"fmt"
	"net"
	"strings"

	api "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/evpn"
)

const (
	extendedCommunityRouteTarget = 0x02
	tunnelTypeVXLAN              = 8
	pmsiTunnelIngressReplication = 6
)

func (c *Client) AnnounceIMET(ctx context.Context, route event.IMETRoute) error {
	path, err := c.buildIMETPath(route, true)
	if err != nil {
		return err
	}
	response, err := c.client.AddPath(ctx, &api.AddPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Path: path})
	if err != nil {
		return fmt.Errorf("add EVPN Type-3 path: %w", err)
	}
	c.mu.Lock()
	c.paths[publishedIMETKey(route)] = append([]byte(nil), response.GetUuid()...)
	c.mu.Unlock()
	return nil
}

func (c *Client) WithdrawIMET(ctx context.Context, route event.IMETRoute) error {
	key := publishedIMETKey(route)
	c.mu.Lock()
	uuid := append([]byte(nil), c.paths[key]...)
	c.mu.Unlock()
	request := &api.DeletePathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: evpnFamily(), Uuid: uuid}
	if len(uuid) == 0 {
		path, err := c.buildIMETPath(route, false)
		if err != nil {
			return err
		}
		request.Path = path
	}
	if _, err := c.client.DeletePath(ctx, request); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete EVPN Type-3 path: %w", err)
	}
	c.mu.Lock()
	delete(c.paths, key)
	c.mu.Unlock()
	return nil
}

func (c *Client) Announce(ctx context.Context, route event.EVPNRoute) error {
	path, err := c.buildPath(route, true)
	if err != nil {
		return err
	}
	response, err := c.client.AddPath(ctx, &api.AddPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Path: path})
	if err != nil {
		return fmt.Errorf("add EVPN path: %w", err)
	}

	c.mu.Lock()
	c.paths[publishedRouteKey(route)] = append([]byte(nil), response.GetUuid()...)
	c.mu.Unlock()
	return nil
}

func (c *Client) Withdraw(ctx context.Context, route event.EVPNRoute) error {
	c.mu.Lock()
	uuid := append([]byte(nil), c.paths[publishedRouteKey(route)]...)
	c.mu.Unlock()

	request := &api.DeletePathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: evpnFamily(), Uuid: uuid}
	if len(uuid) == 0 {
		path, err := c.buildPath(route, false)
		if err != nil {
			return err
		}
		request.Path = path
	}
	if _, err := c.client.DeletePath(ctx, request); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete EVPN path: %w", err)
	}

	c.mu.Lock()
	delete(c.paths, publishedRouteKey(route))
	c.mu.Unlock()
	return nil
}

// buildPath builds a Type-2 path. Announcements require the route target of
// the route's VNI; withdrawals by NLRI may omit it.
func (c *Client) buildPath(route event.EVPNRoute, requireRouteTarget bool) (*api.Path, error) {
	if route.VNI == 0 || route.MAC == "" || net.ParseIP(route.VTEP) == nil {
		return nil, fmt.Errorf("route must contain a valid VNI, MAC, and VTEP")
	}
	if _, err := net.ParseMAC(route.MAC); err != nil {
		return nil, fmt.Errorf("invalid MAC address %q: %w", route.MAC, err)
	}
	rd, err := parseRD(route.RD)
	if err != nil {
		return nil, err
	}

	communities, err := extendedCommunities(route.RouteTarget, requireRouteTarget)
	if err != nil {
		return nil, err
	}

	nlri := &api.NLRI{Nlri: &api.NLRI_EvpnMacadv{EvpnMacadv: &api.EVPNMACIPAdvertisementRoute{
		Rd: rd, Esi: &api.EthernetSegmentIdentifier{}, EthernetTag: route.VNI,
		MacAddress: route.MAC, IpAddress: route.IP, Labels: []uint32{route.VNI},
	}}}
	return &api.Path{
		Family: evpnFamily(),
		Nlri:   nlri,
		Pattrs: []*api.Attribute{
			{Attr: &api.Attribute_Origin{Origin: &api.OriginAttribute{Origin: 0}}},
			{Attr: &api.Attribute_MpReach{MpReach: &api.MpReachNLRIAttribute{Family: evpnFamily(), NextHops: []string{route.VTEP}, Nlris: []*api.NLRI{nlri}}}},
			{Attr: &api.Attribute_ExtendedCommunities{ExtendedCommunities: &api.ExtendedCommunitiesAttribute{Communities: communities}}},
		},
	}, nil
}

// buildIMETPath builds a Type-3 path; see buildPath for requireRouteTarget.
func (c *Client) buildIMETPath(route event.IMETRoute, requireRouteTarget bool) (*api.Path, error) {
	if route.VNI == 0 || net.ParseIP(route.VTEP) == nil {
		return nil, fmt.Errorf("IMET route must contain a valid VNI and VTEP")
	}
	rd, err := parseRD(route.RD)
	if err != nil {
		return nil, err
	}
	communities, err := extendedCommunities(route.RouteTarget, requireRouteTarget)
	if err != nil {
		return nil, err
	}
	vtep := net.ParseIP(route.VTEP)
	id := vtep.To4()
	if id == nil {
		id = vtep.To16()
	}
	nlri := &api.NLRI{Nlri: &api.NLRI_EvpnMulticast{EvpnMulticast: &api.EVPNInclusiveMulticastEthernetTagRoute{
		Rd: rd, EthernetTag: route.VNI, IpAddress: route.VTEP,
	}}}
	return &api.Path{
		Family: evpnFamily(),
		Nlri:   nlri,
		Pattrs: []*api.Attribute{
			{Attr: &api.Attribute_Origin{Origin: &api.OriginAttribute{Origin: 0}}},
			{Attr: &api.Attribute_MpReach{MpReach: &api.MpReachNLRIAttribute{Family: evpnFamily(), NextHops: []string{route.VTEP}, Nlris: []*api.NLRI{nlri}}}},
			{Attr: &api.Attribute_ExtendedCommunities{ExtendedCommunities: &api.ExtendedCommunitiesAttribute{Communities: communities}}},
			{Attr: &api.Attribute_PmsiTunnel{PmsiTunnel: &api.PmsiTunnelAttribute{Type: pmsiTunnelIngressReplication, Label: route.VNI, Id: id}}},
		},
	}, nil
}

func extendedCommunities(routeTarget string, required bool) ([]*api.ExtendedCommunity, error) {
	encap := &api.ExtendedCommunity{Extcom: &api.ExtendedCommunity_Encap{Encap: &api.EncapExtended{TunnelType: tunnelTypeVXLAN}}}
	if routeTarget == "" && !required {
		return []*api.ExtendedCommunity{encap}, nil
	}
	rt, err := parseRouteTarget(routeTarget)
	if err != nil {
		return nil, err
	}
	return []*api.ExtendedCommunity{rt, encap}, nil
}

func parseRD(value string) (*api.RouteDistinguisher, error) {
	rd, err := evpn.ParseRD(value)
	if err != nil {
		return nil, err
	}
	switch rd.Type {
	case evpn.RDIPv4:
		return &api.RouteDistinguisher{Rd: &api.RouteDistinguisher_IpAddress{IpAddress: &api.RouteDistinguisherIPAddress{Admin: rd.IP, Assigned: rd.Assigned}}}, nil
	case evpn.RDFourOctetASN:
		return &api.RouteDistinguisher{Rd: &api.RouteDistinguisher_FourOctetAsn{FourOctetAsn: &api.RouteDistinguisherFourOctetASN{Admin: rd.ASN, Assigned: rd.Assigned}}}, nil
	default:
		return &api.RouteDistinguisher{Rd: &api.RouteDistinguisher_TwoOctetAsn{TwoOctetAsn: &api.RouteDistinguisherTwoOctetASN{Admin: rd.ASN, Assigned: rd.Assigned}}}, nil
	}
}

func parseRouteTarget(value string) (*api.ExtendedCommunity, error) {
	rt, err := evpn.ParseRouteTarget(value)
	if err != nil {
		return nil, err
	}
	if rt.FourOctet {
		return &api.ExtendedCommunity{Extcom: &api.ExtendedCommunity_FourOctetAsSpecific{FourOctetAsSpecific: &api.FourOctetAsSpecificExtended{IsTransitive: true, SubType: extendedCommunityRouteTarget, Asn: rt.ASN, LocalAdmin: rt.Assigned}}}, nil
	}
	return &api.ExtendedCommunity{Extcom: &api.ExtendedCommunity_TwoOctetAsSpecific{TwoOctetAsSpecific: &api.TwoOctetAsSpecificExtended{IsTransitive: true, SubType: extendedCommunityRouteTarget, Asn: rt.ASN, LocalAdmin: rt.Assigned}}}, nil
}

func evpnFamily() *api.Family {
	return &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}
}

func publishedRouteKey(route event.EVPNRoute) string {
	return fmt.Sprintf("type2/%s/%d/%s/%s", route.RD, route.VNI, strings.ToLower(route.MAC), route.IP)
}

func publishedIMETKey(route event.IMETRoute) string {
	return fmt.Sprintf("type3/%s/%d/%s", route.RD, route.VNI, route.VTEP)
}
