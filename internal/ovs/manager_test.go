package ovs

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/evpn"
)

// recordingOVS is a dataplane fake whose bridge presence can be toggled.
type recordingOVS struct {
	mu           sync.Mutex
	exists       bool
	addedMACs    []event.EVPNRoute
	addedVTEPs   []event.IMETRoute
	deletedVTEPs []event.IMETRoute
	staleScans   int
	clears       int
}

func (o *recordingOVS) BridgeExists(context.Context) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.exists, nil
}
func (o *recordingOVS) RemoveStalePorts(context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.staleScans++
	return nil
}
func (o *recordingOVS) ClearRemoteState(context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.clears++
	return nil
}
func (o *recordingOVS) AddRemoteMAC(_ context.Context, route event.EVPNRoute) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.addedMACs = append(o.addedMACs, route)
	return nil
}
func (*recordingOVS) DeleteRemoteMAC(context.Context, event.EVPNRoute) error { return nil }
func (o *recordingOVS) AddRemoteVTEP(_ context.Context, route event.IMETRoute) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.addedVTEPs = append(o.addedVTEPs, route)
	return nil
}
func (o *recordingOVS) DeleteRemoteVTEP(_ context.Context, route event.IMETRoute) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deletedVTEPs = append(o.deletedVTEPs, route)
	return nil
}
func (*recordingOVS) LocalMACs(context.Context) ([]FDBEntry, error) { return nil, nil }

// imetAnnouncer records Type-3 operations and can fail selected VNIs.
type imetAnnouncer struct {
	mu          sync.Mutex
	announced   []uint32
	withdrawn   []uint32
	operations  []string
	failVNI     map[uint32]int
	withdrawErr error
}

func (*imetAnnouncer) Announce(context.Context, event.EVPNRoute) error { return nil }
func (a *imetAnnouncer) Withdraw(_ context.Context, route event.EVPNRoute) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.operations = append(a.operations, "type2:"+route.MAC)
	return a.withdrawErr
}
func (a *imetAnnouncer) AnnounceIMET(_ context.Context, route event.IMETRoute) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failVNI[route.VNI] > 0 {
		a.failVNI[route.VNI]--
		return errors.New("gobgp rejected the path")
	}
	a.announced = append(a.announced, route.VNI)
	return nil
}
func (a *imetAnnouncer) WithdrawIMET(_ context.Context, route event.IMETRoute) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.withdrawn = append(a.withdrawn, route.VNI)
	a.operations = append(a.operations, "type3")
	return a.withdrawErr
}

type fakeRunnerNoop struct{}

func (fakeRunnerNoop) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }

func testTopology(t *testing.T, bridges ...evpn.Bridge) evpn.Topology {
	t.Helper()
	return evpn.Topology{VTEP: "172.31.0.11", Bridges: bridges}
}

