package ovs

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/xslonikx/snout/internal/event"
)

// FDBEntry describes one MAC learned by Open vSwitch.
type FDBEntry struct {
	Port string
	VLAN uint16
	MAC  string
}

// OpenVSwitch is the dataplane boundary used by Snout.
type OpenVSwitch interface {
	IsVXLANBridge(context.Context) (bool, error)
	EnableSplitHorizon(context.Context) error
	ClearRemoteState(context.Context) error
	AddRemoteMAC(context.Context, event.EVPNRoute) error
	DeleteRemoteMAC(context.Context, event.EVPNRoute) error
	AddRemoteVTEP(context.Context, event.IMETRoute) error
	DeleteRemoteVTEP(context.Context, event.IMETRoute) error
	LocalMACs(context.Context) ([]FDBEntry, error)
}

func (c *Client) IsVXLANBridge(ctx context.Context) (bool, error) {
	output, err := c.runner.Run(ctx, "ovs-vsctl", "list-ifaces", c.bridge)
	if err != nil {
		return false, fmt.Errorf("list interfaces on OVS bridge %s: %w", c.bridge, err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "sn-") {
			continue
		}
		interfaceType, err := c.runner.Run(ctx, "ovs-vsctl", "get", "Interface", name, "type")
		if err != nil {
			return false, fmt.Errorf("get OVS interface %s type: %w", name, err)
		}
		if strings.EqualFold(strings.Trim(strings.TrimSpace(string(interfaceType)), "\""), "vxlan") {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) ClearRemoteState(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	for key := range c.routes {
		separator := strings.Index(key, "/")
		if separator == -1 {
			continue
		}
		mac := key[separator+1:]
		if _, err := c.runner.Run(ctx, "ovs-appctl", "fdb/del", c.bridge, "0", mac); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete MAC %s from OVS FDB: %w", mac, err)
		}
	}
	ports := make(map[string]struct{}, len(c.tunnels))
	for port := range c.tunnels {
		ports[port] = struct{}{}
	}
	output, err := c.runner.Run(ctx, "ovs-vsctl", "list-ifaces", c.bridge)
	if err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("list interfaces on OVS bridge %s: %w", c.bridge, err)
		}
	} else {
		for _, line := range strings.Split(string(output), "\n") {
			name := strings.TrimSpace(line)
			if strings.HasPrefix(name, "sn-") {
				ports[name] = struct{}{}
			}
		}
	}
	for port := range ports {
		if _, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", c.bridge, port); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete VXLAN port %s: %w", port, err)
		}
	}
	c.routes = make(map[string]string)
	c.members = make(map[string]string)
	c.tunnels = make(map[string]*tunnelReferences)
	return firstErr
}

func (c *Client) EnableSplitHorizon(ctx context.Context) error {
	output, err := c.runner.Run(ctx, "ovs-vsctl", "list-ifaces", c.bridge)
	if err != nil {
		return fmt.Errorf("list interfaces on OVS bridge %s: %w", c.bridge, err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		interfaceType, err := c.runner.Run(ctx, "ovs-vsctl", "get", "Interface", name, "type")
		if err != nil {
			return fmt.Errorf("get OVS interface %s type: %w", name, err)
		}
		if !strings.EqualFold(strings.Trim(strings.TrimSpace(string(interfaceType)), "\""), "vxlan") {
			continue
		}
		if _, err := c.runner.Run(ctx, "ovs-vsctl", "set", "Port", name, "protected=true"); err != nil {
			return fmt.Errorf("protect OVS VXLAN port %s: %w", name, err)
		}
	}
	return nil
}

// Runner executes Open vSwitch command-line tools.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// Client implements OpenVSwitch with ovs-vsctl and ovs-appctl.
type Client struct {
	bridge string
	runner Runner

	mu      sync.Mutex
	routes  map[string]string
	members map[string]string
	tunnels map[string]*tunnelReferences
}

type tunnelReferences struct {
	macs int
	imet bool
}

func NewClient(bridge string, runner Runner) (*Client, error) {
	if bridge == "" {
		return nil, fmt.Errorf("OVS bridge is required")
	}
	if runner == nil {
		return nil, fmt.Errorf("OVS command runner is required")
	}

	return &Client{
		bridge:  bridge,
		runner:  runner,
		routes:  make(map[string]string),
		members: make(map[string]string),
		tunnels: make(map[string]*tunnelReferences),
	}, nil
}

func (c *Client) AddRemoteMAC(ctx context.Context, route event.EVPNRoute) error {
	if err := validateRoute(route); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := routeKey(route)
	port := tunnelPort(route.VNI, route.VTEP)
	if oldPort, ok := c.routes[key]; ok && oldPort == port {
		return nil
	}

	if err := c.ensureTunnelPort(ctx, route.VNI, route.VTEP, port); err != nil {
		return err
	}
	if _, err := c.runner.Run(ctx, "ovs-appctl", "fdb/add", c.bridge, port, "0", route.MAC); err != nil {
		return fmt.Errorf("add MAC %s to OVS FDB: %w", route.MAC, err)
	}

	oldPort, hadOldPort := c.routes[key]
	c.routes[key] = port
	c.tunnels[port].macs++
	if hadOldPort {
		return c.releaseMACPort(ctx, oldPort)
	}
	return nil
}

func (c *Client) AddRemoteVTEP(ctx context.Context, route event.IMETRoute) error {
	if err := validateIMETRoute(route); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := imetKey(route)
	if _, exists := c.members[key]; exists {
		return nil
	}
	port := tunnelPort(route.VNI, route.VTEP)
	if err := c.ensureTunnelPort(ctx, route.VNI, route.VTEP, port); err != nil {
		return err
	}
	c.members[key] = port
	c.tunnels[port].imet = true
	return nil
}

func (c *Client) DeleteRemoteVTEP(ctx context.Context, route event.IMETRoute) error {
	if err := validateIMETRoute(route); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := imetKey(route)
	port, exists := c.members[key]
	if !exists {
		return nil
	}
	delete(c.members, key)
	c.tunnels[port].imet = false
	return c.cleanupPort(ctx, port)
}

func (c *Client) DeleteRemoteMAC(ctx context.Context, route event.EVPNRoute) error {
	if err := validateRoute(route); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.runner.Run(ctx, "ovs-appctl", "fdb/del", c.bridge, "0", route.MAC); err != nil {
		return fmt.Errorf("delete MAC %s from OVS FDB: %w", route.MAC, err)
	}

	key := routeKey(route)
	port, ok := c.routes[key]
	if !ok {
		return nil
	}
	delete(c.routes, key)
	return c.releaseMACPort(ctx, port)
}

func (c *Client) releaseMACPort(ctx context.Context, port string) error {
	references := c.tunnels[port]
	if references == nil {
		return nil
	}
	if references.macs > 0 {
		references.macs--
	}
	return c.cleanupPort(ctx, port)
}

func (c *Client) cleanupPort(ctx context.Context, port string) error {
	references := c.tunnels[port]
	if references == nil {
		return nil
	}
	if references.macs > 0 || references.imet {
		return nil
	}
	delete(c.tunnels, port)
	if _, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", c.bridge, port); err != nil {
		return fmt.Errorf("delete unused VXLAN port %s: %w", port, err)
	}
	return nil
}

