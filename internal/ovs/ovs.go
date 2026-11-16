package ovs

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/evpn"
)

// FDBEntry describes one MAC learned by Open vSwitch.
type FDBEntry struct {
	Port string
	VLAN uint16
	MAC  string
}

// OpenVSwitch is the dataplane boundary used by Snout.
type OpenVSwitch interface {
	BridgeExists(context.Context) (bool, error)
	RemoveStalePorts(context.Context) error
	ClearRemoteState(context.Context) error
	AddRemoteMAC(context.Context, event.EVPNRoute) error
	DeleteRemoteMAC(context.Context, event.EVPNRoute) error
	AddRemoteVTEP(context.Context, event.IMETRoute) error
	DeleteRemoteVTEP(context.Context, event.IMETRoute) error
	LocalMACs(context.Context) ([]FDBEntry, error)
}

// Runner executes Open vSwitch command-line tools.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// Client implements OpenVSwitch for one bridge with ovs-vsctl and ovs-appctl.
// Remote routes are mapped to the bridge VLAN of their VNI; on a flat bridge
// the VLAN is 0 and tunnel ports are untagged.
type Client struct {
	bridge evpn.Bridge
	runner Runner

	mu      sync.Mutex
	routes  map[string]installedMAC
	members map[string]string
	tunnels map[string]*tunnelReferences
}

type installedMAC struct {
	port string
	vlan uint16
	mac  string
}

type tunnelReferences struct {
	macs int
	imet bool
}

func NewClient(bridge evpn.Bridge, runner Runner) (*Client, error) {
	if bridge.Name == "" {
		return nil, fmt.Errorf("OVS bridge is required")
	}
	if len(bridge.Segments) == 0 {
		return nil, fmt.Errorf("OVS bridge %s has no active VNI", bridge.Name)
	}
	if runner == nil {
		return nil, fmt.Errorf("OVS command runner is required")
	}
	return &Client{
		bridge:  bridge,
		runner:  runner,
		routes:  make(map[string]installedMAC),
		members: make(map[string]string),
		tunnels: make(map[string]*tunnelReferences),
	}, nil
}

func (c *Client) BridgeExists(ctx context.Context) (bool, error) {
	output, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "get", "Bridge", c.bridge.Name, "name")
	if err != nil {
		return false, fmt.Errorf("inspect OVS bridge %s: %w", c.bridge.Name, err)
	}
	return unquote(output) != "", nil
}

// ownedPorts returns the Snout-owned interfaces on the bridge and their
// snout-vni marker.
func (c *Client) ownedPorts(ctx context.Context) (map[string]string, error) {
	output, err := c.runner.Run(ctx, "ovs-vsctl", "list-ifaces", c.bridge.Name)
	if err != nil {
		return nil, fmt.Errorf("list interfaces on OVS bridge %s: %w", c.bridge.Name, err)
	}
	owned := make(map[string]string)
	var errs []error
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		isOwned, err := c.isOwned(ctx, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !isOwned {
			continue
		}
		vniOutput, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "get", "Interface", name, "external_ids:snout-vni")
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect OVS interface %s VNI: %w", name, err))
			owned[name] = ""
			continue
		}
		owned[name] = unquote(vniOutput)
	}
	return owned, errors.Join(errs...)
}

func (c *Client) isOwned(ctx context.Context, name string) (bool, error) {
	output, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "get", "Interface", name, "external_ids:snout-owned")
	if err != nil {
		return false, fmt.Errorf("inspect OVS interface %s ownership: %w", name, err)
	}
	return unquote(output) == "true", nil
}

