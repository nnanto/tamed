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
type TamedOption struct {
	InactivityTimeLimit           time.Duration // Time after which a node is considered inactive
	HeartbeatInterval             time.Duration // Interval between subsequent heartbeats
	InactivePeerHeartBeatInterval time.Duration // Time after which an inactive peer should be pinged
	TailScaleSocket               string
	ListenerCh                    chan Notify
	Logger                        func(string, ...interface{})
}

func DefaultOptions() *TamedOption {
	op := &TamedOption{
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

func NewPingRequest(ps *PeerStatus, pingTime time.Time) *PingRequest {
	return &PingRequest{CurrentPingAt: pingTime, PrevPingAt: ps.LastPingRequest, PeerStatus: &ps.IpnPeerStatus}
}

type Notify struct {
	ipn.Notify
	PingRequest *PingRequest
	PeerRemoved *PeerRemoved
}

// Tamed is TailScale as Membership Discovery client client connected to the tailScale daemon running on the machine.
// In addition to the connection to tailScale client daemon, this also tracks the peerStatus by sending heartbeat pings
// to all peers and collecting their status periodically
type Tamed struct {
	sync.RWMutex                        // Lock to prevent concurrent modification, mainly peers map
	conn         net.Conn               // The connection to the tailScale client daemon
	listener     chan Notify            // Event receiver for tailScale daemon & membership events
	bc           *ipn.BackendClient     // TailScale backend client to the daemon
	peers        map[string]*PeerStatus // map from TailScale IP to PeerStatus
	statusTicker *time.Ticker           // Ticker for heartbeat ping message & status update
	*TamedOption
}

// Start attaches a tailScale client to machine's running daemon
func Start(ctx context.Context, options *TamedOption) *Tamed {
	t := &Tamed{}
	t.peers = make(map[string]*PeerStatus)
	if options == nil {
		options = DefaultOptions()
	}
	t.TamedOption = options
	t.listener = options.ListenerCh
	if err := t.start(ctx); err != nil {
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
		t.close()
	}()

	return t
}

func (t *Tamed) Logf(format string, args ...interface{}) {
	if t.Logger != nil {
		t.Logger(format, args...)
	}
}

// ActivePeers returns a list of active peers from the current snapshot of peers
func (t *Tamed) ActivePeers() []PeerStatus {
	var activePeers []PeerStatus
	t.RLock()
	defer t.RUnlock()
	for _, ps := range t.peers {
		if t.isActive(ps) {
			activePeers = append(activePeers, *ps)
		}
	}
	return activePeers
}

// ActivePeers returns a list of current snapshot of peers
func (t *Tamed) AllPeers() []PeerStatus {
	var allPeers []PeerStatus
	t.RLock()
	defer t.RUnlock()
	for _, ps := range t.peers {
		allPeers = append(allPeers, *ps)
	}
	return allPeers
}

// isActive informs whether the peer is active given its LastPingResponse
// Peer's activeness is from the perspective of the calling node. i.e, Peer A could be inactive according
// to this node but could be active wrt to all other nodes
func (t *Tamed) isActive(ps *PeerStatus) bool {
	return !ps.LastPingResponse.IsZero() && time.Since(ps.LastPingResponse) < t.InactivityTimeLimit
}

// starts backend client to tailscale daemon
func (t *Tamed) start(ctx context.Context) error {
	//verifyTailScaleDaemonRunning()
	// create socket connection with ts running on machine
	var err error
	t.conn, err = safesocket.Connect(t.TailScaleSocket, 41112)
	if err != nil {
		return err
	}

	// create a backend client to communicate with running ts backend
	t.bc = ipn.NewBackendClient(t.Logf, func(data []byte) {
		if ctx.Err() != nil {
			return
		}
		if err := ipn.WriteMsg(t.conn, data); err != nil {
			log.Fatalf("error while sending command to ts backend (%v)", err)
		}
	})
	t.bc.AllowVersionSkew = true

	// set notification callback for ts events
	t.bc.SetNotifyCallback(t.handleNotificationCallback)

	// start timer once we've client ready
	t.statusTicker = time.NewTicker(t.HeartbeatInterval)

	go t.runNotificationReader(ctx)
	go t.heartbeat(ctx)

	return nil
}

func (t *Tamed) handleNotificationCallback(notify ipn.Notify) {

	if notify.ErrMessage != nil {
		log.Fatalf("notification error from ts backend (%v)", *notify.ErrMessage)
	}
	// TODO: handle state
	if notify.Status != nil {
		t.updatePeerStatus(notify.Status.Peer)
	} else if notify.NetMap != nil {
		// TODO: check if we need to do something about this
	} else if notify.PingResult != nil {
		t.updatePingResult(notify.PingResult)
	} else if notify.Engine != nil {
		// TODO: check if we need to do something about this
	}

	t.informListener(Notify{Notify: notify})

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
	//t.Logf("ts backend is running in IP [%v], Interface [%v]", ip.String(), inf.Name)
}

func (t *Tamed) updatePeerStatus(peers map[key.Public]*ipnstate.PeerStatus) {
	t.Lock()
	defer t.Unlock()

	// List of peers we see in the current status
	currentPeerSet := make(map[string]bool) // map of tailscale ip and presence

	for _, status := range peers {
		tsip := status.TailAddr
		currentPeerSet[tsip] = true
		if _, ok := t.peers[tsip]; !ok {
			// Start peer found
			t.peers[tsip] = &PeerStatus{firstSeen: time.Now()}
		}
		// update IpnPeerStatus
		t.peers[tsip].IpnPeerStatus = *status
		t.Logf(t.peers[tsip].String())
	}

	for tsip, peer := range t.peers {
		if _, ok := currentPeerSet[tsip]; !ok {
			// This peer has been removed so delete from our list of peers
			t.informListener(Notify{PeerRemoved: &PeerRemoved{&peer.IpnPeerStatus}})
			delete(t.peers, tsip)
		}
	}
}

func (t *Tamed) updatePingResult(pingResult *ipnstate.PingResult) {
	t.Lock()
	defer t.Unlock()
	tsip := pingResult.NodeIP
	ps, ok := t.peers[tsip]
	if !ok || ps == nil {
		// should not be the case but could've been removed in the mean time
		return
	}
	ps.LastPingResponse = time.Now()
	ps.IpnPingResult = *pingResult
}

// checks status periodically for every `HeartbeatInterval`
func (t *Tamed) heartbeat(ctx context.Context) {
	// initialize peer status by triggering a heartbeat
	t.triggerHeartbeat()

	for {
		select {
		case <-t.statusTicker.C:
			t.triggerHeartbeat()

		case <-ctx.Done():
			return
		}
	}
}

func (t *Tamed) triggerHeartbeat() {
	t.Logf("Heart beat triggered at %v\n", time.Now().Format(time.UnixDate))
	t.bc.RequestStatus()

	t.Lock()
	defer t.Unlock()

	for peerIP, peerStatus := range t.peers {
		if !t.isActive(peerStatus) {
			// For inactive peer ping iff lastPing was before InactivePeerHeartBeatInterval
			if peerStatus.LastPingRequest.IsZero() ||
				time.Since(peerStatus.LastPingRequest) >= t.InactivePeerHeartBeatInterval {
				t.pingPeer(peerIP, peerStatus)
			}
		} else {
			// TODO: optimize calling of ping if we've talked to this IP in the mean time
			t.pingPeer(peerIP, peerStatus)
		}

	}
}

// wrapper around ping for updating state & event
func (t *Tamed) pingPeer(peerIP string, peerStatus *PeerStatus) {
	t.Logf("Pinging %v for membership\n", peerIP)
	t.bc.Ping(peerIP)
	pingTime := time.Now()
	t.informListener(Notify{PingRequest: NewPingRequest(peerStatus, pingTime)})
	// update this after informing listener about ping request, else we'll update the last ping request
	peerStatus.LastPingRequest = pingTime
}

// notify listeners if any
func (t *Tamed) informListener(notify Notify) {
	if t.listener != nil {
		t.listener <- notify
	}
}

// runNotificationReader listens for any message from tailscale daemon and calls handleNotificationCallback
func (t *Tamed) runNotificationReader(ctx context.Context) {

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// read from connection
			msg, err := ipn.ReadMsg(t.conn)
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					// ignorable error
					t.Logf("use of closed connection\n")
				} else {
					log.Fatalf("error while reading notification: %v\n", err)
				}
			}
			if ctx.Err() == nil {
				// send notification message to the callback
				t.bc.GotNotifyMsg(msg)
			}
		}
	}
}

func (t *Tamed) close() {
	_ = t.conn.Close()
	t.statusTicker.Stop()
	if t.listener != nil {
		ch := t.listener
		t.listener = nil
		close(ch)
	}
}
