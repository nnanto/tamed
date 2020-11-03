package main

import (
	"context"
	"fmt"
	"github.com/nnanto/tamed/client"
	"github.com/nnanto/tamed/server"
	"log"
)

func main() {
	ctx, done := context.WithCancel(context.Background())
	defer done()

	serverNotification := make(chan server.Notify)
	// start a server
	go func() {
		if err := server.Start(ctx, nil, serverNotification); err != nil {
			log.Fatalf("Unable to start tailscaled server: %v", err)
		}
	}()

	for notify := range serverNotification {
		if notify.Authenticated != nil {
			// start client once we get authenticated message
			if err := startClient(ctx); err != nil {
				log.Fatalf("Unable to start tamed client: %v\n", err)
			}
		}
	}
}

func startClient(ctx context.Context) error {
	option := client.DefaultOptions()
	option.ListenerCh = make(chan client.Notify)
	option.Logger = log.Printf
	go listen(option.ListenerCh)


	if _, err := client.Start(ctx, option); err != nil {
		return err
	}
	return nil
}

// listen receives notification from tamed client
func listen(ch chan client.Notify) {
	for notify := range ch {
		if notify.PingRequest != nil {
			fmt.Printf("Ping Request %v\n", notify.PingRequest)
		}
	}
}
