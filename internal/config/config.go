package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const (
	Version           = "0.0.1"
	DefaultConfigFile = "snout.toml"
)

type Config struct {
	GoBGPGRPCSocket    string
	LogLevel           string
	SyslogFacility     string
	BridgeList         []string
	BridgeScanInterval time.Duration
	Foreground         bool
	ConfigFile         string
}

func Default() Config {
	return Config{
		GoBGPGRPCSocket:    "127.0.0.1:50051",
		LogLevel:           "info",
		SyslogFacility:     "local0",
		BridgeScanInterval: 5 * time.Second,
		ConfigFile:         DefaultConfigFile,
	}
}

func Parse(args []string, output io.Writer) (Config, error) {
	config := Default()
	if hasArg(args, "-v") {
		fmt.Fprintf(output, "snout %s\n", Version)
		return Config{}, ErrVersion
	}
	configFile, explicit := configFileFromArgs(args)
	config.ConfigFile = configFile
	if !hasArg(args, "-h", "--help") {
		if err := loadTOML(configFile, &config); err != nil && (explicit || !errors.Is(err, os.ErrNotExist)) {
			return Config{}, err
		}
	}

	bridgeList := strings.Join(config.BridgeList, ",")
	bridgeScanEvery := config.BridgeScanInterval.String()
	flags := flag.NewFlagSet("snout", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&config.ConfigFile, "c", configFile, "path to snout.toml")
	flags.StringVar(&config.GoBGPGRPCSocket, "gobgp-grpc-socket", config.GoBGPGRPCSocket, "gobgpd gRPC socket or address")
	flags.StringVar(&config.LogLevel, "log-level", config.LogLevel, "syslog level: debug, info, warning, or error")
	flags.StringVar(&config.SyslogFacility, "syslog-facility", config.SyslogFacility, "syslog facility, for example local0")
	flags.StringVar(&bridgeList, "bridge-list", bridgeList, "comma-separated OVS bridge interfaces")
	flags.StringVar(&bridgeScanEvery, "bridge-scan-interval", bridgeScanEvery, "OVS bridge scan interval")
	flags.BoolVar(&config.Foreground, "f", false, "run in foreground")
	flags.Bool("v", false, "print version and exit")
	flags.Usage = func() {
		fmt.Fprintf(output, "Usage: snout [options]\n\nOptions:\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	config.BridgeList = parseBridgeList(bridgeList)
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
	return config, nil
}

var ErrVersion = errors.New("version requested")

func loadTOML(path string, config *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	fileConfig := struct {
		GoBGPGRPCSocket    string   `toml:"gobgp_grpc_socket"`
		LogLevel           string   `toml:"log_level"`
		SyslogFacility     string   `toml:"syslog_facility"`
		BridgeList         []string `toml:"bridge_list"`
		BridgeScanInterval string   `toml:"bridge_scan_interval"`
	}{
		GoBGPGRPCSocket:    config.GoBGPGRPCSocket,
		LogLevel:           config.LogLevel,
		SyslogFacility:     config.SyslogFacility,
		BridgeList:         config.BridgeList,
		BridgeScanInterval: config.BridgeScanInterval.String(),
	}
	if err := toml.Unmarshal(data, &fileConfig); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	interval, err := time.ParseDuration(fileConfig.BridgeScanInterval)
	if err != nil || interval <= 0 {
		return fmt.Errorf("parse config %s: bridge_scan_interval must be a positive duration", path)
	}
	config.GoBGPGRPCSocket = fileConfig.GoBGPGRPCSocket
	config.LogLevel = fileConfig.LogLevel
	config.SyslogFacility = fileConfig.SyslogFacility
	config.BridgeList = fileConfig.BridgeList
	config.BridgeScanInterval = interval
	return nil
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

func parseBridgeList(value string) []string {
	seen := make(map[string]struct{})
	var bridges []string
	for _, item := range strings.Split(value, ",") {
		bridge := strings.TrimSpace(item)
		if bridge == "" {
			continue
		}
		if _, exists := seen[bridge]; exists {
			continue
		}
		seen[bridge] = struct{}{}
		bridges = append(bridges, bridge)
	}
	return bridges
}
