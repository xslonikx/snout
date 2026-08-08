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

func (*monitorOVS) IsVXLANBridge(context.Context) (bool, error)            { return true, nil }
func (*monitorOVS) AddRemoteMAC(context.Context, event.EVPNRoute) error    { return nil }
func (*monitorOVS) DeleteRemoteMAC(context.Context, event.EVPNRoute) error { return nil }
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

type recordingAnnouncer struct {
	mu        sync.Mutex
	announced []event.EVPNRoute
	withdrawn []event.EVPNRoute
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
	a.cancel()
	return nil
}

func TestMonitorAnnouncesAndWithdrawsMAC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ovs := &monitorOVS{scans: [][]FDBEntry{{{Port: "1", MAC: "02:00:00:00:00:01"}}, {}}}
	announcer := &recordingAnnouncer{cancel: cancel}
	config := MonitorConfig{RD: "65000:100", VNI: 100, VTEP: "192.0.2.10", Interval: time.Millisecond}

	err := MonitorLocalMACs(ctx, ovs, announcer, config)
	if err != context.Canceled {
		t.Fatalf("unexpected monitor error: %v", err)
	}
	if len(announcer.announced) != 1 || len(announcer.withdrawn) != 1 {
		t.Fatalf("got %d announcements and %d withdrawals", len(announcer.announced), len(announcer.withdrawn))
	}
	if announcer.announced[0] != announcer.withdrawn[0] {
		t.Fatalf("withdrawal does not match announcement")
	}
}
