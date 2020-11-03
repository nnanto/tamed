package main

import (
	"context"
	"fmt"
	"github.com/nnanto/tamed/server"
	"log"
	"os"
	"os/signal"
)

func main() {
	ctx, done := context.WithCancel(context.Background())
	defer done()

	serverNotification := make(chan server.Notify)
	// start a server
	go func() {
		if err := server.Start(ctx, log.Printf, serverNotification); err != nil {
			log.Fatalf("Unable to start tailscaled server: %v", err)
		}
	}()

	go func() {
		for notify := range serverNotification {
			if notify.Started != nil {
				fmt.Printf("Server started listening \n")
			} else if notify.LoginURL != nil {
				fmt.Printf("Please Login: %v\n", notify.LoginURL)
			} else if notify.Authenticated != nil {
				fmt.Printf("Server Authenticated \n")
			}
		}
	}()
	// wait for termination
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, os.Kill)
	<-ch
}
