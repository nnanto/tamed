# TAMED (Tailscale As Membership Discovery)

TAMED client is a tailscale client that maintains membership information of peers. This is achieved by sending and receiving heartbeat pings to connected peers. 

## Server
[Example](https://github.com/nnanto/tamed/tree/main/example/server)

Tamed Server starts a tailscale service that communicates with the tailscale coordination server (control plane)

## Client
[Example](https://github.com/nnanto/tamed/tree/main/example/client)

Client communicates with the server and receives events from control plane or other peers in the network.
Client provides a list of **active peers** in the current network by tracking heartbeat between peers


### About Tailscale


[Tailscale](https://tailscale.com/) provides an encrypted virtual private network connecting your devices.
Connecting the devices is as simple as signing in through your existing identity provider.

Here's [getting started with tailscale](https://tailscale.com/kb/1017/install) to play around
