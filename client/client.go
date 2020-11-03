package client

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/paths"
	"tailscale.com/safesocket"
	"tailscale.com/types/key"
	"time"
)

type LoggerFunc func(format string, args ...interface{})

// PeerStatus contains information about a single peer
type PeerStatus struct {
	IpnPeerStatus    ipnstate.PeerStatus
	IpnPingResult    ipnstate.PingResult // Might be default if no ping response was ever received
	LastPingRequest  time.Time
	LastPingResponse time.Time
	firstSeen        time.Time
}

// TailScale IP of the Peer
func (ps *PeerStatus) IP() string {
	return ps.IpnPeerStatus.TailAddr
}

// Returns true if response from this Peer has ever been received
func (ps *PeerStatus) HasBeenPinged() bool {
	return !ps.LastPingResponse.IsZero()
}

// Latency to ping the Peer. If peer has been inactive then returns math.MaxFloat64
func (ps *PeerStatus) LatencyInSeconds() float64 {
	if !ps.HasBeenPinged() {
		return math.MaxFloat64
	}

	return ps.IpnPingResult.LatencySeconds
}

func (ps *PeerStatus) String() string {
	return fmt.Sprintf("Tailscale IP: %v, Node Name: %v, LastPingResponse: %v, LastPingRequest: %v\n",
		ps.IpnPeerStatus.TailAddr, ps.IpnPeerStatus.SimpleHostName(), ps.LastPingResponse, ps.LastPingRequest)
}

// Set of options, mainly targeted to membership
type Option struct {
	InactivityTimeLimit           time.Duration // Time after which a node is considered inactive
	HeartbeatInterval             time.Duration // Interval between subsequent heartbeats
	InactivePeerHeartBeatInterval time.Duration // Time after which an inactive peer should be pinged
	TailScaleSocket               string
	ListenerCh                    chan Notify
	Logger                        LoggerFunc
	StartServer                   bool
}

func DefaultOptions() *Option {
	op := &Option{
		InactivityTimeLimit:           2 * time.Minute,
		HeartbeatInterval:             5 * time.Second,
		InactivePeerHeartBeatInterval: 1 * time.Minute,
		TailScaleSocket:               paths.DefaultTailscaledSocket(),
	}
	return op
}

type PeerRemoved struct {
	*ipnstate.PeerStatus
}

type PingRequest struct {
	*ipnstate.PeerStatus
	CurrentPingAt time.Time
	PrevPingAt    time.Time
}

// Ping sends a ping request to the peer
func Ping(ps *PeerStatus) *PingRequest {
	return &PingRequest{CurrentPingAt: time.Now(), PrevPingAt: ps.LastPingRequest, PeerStatus: &ps.IpnPeerStatus}
}

type Notify struct {
	ipn.Notify
	PingRequest *PingRequest
	PeerRemoved *PeerRemoved
}

// Client is TailScale as Membership Discovery client client connected to the tailScale daemon running on the machine.
// In addition to the connection to tailScale client daemon, this also tracks the peerStatus by sending heartbeat pings
// to all peers and collecting their status periodically
type Client struct {
	sync.RWMutex                        // Lock to prevent concurrent modification, mainly peers map
	conn         net.Conn               // The connection to the tailScale client daemon
	listener     chan Notify            // Event receiver for tailScale daemon & membership events
	bc           *ipn.BackendClient     // TailScale backend client to the daemon
	peers        map[string]*PeerStatus // map from TailScale IP to PeerStatus
	statusTicker *time.Ticker           // Ticker for heartbeat ping message & status update
	*Option
}

// Start starts a tailScale client attached to machine's running daemon
func Start(ctx context.Context, options *Option) (*Client, error) {
	t := &Client{
		peers: make(map[string]*PeerStatus),
	}
	if options == nil {
		options = DefaultOptions()
	}
	t.Option = options
	t.listener = options.ListenerCh
	if err := t.start(ctx); err != nil {
		return nil, err
	}

	return t, nil
}

func (c *Client) logf(format string, args ...interface{}) {
	if c.Logger != nil {
		c.Logger(format, args...)
	}
}

// ActivePeers returns a list of active peers from the current snapshot of peers
func (c *Client) ActivePeers() []PeerStatus {
	var activePeers []PeerStatus
	c.RLock()
	defer c.RUnlock()
	for _, ps := range c.peers {
		if c.isActive(ps) {
			activePeers = append(activePeers, *ps)
		}
	}
	return activePeers
}

// ActivePeers returns a list of current snapshot of peers
func (c *Client) AllPeers() []PeerStatus {
	var allPeers []PeerStatus
	c.RLock()
	defer c.RUnlock()
	for _, ps := range c.peers {
		allPeers = append(allPeers, *ps)
	}
	return allPeers
}

// isActive informs whether the peer is active given its LastPingResponse
// Peer's activeness is from the perspective of the calling node. i.e, Peer A could be inactive according
// to this node but could be active wrt to all other nodes
func (c *Client) isActive(ps *PeerStatus) bool {
	return !ps.LastPingResponse.IsZero() && time.Since(ps.LastPingResponse) < c.InactivityTimeLimit
}

