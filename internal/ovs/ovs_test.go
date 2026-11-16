package ovs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/evpn"
)

type command struct {
	name string
	args []string
}

type fakeRunner struct {
	commands []command
	outputs  map[string][]byte
	errors   map[string]error
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, command{name: name, args: append([]string(nil), args...)})
	key := name + " " + strings.Join(args, " ")
	return r.outputs[key], r.errors[key]
}

func (r *fakeRunner) ran(name string, args ...string) bool {
	for _, got := range r.commands {
		if got.name == name && reflect.DeepEqual(got.args, args) {
			return true
		}
	}
	return false
}

func segment(vlan uint16, vni uint32) evpn.Segment {
	return evpn.Segment{VLAN: vlan, VNI: vni, RD: fmt.Sprintf("172.31.0.11:%d", vni%65536), RouteTarget: fmt.Sprintf("65000:%d", vni)}
}

func flatBridge(t *testing.T, name string, vni uint32) evpn.Bridge {
	t.Helper()
	bridge, err := evpn.NewBridge(name, true, []evpn.Segment{segment(0, vni)})
	if err != nil {
		t.Fatal(err)
	}
	return bridge
}

func vlanBridge(t *testing.T, name string, segments ...evpn.Segment) evpn.Bridge {
	t.Helper()
	bridge, err := evpn.NewBridge(name, false, segments)
	if err != nil {
		t.Fatal(err)
	}
	return bridge
}

func newTestClient(t *testing.T, bridge evpn.Bridge, runner Runner) *Client {
	t.Helper()
	client, err := NewClient(bridge, runner)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient(evpn.Bridge{}, &fakeRunner{}); err == nil {
		t.Fatal("bridge without name was accepted")
	}
	if _, err := NewClient(evpn.Bridge{Name: "br0"}, &fakeRunner{}); err == nil {
		t.Fatal("bridge without segments was accepted")
	}
	if _, err := NewClient(flatBridge(t, "br0", 100), nil); err == nil {
		t.Fatal("nil runner was accepted")
	}
}

