package ovs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/logging"
)

type managedBridge struct {
	ovs    OpenVSwitch
	cancel context.CancelFunc
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
	states    map[string]bool
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
		routes: make(map[string]event.EVPNRoute), states: make(map[string]bool),
	}, nil
}

func (m *Manager) Consume(value any) {
	routeEvent, ok := value.(event.RouteEvent)
	if !ok || routeEvent.Route.VTEP == m.monitor.VTEP || routeEvent.Route.VNI != m.monitor.VNI {
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
			m.activate(ctx, bridge, client)
		} else {
			m.deactivate(bridge)
		}
	}
}

func (m *Manager) activate(ctx context.Context, name string, client OpenVSwitch) {
	m.mu.Lock()
	if _, exists := m.active[name]; exists {
		m.mu.Unlock()
		return
	}
	monitorCtx, cancel := context.WithCancel(ctx)
	m.active[name] = managedBridge{ovs: client, cancel: cancel}
	routes := make([]event.EVPNRoute, 0, len(m.routes))
	for _, route := range m.routes {
		routes = append(routes, route)
	}
	m.waitGroup.Add(1)
	m.mu.Unlock()

	for _, route := range routes {
		if err := client.AddRemoteMAC(ctx, route); err != nil {
			logging.Errorf("restore EVPN route on OVS bridge %s: %v", name, err)
		}
	}
	go func() {
		defer m.waitGroup.Done()
		if err := MonitorLocalMACs(monitorCtx, client, m.announcer, m.monitor); err != nil && monitorCtx.Err() == nil {
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
	m.mu.Unlock()
	if exists {
		bridge.cancel()
	}
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
}
