package ovs

import (
	"context"
	"errors"
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
	case "ovs-vsctl --if-exists get Bridge br0 name":
		r.scans++
		if r.scans < 3 {
			return []byte("\"br0\"\n"), nil
		}
		return nil, nil
	case "ovs-vsctl --format=csv --data=bare --no-heading --columns=name,ofport find Interface type=vxlan":
		return nil, nil
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
	clears       int
}

func (*recordingOVS) BridgeExists(context.Context) (bool, error)             { return true, nil }
func (o *recordingOVS) ClearRemoteState(context.Context) error               { o.clears++; return nil }
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

func TestManagerActivatesExistingBridgeWithoutVXLAN(t *testing.T) {
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
	if len(manager.active) != 1 {
		t.Fatal("existing bridge must activate without a VXLAN interface")
	}
	manager.scan(ctx)
	if announcer.imetAnnouncements != 1 {
		t.Fatalf("got %d IMET announcements, want 1", announcer.imetAnnouncements)
	}
	manager.scan(ctx)
	if len(manager.active) != 0 {
		t.Fatal("bridge must be deactivated after it disappears")
	}
	if announcer.imetWithdrawals != 1 {
		t.Fatalf("got %d IMET withdrawals, want 1", announcer.imetWithdrawals)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
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

type orderedAnnouncer struct {
	operations  []string
	withdrawErr error
}

func (*orderedAnnouncer) Announce(context.Context, event.EVPNRoute) error { return nil }
func (a *orderedAnnouncer) Withdraw(_ context.Context, route event.EVPNRoute) error {
	a.operations = append(a.operations, "type2:"+route.MAC)
	return a.withdrawErr
}
func (*orderedAnnouncer) AnnounceIMET(context.Context, event.IMETRoute) error { return nil }
func (a *orderedAnnouncer) WithdrawIMET(context.Context, event.IMETRoute) error {
	a.operations = append(a.operations, "type3")
	return a.withdrawErr
}

func TestShutdownWithdrawsType2BeforeType3AndClearsOVS(t *testing.T) {
	announcer := &orderedAnnouncer{}
	manager, err := NewManager(context.Background(), []string{"br0"}, &changingBridgeRunner{}, announcer, MonitorConfig{
		RD: "65000:100", VNI: 100, VTEP: "192.0.2.10", Interval: time.Hour,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	route := event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.10"}
	secondRoute := event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:02", VTEP: "192.0.2.10"}
	manager.localRoutes[routeKey(route)] = route
	manager.localRoutes[routeKey(secondRoute)] = secondRoute
	manager.localIMET = true
	client := &recordingOVS{}
	manager.active["br0"] = managedBridge{name: "br0", ovs: client, cancel: func() {}}

	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(announcer.operations) != 3 || announcer.operations[2] != "type3" {
		t.Fatalf("shutdown order: %v", announcer.operations)
	}
	if client.clears != 1 {
		t.Fatalf("got %d OVS cleanups, want 1", client.clears)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.clears != 1 {
		t.Fatal("repeated shutdown must be idempotent")
	}
}

func TestGoBGPWithdrawalFailureDoesNotPreventOVSCleanup(t *testing.T) {
	announcer := &orderedAnnouncer{withdrawErr: errors.New("gobgp unavailable")}
	manager, err := NewManager(context.Background(), []string{"br0"}, &changingBridgeRunner{}, announcer, MonitorConfig{
		RD: "65000:100", VNI: 100, VTEP: "192.0.2.10", Interval: time.Hour,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	route := event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.10"}
	secondRoute := event.EVPNRoute{RD: "65000:100", VNI: 100, MAC: "02:00:00:00:00:02", VTEP: "192.0.2.10"}
	manager.localRoutes[routeKey(route)] = route
	manager.localRoutes[routeKey(secondRoute)] = secondRoute
	manager.localIMET = true
	client := &recordingOVS{}
	manager.active["br0"] = managedBridge{name: "br0", ovs: client, cancel: func() {}}
	if err := manager.Shutdown(context.Background()); err == nil {
		t.Fatal("withdrawal failure was not reported")
	}
	if client.clears != 1 {
		t.Fatal("OVS cleanup was skipped after GoBGP failure")
	}
	if len(announcer.operations) != 3 || announcer.operations[2] != "type3" {
		t.Fatalf("not all withdrawals were attempted in Type-2/Type-3 order: %v", announcer.operations)
	}
}