// RemoveStalePorts deletes Snout-owned ports whose VNI is not active on the
// bridge, for example after a VNI was removed from the configuration.
func (c *Client) RemoveStalePorts(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	owned, err := c.ownedPorts(ctx)
	errs := []error{err}
	for port, vniText := range owned {
		if vni, parseErr := strconv.ParseUint(vniText, 10, 32); parseErr == nil {
			if _, active := c.bridge.SegmentByVNI(uint32(vni)); active {
				continue
			}
		}
		if c.tunnels[port] != nil {
			continue
		}
		if _, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", c.bridge.Name, port); err != nil {
			errs = append(errs, fmt.Errorf("delete stale VXLAN port %s: %w", port, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Client) ClearRemoteState(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, installed := range c.routes {
		if _, err := c.runner.Run(ctx, "ovs-appctl", "fdb/del", c.bridge.Name, strconv.Itoa(int(installed.vlan)), installed.mac); err != nil {
			errs = append(errs, fmt.Errorf("delete MAC %s from OVS FDB: %w", installed.mac, err))
		}
	}
	ports := make(map[string]struct{}, len(c.tunnels))
	for port := range c.tunnels {
		ports[port] = struct{}{}
	}
	owned, err := c.ownedPorts(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for port := range owned {
		ports[port] = struct{}{}
	}
	for port := range ports {
		if _, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", c.bridge.Name, port); err != nil {
			errs = append(errs, fmt.Errorf("delete VXLAN port %s: %w", port, err))
		}
	}
	c.routes = make(map[string]installedMAC)
	c.members = make(map[string]string)
	c.tunnels = make(map[string]*tunnelReferences)
	return errors.Join(errs...)
}

func (c *Client) segment(vni uint32) (evpn.Segment, error) {
	segment, ok := c.bridge.SegmentByVNI(vni)
	if !ok {
		return evpn.Segment{}, fmt.Errorf("VNI %d is not configured on OVS bridge %s", vni, c.bridge.Name)
	}
	return segment, nil
}

func (c *Client) AddRemoteMAC(ctx context.Context, route event.EVPNRoute) error {
	if err := validateRoute(route); err != nil {
		return err
	}
	segment, err := c.segment(route.VNI)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := routeKey(route)
	port := tunnelPort(route.VNI, route.VTEP)
	if old, ok := c.routes[key]; ok && old.port == port {
		return nil
	}

	if err := c.ensureTunnelPort(ctx, segment, route.VTEP, port); err != nil {
		return err
	}
	vlan := strconv.Itoa(int(segment.VLAN))
	if _, err := c.runner.Run(ctx, "ovs-appctl", "fdb/add", c.bridge.Name, port, vlan, route.MAC); err != nil {
		return fmt.Errorf("add MAC %s to OVS FDB: %w", route.MAC, err)
	}

	old, hadOld := c.routes[key]
	c.routes[key] = installedMAC{port: port, vlan: segment.VLAN, mac: route.MAC}
	c.tunnels[port].macs++
	if hadOld {
		return c.releaseMACPort(ctx, old.port)
	}
	return nil
}

func (c *Client) AddRemoteVTEP(ctx context.Context, route event.IMETRoute) error {
	if err := validateIMETRoute(route); err != nil {
		return err
	}
	segment, err := c.segment(route.VNI)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := imetKey(route)
	if _, exists := c.members[key]; exists {
		return nil
	}
	port := tunnelPort(route.VNI, route.VTEP)
	if err := c.ensureTunnelPort(ctx, segment, route.VTEP, port); err != nil {
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

	key := routeKey(route)
	installed, ok := c.routes[key]
	if !ok {
		return nil
	}
	if _, err := c.runner.Run(ctx, "ovs-appctl", "fdb/del", c.bridge.Name, strconv.Itoa(int(installed.vlan)), installed.mac); err != nil {
		return fmt.Errorf("delete MAC %s from OVS FDB: %w", installed.mac, err)
	}
	delete(c.routes, key)
	return c.releaseMACPort(ctx, installed.port)
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
	if _, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", c.bridge.Name, port); err != nil {
		return fmt.Errorf("delete unused VXLAN port %s: %w", port, err)
	}
	return nil
}

// tunnelSettings returns the ovs-vsctl operations that describe the desired
// state of a Snout tunnel port.
func (c *Client) tunnelSettings(segment evpn.Segment, vtep, port string, reconcile bool) []string {
	vni := strconv.FormatUint(uint64(segment.VNI), 10)
	args := []string{"set", "Interface", port, "type=vxlan", "options:remote_ip=" + vtep, "options:key=" + vni,
		"external_ids:snout-owned=true", "external_ids:snout-vni=" + vni, "external_ids:snout-vtep=" + vtep}
	if !c.bridge.Flat {
		args = append(args, "external_ids:snout-vlan="+strconv.Itoa(int(segment.VLAN)))
	}
	args = append(args, "--", "set", "Port", port, "protected=true")
	if !c.bridge.Flat {
		args = append(args, "tag="+strconv.Itoa(int(segment.VLAN)), "vlan_mode=access")
	} else if reconcile {
		args = append(args, "--", "clear", "Port", port, "tag", "vlan_mode",
			"--", "remove", "Interface", port, "external_ids", "snout-vlan")
	}
	return args
}

func (c *Client) ensureTunnelPort(ctx context.Context, segment evpn.Segment, vtep, port string) error {
	if c.tunnels[port] != nil {
		return nil
	}
	existingOutput, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "get", "Interface", port, "name")
	if err != nil {
		return fmt.Errorf("inspect VXLAN port %s: %w", port, err)
	}
	if unquote(existingOutput) != "" {
		owned, err := c.isOwned(ctx, port)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("refuse to replace non-Snout OVS interface %s", port)
		}
		// Adopt the port left by a previous run and correct its settings.
		if _, err := c.runner.Run(ctx, "ovs-vsctl", c.tunnelSettings(segment, vtep, port, true)...); err != nil {
			return fmt.Errorf("update VXLAN port %s: %w", port, err)
		}
		c.tunnels[port] = &tunnelReferences{}
		return nil
	}
	args := append([]string{"--may-exist", "add-port", c.bridge.Name, port, "--"}, c.tunnelSettings(segment, vtep, port, false)...)
	if _, err := c.runner.Run(ctx, "ovs-vsctl", args...); err != nil {
		return fmt.Errorf("create VXLAN port %s: %w", port, err)
	}
	c.tunnels[port] = &tunnelReferences{}
	return nil
}

// LocalMACs returns FDB entries learned on non-VXLAN, non-LOCAL ports with
// their VLAN. Mapping VLANs to VNIs is up to the caller.
func (c *Client) LocalMACs(ctx context.Context) ([]FDBEntry, error) {
	tunnelOutput, err := c.runner.Run(ctx, "ovs-vsctl", "--format=csv", "--data=bare", "--no-heading", "--columns=name,ofport", "find", "Interface", "type=vxlan")
	if err != nil {
		return nil, fmt.Errorf("list OVS VXLAN interfaces: %w", err)
	}
	tunnelPorts := parseTunnelPorts(tunnelOutput)

	fdbOutput, err := c.runner.Run(ctx, "ovs-appctl", "fdb/show", c.bridge.Name)
	if err != nil {
		return nil, fmt.Errorf("show OVS FDB: %w", err)
	}
	return parseFDB(fdbOutput, tunnelPorts), nil
}

func unquote(output []byte) string {
	return strings.Trim(strings.TrimSpace(string(output)), "\"")
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
