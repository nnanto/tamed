package server

import (
	"context"
	"fmt"
	"testing"
)

func TestServer(t *testing.T) {
	ch := make(chan Notify)
	ctx, done := context.WithCancel(context.Background())
	go Start(ctx, nil , ch)
	for notify := range ch {
		if notify.Started != nil {
			fmt.Printf("Server started\n")
		} else if notify.LoginURL != nil {
			fmt.Printf("Please Login : %v\n", *notify.LoginURL)
		} else if notify.Authenticated != nil {
			fmt.Printf("Authentication Success\n")
			done()
			close(ch)
		}
	}
}