// newTestManager builds a manager whose bridges are served by recordingOVS
// fakes, created on demand and returned through the map.
func newTestManager(t *testing.T, topology evpn.Topology, announcer Announcer) (*Manager, map[string]*recordingOVS) {
	t.Helper()
	manager, err := NewManager(context.Background(), topology, fakeRunnerNoop{}, announcer, MonitorConfig{VTEP: topology.VTEP, Interval: time.Hour}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clients := make(map[string]*recordingOVS)
	for _, bridge := range topology.Bridges {
		clients[bridge.Name] = &recordingOVS{exists: true}
	}
	manager.newClient = func(bridge evpn.Bridge, _ Runner) (OpenVSwitch, error) {
		return clients[bridge.Name], nil
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	return manager, clients
}

func sortedVNIs(values []uint32) []uint32 {
	result := append([]uint32(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func TestNewManagerValidation(t *testing.T) {
	topology := testTopology(t, flatBridge(t, "br0", 100))
	config := MonitorConfig{VTEP: "172.31.0.11", Interval: time.Second}
	if _, err := NewManager(context.Background(), evpn.Topology{}, fakeRunnerNoop{}, &imetAnnouncer{}, config, time.Second); err == nil {
		t.Fatal("empty topology was accepted")
	}
	if _, err := NewManager(context.Background(), topology, nil, &imetAnnouncer{}, config, time.Second); err == nil {
		t.Fatal("nil runner was accepted")
	}
	if _, err := NewManager(context.Background(), topology, fakeRunnerNoop{}, &imetAnnouncer{}, MonitorConfig{VTEP: "bad", Interval: time.Second}, time.Second); err == nil {
		t.Fatal("invalid VTEP was accepted")
	}
	if _, err := NewManager(context.Background(), topology, fakeRunnerNoop{}, &imetAnnouncer{}, config, 0); err == nil {
		t.Fatal("zero scan interval was accepted")
	}
}

func TestManagerAnnouncesIMETPerVNIAndWithdrawsOnBridgeLoss(t *testing.T) {
	announcer := &imetAnnouncer{}
	manager, clients := newTestManager(t, testTopology(t, vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200))), announcer)
	ctx := context.Background()

	manager.scan(ctx)
	manager.scan(ctx)
	if got := sortedVNIs(announcer.announced); len(got) != 2 || got[0] != 10100 || got[1] != 10200 {
		t.Fatalf("got IMET announcements %v, want one per VNI", announcer.announced)
	}
	if clients["br0"].staleScans != 1 {
		t.Fatalf("stale ports must be removed once on activation, got %d", clients["br0"].staleScans)
	}

	clients["br0"].exists = false
	manager.scan(ctx)
	if len(manager.active) != 0 {
		t.Fatal("bridge must be deactivated after it disappears")
	}
	if got := sortedVNIs(announcer.withdrawn); len(got) != 2 || got[0] != 10100 || got[1] != 10200 {
		t.Fatalf("got IMET withdrawals %v, want both VNIs", announcer.withdrawn)
	}
	if clients["br0"].clears != 1 {
		t.Fatal("remote state was not cleared")
	}
}

func TestManagerRetriesFailedIMETWithoutBlockingOtherVNIs(t *testing.T) {
	announcer := &imetAnnouncer{failVNI: map[uint32]int{10200: 1}}
	manager, _ := newTestManager(t, testTopology(t, vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200))), announcer)
	ctx := context.Background()

	manager.scan(ctx)
	if len(manager.active) != 1 {
		t.Fatal("a failing VNI must not prevent bridge activation")
	}
	if got := announcer.announced; len(got) != 1 || got[0] != 10100 {
		t.Fatalf("after first scan got %v", got)
	}
	manager.scan(ctx)
	if got := sortedVNIs(announcer.announced); len(got) != 2 || got[1] != 10200 {
		t.Fatalf("failed IMET was not retried: %v", got)
	}
}

func TestManagerRoutesEventsToTheBridgeOwningTheVNI(t *testing.T) {
	topology := testTopology(t,
		vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200)),
		flatBridge(t, "br-flat", 50),
	)
	manager, clients := newTestManager(t, topology, &imetAnnouncer{})
	manager.scan(context.Background())

	manager.Consume(event.RouteEvent{Action: event.RouteAdd, Route: event.EVPNRoute{VNI: 10200, MAC: "02:00:00:00:00:01", VTEP: "172.31.0.12"}})
	manager.Consume(event.RouteEvent{Action: event.RouteAdd, Route: event.EVPNRoute{VNI: 50, MAC: "02:00:00:00:00:02", VTEP: "172.31.0.12"}})
	manager.Consume(event.RouteEvent{Action: event.RouteAdd, Route: event.EVPNRoute{VNI: 9999, MAC: "02:00:00:00:00:03", VTEP: "172.31.0.12"}})
	manager.Consume(event.RouteEvent{Action: event.RouteAdd, Route: event.EVPNRoute{VNI: 10100, MAC: "02:00:00:00:00:04", VTEP: "172.31.0.11"}})
	manager.Consume(event.IMETEvent{Action: event.RouteAdd, Route: event.IMETRoute{VNI: 10100, VTEP: "172.31.0.13"}})
	manager.Consume(event.IMETEvent{Action: event.RouteAdd, Route: event.IMETRoute{VNI: 9999, VTEP: "172.31.0.13"}})

	vlanClient, flatClient := clients["br0"], clients["br-flat"]
	if len(vlanClient.addedMACs) != 1 || vlanClient.addedMACs[0].VNI != 10200 {
		t.Fatalf("VLAN bridge MACs: %#v", vlanClient.addedMACs)
	}
	if len(flatClient.addedMACs) != 1 || flatClient.addedMACs[0].VNI != 50 {
		t.Fatalf("flat bridge MACs: %#v", flatClient.addedMACs)
	}
	if len(vlanClient.addedVTEPs) != 1 || vlanClient.addedVTEPs[0].VNI != 10100 || len(flatClient.addedVTEPs) != 0 {
		t.Fatalf("IMET routing: vlan=%#v flat=%#v", vlanClient.addedVTEPs, flatClient.addedVTEPs)
	}
	if len(manager.routes) != 2 || len(manager.imets) != 1 {
		t.Fatalf("unknown VNIs or local routes were stored: routes=%d imets=%d", len(manager.routes), len(manager.imets))
	}
}

