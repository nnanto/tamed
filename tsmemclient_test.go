package tamp

import (
	"context"
	"github.com/stretchr/testify/require"
	"log"
	"testing"
	"time"
)

//TODO: change this by using fake ts daemon if possible
func TestStart(t *testing.T) {
	ctx, done := context.WithCancel(context.Background())
	op := DefaultOptions()
	op.InactivityTimeLimit = 10 * time.Second
	op.InactivePeerHeartBeatInterval = 10 * time.Second
	op.ListenerCh = make(chan Notify, 1)
	op.Logger = log.Printf
	tsc := NewTailScaleMemClient(ctx, op)
	var statusTicks uint64 = 0
	go func() {
		for n := range op.ListenerCh {
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
					require.GreaterOrEqual(t, pr.CurrentPingAt.Unix(),
						pr.PrevPingAt.Add(op.HeartbeatInterval).Unix())
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

//func TestTSMemClient(t *testing.T) {
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//
//	td := t.TempDir()
//	socketPath := filepath.Join(td, "tailscale.sock")
//
//	logf := func(format string, args ...interface{}) {
//		format = strings.TrimRight(format, "\n")
//		t.Logf(format, args...)
//	}
//
//	connect := func() {
//		for i := 1; i <= 2; i++ {
//			logf("connect %d ...", i)
//			op := DefaultOptions()
//			op.InactivityTimeLimit = 10 * time.Second
//			op.InactivePeerHeartBeatInterval = 10 * time.Second
//			op.TailScaleSocket = socketPath
//			op.ListenerCh = make(chan Notify, 1)
//
//			tsc := NewTailScaleMemClient(ctx, op)
//			go func(conn int) {
//				for n := range op.ListenerCh  {
//					if n.PingResult != nil {
//						logf("Active Peers Length for Conn %v: %v", conn, len(tsc.ActivePeers()))
//					}
//				}
//			}(i)
//
//		}
//	}
//
//	logTriggerTestf := func(format string, args ...interface{}) {
//		logf(format, args...)
//		if strings.HasPrefix(format, "Listening on ") {
//			go connect()
//		}
//	}
//
//	eng, err := wgengine.NewFakeUserspaceEngine(logf, 0)
//	if err != nil {
//		t.Fatal(err)
//	}
//	defer eng.Close()
//
//	opts := ipnserver.TSMOption{
//		SocketPath: socketPath,
//	}
//	t.Logf("pre-Run")
//	err = ipnserver.Run(ctx, logTriggerTestf, "dummy_logid", ipnserver.FixedEngine(eng), opts)
//	t.Logf("ipnserver.Run = %v", err)
//}
