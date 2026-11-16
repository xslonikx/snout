package ovs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xslonikx/snout/internal/event"
)

type monitorOVS struct {
	mu    sync.Mutex
	scans [][]FDBEntry
}

func (*monitorOVS) BridgeExists(context.Context) (bool, error)              { return true, nil }
func (*monitorOVS) RemoveStalePorts(context.Context) error                  { return nil }
func (*monitorOVS) ClearRemoteState(context.Context) error                  { return nil }
func (*monitorOVS) AddRemoteMAC(context.Context, event.EVPNRoute) error     { return nil }
func (*monitorOVS) DeleteRemoteMAC(context.Context, event.EVPNRoute) error  { return nil }
func (*monitorOVS) AddRemoteVTEP(context.Context, event.IMETRoute) error    { return nil }
func (*monitorOVS) DeleteRemoteVTEP(context.Context, event.IMETRoute) error { return nil }
func (o *monitorOVS) LocalMACs(context.Context) ([]FDBEntry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.scans) == 0 {
		return nil, nil
	}
	result := o.scans[0]
	o.scans = o.scans[1:]
	return result, nil
}

// recordingAnnouncer cancels the monitor after `stopAfter` withdrawals.
type recordingAnnouncer struct {
	mu        sync.Mutex
	announced []event.EVPNRoute
	withdrawn []event.EVPNRoute
	stopAfter int
	cancel    context.CancelFunc
}

func (a *recordingAnnouncer) Announce(_ context.Context, route event.EVPNRoute) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.announced = append(a.announced, route)
	return nil
}

func (a *recordingAnnouncer) Withdraw(_ context.Context, route event.EVPNRoute) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.withdrawn = append(a.withdrawn, route)
	if len(a.withdrawn) >= a.stopAfter {
		a.cancel()
	}
	return nil
}

func (*recordingAnnouncer) AnnounceIMET(context.Context, event.IMETRoute) error { return nil }
func (*recordingAnnouncer) WithdrawIMET(context.Context, event.IMETRoute) error { return nil }

func TestMonitorAnnouncesAndWithdrawsMACWithMappedVNI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bridge := vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200))
	ovs := &monitorOVS{scans: [][]FDBEntry{
		{{Port: "1", VLAN: 100, MAC: "02:00:00:00:00:01"}, {Port: "2", VLAN: 300, MAC: "02:00:00:00:00:02"}},
		{},
	}}
	announcer := &recordingAnnouncer{cancel: cancel, stopAfter: 1}
	err := MonitorLocalMACs(ctx, ovs, announcer, bridge, MonitorConfig{VTEP: "172.31.0.11", Interval: time.Millisecond})
	if err != context.Canceled {
		t.Fatalf("unexpected monitor error: %v", err)
	}
	want := event.EVPNRoute{RD: "172.31.0.11:10100", VNI: 10100, MAC: "02:00:00:00:00:01", VTEP: "172.31.0.11", RouteTarget: "65000:10100"}
	if len(announcer.announced) != 1 || announcer.announced[0] != want {
		t.Fatalf("announced %#v, want only %#v (unmapped VLAN 300 must be ignored)", announcer.announced, want)
	}
	if len(announcer.withdrawn) != 1 || announcer.withdrawn[0] != want {
		t.Fatalf("withdrawn %#v", announcer.withdrawn)
	}
}

func TestMonitorTracksSameMACPerVNI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bridge := vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200))
	mac := "02:00:00:00:00:01"
	ovs := &monitorOVS{scans: [][]FDBEntry{
		{{Port: "1", VLAN: 100, MAC: mac}, {Port: "2", VLAN: 200, MAC: mac}},
		{{Port: "2", VLAN: 200, MAC: mac}},
		{},
	}}
	announcer := &recordingAnnouncer{cancel: cancel, stopAfter: 2}
	if err := MonitorLocalMACs(ctx, ovs, announcer, bridge, MonitorConfig{VTEP: "172.31.0.11", Interval: time.Millisecond}); err != context.Canceled {
		t.Fatalf("unexpected monitor error: %v", err)
	}
	if len(announcer.announced) != 2 {
		t.Fatalf("got %d announcements, want one per VNI: %#v", len(announcer.announced), announcer.announced)
	}
	if len(announcer.withdrawn) != 2 || announcer.withdrawn[0].VNI != 10100 || announcer.withdrawn[1].VNI != 10200 {
		t.Fatalf("withdrawals must follow the VNI in which the MAC disappeared: %#v", announcer.withdrawn)
	}
}

func TestMonitorFlatBridgeUsesUntaggedEntriesOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bridge := flatBridge(t, "br-flat", 100)
	ovs := &monitorOVS{scans: [][]FDBEntry{
		{{Port: "1", VLAN: 0, MAC: "02:00:00:00:00:01"}, {Port: "1", VLAN: 5, MAC: "02:00:00:00:00:02"}},
		{},
	}}
	announcer := &recordingAnnouncer{cancel: cancel, stopAfter: 1}
	if err := MonitorLocalMACs(ctx, ovs, announcer, bridge, MonitorConfig{VTEP: "172.31.0.11", Interval: time.Millisecond}); err != context.Canceled {
		t.Fatalf("unexpected monitor error: %v", err)
	}
	if len(announcer.announced) != 1 || announcer.announced[0].VNI != 100 || announcer.announced[0].MAC != "02:00:00:00:00:01" {
		t.Fatalf("unexpected announcements: %#v", announcer.announced)
	}
}

func TestMonitorValidatesArguments(t *testing.T) {
	bridge := flatBridge(t, "br0", 100)
	config := MonitorConfig{VTEP: "172.31.0.11", Interval: time.Second}
	ctx := context.Background()
	if err := MonitorLocalMACs(ctx, nil, &recordingAnnouncer{}, bridge, config); err == nil {
		t.Fatal("nil OVS was accepted")
	}
	if err := MonitorLocalMACs(ctx, &monitorOVS{}, &recordingAnnouncer{}, bridge, MonitorConfig{VTEP: "x", Interval: time.Second}); err == nil {
		t.Fatal("invalid VTEP was accepted")
	}
	if err := MonitorLocalMACs(ctx, &monitorOVS{}, &recordingAnnouncer{}, bridge, MonitorConfig{VTEP: "172.31.0.11"}); err == nil {
		t.Fatal("zero interval was accepted")
	}
}
