package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/xslonikx/snout/internal/evpn"
)

const (
	Version           = "0.0.1"
	DefaultConfigFile = "snout.toml"
)

type Config struct {
	GoBGPGRPCSocket    string
	LogLevel           string
	SyslogFacility     string
	BridgeScanInterval time.Duration
	OVSPollInterval    time.Duration
	Topology           evpn.Topology
	Foreground         bool
	ConfigFile         string
}

func Default() Config {
	return Config{
		GoBGPGRPCSocket:    "127.0.0.1:50051",
		LogLevel:           "info",
		SyslogFacility:     "local0",
		BridgeScanInterval: 5 * time.Second,
		OVSPollInterval:    5 * time.Second,
		ConfigFile:         DefaultConfigFile,
	}
}

var ErrVersion = errors.New("version requested")

func Parse(args []string, output io.Writer) (Config, error) {
	config := Default()
	if hasArg(args, "-v") {
		fmt.Fprintf(output, "snout %s\n", Version)
		return Config{}, ErrVersion
	}
	configFile, explicit := configFileFromArgs(args)
	config.ConfigFile = configFile
	var spec evpn.Spec
	missing := false
	if !hasArg(args, "-h", "--help") {
		loaded, err := loadTOML(configFile, &config)
		switch {
		case err == nil:
			spec = loaded
		case !explicit && errors.Is(err, os.ErrNotExist):
			missing = true
		default:
			return Config{}, err
		}
	}

	bridgeScanEvery := config.BridgeScanInterval.String()
	flags := flag.NewFlagSet("snout", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&config.ConfigFile, "c", configFile, "path to snout.toml")
	flags.StringVar(&config.GoBGPGRPCSocket, "gobgp-grpc-socket", config.GoBGPGRPCSocket, "gobgpd gRPC socket or address")
	flags.StringVar(&config.LogLevel, "log-level", config.LogLevel, "syslog level: debug, info, warning, or error")
	flags.StringVar(&config.SyslogFacility, "syslog-facility", config.SyslogFacility, "syslog facility, for example local0")
	flags.StringVar(&bridgeScanEvery, "bridge-scan-interval", bridgeScanEvery, "OVS bridge scan interval")
	flags.BoolVar(&config.Foreground, "f", false, "run in foreground")
	flags.Bool("v", false, "print version and exit")
	flags.Usage = func() {
		fmt.Fprintf(output, "Usage: snout [options]\n\nBridges, VNIs, vtep and asn are configured in snout.toml.\n\nOptions:\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	interval, err := time.ParseDuration(bridgeScanEvery)
	if err != nil || interval <= 0 {
		return Config{}, fmt.Errorf("bridge_scan_interval must be a positive duration")
	}
	config.BridgeScanInterval = interval
	config.LogLevel = strings.ToLower(strings.TrimSpace(config.LogLevel))
	config.SyslogFacility = strings.ToLower(strings.TrimSpace(config.SyslogFacility))
	if config.GoBGPGRPCSocket == "" {
		return Config{}, fmt.Errorf("gobgp_grpc_socket cannot be empty")
	}
	if missing {
		return Config{}, fmt.Errorf("config %s not found: vtep and at least one [[bridge]] must be configured", configFile)
	}
	topology, err := evpn.Resolve(spec)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", configFile, err)
	}
	config.Topology = topology
	return config, nil
}

type fileConfig struct {
	GoBGPGRPCSocket    string       `toml:"gobgp_grpc_socket"`
	LogLevel           string       `toml:"log_level"`
	SyslogFacility     string       `toml:"syslog_facility"`
	BridgeScanInterval string       `toml:"bridge_scan_interval"`
	OVSPollInterval    string       `toml:"ovs_poll_interval"`
	VTEP               string       `toml:"vtep"`
	ASN                *int64       `toml:"asn"`
	Bridges            []fileBridge `toml:"bridge"`
}

// fileBridge keeps `vni` untyped: it is an integer on a flat bridge and an
// array of [[bridge.vni]] tables on a VLAN-aware bridge.
type fileBridge struct {
	Name        string `toml:"name"`
	VNI         any    `toml:"vni"`
	RD          string `toml:"rd"`
	RouteTarget string `toml:"route_target"`
	VNIOffset   *int64 `toml:"vni_offset"`
	VLANs       []any  `toml:"vlans"`
}

func loadTOML(path string, config *Config) (evpn.Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return evpn.Spec{}, fmt.Errorf("read config %s: %w", path, err)
	}
	file := fileConfig{
		GoBGPGRPCSocket:    config.GoBGPGRPCSocket,
		LogLevel:           config.LogLevel,
		SyslogFacility:     config.SyslogFacility,
		BridgeScanInterval: config.BridgeScanInterval.String(),
		OVSPollInterval:    config.OVSPollInterval.String(),
	}
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return evpn.Spec{}, fmt.Errorf("parse config %s: unknown setting(s):\n%s", path, strict.String())
		}
		return evpn.Spec{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	scan, err := time.ParseDuration(file.BridgeScanInterval)
	if err != nil || scan <= 0 {
		return evpn.Spec{}, fmt.Errorf("parse config %s: bridge_scan_interval must be a positive duration", path)
	}
	poll, err := time.ParseDuration(file.OVSPollInterval)
	if err != nil || poll <= 0 {
		return evpn.Spec{}, fmt.Errorf("parse config %s: ovs_poll_interval must be a positive duration", path)
	}
	config.GoBGPGRPCSocket = file.GoBGPGRPCSocket
	config.LogLevel = file.LogLevel
	config.SyslogFacility = file.SyslogFacility
	config.BridgeScanInterval = scan
	config.OVSPollInterval = poll

	spec := evpn.Spec{VTEP: file.VTEP, ASN: file.ASN}
	for index, bridge := range file.Bridges {
		bridgeSpec, err := convertBridge(bridge)
		if err != nil {
			return evpn.Spec{}, fmt.Errorf("parse config %s: bridge[%d]: %w", path, index, err)
		}
		spec.Bridges = append(spec.Bridges, bridgeSpec)
	}
	return spec, nil
}

