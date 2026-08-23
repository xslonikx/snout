package ovs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/xslonikx/snout/internal/event"
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

func TestClientAddsAndDeletesRemoteMAC(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
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

func TestIMETRetainsTunnelPortAfterType2Withdrawal(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
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
		"ovs-vsctl --if-exists get Interface " + port + " name":         []byte("\"" + port + "\"\n"),
		"ovs-vsctl get Interface " + port + " external_ids:snout-owned": []byte("[]\n"),
	}}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AddRemoteVTEP(context.Background(), route); err == nil {
		t.Fatal("user-owned interface collision was not rejected")
	}
	for _, command := range runner.commands {
		if len(command.args) > 1 && command.args[1] == "add-port" {
			t.Fatal("user interface was modified")
		}
	}
}

func TestType2RetainsTunnelPortAfterIMETWithdrawal(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
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
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
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
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}

	exists, err := client.BridgeExists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("existing bridge was not detected")
	}
}

func TestMissingBridgeDoesNotExist(t *testing.T) {
	runner := &fakeRunner{outputs: make(map[string][]byte)}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}

	exists, err := client.BridgeExists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("missing bridge was detected")
	}
}

func TestClearRemoteStateDeletesOnlyOwnedPorts(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"ovs-vsctl list-ifaces br0":                                   []byte("sn-owned\nvxlan-user\n"),
		"ovs-vsctl get Interface sn-owned external_ids:snout-owned":   []byte("true\n"),
		"ovs-vsctl get Interface vxlan-user external_ids:snout-owned": []byte("[]\n"),
	}}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ClearRemoteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, got := range runner.commands {
		if reflect.DeepEqual(got.args, []string{"--if-exists", "del-port", "br0", "vxlan-user"}) {
			t.Fatal("user VXLAN port was deleted")
		}
	}
	want := command{name: "ovs-vsctl", args: []string{"--if-exists", "del-port", "br0", "sn-owned"}}
	if got := runner.commands[len(runner.commands)-1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("owned port deletion mismatch: got %#v, want %#v", got, want)
	}
}

func TestClearRemoteStateContinuesAfterFDBError(t *testing.T) {
	deleteFDB := "ovs-appctl fdb/del br0 0 02:00:00:00:00:01"
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"ovs-vsctl list-ifaces br0":                                 []byte("sn-owned\n"),
			"ovs-vsctl get Interface sn-owned external_ids:snout-owned": []byte("true\n"),
		},
		errors: map[string]error{deleteFDB: errors.New("FDB failure")},
	}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
	route := event.EVPNRoute{VNI: 100, MAC: "02:00:00:00:00:01", VTEP: "192.0.2.20"}
	client.routes[routeKey(route)] = "sn-owned"
	if err := client.ClearRemoteState(context.Background()); err == nil {
		t.Fatal("FDB error was not returned")
	}
	want := command{name: "ovs-vsctl", args: []string{"--if-exists", "del-port", "br0", "sn-owned"}}
	if got := runner.commands[len(runner.commands)-1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup stopped before port deletion: got %#v", got)
	}
}

func TestLocalMACsExcludesTunnelAndLocalEntries(t *testing.T) {
	find := "ovs-vsctl --format=csv --data=bare --no-heading --columns=name,ofport find Interface type=vxlan"
	show := "ovs-appctl fdb/show br0"
	runner := &fakeRunner{outputs: map[string][]byte{
		find: []byte("sn-tunnel,7\n"),
		show: []byte(" port  VLAN  MAC                Age\n1 0 02:00:00:00:00:01 1\n7 0 02:00:00:00:00:02 0\nLOCAL 0 02:00:00:00:00:03 0\n"),
	}}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := client.LocalMACs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []FDBEntry{{Port: "1", VLAN: 0, MAC: "02:00:00:00:00:01"}}
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
		t.Fatal(fmt.Sprintf("unexpected tunnel port %q", port))
	}
}
