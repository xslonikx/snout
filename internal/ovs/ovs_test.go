package ovs

import (
	"context"
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
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, command{name: name, args: append([]string(nil), args...)})
	return r.outputs[name+" "+strings.Join(args, " ")], nil
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
		{name: "ovs-vsctl", args: []string{"--may-exist", "add-port", "br0", port, "--", "set", "Interface", port, "type=vxlan", "options:remote_ip=192.0.2.20", "options:key=100", "--", "set", "Port", port, "protected=true"}},
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
		if command.name == "ovs-vsctl" && len(command.args) > 0 && command.args[0] == "--if-exists" {
			deleteCommands++
		}
	}
	if deleteCommands != 1 {
		t.Fatalf("got %d tunnel deletion commands, want 1", deleteCommands)
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

func TestEnableSplitHorizonProtectsVXLANPorts(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"ovs-vsctl list-ifaces br0":           []byte("tap0\nvxlan0\n"),
		"ovs-vsctl get Interface tap0 type":   []byte("\"\"\n"),
		"ovs-vsctl get Interface vxlan0 type": []byte("\"vxlan\"\n"),
	}}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.EnableSplitHorizon(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := command{name: "ovs-vsctl", args: []string{"set", "Port", "vxlan0", "protected=true"}}
	if got := runner.commands[len(runner.commands)-1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("split-horizon command mismatch: got %#v, want %#v", got, want)
	}
}

func TestIsVXLANBridge(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"ovs-vsctl list-ifaces br0":           []byte("tap0\nvxlan0\n"),
		"ovs-vsctl get Interface tap0 type":   []byte("\"\"\n"),
		"ovs-vsctl get Interface vxlan0 type": []byte("\"vxlan\"\n"),
	}}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}

	isVXLAN, err := client.IsVXLANBridge(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !isVXLAN {
		t.Fatal("bridge with a VXLAN interface was not detected")
	}
}

func TestRegularBridgeIsNotVXLAN(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"ovs-vsctl list-ifaces br0":         []byte("tap0\n"),
		"ovs-vsctl get Interface tap0 type": []byte("\"\"\n"),
	}}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}

	isVXLAN, err := client.IsVXLANBridge(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if isVXLAN {
		t.Fatal("regular bridge was detected as VXLAN")
	}
}

func TestManagedTunnelDoesNotActivateBridge(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"ovs-vsctl list-ifaces br0": []byte("sn-0123456789ab\n"),
	}}
	client, err := NewClient("br0", runner)
	if err != nil {
		t.Fatal(err)
	}
	isVXLAN, err := client.IsVXLANBridge(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if isVXLAN {
		t.Fatal("Snout-managed tunnel must not activate a bridge by itself")
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
