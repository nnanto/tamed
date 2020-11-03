package main

import (
	"context"
	"fmt"
	"github.com/nnanto/tamed/client"
	"log"
	"os"
	"os/signal"
)

func main() {
	ctx, done := context.WithCancel(context.Background())
	defer done()
	if err := startClient(ctx); err != nil {
		log.Fatal(err)
	}

	// wait for termination
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, os.Kill)
	<-ch
}
func startClient(ctx context.Context) error {
	option := client.DefaultOptions()
	option.ListenerCh = make(chan client.Notify)
	option.Logger = log.Printf
	if _, err := client.Start(ctx, option); err != nil {
		return err
	}
	go listen(option.ListenerCh)
	return nil
}

// listen receives notification from tamed client
func listen(ch chan client.Notify) {
	for notify := range ch {
		if notify.PingRequest != nil {
			fmt.Printf("Ping Request %v\n", notify.PingRequest)
		} else if notify.PingResult != nil {
			fmt.Printf("Ping Result %v\n", notify.PingResult)
		}
	}
}

