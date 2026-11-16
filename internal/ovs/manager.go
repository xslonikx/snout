package ovs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/evpn"
	"github.com/xslonikx/snout/internal/logging"
)

type managedBridge struct {
	name   string
	spec   evpn.Bridge
	ovs    OpenVSwitch
	cancel context.CancelFunc
	done   <-chan struct{}
}

// clientFactory creates the dataplane client of one bridge.
type clientFactory func(evpn.Bridge, Runner) (OpenVSwitch, error)

func newExecClient(bridge evpn.Bridge, runner Runner) (OpenVSwitch, error) {
	return NewClient(bridge, runner)
}

// Manager activates configured OVS bridges, applies remote EVPN routes to the
// bridge that owns their VNI, and tracks every local route and OVS object
// owned by Snout.
type Manager struct {
	ctx          context.Context
	cancel       context.CancelFunc
	topology     evpn.Topology
	runner       Runner
	newClient    clientFactory
	announcer    Announcer
	monitor      MonitorConfig
	scanInterval time.Duration

	mu            sync.Mutex
	active        map[string]managedBridge
	routes        map[string]event.EVPNRoute
	imets         map[string]event.IMETRoute
	states        map[string]bool
	localRoutes   map[string]event.EVPNRoute
	localIMETs    map[uint32]event.IMETRoute
	stopping      bool
	shutdownDone  bool
	shutdownError error
	waitGroup     sync.WaitGroup
}

func NewManager(ctx context.Context, topology evpn.Topology, runner Runner, announcer Announcer, monitor MonitorConfig, scanInterval time.Duration) (*Manager, error) {
	if len(topology.Bridges) == 0 {
		return nil, fmt.Errorf("at least one OVS bridge is required")
	}
	if runner == nil || announcer == nil {
		return nil, fmt.Errorf("OVS runner and announcer are required")
	}
	if net.ParseIP(monitor.VTEP) == nil || monitor.Interval <= 0 {
		return nil, fmt.Errorf("valid VTEP and monitor interval are required")
	}
	if scanInterval <= 0 {
		return nil, fmt.Errorf("bridge scan interval must be positive")
	}
	applyCtx, cancel := context.WithCancel(ctx)
	return &Manager{
		ctx: applyCtx, cancel: cancel, topology: topology, runner: runner, newClient: newExecClient, announcer: announcer,
		monitor: monitor, scanInterval: scanInterval, active: make(map[string]managedBridge), routes: make(map[string]event.EVPNRoute),
		imets: make(map[string]event.IMETRoute), states: make(map[string]bool), localRoutes: make(map[string]event.EVPNRoute),
		localIMETs: make(map[uint32]event.IMETRoute),
	}, nil
}

func (m *Manager) Consume(value any) {
	m.mu.Lock()
	stopping := m.stopping
	m.mu.Unlock()
	if stopping {
		return
	}
	switch routeEvent := value.(type) {
	case event.RouteEvent:
		m.consumeType2(routeEvent)
	case event.IMETEvent:
		m.consumeType3(routeEvent)
	}
}

// StopAccepting synchronously prevents new remote event application and local
// announcements. Shutdown performs withdrawals and dataplane cleanup later.
func (m *Manager) StopAccepting() {
	m.mu.Lock()
	if !m.stopping {
		m.stopping = true
		m.cancel()
	}
	m.mu.Unlock()
}

// owner returns the active client of the bridge that owns vni. known is false
// when no configured bridge carries the VNI.
func (m *Manager) ownerLocked(vni uint32) (client OpenVSwitch, known bool) {
	bridge, _, ok := m.topology.BridgeForVNI(vni)
	if !ok {
		return nil, false
	}
	return m.active[bridge.Name].ovs, true
}

func (m *Manager) consumeType2(routeEvent event.RouteEvent) {
	if sameVTEP(routeEvent.Route.VTEP, m.monitor.VTEP) {
		return
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	client, known := m.ownerLocked(routeEvent.Route.VNI)
	if !known {
		m.mu.Unlock()
		return
	}
	key := routeKey(routeEvent.Route)
	if routeEvent.Action == event.RouteDelete {
		delete(m.routes, key)
	} else if routeEvent.Action == event.RouteAdd {
		m.routes[key] = routeEvent.Route
	}
	m.mu.Unlock()
	if client == nil {
		return
	}
	var err error
	switch routeEvent.Action {
	case event.RouteAdd:
		err = client.AddRemoteMAC(m.ctx, routeEvent.Route)
	case event.RouteDelete:
		err = client.DeleteRemoteMAC(m.ctx, routeEvent.Route)
	}
	if err != nil {
		logging.Errorf("apply EVPN route to OVS: %v", err)
	}
}

func (m *Manager) consumeType3(imetEvent event.IMETEvent) {
	if sameVTEP(imetEvent.Route.VTEP, m.monitor.VTEP) {
		return
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	client, known := m.ownerLocked(imetEvent.Route.VNI)
	if !known {
		m.mu.Unlock()
		return
	}
	key := imetKey(imetEvent.Route)
	if imetEvent.Action == event.RouteDelete {
		delete(m.imets, key)
	} else if imetEvent.Action == event.RouteAdd {
		m.imets[key] = imetEvent.Route
	}
	m.mu.Unlock()
	if client == nil {
		return
	}
	var err error
	switch imetEvent.Action {
	case event.RouteAdd:
		err = client.AddRemoteVTEP(m.ctx, imetEvent.Route)
	case event.RouteDelete:
		err = client.DeleteRemoteVTEP(m.ctx, imetEvent.Route)
	}
	if err != nil {
		logging.Errorf("apply EVPN Type-3 route to OVS: %v", err)
	}
}

func sameVTEP(left, right string) bool {
	leftIP := net.ParseIP(left)
	rightIP := net.ParseIP(right)
	return leftIP != nil && rightIP != nil && leftIP.Equal(rightIP)
}

func (m *Manager) Run(ctx context.Context) error {
	m.scan(ctx)
	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.scan(ctx)
		}
	}
}

