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
		{name: "ovs-vsctl", args: []string{"--may-exist", "add-port", "br0", port, "--", "set", "Interface", port, "type=vxlan", "options:remote_ip=192.0.2.20", "options:key=100"}},
		{name: "ovs-appctl", args: []string{"fdb/add", "br0", port, "0", route.MAC}},
		{name: "ovs-appctl", args: []string{"fdb/del", "br0", "0", route.MAC}},
		{name: "ovs-vsctl", args: []string{"--if-exists", "del-port", "br0", port}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands mismatch\ngot:  %#v\nwant: %#v", runner.commands, want)
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
