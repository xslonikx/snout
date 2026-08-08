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
	AddRemoteMAC(context.Context, event.EVPNRoute) error
	DeleteRemoteMAC(context.Context, event.EVPNRoute) error
	LocalMACs(context.Context) ([]FDBEntry, error)
}

func (c *Client) IsVXLANBridge(ctx context.Context) (bool, error) {
	output, err := c.runner.Run(ctx, "ovs-vsctl", "list-ifaces", c.bridge)
	if err != nil {
		return false, fmt.Errorf("list interfaces on OVS bridge %s: %w", c.bridge, err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
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

// Runner executes Open vSwitch command-line tools.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// Client implements OpenVSwitch with ovs-vsctl and ovs-appctl.
type Client struct {
	bridge string
	runner Runner

	mu        sync.Mutex
	routes    map[string]string
	portUsers map[string]int
}

func NewClient(bridge string, runner Runner) (*Client, error) {
	if bridge == "" {
		return nil, fmt.Errorf("OVS bridge is required")
	}
	if runner == nil {
		return nil, fmt.Errorf("OVS command runner is required")
	}

	return &Client{
		bridge:    bridge,
		runner:    runner,
		routes:    make(map[string]string),
		portUsers: make(map[string]int),
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

	if _, err := c.runner.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", c.bridge, port, "--", "set", "Interface", port, "type=vxlan", "options:remote_ip="+route.VTEP, "options:key="+strconv.FormatUint(uint64(route.VNI), 10)); err != nil {
		return fmt.Errorf("create VXLAN port %s: %w", port, err)
	}
	if _, err := c.runner.Run(ctx, "ovs-appctl", "fdb/add", c.bridge, port, "0", route.MAC); err != nil {
		return fmt.Errorf("add MAC %s to OVS FDB: %w", route.MAC, err)
	}

	oldPort, hadOldPort := c.routes[key]
	c.routes[key] = port
	c.portUsers[port]++
	if hadOldPort {
		return c.releasePort(ctx, oldPort)
	}
	return nil
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
	return c.releasePort(ctx, port)
}

func (c *Client) releasePort(ctx context.Context, port string) error {
	c.portUsers[port]--
	if c.portUsers[port] > 0 {
		return nil
	}
	delete(c.portUsers, port)
	if _, err := c.runner.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", c.bridge, port); err != nil {
		return fmt.Errorf("delete unused VXLAN port %s: %w", port, err)
	}
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

func routeKey(route event.EVPNRoute) string {
	return fmt.Sprintf("%d/%s", route.VNI, strings.ToLower(route.MAC))
}

func tunnelPort(vni uint32, vtep string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", vni, vtep)))
	return fmt.Sprintf("sn-%x", sum[:6])
}