func (m *Manager) scan(ctx context.Context) {
	for _, bridge := range m.topology.Bridges {
		client, err := m.newClient(bridge, m.runner)
		if err != nil {
			logging.Errorf("create OVS client for bridge %s: %v", bridge.Name, err)
			continue
		}
		exists, err := client.BridgeExists(ctx)
		if err != nil {
			logging.Errorf("inspect OVS bridge %s: %v", bridge.Name, err)
			continue
		}
		m.mu.Lock()
		previous, known := m.states[bridge.Name]
		m.states[bridge.Name] = exists
		m.mu.Unlock()
		if !known || previous != exists {
			if exists {
				logging.Infof("OVS bridge %s exists; synchronization of %d VNI(s) is enabled", bridge.Name, len(bridge.Segments))
			} else {
				logging.Infof("OVS bridge %s does not exist; bridge is ignored", bridge.Name)
			}
		}
		if exists {
			m.activate(ctx, bridge, client)
		} else {
			m.deactivate(bridge.Name)
		}
	}
}

// announceBridgeIMET announces the local Type-3 route of every segment of the
// bridge. Already announced routes are skipped, so failed announcements are
// retried on the next scan without affecting the other segments.
func (m *Manager) announceBridgeIMET(ctx context.Context, bridge evpn.Bridge) {
	for _, segment := range bridge.Segments {
		if err := m.AnnounceIMET(ctx, localIMETRoute(segment, m.monitor.VTEP)); err != nil && !errors.Is(err, context.Canceled) {
			logging.Errorf("announce local EVPN Type-3 route for VNI %d on bridge %s: %v", segment.VNI, bridge.Name, err)
		}
	}
}

func (m *Manager) activate(ctx context.Context, bridge evpn.Bridge, client OpenVSwitch) {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	if m.active[bridge.Name].ovs != nil {
		m.mu.Unlock()
		m.announceBridgeIMET(ctx, bridge)
		return
	}
	m.mu.Unlock()

	if err := client.RemoveStalePorts(ctx); err != nil {
		logging.Errorf("remove stale VXLAN ports on bridge %s: %v", bridge.Name, err)
	}
	m.announceBridgeIMET(ctx, bridge)

	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	monitorCtx, cancel := context.WithCancel(m.ctx)
	done := make(chan struct{})
	m.active[bridge.Name] = managedBridge{name: bridge.Name, spec: bridge, ovs: client, cancel: cancel, done: done}
	var routes []event.EVPNRoute
	for _, route := range m.routes {
		if _, ok := bridge.SegmentByVNI(route.VNI); ok {
			routes = append(routes, route)
		}
	}
	var imets []event.IMETRoute
	for _, imet := range m.imets {
		if _, ok := bridge.SegmentByVNI(imet.VNI); ok {
			imets = append(imets, imet)
		}
	}
	m.waitGroup.Add(1)
	m.mu.Unlock()

	for _, imet := range imets {
		if err := client.AddRemoteVTEP(ctx, imet); err != nil {
			logging.Errorf("restore EVPN Type-3 route on OVS bridge %s: %v", bridge.Name, err)
		}
	}
	for _, route := range routes {
		if err := client.AddRemoteMAC(ctx, route); err != nil {
			logging.Errorf("restore EVPN route on OVS bridge %s: %v", bridge.Name, err)
		}
	}
	go func() {
		defer m.waitGroup.Done()
		defer close(done)
		if err := MonitorLocalMACs(monitorCtx, client, m, bridge, m.monitor); err != nil && monitorCtx.Err() == nil {
			logging.Errorf("monitor OVS bridge %s: %v", bridge.Name, err)
		}
	}()
}