func TestManagerReplaysOnlyRoutesOfTheActivatedBridge(t *testing.T) {
	topology := testTopology(t,
		vlanBridge(t, "br0", segment(100, 10100)),
		vlanBridge(t, "br1", evpn.Segment{VLAN: 100, VNI: 20100, RD: "65000:20100", RouteTarget: "65000:20100"}),
	)
	manager, clients := newTestManager(t, topology, &imetAnnouncer{})
	clients["br0"].exists = false
	clients["br1"].exists = false
	manager.scan(context.Background())

	manager.Consume(event.RouteEvent{Action: event.RouteAdd, Route: event.EVPNRoute{VNI: 10100, MAC: "02:00:00:00:00:01", VTEP: "172.31.0.12"}})
	manager.Consume(event.RouteEvent{Action: event.RouteAdd, Route: event.EVPNRoute{VNI: 20100, MAC: "02:00:00:00:00:02", VTEP: "172.31.0.12"}})
	manager.Consume(event.IMETEvent{Action: event.RouteAdd, Route: event.IMETRoute{VNI: 20100, VTEP: "172.31.0.12"}})

	clients["br1"].exists = true
	manager.scan(context.Background())
	if len(clients["br0"].addedMACs) != 0 {
		t.Fatal("routes were applied to an inactive bridge")
	}
	if len(clients["br1"].addedMACs) != 1 || clients["br1"].addedMACs[0].VNI != 20100 {
		t.Fatalf("replayed MACs: %#v", clients["br1"].addedMACs)
	}
	if len(clients["br1"].addedVTEPs) != 1 || clients["br1"].addedVTEPs[0].VNI != 20100 {
		t.Fatalf("replayed VTEPs: %#v", clients["br1"].addedVTEPs)
	}
}

func TestManagerAppliesRemoteIMETAndIgnoresLocalIMET(t *testing.T) {
	topology := evpn.Topology{VTEP: "2001:db8::1", Bridges: []evpn.Bridge{flatBridge(t, "br0", 100)}}
	manager, clients := newTestManager(t, topology, &imetAnnouncer{})
	manager.scan(context.Background())
	remote := event.IMETRoute{RD: "65000:100", VNI: 100, VTEP: "192.0.2.20"}
	manager.Consume(event.IMETEvent{Action: event.RouteAdd, Route: remote})
	manager.Consume(event.IMETEvent{Action: event.RouteDelete, Route: remote})
	manager.Consume(event.IMETEvent{Action: event.RouteAdd, Route: event.IMETRoute{RD: "65000:100", VNI: 100, VTEP: "2001:0db8:0:0:0:0:0:1"}})
	client := clients["br0"]
	if len(client.addedVTEPs) != 1 || client.addedVTEPs[0] != remote {
		t.Fatalf("unexpected added VTEPs: %#v", client.addedVTEPs)
	}
	if len(client.deletedVTEPs) != 1 || client.deletedVTEPs[0] != remote {
		t.Fatalf("unexpected deleted VTEPs: %#v", client.deletedVTEPs)
	}
}

func TestDeactivationWithdrawsOnlyThatBridgesLocalRoutes(t *testing.T) {
	topology := testTopology(t, vlanBridge(t, "br0", segment(100, 10100)), flatBridge(t, "br-flat", 50))
	announcer := &imetAnnouncer{}
	manager, clients := newTestManager(t, topology, announcer)
	manager.scan(context.Background())
	keep := event.EVPNRoute{RD: "172.31.0.11:50", VNI: 50, MAC: "02:00:00:00:00:01", VTEP: "172.31.0.11"}
	drop := event.EVPNRoute{RD: "172.31.0.11:100", VNI: 10100, MAC: "02:00:00:00:00:02", VTEP: "172.31.0.11"}
	manager.localRoutes[routeKey(keep)] = keep
	manager.localRoutes[routeKey(drop)] = drop

	clients["br0"].exists = false
	manager.scan(context.Background())
	if len(announcer.operations) != 2 || announcer.operations[0] != "type2:"+drop.MAC || announcer.operations[1] != "type3" {
		t.Fatalf("unexpected withdrawals: %v", announcer.operations)
	}
	if _, kept := manager.localRoutes[routeKey(keep)]; !kept {
		t.Fatal("route of the other bridge was withdrawn")
	}
	if _, kept := manager.localIMETs[50]; !kept {
		t.Fatal("IMET of the other bridge was withdrawn")
	}
}

