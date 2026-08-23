package ovs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xslonikx/snout/internal/event"
)

type changingBridgeRunner struct {
	mu    sync.Mutex
	scans int
}

func (r *changingBridgeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	command := name + " " + strings.Join(args, " ")
	switch command {
	case "ovs-vsctl list-ifaces br0":
		r.scans++
		if r.scans == 3 || r.scans == 4 {
			return []byte("tap0\nvxlan0\n"), nil
		}
		return []byte("tap0\n"), nil
	case "ovs-vsctl get Interface tap0 type":
		return []byte("\"\"\n"), nil
	case "ovs-vsctl get Interface vxlan0 type":
		return []byte("\"vxlan\"\n"), nil
	case "ovs-vsctl --format=csv --data=bare --no-heading --columns=name,ofport find Interface type=vxlan":
		return []byte("vxlan0,2\n"), nil
	case "ovs-appctl fdb/show br0":
		return []byte("port VLAN MAC Age\n"), nil
	default:
		return nil, nil
	}
}

type noopAnnouncer struct {
	imetAnnouncements int
	imetWithdrawals   int
}

type recordingOVS struct {
	addedVTEPs   []event.IMETRoute
	deletedVTEPs []event.IMETRoute
}

func (*recordingOVS) IsVXLANBridge(context.Context) (bool, error)            { return true, nil }
func (*recordingOVS) EnableSplitHorizon(context.Context) error               { return nil }
func (*recordingOVS) ClearRemoteState(context.Context) error                 { return nil }
func (*recordingOVS) AddRemoteMAC(context.Context, event.EVPNRoute) error    { return nil }
func (*recordingOVS) DeleteRemoteMAC(context.Context, event.EVPNRoute) error { return nil }
func (o *recordingOVS) AddRemoteVTEP(_ context.Context, route event.IMETRoute) error {
	o.addedVTEPs = append(o.addedVTEPs, route)
	return nil
}
func (o *recordingOVS) DeleteRemoteVTEP(_ context.Context, route event.IMETRoute) error {
	o.deletedVTEPs = append(o.deletedVTEPs, route)
	return nil
}
func (*recordingOVS) LocalMACs(context.Context) ([]FDBEntry, error) { return nil, nil }

func (*noopAnnouncer) Announce(context.Context, event.EVPNRoute) error { return nil }
func (*noopAnnouncer) Withdraw(context.Context, event.EVPNRoute) error { return nil }
func (a *noopAnnouncer) AnnounceIMET(context.Context, event.IMETRoute) error {
	a.imetAnnouncements++
	return nil
}
func (a *noopAnnouncer) WithdrawIMET(context.Context, event.IMETRoute) error {
	a.imetWithdrawals++
	return nil
}

func TestManagerTracksBridgeTypeChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	announcer := &noopAnnouncer{}
	manager, err := NewManager(ctx, []string{"br0"}, &changingBridgeRunner{}, announcer, MonitorConfig{
		RD: "65000:100", VNI: 100, VTEP: "192.0.2.10", Interval: time.Hour,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	manager.scan(ctx)
	if len(manager.active) != 0 {
		t.Fatal("regular bridge must be ignored")
	}
	manager.scan(ctx)
	if len(manager.active) != 1 {
		t.Fatal("VXLAN bridge must be activated")
	}
	if announcer.imetAnnouncements != 1 {
		t.Fatalf("got %d IMET announcements, want 1", announcer.imetAnnouncements)
	}
	manager.scan(ctx)
	if len(manager.active) != 0 {
		t.Fatal("bridge must be deactivated after losing its VXLAN interface")
	}
	if announcer.imetWithdrawals != 1 {
		t.Fatalf("got %d IMET withdrawals, want 1", announcer.imetWithdrawals)
	}
	manager.stopAll()
}

func TestManagerAppliesRemoteIMETAndIgnoresLocalIMET(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(ctx, []string{"br0"}, &changingBridgeRunner{}, &noopAnnouncer{}, MonitorConfig{
		RD: "65000:100", VNI: 100, VTEP: "2001:db8::1", Interval: time.Hour,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingOVS{}
	manager.active["br0"] = managedBridge{ovs: client}
	remote := event.IMETRoute{RD: "65000:100", VNI: 100, VTEP: "192.0.2.20"}
	manager.Consume(event.IMETEvent{Action: event.RouteAdd, Route: remote})
	manager.Consume(event.IMETEvent{Action: event.RouteDelete, Route: remote})
	manager.Consume(event.IMETEvent{Action: event.RouteAdd, Route: event.IMETRoute{RD: "65000:100", VNI: 100, VTEP: "2001:0db8:0:0:0:0:0:1"}})
	if len(client.addedVTEPs) != 1 || client.addedVTEPs[0] != remote {
		t.Fatalf("unexpected added VTEPs: %#v", client.addedVTEPs)
	}
	if len(client.deletedVTEPs) != 1 || client.deletedVTEPs[0] != remote {
		t.Fatalf("unexpected deleted VTEPs: %#v", client.deletedVTEPs)
	}
}
