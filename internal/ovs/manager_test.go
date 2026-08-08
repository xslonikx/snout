package ovs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xslonikx/snout/internal/event"
)

type changingBridgeRunner struct {
	mu    sync.Mutex
	scans int
}

func (r *changingBridgeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	command := name + " " + strings.Join(args, " ")
	switch command {
	case "ovs-vsctl list-ifaces br0":
		r.scans++
		if r.scans == 2 {
			return []byte("tap0\nvxlan0\n"), nil
		}
		return []byte("tap0\n"), nil
	case "ovs-vsctl get Interface tap0 type":
		return []byte("\"\"\n"), nil
	case "ovs-vsctl get Interface vxlan0 type":
		return []byte("\"vxlan\"\n"), nil
	case "ovs-vsctl --format=csv --data=bare --no-heading --columns=name,ofport find Interface type=vxlan":
		return []byte("vxlan0,2\n"), nil
	case "ovs-appctl fdb/show br0":
		return []byte("port VLAN MAC Age\n"), nil
	default:
		return nil, nil
	}
}

type noopAnnouncer struct{}

func (noopAnnouncer) Announce(context.Context, event.EVPNRoute) error { return nil }
func (noopAnnouncer) Withdraw(context.Context, event.EVPNRoute) error { return nil }

func TestManagerTracksBridgeTypeChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager, err := NewManager(ctx, []string{"br0"}, &changingBridgeRunner{}, noopAnnouncer{}, MonitorConfig{
		RD: "65000:100", VNI: 100, VTEP: "192.0.2.10", Interval: time.Hour,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	manager.scan(ctx)
	if len(manager.active) != 0 {
		t.Fatal("regular bridge must be ignored")
	}
	manager.scan(ctx)
	if len(manager.active) != 1 {
		t.Fatal("VXLAN bridge must be activated")
	}
	manager.scan(ctx)
	if len(manager.active) != 0 {
		t.Fatal("bridge must be deactivated after losing its VXLAN interface")
	}
	manager.stopAll()
}
