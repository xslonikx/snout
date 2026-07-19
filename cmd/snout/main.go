package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	stdoutconsumer "github.com/xslonikx/snout/internal/consumer/stdout"
	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/gobgp"
)

const (
	defaultGoBGPAddress = "127.0.0.1:50051"
	eventQueueSize      = 128
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	address := os.Getenv("GOBGP_GRPC_ADDRESS")
	if address == "" {
		address = defaultGoBGPAddress
	}

	routeEvents := make(chan event.RouteEvent, eventQueueSize)
	peerEvents := make(chan event.PeerEvent, eventQueueSize)

	bus := event.NewEventBus()
	stdout := stdoutconsumer.Consumer{}
	bus.AddRouteConsumer(stdout)
	bus.AddPeerConsumer(stdout)

	// Future dataplane consumers subscribe only through AddRouteConsumer.
	go bus.RunRouteQueue(ctx, routeEvents)
	go bus.RunPeerQueue(ctx, peerEvents)

	log.Printf("connecting to GoBGP gRPC at %s", address)

	if err := gobgp.Run(ctx, address, routeEvents, peerEvents); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("GoBGP watcher stopped: %v", err)
	}
}
