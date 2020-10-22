package tamp

import (
	"context"
	"fmt"
	"log"
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
	ipnPeerStatus    ipnstate.PeerStatus
	ipnPingResult    ipnstate.PingResult
	lastPingRequest  time.Time
	lastPingResponse time.Time
	firstSeen        time.Time
}

func (ps *PeerStatus) String() string {
	return fmt.Sprintf("Tailscale IP: %v, Node Name: %v, lastPingResponse: %v, lastPingRequest: %v\n",
		ps.ipnPeerStatus.TailAddr, ps.ipnPeerStatus.SimpleHostName(), ps.lastPingResponse, ps.lastPingRequest)
}

type Options struct {
	InactivityTimeLimit           time.Duration // Time after which a node is considered inactive
	HeartbeatInterval             time.Duration // Interval between subsequent heartbeats
	InactivePeerHeartBeatInterval time.Duration // Time after which an inactive peer should be pinged
}

func NewOptions() *Options {
	op := &Options{
		InactivityTimeLimit:           2 * time.Minute,
		HeartbeatInterval:             5 * time.Second,
		InactivePeerHeartBeatInterval: 1 * time.Minute,
	}
	return op
}

// TailScaleMemClient is tailscale client connected to the tailScale client daemon running on the machine.
// In addition to the connection to tailScale client daemon, this also tracks the peerStatus by sending heartbeat pings
// to all peers and collecting their status periodically
type TailScaleMemClient struct {
	sync.RWMutex                          // Lock to prevent concurrent modification, mainly peers map
	conn           net.Conn               // The connection to the tailScale client daemon
	NotificationCh chan ipn.Notify        // Receives notification from tailScale client daemon
	bc             *ipn.BackendClient     // TailScale backend client to the daemon
	peers          map[string]*PeerStatus // map from TailScale IP to PeerStatus
	statusTicker   *time.Ticker           // Ticker for heartbeat ping message & status update
	*Options
}

// Start creates a new TailScaleMemClient and attaches a tailScale client to machine's running daemon
func Start(ctx context.Context, options *Options) *TailScaleMemClient {
	tsc := &TailScaleMemClient{}
	tsc.peers = make(map[string]*PeerStatus)
	if options == nil {
		options = NewOptions()
	}
	tsc.Options = options

	if err := tsc.startBackendClient(ctx); err != nil {
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

// isActive informs whether the peer is active given its lastPingResponse
func (tsc *TailScaleMemClient) isActive(ps *PeerStatus) bool {
	return !ps.lastPingResponse.IsZero() && time.Since(ps.lastPingResponse) < tsc.InactivityTimeLimit
}

func (tsc *TailScaleMemClient) startBackendClient(ctx context.Context) error {
	verifyTailScaleDaemonRunning()
	// create socket connection with ts running on machine
	var err error
	tsc.conn, err = safesocket.Connect(paths.DefaultTailscaledSocket(), 41112)
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

	// notify listeners if any
	if tsc.NotificationCh != nil {
		tsc.NotificationCh <- notify
	}

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
			// New peer found
			tsc.peers[tsip] = &PeerStatus{firstSeen: time.Now()}
		}
		// update ipnPeerStatus
		tsc.peers[tsip].ipnPeerStatus = *status
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
	ps.lastPingResponse = time.Now()
	ps.ipnPingResult = *pingResult
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
			if peerStatus.lastPingRequest.IsZero() ||
				time.Since(peerStatus.lastPingRequest) >= tsc.InactivePeerHeartBeatInterval {
				tsc.pingPeer(peerIP, peerStatus)
			}
		} else {
			tsc.pingPeer(peerIP, peerStatus)
		}

	}
}

// wrapper around ping for logging
func (tsc *TailScaleMemClient) pingPeer(peerIP string, peerStatus *PeerStatus) {
	log.Printf("Pinging %v for membership\n", peerIP)
	tsc.bc.Ping(peerIP)
	peerStatus.lastPingRequest = time.Now()
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
	close(tsc.NotificationCh)
}
