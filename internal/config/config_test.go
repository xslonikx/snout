package config

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xslonikx/snout/internal/evpn"
)

const minimalConfig = `
vtep = "172.31.0.11"
asn = 65000

[[bridge]]
name = "br0"
vni_offset = 10000
vlans = [100, 200]
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snout.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	config, err := Parse([]string{"-c", writeConfig(t, minimalConfig), "-f"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if config.GoBGPGRPCSocket != "127.0.0.1:50051" || config.LogLevel != "info" || config.SyslogFacility != "local0" {
		t.Fatalf("unexpected defaults: %#v", config)
	}
	if !config.Foreground || config.BridgeScanInterval != 5*time.Second || config.OVSPollInterval != 5*time.Second {
		t.Fatalf("unexpected runtime defaults: %#v", config)
	}
	if config.Topology.VTEP != "172.31.0.11" || len(config.Topology.Bridges) != 1 {
		t.Fatalf("unexpected topology: %#v", config.Topology)
	}
	segment, ok := config.Topology.Bridges[0].SegmentByVLAN(200)
	want := evpn.Segment{VLAN: 200, VNI: 10200, RD: "172.31.0.11:200", RouteTarget: "65000:10200"}
	if !ok || segment != want {
		t.Fatalf("got %#v, want %#v", segment, want)
	}
}

func TestMissingDefaultConfigIsAnError(t *testing.T) {
	workingDirectory := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDirectory); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)

	_, err = Parse([]string{"-f"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected result: %v", err)
	}
}

func TestTOMLAndCLIOverride(t *testing.T) {
	path := writeConfig(t, `
gobgp_grpc_socket = "unix:///run/gobgpd.sock"
log_level = "debug"
syslog_facility = "local3"
bridge_scan_interval = "10s"
ovs_poll_interval = "2s"
vtep = "172.31.0.11"
asn = 65000

[[bridge]]
name = "br0"
vni_offset = 10000
vlans = [100]
`)
	config, err := Parse([]string{"-c", path, "--log-level", "warning", "--bridge-scan-interval", "3s"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if config.GoBGPGRPCSocket != "unix:///run/gobgpd.sock" || config.LogLevel != "warning" || config.SyslogFacility != "local3" {
		t.Fatalf("unexpected merged config: %#v", config)
	}
	if config.BridgeScanInterval != 3*time.Second || config.OVSPollInterval != 2*time.Second {
		t.Fatalf("unexpected intervals: %#v", config)
	}
}

func TestFlatAndVLANAwareBridges(t *testing.T) {
	path := writeConfig(t, `
vtep = "172.31.0.11"
asn = 65000

[[bridge]]
name = "br-flat"
vni = 50
route_target = "65000:5050"

[[bridge]]
name = "br0"
vni_offset = 10000
vlans = [100, 200, "300-301"]

  [[bridge.vni]]
  vlan = 200
  vni = 5000
  rd = "65000:5000"

  [[bridge.vni]]
  vlan = 400
  vni = 9400
`)
	config, err := Parse([]string{"-c", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	topology := config.Topology
	if len(topology.Bridges) != 2 {
		t.Fatalf("unexpected bridges: %#v", topology.Bridges)
	}
	flat := topology.Bridges[0]
	if !flat.Flat || flat.Segments[0] != (evpn.Segment{VLAN: 0, VNI: 50, RD: "172.31.0.11:50", RouteTarget: "65000:5050"}) {
		t.Fatalf("unexpected flat bridge: %#v", flat)
	}
	vlanAware := topology.Bridges[1]
	for vlan, vni := range map[uint16]uint32{100: 10100, 200: 5000, 300: 10300, 301: 10301, 400: 9400} {
		segment, ok := vlanAware.SegmentByVLAN(vlan)
		if !ok || segment.VNI != vni {
			t.Fatalf("VLAN %d: got %#v (%v), want VNI %d", vlan, segment, ok, vni)
		}
	}
	if segment, _ := vlanAware.SegmentByVLAN(200); segment.RD != "65000:5000" {
		t.Fatalf("explicit rd was not applied: %#v", segment)
	}
}

func TestRejectsInvalidConfigs(t *testing.T) {
	tests := map[string]struct {
		content  string
		fragment string
	}{
		"old bridge_list": {`vtep = "172.31.0.11"
bridge_list = ["br0"]`, "bridge_list"},
		"unknown vni entry key": {`vtep = "172.31.0.11"
asn = 65000
[[bridge]]
name = "br0"
  [[bridge.vni]]
  vlan = 1
  vni = 1
  colour = "red"`, "colour"},
		"vlan as float": {`vtep = "172.31.0.11"
asn = 65000
[[bridge]]
name = "br0"
vni_offset = 1
vlans = [1.5]`, "vlans[0]"},
		"vni as string": {`vtep = "172.31.0.11"
[[bridge]]
name = "br0"
vni = "100"`, "vni must be"},
		"no bridges":        {`vtep = "172.31.0.11"`, "at least one"},
		"bad poll interval": {`ovs_poll_interval = "never"` + minimalConfig, "ovs_poll_interval"},
		"validation error": {`vtep = "172.31.0.11"
[[bridge]]
name = "br0"
vni_offset = 0
vlans = [100]`, "asn is not set"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]string{"-c", writeConfig(t, test.content)}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.fragment) {
				t.Fatalf("got %v, want error containing %q", err, test.fragment)
			}
		})
	}
}

func TestHelpAndVersionDoNotReadConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.toml")
	var output bytes.Buffer
	_, err := Parse([]string{"-c", missing, "-h"}, &output)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "bridge-scan-interval") {
		t.Fatalf("unexpected help result: err=%v output=%q", err, output.String())
	}
	if strings.Contains(output.String(), "bridge-list") {
		t.Fatal("removed --bridge-list option is still documented")
	}

	output.Reset()
	_, err = Parse([]string{"-c", missing, "-v"}, &output)
	if !errors.Is(err, ErrVersion) || output.String() != "snout 0.0.1\n" {
		t.Fatalf("unexpected version result: err=%v output=%q", err, output.String())
	}
}
