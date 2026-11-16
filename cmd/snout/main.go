package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
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
	eventQueueSize  = 128
	shutdownTimeout = 15 * time.Second
)

var errForcedShutdown = errors.New("graceful shutdown interrupted by a repeated signal")

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
		fmt.Fprintf(os.Stderr, "snout: %v\n", err)
		_ = logging.Close()
		return 1
	}
	_ = logging.Close()
	return 0
}

func run(appConfig config.Config) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	return runWithSignals(appConfig, signals, os.Stderr)
}

func runWithSignals(appConfig config.Config, signals <-chan os.Signal, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(context.Background())
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
	var closeOnce sync.Once
	var closeErr error
	closeBGP := func() error {
		closeOnce.Do(func() { closeErr = bgpClient.Close() })
		return closeErr
	}
	defer closeBGP()
	manager, err := ovs.NewManager(ctx, appConfig.Topology, ovs.ExecRunner{}, bgpClient, ovs.MonitorConfig{
		VTEP:     appConfig.Topology.VTEP,
		Interval: appConfig.OVSPollInterval,
	}, appConfig.BridgeScanInterval)
	if err != nil {
		return fmt.Errorf("create OVS manager: %w", err)
	}
	for _, bridge := range appConfig.Topology.Bridges {
		for _, segment := range bridge.Segments {
			logging.Infof("bridge %s: VLAN %d <-> VNI %d (RD %s, RT %s)", bridge.Name, segment.VLAN, segment.VNI, segment.RD, segment.RouteTarget)
		}
	}
	bus.AddRouteConsumer(manager)

	go bus.RunRouteQueue(ctx, routeEvents)
	go bus.RunIMETQueue(ctx, imetEvents)
	go bus.RunPeerQueue(ctx, peerEvents)
	errs := make(chan error, 2)
	go func() {
		errs <- bgpClient.Watch(ctx, routeEvents, imetEvents, peerEvents)
	}()
	go func() {
		errs <- manager.Run(ctx)
	}()

	select {
	case received := <-signals:
		stopWorkers := func() {
			manager.StopAccepting()
			cancel()
		}
		return handleSignalShutdown(received, signals, stderr, stopWorkers, manager.Shutdown, closeBGP, shutdownTimeout)
	case firstErr := <-errs:
		manager.StopAccepting()
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutdownCancel()
		shutdownErr := manager.Shutdown(shutdownCtx)
		return errors.Join(workerError(firstErr), shutdownErr, closeBGP())
	}
}

func handleSignalShutdown(first os.Signal, signals <-chan os.Signal, stderr io.Writer, stopWorkers func(), cleanup func(context.Context) error, closeConnection func() error, timeout time.Duration) error {
	name := signalName(first)
	message := fmt.Sprintf("snout: received %s, graceful shutdown started", name)
	fmt.Fprintln(stderr, message)
	logging.Infof("received %s, graceful shutdown started", name)
	stopWorkers()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- cleanup(ctx)
	}()

	select {
	case err := <-result:
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_ = closeConnection()
			logging.Errorf("graceful shutdown timed out after %s", timeout)
			return fmt.Errorf("graceful shutdown timed out after %s: %w", timeout, ctx.Err())
		}
		err = errors.Join(err, closeConnection())
		if err != nil {
			logging.Errorf("graceful shutdown failed: %v", err)
			return fmt.Errorf("graceful shutdown failed: %w", err)
		}
		fmt.Fprintln(stderr, "snout: graceful shutdown completed")
		logging.Infof("graceful shutdown completed")
		return nil
	case repeated := <-signals:
		cancel()
		_ = closeConnection()
		repeatedName := signalName(repeated)
		logging.Errorf("received %s during graceful shutdown; forcing termination", repeatedName)
		return fmt.Errorf("%w: received %s", errForcedShutdown, repeatedName)
	case <-ctx.Done():
		_ = closeConnection()
		logging.Errorf("graceful shutdown timed out after %s", timeout)
		return fmt.Errorf("graceful shutdown timed out after %s: %w", timeout, ctx.Err())
	}
}

func signalName(received os.Signal) string {
	switch received {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return received.String()
	}
}

func workerError(errs ...error) error {
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}
