package tamp

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/interfaces"
	"tailscale.com/paths"
	"tailscale.com/safesocket"
	"tailscale.com/types/key"
	"time"
)

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
type Options struct {
	InactivityTimeLimit           time.Duration // Time after which a node is considered inactive
	HeartbeatInterval             time.Duration // Interval between subsequent heartbeats
	InactivePeerHeartBeatInterval time.Duration // Time after which an inactive peer should be pinged
	TailScaleSocket               string
	ListenerCh                    chan Notify
}

func NewOptions() *Options {
	op := &Options{
		InactivityTimeLimit:           2 * time.Minute,
		HeartbeatInterval:             5 * time.Second,
		InactivePeerHeartBeatInterval: 1 * time.Minute,
		TailScaleSocket:               paths.DefaultTailscaledSocket(),
	}
	return op
}

type PingRequest struct {
	*ipnstate.PeerStatus
	CurrentPingAt time.Time
	PrevPingAt    time.Time
}

func NewPingRequest(ps *PeerStatus, pingTime time.Time) *PingRequest {
	return &PingRequest{CurrentPingAt: pingTime, PrevPingAt: ps.LastPingRequest, PeerStatus: &ps.IpnPeerStatus}
}

type Notify struct {
	ipn.Notify
	PingRequest *PingRequest
}

// TailScaleMemClient is tailscale client connected to the tailScale client daemon running on the machine.
// In addition to the connection to tailScale client daemon, this also tracks the peerStatus by sending heartbeat pings
// to all peers and collecting their status periodically
type TailScaleMemClient struct {
	sync.RWMutex                        // Lock to prevent concurrent modification, mainly peers map
	conn         net.Conn               // The connection to the tailScale client daemon
	listener     chan Notify            // Event receiver for tailScale daemon & membership events
	bc           *ipn.BackendClient     // TailScale backend client to the daemon
	peers        map[string]*PeerStatus // map from TailScale IP to PeerStatus
	statusTicker *time.Ticker           // Ticker for heartbeat ping message & status update
	*Options
}

// NewTailScaleMemClient creates a new TailScaleMemClient and attaches a tailScale client to machine's running daemon
func NewTailScaleMemClient(ctx context.Context, options *Options) *TailScaleMemClient {
	tsc := &TailScaleMemClient{}
	tsc.peers = make(map[string]*PeerStatus)
	if options == nil {
		options = NewOptions()
	}
	tsc.Options = options
	tsc.listener = options.ListenerCh
	if err := tsc.start(ctx); err != nil {
		log.Fatalf("couldn't start backend client: %v\n", err)
	}

	go func() {
		interrupt := make(chan os.Signal, 1)
		// listen to interrupt
		signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
		// wait for either interrupt to happen or context to get over
		select {
		case <-interrupt:
		case <-ctx.Done():
		}
		tsc.close()
	}()

	return tsc
}

// ActivePeers returns a list of active peers from the current snapshot of peers
func (tsc *TailScaleMemClient) ActivePeers() []PeerStatus {
	var activePeers []PeerStatus
	tsc.RLock()
	defer tsc.RUnlock()
	for _, ps := range tsc.peers {
		if tsc.isActive(ps) {
			activePeers = append(activePeers, *ps)
		}
	}
	return activePeers
}

// ActivePeers returns a list of current snapshot of peers
func (tsc *TailScaleMemClient) AllPeers() []PeerStatus {
	var allPeers []PeerStatus
	tsc.RLock()
	defer tsc.RUnlock()
	for _, ps := range tsc.peers {
		allPeers = append(allPeers, *ps)
	}
	return allPeers
}

// isActive informs whether the peer is active given its LastPingResponse
// Peer's activeness is from the perspective of the calling node. i.e, Peer A could be inactive according
// to this node but could be active wrt to all other nodes
func (tsc *TailScaleMemClient) isActive(ps *PeerStatus) bool {
	return !ps.LastPingResponse.IsZero() && time.Since(ps.LastPingResponse) < tsc.InactivityTimeLimit
}

// starts backend client to tailscale daemon
func (tsc *TailScaleMemClient) start(ctx context.Context) error {
	//verifyTailScaleDaemonRunning()
	// create socket connection with ts running on machine
	var err error
	tsc.conn, err = safesocket.Connect(tsc.TailScaleSocket, 41112)
	if err != nil {
		return err
	}

	// create a backend client to communicate with running ts backend
	tsc.bc = ipn.NewBackendClient(log.Printf, func(data []byte) {
		if ctx.Err() != nil {
			return
		}
		if err := ipn.WriteMsg(tsc.conn, data); err != nil {
			log.Fatalf("error while sending command to ts backend (%v)", err)
		}
	})
	tsc.bc.AllowVersionSkew = true

	// set notification callback for ts events
	tsc.bc.SetNotifyCallback(tsc.handleNotificationCallback)

	// start timer once we've client ready
	tsc.statusTicker = time.NewTicker(tsc.HeartbeatInterval)

	go tsc.runNotificationReader(ctx)
	go tsc.heartbeat(ctx)

	return nil
}

