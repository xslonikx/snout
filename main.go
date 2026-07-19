package main

import (
	"context"
	"io"
	"log"

	api "github.com/osrg/gobgp/v4/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/prototext"
)

type Event struct {
	Resp *api.WatchEventResponse
}

type Consumer interface {
	Consume(Event)
}

type Dispatcher struct {
	consumers []Consumer
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{}
}

func (d *Dispatcher) Register(c Consumer) {
	d.consumers = append(d.consumers, c)
}

func (d *Dispatcher) Run(events <-chan Event) {
	for event := range events {
		for _, consumer := range d.consumers {
			consumer.Consume(event)
		}
	}
}

type StdoutConsumer struct{}

func (StdoutConsumer) Consume(event Event) {
	switch {
	case event.Resp.GetTable() != nil:
		log.Println("========== TABLE ==========")

	case event.Resp.GetPeer() != nil:
		log.Println("========== PEER ==========")

	default:
		log.Println("========== UNKNOWN ==========")
	}

	log.Print(prototext.Format(event.Resp))
}

func main() {
	ctx := context.Background()

	conn, err := grpc.NewClient(
		"127.0.0.1:50051",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := api.NewGoBgpServiceClient(conn)

	events := make(chan Event, 128)

	dispatcher := NewDispatcher()
	dispatcher.Register(StdoutConsumer{})

	go dispatcher.Run(events)

	go watchTable(ctx, client, events)
	go watchPeers(ctx, client, events)

	select {}
}

func watchTable(
	ctx context.Context,
	client api.GoBgpServiceClient,
	events chan<- Event,
) {
	stream, err := client.WatchEvent(ctx, &api.WatchEventRequest{
		Table: &api.WatchEventRequest_Table{
			Filters: []*api.WatchEventRequest_Table_Filter{
				{
					Type: api.WatchEventRequest_Table_Filter_TYPE_BEST,
					Init: true,
				},
			},
		},
	})
	if err != nil {
		log.Fatalf("watch table: %v", err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Printf("table stream closed: %v", err)
			return
		}

		events <- Event{Resp: resp}
	}
}

func watchPeers(
	ctx context.Context,
	client api.GoBgpServiceClient,
	events chan<- Event,
) {
	stream, err := client.WatchEvent(ctx, &api.WatchEventRequest{
		Peer: &api.WatchEventRequest_Peer{},
	})
	if err != nil {
		log.Fatalf("watch peer: %v", err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Printf("peer stream closed: %v", err)
			return
		}

		events <- Event{Resp: resp}
	}
}