func convertBridge(bridge fileBridge) (evpn.BridgeSpec, error) {
	spec := evpn.BridgeSpec{Name: bridge.Name, RD: bridge.RD, RouteTarget: bridge.RouteTarget, VNIOffset: bridge.VNIOffset}
	for index, item := range bridge.VLANs {
		switch value := item.(type) {
		case int64:
			spec.VLANs = append(spec.VLANs, strconv.FormatInt(value, 10))
		case string:
			spec.VLANs = append(spec.VLANs, value)
		default:
			return evpn.BridgeSpec{}, fmt.Errorf("vlans[%d]: expected a number or a \"low-high\" range, got %v", index, item)
		}
	}
	switch value := bridge.VNI.(type) {
	case nil:
	case int64:
		spec.VNI = &value
	case []any:
		for index, item := range value {
			table, ok := item.(map[string]any)
			if !ok {
				return evpn.BridgeSpec{}, fmt.Errorf("vni[%d]: expected a [[bridge.vni]] table", index)
			}
			entry, err := convertVNI(table)
			if err != nil {
				return evpn.BridgeSpec{}, fmt.Errorf("vni[%d]: %w", index, err)
			}
			spec.VNIs = append(spec.VNIs, entry)
		}
	default:
		return evpn.BridgeSpec{}, fmt.Errorf("vni must be an integer (flat bridge) or [[bridge.vni]] tables, got %v", value)
	}
	return spec, nil
}

func convertVNI(table map[string]any) (evpn.VNISpec, error) {
	var entry evpn.VNISpec
	for key, raw := range table {
		switch key {
		case "vlan", "vni":
			number, ok := raw.(int64)
			if !ok {
				return evpn.VNISpec{}, fmt.Errorf("%s must be an integer", key)
			}
			if key == "vlan" {
				entry.VLAN = &number
			} else {
				entry.VNI = &number
			}
		case "rd", "route_target":
			text, ok := raw.(string)
			if !ok {
				return evpn.VNISpec{}, fmt.Errorf("%s must be a string", key)
			}
			if key == "rd" {
				entry.RD = text
			} else {
				entry.RouteTarget = text
			}
		default:
			return evpn.VNISpec{}, fmt.Errorf("unknown setting %q", key)
		}
	}
	return entry, nil
}

func hasArg(args []string, values ...string) bool {
	for _, arg := range args {
		for _, value := range values {
			if arg == value {
				return true
			}
		}
	}
	return false
}

func configFileFromArgs(args []string) (string, bool) {
	for index, arg := range args {
		if arg == "-c" && index+1 < len(args) {
			return args[index+1], true
		}
		if strings.HasPrefix(arg, "-c=") {
			return strings.TrimPrefix(arg, "-c="), true
		}
	}
	return DefaultConfigFile, false
}
