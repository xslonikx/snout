package ovs

import (
	"context"
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

// Manager periodically discovers VXLAN bridges, applies remote routes to all
// active bridges, and runs one local FDB monitor per active bridge.
type Manager struct {
	ctx          context.Context
	bridges      []string
	runner       Runner
	announcer    Announcer
	monitor      MonitorConfig
	scanInterval time.Duration

	mu        sync.Mutex
	active    map[string]managedBridge
	routes    map[string]event.EVPNRoute
	imets     map[string]event.IMETRoute
	states    map[string]bool
	localIMET bool
	waitGroup sync.WaitGroup
}

func NewManager(ctx context.Context, bridges []string, runner Runner, announcer Announcer, monitor MonitorConfig, scanInterval time.Duration) (*Manager, error) {
	if len(bridges) == 0 {
		return nil, fmt.Errorf("at least one OVS bridge is required")
	}
	if runner == nil || announcer == nil {
		return nil, fmt.Errorf("OVS runner and announcer are required")
	}
	if scanInterval <= 0 {
		return nil, fmt.Errorf("bridge scan interval must be positive")
	}
	return &Manager{
		ctx: ctx, bridges: append([]string(nil), bridges...), runner: runner, announcer: announcer,
		monitor: monitor, scanInterval: scanInterval, active: make(map[string]managedBridge),
		routes: make(map[string]event.EVPNRoute), imets: make(map[string]event.IMETRoute), states: make(map[string]bool),
	}, nil
}

func (m *Manager) Consume(value any) {
	switch routeEvent := value.(type) {
	case event.RouteEvent:
		m.consumeType2(routeEvent)
	case event.IMETEvent:
		m.consumeType3(routeEvent)
	}
}

func (m *Manager) consumeType2(routeEvent event.RouteEvent) {
	if sameVTEP(routeEvent.Route.VTEP, m.monitor.VTEP) || routeEvent.Route.VNI != m.monitor.VNI {
		return
	}

	m.mu.Lock()
	key := routeKey(routeEvent.Route)
	if routeEvent.Action == event.RouteDelete {
		delete(m.routes, key)
	} else if routeEvent.Action == event.RouteAdd {
		m.routes[key] = routeEvent.Route
	}
	clients := make([]OpenVSwitch, 0, len(m.active))
	for _, bridge := range m.active {
		clients = append(clients, bridge.ovs)
	}
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
	key := imetKey(imetEvent.Route)
	if imetEvent.Action == event.RouteDelete {
		delete(m.imets, key)
	} else if imetEvent.Action == event.RouteAdd {
		m.imets[key] = imetEvent.Route
	}
	clients := make([]OpenVSwitch, 0, len(m.active))
	for _, bridge := range m.active {
		clients = append(clients, bridge.ovs)
	}
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

func sameVTEP(left, right string) bool {
	leftIP := net.ParseIP(left)
	rightIP := net.ParseIP(right)
	return leftIP != nil && rightIP != nil && leftIP.Equal(rightIP)
}

func (m *Manager) Run(ctx context.Context) error {
	m.scan(ctx)
	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()
	defer m.stopAll()

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
		isVXLAN, err := client.IsVXLANBridge(ctx)
		if err != nil {
			logging.Errorf("inspect OVS bridge %s: %v", bridge, err)
			continue
		}

		m.mu.Lock()
		_, wasKnown := m.states[bridge]
		wasVXLAN := m.states[bridge]
		m.states[bridge] = isVXLAN
		m.mu.Unlock()
		if !wasKnown || wasVXLAN != isVXLAN {
			if isVXLAN {
				logging.Infof("OVS bridge %s has a VXLAN interface; synchronization is enabled", bridge)
			} else {
				logging.Infof("OVS bridge %s has no VXLAN interfaces; bridge is ignored", bridge)
			}
		}

		if isVXLAN {
			if err := client.EnableSplitHorizon(ctx); err != nil {
				logging.Errorf("enable split horizon on OVS bridge %s: %v", bridge, err)
				continue
			}
			m.activate(ctx, bridge, client)
		} else {
			if !m.deactivate(bridge) {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := client.ClearRemoteState(cleanupCtx); err != nil {
					logging.Errorf("clear inactive OVS bridge %s: %v", bridge, err)
				}
				cancel()
			}
		}
	}
}

func (m *Manager) activate(ctx context.Context, name string, client OpenVSwitch) {
	m.mu.Lock()
	if _, exists := m.active[name]; exists {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	if err := m.announceLocalIMET(ctx); err != nil {
		logging.Errorf("announce local EVPN Type-3 route for bridge %s: %v", name, err)
		return
	}

	m.mu.Lock()
	monitorCtx, cancel := context.WithCancel(ctx)
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
		if err := MonitorLocalMACs(monitorCtx, client, m.announcer, m.monitor); err != nil && monitorCtx.Err() == nil {
			logging.Errorf("monitor OVS bridge %s: %v", name, err)
		}
	}()
}

func (m *Manager) deactivate(name string) bool {
	m.mu.Lock()
	bridge, exists := m.active[name]
	if exists {
		delete(m.active, name)
	}
	m.mu.Unlock()
	if exists {
		bridge.cancel()
		<-bridge.done
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := bridge.ovs.ClearRemoteState(cleanupCtx); err != nil {
			logging.Errorf("clear remote OVS state on bridge %s: %v", name, err)
		}
		cancel()
	}
	m.withdrawLocalIMETIfUnused()
	return exists
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	bridges := make([]managedBridge, 0, len(m.active))
	for _, bridge := range m.active {
		bridges = append(bridges, bridge)
	}
	m.active = make(map[string]managedBridge)
	m.mu.Unlock()
	for _, bridge := range bridges {
		bridge.cancel()
	}
	m.waitGroup.Wait()
	for _, bridge := range bridges {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := bridge.ovs.ClearRemoteState(ctx); err != nil {
			logging.Errorf("clear remote OVS state on bridge %s: %v", bridge.name, err)
		}
		cancel()
	}
	m.withdrawLocalIMETIfUnused()
}

func (m *Manager) announceLocalIMET(ctx context.Context) error {
	m.mu.Lock()
	if m.localIMET {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	route := event.IMETRoute{RD: m.monitor.RD, VNI: m.monitor.VNI, VTEP: m.monitor.VTEP}
	if err := m.announcer.AnnounceIMET(ctx, route); err != nil {
		return err
	}
	m.mu.Lock()
	m.localIMET = true
	m.mu.Unlock()
	return nil
}

func (m *Manager) withdrawLocalIMETIfUnused() {
	m.mu.Lock()
	if len(m.active) != 0 || !m.localIMET {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	route := event.IMETRoute{RD: m.monitor.RD, VNI: m.monitor.VNI, VTEP: m.monitor.VTEP}
	if err := m.announcer.WithdrawIMET(ctx, route); err != nil {
		logging.Errorf("withdraw local EVPN Type-3 route: %v", err)
		return
	}
	m.mu.Lock()
	m.localIMET = false
	m.mu.Unlock()
}
