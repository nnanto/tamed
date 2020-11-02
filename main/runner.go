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
	go server.Start(ctx, log.Printf, serverNotification)


	for notify := range serverNotification {
		if notify.Authenticated != nil{
			startClient(ctx)
		}
	}


}

func startClient(ctx context.Context) {
	option := client.DefaultOptions()
	option.ListenerCh = make(chan client.Notify)
	option.Logger = log.Printf
	client.Start(ctx, option)
	go listen(option.ListenerCh)
}

func listen(ch chan client.Notify) {
	for notify := range ch {
		if notify.PingRequest != nil {
			fmt.Printf("Ping Request %v\n", notify.PingRequest)
		}
	}
}