func (tsc *TailScaleMemClient) handleNotificationCallback(notify ipn.Notify) {

	if notify.ErrMessage != nil {
		log.Fatalf("notification error from ts backend (%v)", *notify.ErrMessage)
	}
	// TODO: handle state
	if notify.Status != nil {
		tsc.updatePeerStatus(notify.Status.Peer)
	} else if notify.NetMap != nil {
		// TODO: check if we need to do something about this
	} else if notify.PingResult != nil {
		tsc.updatePingResult(notify.PingResult)
	} else if notify.Engine != nil {
		// TODO: check if we need to do something about this
	}

	tsc.informListener(Notify{notify, nil})

}

// check if we've TailScale daemon running
func verifyTailScaleDaemonRunning() {
	ip, inf, err := interfaces.Tailscale()
	errmsg := "error connecting to local tailscale"

	if err != nil {
		log.Fatalf(errmsg+" (%v)", err)
	}
	if ip == nil || inf == nil {
		log.Fatalf(errmsg)
	}
	log.Printf("ts backend is running in IP [%v], Interface [%v]", ip.String(), inf.Name)
}

func (tsc *TailScaleMemClient) updatePeerStatus(peers map[key.Public]*ipnstate.PeerStatus) {
	tsc.Lock()
	defer tsc.Unlock()

	// List of peers we see in the current status
	currentPeerSet := make(map[string]bool) // map of tailscale ip and presence

	for _, status := range peers {
		tsip := status.TailAddr
		currentPeerSet[tsip] = true
		if _, ok := tsc.peers[tsip]; !ok {
			// NewTailScaleMemClient peer found
			tsc.peers[tsip] = &PeerStatus{firstSeen: time.Now()}
		}
		// update IpnPeerStatus
		tsc.peers[tsip].IpnPeerStatus = *status
		log.Printf(tsc.peers[tsip].String())
	}

	for tsip := range tsc.peers {
		if _, ok := currentPeerSet[tsip]; !ok {
			// This peer has been removed so delete from our list of peers
			delete(tsc.peers, tsip)
		}
	}
}

func (tsc *TailScaleMemClient) updatePingResult(pingResult *ipnstate.PingResult) {
	tsc.Lock()
	defer tsc.Unlock()
	tsip := pingResult.NodeIP
	ps, ok := tsc.peers[tsip]
	if !ok || ps == nil {
		// should not be the case but could've been removed in the mean time
		return
	}
	ps.LastPingResponse = time.Now()
	ps.IpnPingResult = *pingResult
}

// checks status periodically for every `HeartbeatInterval`
func (tsc *TailScaleMemClient) heartbeat(ctx context.Context) {
	// initialize peer status by triggering a heartbeat
	tsc.triggerHeartbeat()

	for {
		select {
		case <-tsc.statusTicker.C:
			tsc.triggerHeartbeat()

		case <-ctx.Done():
			return
		}
	}
}

func (tsc *TailScaleMemClient) triggerHeartbeat() {
	log.Printf("Heart beat triggered at %v", time.Now().Format(time.UnixDate))
	tsc.bc.RequestStatus()

	tsc.Lock()
	defer tsc.Unlock()

	for peerIP, peerStatus := range tsc.peers {
		if !tsc.isActive(peerStatus) {
			// For inactive peer ping iff lastPing was before InactivePeerHeartBeatInterval
			if peerStatus.LastPingRequest.IsZero() ||
				time.Since(peerStatus.LastPingRequest) >= tsc.InactivePeerHeartBeatInterval {
				tsc.pingPeer(peerIP, peerStatus)
			}
		} else {
			tsc.pingPeer(peerIP, peerStatus)
		}

	}
}

// wrapper around ping for updating state & event
func (tsc *TailScaleMemClient) pingPeer(peerIP string, peerStatus *PeerStatus) {
	log.Printf("Pinging %v for membership\n", peerIP)
	tsc.bc.Ping(peerIP)
	pingTime := time.Now()
	tsc.informListener(Notify{PingRequest: NewPingRequest(peerStatus, pingTime)})
	// update this after informing listener about ping request, else we'll update the last ping request
	peerStatus.LastPingRequest = pingTime
}

// notify listeners if any
func (tsc *TailScaleMemClient) informListener(notify Notify) {
	if tsc.listener != nil {
		tsc.listener <- notify
	}
}

// runNotificationReader listens for any message from tailscale daemon and calls handleNotificationCallback
func (tsc *TailScaleMemClient) runNotificationReader(ctx context.Context) {

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// read from connection
			msg, err := ipn.ReadMsg(tsc.conn)
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					// ignorable error
					log.Println("use of closed connection")
				} else {
					log.Fatalf("error while reading notification: %v\n", err)
				}
			}
			if ctx.Err() == nil {
				// send notification message to the callback
				tsc.bc.GotNotifyMsg(msg)
			}
		}
	}
}

func (tsc *TailScaleMemClient) close() {
	_ = tsc.conn.Close()
	tsc.statusTicker.Stop()
	if tsc.listener != nil {
		ch := tsc.listener
		tsc.listener = nil
		close(ch)
	}
}
