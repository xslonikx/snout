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

func main() {
	ctx := context.Background()

	conn, err := grpc.NewClient(
		"127.0.0.1:50051",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)

	client := api.NewGoBgpServiceClient(conn)

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
		log.Fatal(err)
	}

	log.Println("Waiting for table events...")

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			log.Println("stream closed")
			return
		}
		if err != nil {
			log.Fatal(err)
		}

		log.Println("========================================")
		log.Println(prototext.Format(resp))
	}
}
