package tamp

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

//TODO: change this by using fake ts daemon if possible
func TestStart(t *testing.T) {
	ctx, done := context.WithCancel(context.Background())
	op := NewOptions()
	op.InactivityTimeLimit = 10 * time.Second
	op.InactivePeerHeartBeatInterval = 10 * time.Second

	tsc := NewTailScaleMemClient(ctx, op)
	tsc.ListenerCh = make(chan Notify, 1)
	var statusTicks uint64 = 0
	go func() {
		for n := range tsc.ListenerCh {
			if n.Status != nil {
				statusTicks++
				t.Logf("status - Len Peers: %v\n", len(n.Status.Peer))
			} else if n.NetMap != nil {
				t.Logf("netmap - Len Peers: %v\n", len(n.NetMap.Peers))
			} else if n.Engine != nil {
				t.Logf("engine status")
			} else if n.PingRequest != nil {
				pr := n.PingRequest

				// verify if prev ping happened at-least HeartbeatInterval before the current ping
				if !pr.PrevPingAt.IsZero() {
					require.Greater(t, pr.CurrentPingAt.Unix(),
						pr.PrevPingAt.Add(op.HeartbeatInterval-time.Second).Unix())
				}
				t.Logf("Ping Request sent to %v at %v with prev ping at %v\n", pr.TailAddr,
					pr.CurrentPingAt, pr.PrevPingAt)
			}
		}
	}()
	noOfHeartBeatsToWaitFor := time.Duration(5)
	time.Sleep(op.HeartbeatInterval * noOfHeartBeatsToWaitFor)
	// Prevent further ticks
	done()
	require.GreaterOrEqual(t, statusTicks, uint64(noOfHeartBeatsToWaitFor))
	require.GreaterOrEqual(t, len(tsc.peers), 1)
	//require.GreaterOrEqual(t, len(tsc.ActivePeers()), 1)

}
