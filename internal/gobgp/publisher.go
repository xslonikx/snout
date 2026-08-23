package gobgp

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	api "github.com/osrg/gobgp/v4/api"

	"github.com/xslonikx/snout/internal/event"
)

const (
	extendedCommunityRouteTarget = 0x02
	tunnelTypeVXLAN              = 8
	pmsiTunnelIngressReplication = 6
)

type PublisherConfig struct {
	RouteTarget string
}

func (c *Client) AnnounceIMET(ctx context.Context, route event.IMETRoute) error {
	path, err := c.buildIMETPath(route)
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
		path, err := c.buildIMETPath(route)
		if err != nil {
			return err
		}
		request.Path = path
	}
	if _, err := c.client.DeletePath(ctx, request); err != nil {
		return fmt.Errorf("delete EVPN Type-3 path: %w", err)
	}
	c.mu.Lock()
	delete(c.paths, key)
	c.mu.Unlock()
	return nil
}

func (c *Client) SetPublisherConfig(config PublisherConfig) error {
	if _, err := parseRouteTarget(config.RouteTarget); err != nil {
		return err
	}
	c.mu.Lock()
	c.publisherConfig = config
	c.mu.Unlock()
	return nil
}

func (c *Client) Announce(ctx context.Context, route event.EVPNRoute) error {
	path, err := c.buildPath(route)
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
		path, err := c.buildPath(route)
		if err != nil {
			return err
		}
		request.Path = path
	}
	if _, err := c.client.DeletePath(ctx, request); err != nil {
		return fmt.Errorf("delete EVPN path: %w", err)
	}

	c.mu.Lock()
	delete(c.paths, publishedRouteKey(route))
	c.mu.Unlock()
	return nil
}

func (c *Client) buildPath(route event.EVPNRoute) (*api.Path, error) {
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

	c.mu.Lock()
	routeTarget := c.publisherConfig.RouteTarget
	c.mu.Unlock()
	rt, err := parseRouteTarget(routeTarget)
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
			{Attr: &api.Attribute_ExtendedCommunities{ExtendedCommunities: &api.ExtendedCommunitiesAttribute{Communities: []*api.ExtendedCommunity{
				rt,
				{Extcom: &api.ExtendedCommunity_Encap{Encap: &api.EncapExtended{TunnelType: tunnelTypeVXLAN}}},
			}}}},
		},
	}, nil
}

func (c *Client) buildIMETPath(route event.IMETRoute) (*api.Path, error) {
	if route.VNI == 0 || net.ParseIP(route.VTEP) == nil {
		return nil, fmt.Errorf("IMET route must contain a valid VNI and VTEP")
	}
	rd, err := parseRD(route.RD)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	routeTarget := c.publisherConfig.RouteTarget
	c.mu.Unlock()
	rt, err := parseRouteTarget(routeTarget)
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
			{Attr: &api.Attribute_ExtendedCommunities{ExtendedCommunities: &api.ExtendedCommunitiesAttribute{Communities: []*api.ExtendedCommunity{
				rt,
				{Extcom: &api.ExtendedCommunity_Encap{Encap: &api.EncapExtended{TunnelType: tunnelTypeVXLAN}}},
			}}}},
			{Attr: &api.Attribute_PmsiTunnel{PmsiTunnel: &api.PmsiTunnelAttribute{Type: pmsiTunnelIngressReplication, Label: route.VNI, Id: id}}},
		},
	}, nil
}

func parseRD(value string) (*api.RouteDistinguisher, error) {
	admin, assigned, err := splitAdminAssigned("route distinguisher", value)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(admin); ip != nil && ip.To4() != nil {
		if assigned > 65535 {
			return nil, fmt.Errorf("IPv4 route distinguisher assigned value exceeds 65535")
		}
		return &api.RouteDistinguisher{Rd: &api.RouteDistinguisher_IpAddress{IpAddress: &api.RouteDistinguisherIPAddress{Admin: admin, Assigned: uint32(assigned)}}}, nil
	}
	asn, err := strconv.ParseUint(admin, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid route distinguisher administrator %q", admin)
	}
	if asn <= 65535 {
		return &api.RouteDistinguisher{Rd: &api.RouteDistinguisher_TwoOctetAsn{TwoOctetAsn: &api.RouteDistinguisherTwoOctetASN{Admin: uint32(asn), Assigned: uint32(assigned)}}}, nil
	}
	if assigned > 65535 {
		return nil, fmt.Errorf("four-octet ASN route distinguisher assigned value exceeds 65535")
	}
	return &api.RouteDistinguisher{Rd: &api.RouteDistinguisher_FourOctetAsn{FourOctetAsn: &api.RouteDistinguisherFourOctetASN{Admin: uint32(asn), Assigned: uint32(assigned)}}}, nil
}

func parseRouteTarget(value string) (*api.ExtendedCommunity, error) {
	admin, assigned, err := splitAdminAssigned("route target", value)
	if err != nil {
		return nil, err
	}
	asn, err := strconv.ParseUint(admin, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("route target must use ASN:assigned form: %w", err)
	}
	if asn <= 65535 {
		return &api.ExtendedCommunity{Extcom: &api.ExtendedCommunity_TwoOctetAsSpecific{TwoOctetAsSpecific: &api.TwoOctetAsSpecificExtended{IsTransitive: true, SubType: extendedCommunityRouteTarget, Asn: uint32(asn), LocalAdmin: uint32(assigned)}}}, nil
	}
	if assigned > 65535 {
		return nil, fmt.Errorf("four-octet ASN route target assigned value exceeds 65535")
	}
	return &api.ExtendedCommunity{Extcom: &api.ExtendedCommunity_FourOctetAsSpecific{FourOctetAsSpecific: &api.FourOctetAsSpecificExtended{IsTransitive: true, SubType: extendedCommunityRouteTarget, Asn: uint32(asn), LocalAdmin: uint32(assigned)}}}, nil
}

func splitAdminAssigned(name, value string) (string, uint64, error) {
	separator := strings.LastIndex(value, ":")
	if separator < 1 || separator == len(value)-1 {
		return "", 0, fmt.Errorf("%s must have administrator:assigned form", name)
	}
	assigned, err := strconv.ParseUint(value[separator+1:], 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("invalid %s assigned value: %w", name, err)
	}
	return value[:separator], assigned, nil
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
