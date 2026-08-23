package ovs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/logging"
)

type managedBridge struct {
	name   string
	ovs    OpenVSwitch
	cancel context.CancelFunc
	done   <-chan struct{}
}

// Manager activates configured OVS bridges, applies remote EVPN routes, and
// tracks every local route and OVS object owned by Snout.
type Manager struct {
	ctx          context.Context
	cancel       context.CancelFunc
	bridges      []string
	runner       Runner
	announcer    Announcer
	monitor      MonitorConfig
	scanInterval time.Duration

	mu            sync.Mutex
	active        map[string]managedBridge
	routes        map[string]event.EVPNRoute
	imets         map[string]event.IMETRoute
	states        map[string]bool
	localRoutes   map[string]event.EVPNRoute
	localIMET     bool
	stopping      bool
	shutdownDone  bool
	shutdownError error
	waitGroup     sync.WaitGroup
}

func NewManager(ctx context.Context, bridges []string, runner Runner, announcer Announcer, monitor MonitorConfig, scanInterval time.Duration) (*Manager, error) {
	if len(bridges) == 0 {
		return nil, fmt.Errorf("at least one OVS bridge is required")
	}
	if runner == nil || announcer == nil {
		return nil, fmt.Errorf("OVS runner and announcer are required")
	}
	if monitor.RD == "" || monitor.VNI == 0 || net.ParseIP(monitor.VTEP) == nil || monitor.Interval <= 0 {
		return nil, fmt.Errorf("valid RD, VNI, VTEP, and monitor interval are required")
	}
	if scanInterval <= 0 {
		return nil, fmt.Errorf("bridge scan interval must be positive")
	}
	applyCtx, cancel := context.WithCancel(ctx)
	return &Manager{
		ctx: applyCtx, cancel: cancel, bridges: append([]string(nil), bridges...), runner: runner, announcer: announcer,
		monitor: monitor, scanInterval: scanInterval, active: make(map[string]managedBridge), routes: make(map[string]event.EVPNRoute),
		imets: make(map[string]event.IMETRoute), states: make(map[string]bool), localRoutes: make(map[string]event.EVPNRoute),
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

func (m *Manager) consumeType2(routeEvent event.RouteEvent) {
	if sameVTEP(routeEvent.Route.VTEP, m.monitor.VTEP) || routeEvent.Route.VNI != m.monitor.VNI {
		return
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	key := routeKey(routeEvent.Route)
	if routeEvent.Action == event.RouteDelete {
		delete(m.routes, key)
	} else if routeEvent.Action == event.RouteAdd {
		m.routes[key] = routeEvent.Route
	}
	clients := m.activeClientsLocked()
	m.mu.Unlock()
	for _, client := range clients {
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
}

func (m *Manager) consumeType3(imetEvent event.IMETEvent) {
	if sameVTEP(imetEvent.Route.VTEP, m.monitor.VTEP) || imetEvent.Route.VNI != m.monitor.VNI {
		return
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	key := imetKey(imetEvent.Route)
	if imetEvent.Action == event.RouteDelete {
		delete(m.imets, key)
	} else if imetEvent.Action == event.RouteAdd {
		m.imets[key] = imetEvent.Route
	}
	clients := m.activeClientsLocked()
	m.mu.Unlock()
	for _, client := range clients {
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
}

func (m *Manager) activeClientsLocked() []OpenVSwitch {
	clients := make([]OpenVSwitch, 0, len(m.active))
	for _, bridge := range m.active {
		clients = append(clients, bridge.ovs)
	}
	return clients
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
	for _, bridge := range m.bridges {
		client, err := NewClient(bridge, m.runner)
		if err != nil {
			logging.Errorf("create OVS client for bridge %s: %v", bridge, err)
			continue
		}
		exists, err := client.BridgeExists(ctx)
		if err != nil {
			logging.Errorf("inspect OVS bridge %s: %v", bridge, err)
			continue
		}
		m.mu.Lock()
		previous, known := m.states[bridge]
		m.states[bridge] = exists
		m.mu.Unlock()
		if !known || previous != exists {
			if exists {
				logging.Infof("OVS bridge %s exists; VNI %d synchronization is enabled", bridge, m.monitor.VNI)
			} else {
				logging.Infof("OVS bridge %s does not exist; bridge is ignored", bridge)
			}
		}
		if exists {
			m.activate(ctx, bridge, client)
		} else {
			m.deactivate(bridge)
		}
	}
}

func (m *Manager) activate(ctx context.Context, name string, client OpenVSwitch) {
	m.mu.Lock()
	if m.stopping || m.active[name].ovs != nil {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	if err := m.AnnounceIMET(ctx, event.IMETRoute{RD: m.monitor.RD, VNI: m.monitor.VNI, VTEP: m.monitor.VTEP}); err != nil {
		logging.Errorf("announce local EVPN Type-3 route for bridge %s: %v", name, err)
		return
	}

	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	monitorCtx, cancel := context.WithCancel(m.ctx)
	done := make(chan struct{})
	m.active[name] = managedBridge{name: name, ovs: client, cancel: cancel, done: done}
	routes := make([]event.EVPNRoute, 0, len(m.routes))
	for _, route := range m.routes {
		routes = append(routes, route)
	}
	imets := make([]event.IMETRoute, 0, len(m.imets))
	for _, imet := range m.imets {
		imets = append(imets, imet)
	}
	m.waitGroup.Add(1)
	m.mu.Unlock()

	for _, imet := range imets {
		if err := client.AddRemoteVTEP(ctx, imet); err != nil {
			logging.Errorf("restore EVPN Type-3 route on OVS bridge %s: %v", name, err)
		}
	}
	for _, route := range routes {
		if err := client.AddRemoteMAC(ctx, route); err != nil {
			logging.Errorf("restore EVPN route on OVS bridge %s: %v", name, err)
		}
	}
	go func() {
		defer m.waitGroup.Done()
		defer close(done)
		if err := MonitorLocalMACs(monitorCtx, client, m, m.monitor); err != nil && monitorCtx.Err() == nil {
			logging.Errorf("monitor OVS bridge %s: %v", name, err)
		}
	}()
}

func (m *Manager) deactivate(name string) {
	m.mu.Lock()
	bridge, exists := m.active[name]
	if exists {
		delete(m.active, name)
	}
	last := len(m.active) == 0
	m.mu.Unlock()
	if !exists {
		return
	}
	bridge.cancel()
	<-bridge.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if last {
		if err := m.withdrawLocalRoutes(ctx); err != nil {
			logging.Errorf("withdraw local EVPN Type-2 routes: %v", err)
		}
		if err := m.withdrawLocalIMET(ctx); err != nil {
			logging.Errorf("withdraw local EVPN Type-3 route: %v", err)
		}
	}
	if err := bridge.ovs.ClearRemoteState(ctx); err != nil {
		logging.Errorf("clear remote OVS state on bridge %s: %v", name, err)
	}
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
	if m.localIMET {
		return nil
	}
	if err := m.announcer.AnnounceIMET(ctx, route); err != nil {
		return err
	}
	m.localIMET = true
	return nil
}

func (m *Manager) WithdrawIMET(ctx context.Context, route event.IMETRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.localIMET {
		return nil
	}
	if err := m.announcer.WithdrawIMET(ctx, route); err != nil {
		return err
	}
	m.localIMET = false
	return nil
}

func (m *Manager) withdrawLocalRoutes(ctx context.Context) error {
	m.mu.Lock()
	routes := make([]event.EVPNRoute, 0, len(m.localRoutes))
	for _, route := range m.localRoutes {
		routes = append(routes, route)
	}
	m.mu.Unlock()
	var errs []error
	for _, route := range routes {
		if err := m.Withdraw(ctx, route); err != nil {
			errs = append(errs, fmt.Errorf("withdraw local MAC %s: %w", route.MAC, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) withdrawLocalIMET(ctx context.Context) error {
	route := event.IMETRoute{RD: m.monitor.RD, VNI: m.monitor.VNI, VTEP: m.monitor.VTEP}
	if err := m.WithdrawIMET(ctx, route); err != nil {
		return fmt.Errorf("withdraw local EVPN Type-3 route: %w", err)
	}
	return nil
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
	if err := m.withdrawLocalRoutes(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := m.withdrawLocalIMET(ctx); err != nil {
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