func TestShutdownWithdrawsType2BeforeType3AndClearsOVS(t *testing.T) {
	announcer := &imetAnnouncer{}
	manager, clients := newTestManager(t, testTopology(t, flatBridge(t, "br0", 100)), announcer)
	manager.scan(context.Background())
	route := event.EVPNRoute{RD: "172.31.0.11:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "172.31.0.11"}
	secondRoute := event.EVPNRoute{RD: "172.31.0.11:100", VNI: 100, MAC: "02:00:00:00:00:02", VTEP: "172.31.0.11"}
	manager.localRoutes[routeKey(route)] = route
	manager.localRoutes[routeKey(secondRoute)] = secondRoute

	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(announcer.operations) != 3 || announcer.operations[2] != "type3" {
		t.Fatalf("shutdown order: %v", announcer.operations)
	}
	if clients["br0"].clears != 1 {
		t.Fatalf("got %d OVS cleanups, want 1", clients["br0"].clears)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if clients["br0"].clears != 1 {
		t.Fatal("repeated shutdown must be idempotent")
	}
}

func TestGoBGPWithdrawalFailureDoesNotPreventOVSCleanup(t *testing.T) {
	announcer := &imetAnnouncer{}
	manager, clients := newTestManager(t, testTopology(t, flatBridge(t, "br0", 100)), announcer)
	manager.scan(context.Background())
	announcer.withdrawErr = errors.New("gobgp unavailable")
	route := event.EVPNRoute{RD: "172.31.0.11:100", VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "172.31.0.11"}
	secondRoute := event.EVPNRoute{RD: "172.31.0.11:100", VNI: 100, MAC: "02:00:00:00:00:02", VTEP: "172.31.0.11"}
	manager.localRoutes[routeKey(route)] = route
	manager.localRoutes[routeKey(secondRoute)] = secondRoute
	if err := manager.Shutdown(context.Background()); err == nil {
		t.Fatal("withdrawal failure was not reported")
	}
	if clients["br0"].clears != 1 {
		t.Fatal("OVS cleanup was skipped after GoBGP failure")
	}
	if len(announcer.operations) != 3 || announcer.operations[2] != "type3" {
		t.Fatalf("not all withdrawals were attempted in Type-2/Type-3 order: %v", announcer.operations)
	}
}

type type2Announcer struct {
	imetAnnouncer
	announced []event.EVPNRoute
}

func (a *type2Announcer) Announce(_ context.Context, route event.EVPNRoute) error {
	a.announced = append(a.announced, route)
	return nil
}

func TestManagerTracksLocalType2PerVNI(t *testing.T) {
	announcer := &type2Announcer{}
	manager, _ := newTestManager(t, testTopology(t, vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200))), announcer)
	ctx := context.Background()
	first := event.EVPNRoute{RD: "172.31.0.11:100", VNI: 10100, MAC: "02:00:00:00:00:01", VTEP: "172.31.0.11", RouteTarget: "65000:10100"}
	second := first
	second.VNI, second.RD, second.RouteTarget = 10200, "172.31.0.11:200", "65000:10200"
	for _, route := range []event.EVPNRoute{first, second} {
		if err := manager.Announce(ctx, route); err != nil {
			t.Fatal(err)
		}
	}
	if len(manager.localRoutes) != 2 || len(announcer.announced) != 2 {
		t.Fatalf("the same MAC in two VNIs must be two local routes: %#v", manager.localRoutes)
	}
	if err := manager.Withdraw(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, exists := manager.localRoutes[routeKey(second)]; !exists || len(manager.localRoutes) != 1 {
		t.Fatalf("withdrawal removed the wrong route: %#v", manager.localRoutes)
	}
	manager.StopAccepting()
	if err := manager.Announce(ctx, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("announcement after stop: %v", err)
	}
}
