### Tamed Client

`go run example/client/client.go`

Tamed client attaches itself to a running tailscale
daemon. It receives events and maintains peer's membership (map) by sending
& receiving heartbeat pings