func TestFlatClientAddsAndDeletesRemoteMAC(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client := newTestClient(t, flatBridge(t, "br0", 100), runner)
	route := event.EVPNRoute{VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.20"}
	port := tunnelPort(route.VNI, route.VTEP)

	if err := client.AddRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}

	want := []command{
		{name: "ovs-vsctl", args: []string{"--if-exists", "get", "Interface", port, "name"}},
		{name: "ovs-vsctl", args: []string{"--may-exist", "add-port", "br0", port, "--", "set", "Interface", port, "type=vxlan", "options:remote_ip=192.0.2.20", "options:key=100", "external_ids:snout-owned=true", "external_ids:snout-vni=100", "external_ids:snout-vtep=192.0.2.20", "--", "set", "Port", port, "protected=true"}},
		{name: "ovs-appctl", args: []string{"fdb/add", "br0", port, "0", route.MAC}},
		{name: "ovs-appctl", args: []string{"fdb/del", "br0", "0", route.MAC}},
		{name: "ovs-vsctl", args: []string{"--if-exists", "del-port", "br0", port}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands mismatch\ngot:  %#v\nwant: %#v", runner.commands, want)
	}
}

func TestVLANClientUsesMappedVLANAndTagsTunnel(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client := newTestClient(t, vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200)), runner)
	route := event.EVPNRoute{VNI: 10200, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.20"}
	port := tunnelPort(route.VNI, route.VTEP)

	if err := client.AddRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	want := []command{
		{name: "ovs-vsctl", args: []string{"--if-exists", "get", "Interface", port, "name"}},
		{name: "ovs-vsctl", args: []string{"--may-exist", "add-port", "br0", port, "--", "set", "Interface", port, "type=vxlan", "options:remote_ip=192.0.2.20", "options:key=10200", "external_ids:snout-owned=true", "external_ids:snout-vni=10200", "external_ids:snout-vtep=192.0.2.20", "external_ids:snout-vlan=200", "--", "set", "Port", port, "protected=true", "tag=200", "vlan_mode=access"}},
		{name: "ovs-appctl", args: []string{"fdb/add", "br0", port, "200", route.MAC}},
		{name: "ovs-appctl", args: []string{"fdb/del", "br0", "200", route.MAC}},
		{name: "ovs-vsctl", args: []string{"--if-exists", "del-port", "br0", port}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands mismatch\ngot:  %#v\nwant: %#v", runner.commands, want)
	}
}

func TestUnknownVNIIsRejectedWithoutTouchingOVS(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client := newTestClient(t, vlanBridge(t, "br0", segment(100, 10100)), runner)
	if err := client.AddRemoteMAC(context.Background(), event.EVPNRoute{VNI: 999, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.20"}); err == nil {
		t.Fatal("route for an unknown VNI was accepted")
	}
	if err := client.AddRemoteVTEP(context.Background(), event.IMETRoute{VNI: 999, VTEP: "192.0.2.20"}); err == nil {
		t.Fatal("IMET route for an unknown VNI was accepted")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("unknown VNI touched OVS: %#v", runner.commands)
	}
}

func TestSameMACInTwoVNIsIsIndependent(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client := newTestClient(t, vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200)), runner)
	first := event.EVPNRoute{VNI: 10100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.20"}
	second := event.EVPNRoute{VNI: 10200, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.20"}
	for _, route := range []event.EVPNRoute{first, second} {
		if err := client.AddRemoteMAC(context.Background(), route); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.routes) != 2 || len(client.tunnels) != 2 {
		t.Fatalf("got %d routes and %d tunnels, want 2 and 2", len(client.routes), len(client.tunnels))
	}
	if err := client.DeleteRemoteMAC(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if !runner.ran("ovs-appctl", "fdb/del", "br0", "100", first.MAC) {
		t.Fatal("MAC was not removed from VLAN 100")
	}
	if runner.ran("ovs-appctl", "fdb/del", "br0", "200", first.MAC) {
		t.Fatal("MAC was removed from VLAN 200")
	}
	if _, exists := client.routes[routeKey(second)]; !exists {
		t.Fatal("route in the other VNI was removed")
	}
}

func TestSameVTEPInTwoVNIsGetsSeparatePorts(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client := newTestClient(t, vlanBridge(t, "br0", segment(100, 10100), segment(200, 10200)), runner)
	for _, vni := range []uint32{10100, 10200} {
		if err := client.AddRemoteVTEP(context.Background(), event.IMETRoute{VNI: vni, VTEP: "192.0.2.20"}); err != nil {
			t.Fatal(err)
		}
	}
	first, second := tunnelPort(10100, "192.0.2.20"), tunnelPort(10200, "192.0.2.20")
	if first == second {
		t.Fatal("tunnel ports of different VNIs must differ")
	}
	if client.tunnels[first] == nil || client.tunnels[second] == nil {
		t.Fatalf("missing tunnel ports: %#v", client.tunnels)
	}
}

func TestAdoptedPortIsReconciled(t *testing.T) {
	route := event.IMETRoute{VNI: 10100, VTEP: "192.0.2.20"}
	port := tunnelPort(route.VNI, route.VTEP)
	outputs := map[string][]byte{
		"ovs-vsctl --if-exists get Interface " + port + " name":                     []byte("\"" + port + "\"\n"),
		"ovs-vsctl --if-exists get Interface " + port + " external_ids:snout-owned": []byte("\"true\"\n"),
	}

	runner := &fakeRunner{outputs: outputs}
	client := newTestClient(t, vlanBridge(t, "br0", segment(100, 10100)), runner)
	if err := client.AddRemoteVTEP(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if !runner.ran("ovs-vsctl", "set", "Interface", port, "type=vxlan", "options:remote_ip=192.0.2.20", "options:key=10100", "external_ids:snout-owned=true", "external_ids:snout-vni=10100", "external_ids:snout-vtep=192.0.2.20", "external_ids:snout-vlan=100", "--", "set", "Port", port, "protected=true", "tag=100", "vlan_mode=access") {
		t.Fatalf("VLAN-aware port was not reconciled: %#v", runner.commands)
	}

	flatRoute := event.IMETRoute{VNI: 100, VTEP: "192.0.2.20"}
	flatPort := tunnelPort(flatRoute.VNI, flatRoute.VTEP)
	runner = &fakeRunner{outputs: map[string][]byte{
		"ovs-vsctl --if-exists get Interface " + flatPort + " name":                     []byte(flatPort + "\n"),
		"ovs-vsctl --if-exists get Interface " + flatPort + " external_ids:snout-owned": []byte("true\n"),
	}}
	client = newTestClient(t, flatBridge(t, "br0", 100), runner)
	if err := client.AddRemoteVTEP(context.Background(), flatRoute); err != nil {
		t.Fatal(err)
	}
	if !runner.ran("ovs-vsctl", "set", "Interface", flatPort, "type=vxlan", "options:remote_ip=192.0.2.20", "options:key=100", "external_ids:snout-owned=true", "external_ids:snout-vni=100", "external_ids:snout-vtep=192.0.2.20", "--", "set", "Port", flatPort, "protected=true", "--", "clear", "Port", flatPort, "tag", "vlan_mode", "--", "remove", "Interface", flatPort, "external_ids", "snout-vlan") {
		t.Fatalf("flat port was not reconciled: %#v", runner.commands)
	}
	for _, got := range runner.commands {
		if len(got.args) > 1 && got.args[1] == "add-port" {
			t.Fatal("adopted port was re-created")
		}
	}
}

func TestIMETRetainsTunnelPortAfterType2Withdrawal(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client := newTestClient(t, flatBridge(t, "br0", 100), runner)
	imet := event.IMETRoute{VNI: 100, VTEP: "192.0.2.20"}
	route := event.EVPNRoute{VNI: 100, MAC: "02:00:00:00:00:01", VTEP: imet.VTEP}
	if err := client.AddRemoteVTEP(context.Background(), imet); err != nil {
		t.Fatal(err)
	}
	if err := client.AddRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if len(client.tunnels) != 1 {
		t.Fatal("Type-3 membership must retain the tunnel after Type-2 withdrawal")
	}
	if err := client.DeleteRemoteVTEP(context.Background(), imet); err != nil {
		t.Fatal(err)
	}
	if len(client.tunnels) != 0 {
		t.Fatal("tunnel must be removed after both Type-2 and Type-3 references disappear")
	}
	deleteCommands := 0
	for _, command := range runner.commands {
		if command.name == "ovs-vsctl" && len(command.args) > 1 && command.args[0] == "--if-exists" && command.args[1] == "del-port" {
			deleteCommands++
		}
	}
	if deleteCommands != 1 {
		t.Fatalf("got %d tunnel deletion commands, want 1", deleteCommands)
	}
}

func TestTunnelCreationDoesNotReplaceUserInterface(t *testing.T) {
	route := event.IMETRoute{VNI: 100, VTEP: "192.0.2.20"}
	port := tunnelPort(route.VNI, route.VTEP)
	runner := &fakeRunner{outputs: map[string][]byte{
		"ovs-vsctl --if-exists get Interface " + port + " name": []byte("\"" + port + "\"\n"),
	}}
	client := newTestClient(t, flatBridge(t, "br0", 100), runner)
	if err := client.AddRemoteVTEP(context.Background(), route); err == nil {
		t.Fatal("user-owned interface collision was not rejected")
	}
	for _, command := range runner.commands {
		if len(command.args) > 0 && (command.args[0] == "set" || (len(command.args) > 1 && command.args[1] == "add-port")) {
			t.Fatal("user interface was modified")
		}
	}
}

func TestType2RetainsTunnelPortAfterIMETWithdrawal(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client := newTestClient(t, flatBridge(t, "br0", 100), runner)
	imet := event.IMETRoute{VNI: 100, VTEP: "192.0.2.20"}
	route := event.EVPNRoute{VNI: 100, MAC: "02:00:00:00:00:01", VTEP: imet.VTEP}
	if err := client.AddRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if err := client.AddRemoteVTEP(context.Background(), imet); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteRemoteVTEP(context.Background(), imet); err != nil {
		t.Fatal(err)
	}
	if len(client.tunnels) != 1 {
		t.Fatal("Type-2 route must retain the tunnel after Type-3 withdrawal")
	}
	if err := client.DeleteRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if len(client.tunnels) != 0 {
		t.Fatal("tunnel must be removed after both references disappear")
	}
}

func TestDeleteUnknownRemoteMACIsIdempotent(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client := newTestClient(t, flatBridge(t, "br0", 100), runner)
	route := event.EVPNRoute{VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.20"}
	if err := client.DeleteRemoteMAC(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("unknown MAC deletion touched OVS: %#v", runner.commands)
	}
}

func TestBridgeExists(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"ovs-vsctl --if-exists get Bridge br0 name": []byte("\"br0\"\n"),
	}}
	client := newTestClient(t, flatBridge(t, "br0", 100), runner)
	exists, err := client.BridgeExists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("existing bridge was not detected")
	}
}

func TestMissingBridgeDoesNotExist(t *testing.T) {
	client := newTestClient(t, flatBridge(t, "br0", 100), &fakeRunner{outputs: make(map[string][]byte)})
	exists, err := client.BridgeExists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("missing bridge was detected")
	}
}

func ownedOutputs(owned map[string]string, user ...string) map[string][]byte {
	names := make([]string, 0, len(owned)+len(user))
	outputs := make(map[string][]byte)
	for name, vni := range owned {
		names = append(names, name)
		outputs["ovs-vsctl --if-exists get Interface "+name+" external_ids:snout-owned"] = []byte("\"true\"\n")
		outputs["ovs-vsctl --if-exists get Interface "+name+" external_ids:snout-vni"] = []byte("\"" + vni + "\"\n")
	}
	names = append(names, user...)
	outputs["ovs-vsctl list-ifaces br0"] = []byte(strings.Join(names, "\n") + "\n")
	return outputs
}

func TestClearRemoteStateDeletesOnlyOwnedPorts(t *testing.T) {
	runner := &fakeRunner{outputs: ownedOutputs(map[string]string{"sn-owned": "100"}, "vxlan-user", "tap0")}
	client := newTestClient(t, flatBridge(t, "br0", 100), runner)
	if err := client.ClearRemoteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.ran("ovs-vsctl", "--if-exists", "del-port", "br0", "vxlan-user") || runner.ran("ovs-vsctl", "--if-exists", "del-port", "br0", "tap0") {
		t.Fatal("user port was deleted")
	}
	if !runner.ran("ovs-vsctl", "--if-exists", "del-port", "br0", "sn-owned") {
		t.Fatalf("owned port was not deleted: %#v", runner.commands)
	}
}

func TestClearRemoteStateUsesInstalledVLANAndContinuesAfterFDBError(t *testing.T) {
	deleteFDB := "ovs-appctl fdb/del br0 100 02:00:00:00:00:01"
	runner := &fakeRunner{
		outputs: ownedOutputs(map[string]string{"sn-owned": "10100"}),
		errors:  map[string]error{deleteFDB: errors.New("FDB failure")},
	}
	client := newTestClient(t, vlanBridge(t, "br0", segment(100, 10100)), runner)
	route := event.EVPNRoute{VNI: 10100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.20"}
	client.routes[routeKey(route)] = installedMAC{port: "sn-owned", vlan: 100, mac: route.MAC}
	if err := client.ClearRemoteState(context.Background()); err == nil {
		t.Fatal("FDB error was not returned")
	}
	if !runner.ran("ovs-appctl", "fdb/del", "br0", "100", route.MAC) {
		t.Fatal("FDB entry was not deleted from its VLAN")
	}
	want := command{name: "ovs-vsctl", args: []string{"--if-exists", "del-port", "br0", "sn-owned"}}
	if got := runner.commands[len(runner.commands)-1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup stopped before port deletion: got %#v", got)
	}
	if len(client.routes) != 0 || len(client.tunnels) != 0 {
		t.Fatal("state was not reset")
	}
}

func TestRemoveStalePortsDeletesOnlyInactiveOwnedPorts(t *testing.T) {
	runner := &fakeRunner{outputs: ownedOutputs(map[string]string{"sn-active": "10100", "sn-stale": "10300", "sn-broken": ""}, "tap0")}
	client := newTestClient(t, vlanBridge(t, "br0", segment(100, 10100)), runner)
	if err := client.RemoveStalePorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runner.ran("ovs-vsctl", "--if-exists", "del-port", "br0", "sn-stale") || !runner.ran("ovs-vsctl", "--if-exists", "del-port", "br0", "sn-broken") {
		t.Fatalf("stale ports were not deleted: %#v", runner.commands)
	}
	if runner.ran("ovs-vsctl", "--if-exists", "del-port", "br0", "sn-active") || runner.ran("ovs-vsctl", "--if-exists", "del-port", "br0", "tap0") {
		t.Fatal("active or user port was deleted")
	}
}

func TestLocalMACsExcludesTunnelAndLocalEntries(t *testing.T) {
	find := "ovs-vsctl --format=csv --data=bare --no-heading --columns=name,ofport find Interface type=vxlan"
	show := "ovs-appctl fdb/show br0"
	runner := &fakeRunner{outputs: map[string][]byte{
		find: []byte("sn-tunnel,7\n"),
		show: []byte(" port  VLAN  MAC                Age\n1 100 02:00:00:00:00:01 1\n7 100 02:00:00:00:00:02 0\nLOCAL 0 02:00:00:00:00:03 0\n2 200 02:00:00:00:00:04 3\n"),
	}}
	client := newTestClient(t, vlanBridge(t, "br0", segment(100, 10100)), runner)
	entries, err := client.LocalMACs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []FDBEntry{{Port: "1", VLAN: 100, MAC: "02:00:00:00:00:01"}, {Port: "2", VLAN: 200, MAC: "02:00:00:00:00:04"}}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries mismatch: got %#v, want %#v", entries, want)
	}
}

func TestTunnelPortFitsLinuxInterfaceName(t *testing.T) {
	port := tunnelPort(16777215, "2001:db8::1")
	if len(port) > 15 {
		t.Fatalf("tunnel port %q is %d characters", port, len(port))
	}
	if !strings.HasPrefix(port, "sn-") {
		t.Fatalf("unexpected tunnel port %q", port)
	}
}
