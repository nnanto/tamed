# TAMED (Tailscale As MEmbership Discovery)

[Tailscale](https://tailscale.com/) provides an encrypted virtual private network connecting your devices.
Connecting the devices is as simple as signing in through your existing identity provider.

Here's [getting started with tailscale](https://tailscale.com/kb/1017/install) to play around

## Server
Tamed Server starts a tailscale service that communicates with the tailscale coordination server (control plane)

## Client
Client communicates with the server and receives events from control plane or other peers in the network.
Client provides a list of **active peers** in the current network by tracking heartbeat between peers
