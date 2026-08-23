package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/xslonikx/snout/internal/config"
	stdoutconsumer "github.com/xslonikx/snout/internal/consumer/stdout"
	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/gobgp"
	"github.com/xslonikx/snout/internal/logging"
	"github.com/xslonikx/snout/internal/ovs"
)

const (
	eventQueueSize      = 128
	defaultPollInterval = 5 * time.Second
)

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	appConfig, err := config.Parse(os.Args[1:], os.Stdout)
	if errors.Is(err, flag.ErrHelp) || errors.Is(err, config.ErrVersion) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "snout: %v\n", err)
		return 2
	}
	if err := logging.Validate(appConfig.LogLevel, appConfig.SyslogFacility); err != nil {
		fmt.Fprintf(os.Stderr, "snout: %v\n", err)
		return 2
	}
	if !appConfig.Foreground {
		if err := daemonize(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "snout: %v\n", err)
			return 1
		}
		return 0
	}
	if err := logging.Initialize(appConfig.LogLevel, appConfig.SyslogFacility); err != nil {
		fmt.Fprintf(os.Stderr, "snout: %v\n", err)
		return 1
	}

	if err := run(appConfig); err != nil && !errors.Is(err, context.Canceled) {
		logging.Errorf("Snout stopped: %v", err)
		_ = logging.Close()
		return 1
	}
	_ = logging.Close()
	return 0
}

func run(appConfig config.Config) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	routeEvents := make(chan event.RouteEvent, eventQueueSize)
	imetEvents := make(chan event.IMETEvent, eventQueueSize)
	peerEvents := make(chan event.PeerEvent, eventQueueSize)
	bus := event.NewEventBus()
	stdout := stdoutconsumer.Consumer{}
	bus.AddRouteConsumer(stdout)
	bus.AddPeerConsumer(stdout)

	logging.Infof("connecting to GoBGP gRPC at %s", appConfig.GoBGPGRPCSocket)
	bgpClient, err := gobgp.NewClient(appConfig.GoBGPGRPCSocket)
	if err != nil {
		return fmt.Errorf("create GoBGP client: %w", err)
	}
	defer bgpClient.Close()

	var manager *ovs.Manager
	if len(appConfig.BridgeList) != 0 {
		monitorConfig, routeTarget, err := ovsConfigFromEnv()
		if err != nil {
			return fmt.Errorf("invalid OVS configuration: %w", err)
		}
		if err := bgpClient.SetPublisherConfig(gobgp.PublisherConfig{RouteTarget: routeTarget}); err != nil {
			return fmt.Errorf("configure GoBGP publisher: %w", err)
		}
		manager, err = ovs.NewManager(ctx, appConfig.BridgeList, ovs.ExecRunner{}, bgpClient, monitorConfig, appConfig.BridgeScanInterval)
		if err != nil {
			return fmt.Errorf("create OVS manager: %w", err)
		}
		bus.AddRouteConsumer(manager)
	}

	go bus.RunRouteQueue(ctx, routeEvents)
	go bus.RunIMETQueue(ctx, imetEvents)
	go bus.RunPeerQueue(ctx, peerEvents)
	if manager == nil {
		return bgpClient.Watch(ctx, routeEvents, imetEvents, peerEvents)
	}

	errs := make(chan error, 2)
	go func() {
		errs <- bgpClient.Watch(ctx, routeEvents, imetEvents, peerEvents)
	}()
	go func() {
		errs <- manager.Run(ctx)
	}()

	firstErr := <-errs
	cancel()
	secondErr := <-errs
	return workerError(firstErr, secondErr)
}

func workerError(errs ...error) error {
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func ovsConfigFromEnv() (ovs.MonitorConfig, string, error) {
	vni, err := strconv.ParseUint(os.Getenv("SNOUT_VNI"), 10, 24)
	if err != nil || vni == 0 {
		return ovs.MonitorConfig{}, "", errors.New("SNOUT_VNI must be an integer from 1 to 16777215")
	}
	interval := defaultPollInterval
	if value := os.Getenv("SNOUT_OVS_POLL_INTERVAL"); value != "" {
		interval, err = time.ParseDuration(value)
		if err != nil || interval <= 0 {
			return ovs.MonitorConfig{}, "", errors.New("SNOUT_OVS_POLL_INTERVAL must be a positive duration")
		}
	}
	monitorConfig := ovs.MonitorConfig{
		RD:       os.Getenv("SNOUT_RD"),
		VNI:      uint32(vni),
		VTEP:     os.Getenv("SNOUT_VTEP"),
		Interval: interval,
	}
	if monitorConfig.RD == "" || monitorConfig.VTEP == "" || os.Getenv("SNOUT_ROUTE_TARGET") == "" {
		return ovs.MonitorConfig{}, "", errors.New("SNOUT_RD, SNOUT_ROUTE_TARGET, and SNOUT_VTEP are required")
	}
	return monitorConfig, os.Getenv("SNOUT_ROUTE_TARGET"), nil
}
