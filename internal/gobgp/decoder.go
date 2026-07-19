package gobgp

import (
	"errors"
	"fmt"

	api "github.com/osrg/gobgp/v4/api"

	"github.com/xslonikx/snout/internal/event"
)

// DecodeRoute converts a GoBGP Path into an internal RouteEvent.
func DecodeRoute(path *api.Path) (*event.RouteEvent, error) {
	if path == nil {
		return nil, errors.New("nil path")
	}

	route, err := decodeEVPNNLRI(path.GetNlri())
	if err != nil {
		return nil, err
	}

	vtep, err := decodeVTEP(path.GetPattrs())
	if err != nil {
		return nil, err
	}

	route.VTEP = vtep

	action := event.RouteAdd
	if path.GetIsWithdraw() {
		action = event.RouteDelete
	}

	return &event.RouteEvent{Action: action, Route: route}, nil
}

func decodeEVPNNLRI(nlri *api.NLRI) (event.EVPNRoute, error) {
	if nlri == nil {
		return event.EVPNRoute{}, errors.New("path has no NLRI")
	}

	macAdvertisement := nlri.GetEvpnMacadv()
	if macAdvertisement == nil {
		return event.EVPNRoute{}, errors.New("unsupported NLRI: expected EVPN MAC/IP advertisement route")
	}

	mac := macAdvertisement.GetMacAddress()
	if mac == "" {
		return event.EVPNRoute{}, errors.New("EVPN MAC/IP advertisement route has no MAC address")
	}

	rd, err := formatRouteDistinguisher(macAdvertisement.GetRd())
	if err != nil {
		return event.EVPNRoute{}, fmt.Errorf("decode route distinguisher: %w", err)
	}

	return event.EVPNRoute{
		RD:  rd,
		VNI: macAdvertisement.GetEthernetTag(),
		MAC: mac,
		IP:  macAdvertisement.GetIpAddress(),
	}, nil
}

// decodeVTEP deliberately uses MP_REACH next hops: NeighborIp may identify a
// route reflector rather than the originating VTEP.
func decodeVTEP(attributes []*api.Attribute) (string, error) {
	for _, attribute := range attributes {
		if attribute == nil {
			continue
		}

		mpReach := attribute.GetMpReach()
		if mpReach == nil {
			continue
		}

		nextHops := mpReach.GetNextHops()
		if len(nextHops) == 0 {
			return "", errors.New("MP_REACH attribute has no next-hop")
		}
		if nextHops[0] == "" {
			return "", errors.New("MP_REACH attribute contains an empty next-hop")
		}

		return nextHops[0], nil
	}

	return "", errors.New("MP_REACH attribute not found")
}

func formatRouteDistinguisher(rd *api.RouteDistinguisher) (string, error) {
	if rd == nil {
		return "", errors.New("route distinguisher is missing")
	}

	if value := rd.GetTwoOctetAsn(); value != nil {
		return fmt.Sprintf("%d:%d", value.GetAdmin(), value.GetAssigned()), nil
	}
	if value := rd.GetIpAddress(); value != nil {
		if value.GetAdmin() == "" {
			return "", errors.New("route distinguisher has an empty IP administrator")
		}
		return fmt.Sprintf("%s:%d", value.GetAdmin(), value.GetAssigned()), nil
	}
	if value := rd.GetFourOctetAsn(); value != nil {
		return fmt.Sprintf("%d:%d", value.GetAdmin(), value.GetAssigned()), nil
	}

	return "", errors.New("unsupported route distinguisher type")
}

// DecodePeer ignores synchronization markers and converts state changes into
// the application's peer_up and peer_down events.
func DecodePeer(peerEvent *api.WatchEventResponse_PeerEvent) (*event.PeerEvent, error) {
	if peerEvent == nil {
		return nil, errors.New("nil peer event")
	}
	if peerEvent.GetType() != api.WatchEventResponse_PeerEvent_TYPE_STATE {
		return nil, nil
	}

	peer := peerEvent.GetPeer()
	if peer == nil {
		return nil, errors.New("peer state event contains no peer")
	}
	state := peer.GetState()
	if state == nil {
		return nil, errors.New("peer state event contains no state")
	}
	peerAddress := state.GetNeighborAddress()
	if peerAddress == "" {
		return nil, errors.New("peer state event contains no neighbor address")
	}

	eventType := event.PeerDown
	if state.GetSessionState() == api.PeerState_SESSION_STATE_ESTABLISHED {
		eventType = event.PeerUp
	}

	return &event.PeerEvent{Event: eventType, Peer: peerAddress}, nil
}
