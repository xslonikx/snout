package ovs

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/evpn"
	"github.com/xslonikx/snout/internal/logging"
)

type Announcer interface {
	Announce(context.Context, event.EVPNRoute) error
	Withdraw(context.Context, event.EVPNRoute) error
	AnnounceIMET(context.Context, event.IMETRoute) error
	WithdrawIMET(context.Context, event.IMETRoute) error
}

// MonitorConfig holds the settings shared by all bridges.
type MonitorConfig struct {
	VTEP     string
	Interval time.Duration
}

// localRoute builds the Type-2 route for a local FDB entry. ok is false when
// the entry's VLAN is not an active segment of the bridge.
func localRoute(bridge evpn.Bridge, vtep string, entry FDBEntry) (event.EVPNRoute, bool) {
	segment, ok := bridge.SegmentByVLAN(entry.VLAN)
	if !ok {
		return event.EVPNRoute{}, false
	}
	return event.EVPNRoute{RD: segment.RD, VNI: segment.VNI, MAC: entry.MAC, VTEP: vtep, RouteTarget: segment.RouteTarget}, true
}

// localIMETRoute builds the Type-3 route of one local segment.
func localIMETRoute(segment evpn.Segment, vtep string) event.IMETRoute {
	return event.IMETRoute{RD: segment.RD, VNI: segment.VNI, VTEP: vtep, RouteTarget: segment.RouteTarget}
}

// MonitorLocalMACs polls the bridge FDB and announces or withdraws one Type-2
// route per (VNI, MAC). Entries on VLANs that are not active on the bridge
// are ignored.
func MonitorLocalMACs(ctx context.Context, ovs OpenVSwitch, announcer Announcer, bridge evpn.Bridge, config MonitorConfig) error {
	if ovs == nil || announcer == nil {
		return fmt.Errorf("OVS and announcer are required")
	}
	if net.ParseIP(config.VTEP) == nil {
		return fmt.Errorf("a valid VTEP is required")
	}
	if len(bridge.Segments) == 0 {
		return fmt.Errorf("bridge %q has no active VNI", bridge.Name)
	}
	if config.Interval <= 0 {
		return fmt.Errorf("monitor interval must be positive")
	}

	known := make(map[string]event.EVPNRoute)
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()

	for {
		entries, err := ovs.LocalMACs(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logging.Errorf("read local OVS FDB on bridge %s: %v", bridge.Name, err)
		} else {
			current := make(map[string]event.EVPNRoute, len(entries))
			for _, entry := range entries {
				route, ok := localRoute(bridge, config.VTEP, entry)
				if !ok {
					continue
				}
				key := routeKey(route)
				current[key] = route
				if _, exists := known[key]; !exists {
					if err := announcer.Announce(ctx, route); err != nil {
						logging.Errorf("announce local MAC %s in VNI %d: %v", route.MAC, route.VNI, err)
						delete(current, key)
					}
				}
			}
			for key, route := range known {
				if _, exists := current[key]; exists {
					continue
				}
				if err := announcer.Withdraw(ctx, route); err != nil {
					logging.Errorf("withdraw local MAC %s in VNI %d: %v", route.MAC, route.VNI, err)
					current[key] = route
				}
			}
			known = current
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