func (c *Client) ensureTunnelPort(ctx context.Context, vni uint32, vtep, port string) error {
	if c.tunnels[port] != nil {
		return nil
	}
	if _, err := c.runner.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", c.bridge, port, "--", "set", "Interface", port, "type=vxlan", "options:remote_ip="+vtep, "options:key="+strconv.FormatUint(uint64(vni), 10), "--", "set", "Port", port, "protected=true"); err != nil {
		return fmt.Errorf("create VXLAN port %s: %w", port, err)
	}
	c.tunnels[port] = &tunnelReferences{}
	return nil
}

func (c *Client) LocalMACs(ctx context.Context) ([]FDBEntry, error) {
	tunnelOutput, err := c.runner.Run(ctx, "ovs-vsctl", "--format=csv", "--data=bare", "--no-heading", "--columns=name,ofport", "find", "Interface", "type=vxlan")
	if err != nil {
		return nil, fmt.Errorf("list OVS VXLAN interfaces: %w", err)
	}
	tunnelPorts := parseTunnelPorts(tunnelOutput)

	fdbOutput, err := c.runner.Run(ctx, "ovs-appctl", "fdb/show", c.bridge)
	if err != nil {
		return nil, fmt.Errorf("show OVS FDB: %w", err)
	}
	return parseFDB(fdbOutput, tunnelPorts), nil
}

func parseTunnelPorts(output []byte) map[string]struct{} {
	ports := make(map[string]struct{})
	reader := csv.NewReader(strings.NewReader(string(output)))
	reader.TrimLeadingSpace = true
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(record) != 2 {
			continue
		}
		name := strings.TrimSpace(record[0])
		ofport := strings.TrimSpace(record[1])
		if name != "" {
			ports[name] = struct{}{}
		}
		if ofport != "" {
			ports[ofport] = struct{}{}
		}
	}
	return ports
}

func parseFDB(output []byte, tunnelPorts map[string]struct{}) []FDBEntry {
	var entries []FDBEntry
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.EqualFold(fields[0], "port") {
			continue
		}
		if strings.EqualFold(fields[0], "LOCAL") {
			continue
		}
		if _, tunnel := tunnelPorts[fields[0]]; tunnel {
			continue
		}
		vlan, err := strconv.ParseUint(fields[1], 10, 16)
		if err != nil {
			continue
		}
		mac, err := net.ParseMAC(fields[2])
		if err != nil {
			continue
		}
		entries = append(entries, FDBEntry{Port: fields[0], VLAN: uint16(vlan), MAC: strings.ToLower(mac.String())})
	}
	return entries
}

func validateRoute(route event.EVPNRoute) error {
	if route.VNI == 0 || route.VTEP == "" || route.MAC == "" {
		return fmt.Errorf("route must contain VNI, VTEP, and MAC")
	}
	if net.ParseIP(route.VTEP) == nil {
		return fmt.Errorf("invalid VTEP address %q", route.VTEP)
	}
	if _, err := net.ParseMAC(route.MAC); err != nil {
		return fmt.Errorf("invalid MAC address %q: %w", route.MAC, err)
	}
	return nil
}

func validateIMETRoute(route event.IMETRoute) error {
	if route.VNI == 0 || net.ParseIP(route.VTEP) == nil {
		return fmt.Errorf("IMET route must contain a valid VNI and VTEP")
	}
	return nil
}

func routeKey(route event.EVPNRoute) string {
	return fmt.Sprintf("%d/%s", route.VNI, strings.ToLower(route.MAC))
}

func imetKey(route event.IMETRoute) string {
	return fmt.Sprintf("%d/%s", route.VNI, route.VTEP)
}

func tunnelPort(vni uint32, vtep string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", vni, vtep)))
	return fmt.Sprintf("sn-%x", sum[:6])
}
