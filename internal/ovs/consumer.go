package ovs

import (
	"context"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/logging"
)

// Consumer applies remote EVPN route events to the OVS dataplane.
type Consumer struct {
	ctx       context.Context
	ovs       OpenVSwitch
	localVTEP string
	localVNI  uint32
}

func NewConsumer(ctx context.Context, ovs OpenVSwitch, localVTEP string, localVNI uint32) Consumer {
	return Consumer{ctx: ctx, ovs: ovs, localVTEP: localVTEP, localVNI: localVNI}
}

func (c Consumer) Consume(value any) {
	routeEvent, ok := value.(event.RouteEvent)
	if !ok || routeEvent.Route.VTEP == c.localVTEP || routeEvent.Route.VNI != c.localVNI {
		return
	}

	var err error
	switch routeEvent.Action {
	case event.RouteAdd:
		err = c.ovs.AddRemoteMAC(c.ctx, routeEvent.Route)
	case event.RouteDelete:
		err = c.ovs.DeleteRemoteMAC(c.ctx, routeEvent.Route)
	}
	if err != nil {
		logging.Errorf("apply EVPN route to OVS: %v", err)
	}
}