func (m *Manager) deactivate(name string) {
	m.mu.Lock()
	bridge, exists := m.active[name]
	if exists {
		delete(m.active, name)
	}
	m.mu.Unlock()
	if !exists {
		return
	}
	bridge.cancel()
	<-bridge.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vnis := bridgeVNIs(bridge.spec)
	if err := m.withdrawLocalRoutes(ctx, vnis); err != nil {
		logging.Errorf("withdraw local EVPN Type-2 routes of bridge %s: %v", name, err)
	}
	if err := m.withdrawLocalIMET(ctx, vnis); err != nil {
		logging.Errorf("withdraw local EVPN Type-3 routes of bridge %s: %v", name, err)
	}
	if err := bridge.ovs.ClearRemoteState(ctx); err != nil {
		logging.Errorf("clear remote OVS state on bridge %s: %v", name, err)
	}
}

func bridgeVNIs(bridge evpn.Bridge) map[uint32]struct{} {
	vnis := make(map[uint32]struct{}, len(bridge.Segments))
	for _, segment := range bridge.Segments {
		vnis[segment.VNI] = struct{}{}
	}
	return vnis
}

func (m *Manager) Announce(ctx context.Context, route event.EVPNRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return context.Canceled
	}
	if err := m.announcer.Announce(ctx, route); err != nil {
		return err
	}
	m.localRoutes[routeKey(route)] = route
	return nil
}

func (m *Manager) Withdraw(ctx context.Context, route event.EVPNRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := routeKey(route)
	if _, exists := m.localRoutes[key]; !exists {
		return nil
	}
	if err := m.announcer.Withdraw(ctx, route); err != nil {
		return err
	}
	delete(m.localRoutes, key)
	return nil
}

func (m *Manager) AnnounceIMET(ctx context.Context, route event.IMETRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return context.Canceled
	}
	if _, exists := m.localIMETs[route.VNI]; exists {
		return nil
	}
	if err := m.announcer.AnnounceIMET(ctx, route); err != nil {
		return err
	}
	m.localIMETs[route.VNI] = route
	return nil
}

func (m *Manager) WithdrawIMET(ctx context.Context, route event.IMETRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.localIMETs[route.VNI]; !exists {
		return nil
	}
	if err := m.announcer.WithdrawIMET(ctx, route); err != nil {
		return err
	}
	delete(m.localIMETs, route.VNI)
	return nil
}

// withdrawLocalRoutes withdraws local Type-2 routes; a nil vnis withdraws
// all of them.
func (m *Manager) withdrawLocalRoutes(ctx context.Context, vnis map[uint32]struct{}) error {
	m.mu.Lock()
	routes := make([]event.EVPNRoute, 0, len(m.localRoutes))
	for _, route := range m.localRoutes {
		if _, selected := vnis[route.VNI]; vnis == nil || selected {
			routes = append(routes, route)
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, route := range routes {
		if err := m.Withdraw(ctx, route); err != nil {
			errs = append(errs, fmt.Errorf("withdraw local MAC %s in VNI %d: %w", route.MAC, route.VNI, err))
		}
	}
	return errors.Join(errs...)
}

// withdrawLocalIMET withdraws local Type-3 routes; a nil vnis withdraws all
// of them.
func (m *Manager) withdrawLocalIMET(ctx context.Context, vnis map[uint32]struct{}) error {
	m.mu.Lock()
	routes := make([]event.IMETRoute, 0, len(m.localIMETs))
	for vni, route := range m.localIMETs {
		if _, selected := vnis[vni]; vnis == nil || selected {
			routes = append(routes, route)
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, route := range routes {
		if err := m.WithdrawIMET(ctx, route); err != nil {
			errs = append(errs, fmt.Errorf("withdraw local EVPN Type-3 route for VNI %d: %w", route.VNI, err))
		}
	}
	return errors.Join(errs...)
}

// Shutdown stops event application and local discovery, then withdraws local
// Type-2 routes before Type-3 and finally removes only Snout-owned OVS state.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.StopAccepting()
	m.mu.Lock()
	if m.shutdownDone {
		err := m.shutdownError
		m.mu.Unlock()
		return err
	}
	bridges := make([]managedBridge, 0, len(m.active))
	for _, bridge := range m.active {
		bridges = append(bridges, bridge)
		bridge.cancel()
	}
	m.active = make(map[string]managedBridge)
	m.mu.Unlock()

	waitDone := make(chan struct{})
	go func() {
		m.waitGroup.Wait()
		close(waitDone)
	}()
	var errs []error
	select {
	case <-waitDone:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("stop local MAC monitors: %w", ctx.Err()))
	}
	if err := m.withdrawLocalRoutes(ctx, nil); err != nil {
		errs = append(errs, err)
	}
	if err := m.withdrawLocalIMET(ctx, nil); err != nil {
		errs = append(errs, err)
	}
	for _, bridge := range bridges {
		if err := bridge.ovs.ClearRemoteState(ctx); err != nil {
			errs = append(errs, fmt.Errorf("clear remote OVS state on bridge %s: %w", bridge.name, err))
		}
	}
	err := errors.Join(errs...)
	if err != nil {
		logging.Errorf("graceful shutdown cleanup: %v", err)
	}
	m.mu.Lock()
	m.shutdownDone = true
	m.shutdownError = err
	m.mu.Unlock()
	return err
}
