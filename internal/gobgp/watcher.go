package gobgp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"

	api "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xslonikx/snout/internal/event"
)

// Run connects to GoBGP and starts independent EVPN table and peer-state
// streams. Protobuf values do not cross the internal/gobgp package boundary.
func Run(ctx context.Context, address string, routeEvents chan<- event.RouteEvent, peerEvents chan<- event.PeerEvent) error {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("create GoBGP gRPC connection: %w", err)
	}
	defer conn.Close()

	client := api.NewGoBgpServiceClient(conn)
	errs := make(chan error, 2)

	go func() {
		errs <- watchTable(ctx, client, routeEvents)
	}()
	go func() {
		errs <- watchPeers(ctx, client, peerEvents)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errs:
		return err
	}
}

func watchTable(ctx context.Context, client api.GoBgpServiceClient, events chan<- event.RouteEvent) error {
	stream, err := client.WatchEvent(ctx, &api.WatchEventRequest{
		Table: &api.WatchEventRequest_Table{
			Filters: []*api.WatchEventRequest_Table_Filter{
				{Type: api.WatchEventRequest_Table_Filter_TYPE_BEST, Init: true},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("start table watcher: %w", err)
	}

	for {
		response, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("table watcher reached EOF")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("receive table event: %w", err)
		}

		table := response.GetTable()
		if table == nil {
			continue
		}

		for _, path := range table.GetPaths() {
			routeEvent, err := DecodeRoute(path)
			if err != nil {
				log.Printf("skip EVPN path: %v", err)
				continue
			}

			select {
			case events <- *routeEvent:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func watchPeers(ctx context.Context, client api.GoBgpServiceClient, events chan<- event.PeerEvent) error {
	stream, err := client.WatchEvent(ctx, &api.WatchEventRequest{Peer: &api.WatchEventRequest_Peer{}})
	if err != nil {
		return fmt.Errorf("start peer watcher: %w", err)
	}

	for {
		response, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("peer watcher reached EOF")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("receive peer event: %w", err)
		}

		peerEvent := response.GetPeer()
		if peerEvent == nil {
			continue
		}

		decodedEvent, err := DecodePeer(peerEvent)
		if err != nil {
			log.Printf("skip peer event: %v", err)
			continue
		}
		if decodedEvent == nil {
			continue
		}

		select {
		case events <- *decodedEvent:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
