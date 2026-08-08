package config

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	workingDirectory := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDirectory); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)

	config, err := Parse([]string{"-f"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if config.GoBGPGRPCSocket != "127.0.0.1:50051" || config.LogLevel != "info" || config.SyslogFacility != "local0" {
		t.Fatalf("unexpected defaults: %#v", config)
	}
	if !config.Foreground || config.BridgeScanInterval != 5*time.Second {
		t.Fatalf("unexpected runtime defaults: %#v", config)
	}
}

func TestTOMLAndCLIOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snout.toml")
	data := []byte("gobgp_grpc_socket = \"unix:///run/gobgpd.sock\"\nlog_level = \"debug\"\nsyslog_facility = \"local3\"\nbridge_list = [\"br0\", \"br1\"]\nbridge_scan_interval = \"10s\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := Parse([]string{"-c", path, "--log-level", "warning", "--bridge-list", "br2, br3,br2"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if config.GoBGPGRPCSocket != "unix:///run/gobgpd.sock" || config.LogLevel != "warning" || config.SyslogFacility != "local3" {
		t.Fatalf("unexpected merged config: %#v", config)
	}
	if !reflect.DeepEqual(config.BridgeList, []string{"br2", "br3"}) || config.BridgeScanInterval != 10*time.Second {
		t.Fatalf("unexpected bridge config: %#v", config)
	}
}

func TestHelpAndVersionDoNotReadConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.toml")
	var output bytes.Buffer
	_, err := Parse([]string{"-c", missing, "-h"}, &output)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "bridge-list") {
		t.Fatalf("unexpected help result: err=%v output=%q", err, output.String())
	}

	output.Reset()
	_, err = Parse([]string{"-c", missing, "-v"}, &output)
	if !errors.Is(err, ErrVersion) || output.String() != "snout 0.0.1\n" {
		t.Fatalf("unexpected version result: err=%v output=%q", err, output.String())
	}
}
