package main

import (
	"context"
	"fmt"
	"io"
	"log"

	api "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"google.golang.org/protobuf/encoding/prototext"
)

func main() {
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
		}
	}(conn)

	if err != nil {
		log.Fatal(err)
	}

	client := api.NewGoBgpServiceClient(conn)

	stream, err := client.WatchEvent(context.Background(), &api.WatchEventRequest{
		Table: &api.WatchEventRequest_Table{},
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Waiting for events...")

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println("======================================")
		fmt.Println(prototext.Format(resp))
	}
}