// starts backend client to tailscale daemon
func (c *Client) start(ctx context.Context) error {
	//verifyTailScaleDaemonRunning()
	// create socket connection with ts running on machine
	var err error
	c.conn, err = safesocket.Connect(c.TailScaleSocket, 41112)
	if err != nil {
		return err
	}

	// create a backend client to communicate with running ts backend
	c.bc = ipn.NewBackendClient(c.logf, func(data []byte) {
		if ctx.Err() != nil {
			return
		}

		if err := ipn.WriteMsg(c.conn, data); err != nil {
			log.Fatalf("error while sending command to ts backend %v", err)
		}
	})
	c.bc.AllowVersionSkew = true

	// set notification callback for ts events
	c.bc.SetNotifyCallback(c.handleNotificationCallback)

	// start timer once we've client ready
	c.statusTicker = time.NewTicker(c.HeartbeatInterval)

	go c.runNotificationReader(ctx)

	go c.heartbeat(ctx)
	return nil
}

func (c *Client) handleNotificationCallback(notify ipn.Notify) {

	if notify.ErrMessage != nil {
		c.logf("notification error from ts backend (%v)", *notify.ErrMessage)
	}
	// TODO: handle state
	if notify.Status != nil {
		c.updatePeerStatus(notify.Status.Peer)
	} else if notify.NetMap != nil {
		// TODO: check if we need to do something about this
	} else if notify.PingResult != nil {
		c.updatePingResult(notify.PingResult)
	} else if notify.Engine != nil {
		// TODO: check if we need to do something about this
	}

	c.informListener(Notify{Notify: notify})

}

func (c *Client) updatePeerStatus(peers map[key.Public]*ipnstate.PeerStatus) {
	c.Lock()
	defer c.Unlock()

	// List of peers we see in the current status
	currentPeerSet := make(map[string]bool) // map of tailscale ip and presence

	for _, status := range peers {
		tsip := status.TailAddr
		currentPeerSet[tsip] = true
		if _, ok := c.peers[tsip]; !ok {
			// peer found
			c.peers[tsip] = &PeerStatus{firstSeen: time.Now()}
		}
		// update IpnPeerStatus
		c.peers[tsip].IpnPeerStatus = *status
		c.logf(c.peers[tsip].String())
	}

	for tsip, peer := range c.peers {
		if _, ok := currentPeerSet[tsip]; !ok {
			// This peer has been removed so delete from our list of peers
			c.informListener(Notify{PeerRemoved: &PeerRemoved{&peer.IpnPeerStatus}})
			delete(c.peers, tsip)
		}
	}
}

func (c *Client) updatePingResult(pingResult *ipnstate.PingResult) {
	c.Lock()
	defer c.Unlock()
	tsip := pingResult.NodeIP
	ps, ok := c.peers[tsip]
	if !ok || ps == nil {
		// should not be the case but could've been removed in the mean time
		return
	}
	ps.LastPingResponse = time.Now()
	ps.IpnPingResult = *pingResult
}

// checks status periodically for every `HeartbeatInterval`
func (c *Client) heartbeat(ctx context.Context) {
	// initialize peer status by triggering a heartbeat
	c.triggerHeartbeat()

	for {
		select {
		case <-c.statusTicker.C:
			c.triggerHeartbeat()

		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) triggerHeartbeat() {
	c.logf("Heart beat triggered at %v\n", time.Now().Format(time.UnixDate))
	c.bc.RequestStatus()

	c.Lock()
	defer c.Unlock()

	for peerIP, peerStatus := range c.peers {
		if !c.isActive(peerStatus) {
			// For inactive peer ping iff lastPing was before InactivePeerHeartBeatInterval
			if peerStatus.LastPingRequest.IsZero() ||
				time.Since(peerStatus.LastPingRequest) >= c.InactivePeerHeartBeatInterval {
				c.pingPeer(peerIP, peerStatus)
			}
		} else {
			// TODO: optimize calling of ping if we've talked to this IP in the mean time
			c.pingPeer(peerIP, peerStatus)
		}

	}
}

// wrapper around ping for updating state & event
func (c *Client) pingPeer(peerIP string, peerStatus *PeerStatus) {
	c.logf("Pinging %v for membership\n", peerIP)
	c.bc.Ping(peerIP)
	pingTime := time.Now()
	c.informListener(Notify{PingRequest: Ping(peerStatus)})
	// update this after informing listener about ping request, else we'll update the last ping request
	peerStatus.LastPingRequest = pingTime
}

// notify listeners if any
func (c *Client) informListener(notify Notify) {
	if c.listener != nil {
		c.listener <- notify
	}
}

// runNotificationReader listens for any message from tailscale daemon and calls handleNotificationCallback
func (c *Client) runNotificationReader(ctx context.Context) {

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// read from connection
			msg, err := ipn.ReadMsg(c.conn)
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					// ignorable error
					c.logf("use of closed connection\n")
				} else {
					log.Fatalf("error while reading notification: %v\n", err)
				}
			}
			if ctx.Err() == nil {
				// send notification message to the callback
				c.bc.GotNotifyMsg(msg)
			}
		}
	}
}

func (c *Client) close() {
	_ = c.conn.Close()
	c.statusTicker.Stop()
	c.logf("closed connection and ticker")
	if c.listener != nil {
		ch := c.listener
		c.listener = nil

		close(ch)
		c.logf("closed external listener")
	}
}
