package server

import (
	"context"
	"github.com/nnanto/tamed/client"
	"log"
	"runtime"
	"strings"
	"tailscale.com/control/controlclient"
	"tailscale.com/ipn/ipnserver"
	"tailscale.com/paths"
	"tailscale.com/wgengine"
)

func defaultTunName() string {
	switch runtime.GOOS {
	case "darwin":
		return "utun12"
	case "openbsd":
		return "tun"
	case "windows":
		return "Tailscale"
	}
	return "tailscale0"
}

const (
	PORT = 41112
	StateFilePath = "/var/lib/tailscale/tailscaled.state"
)

const (
	autoStartStateKey     = "_daemon"
	startIndicatingLogMsg = "Listening on " // log message indicating start of server
	authLoginLogMsg       = "control: AuthURL is "
	authenticatedLogMsg   = "control: authRoutine"
	logCollectionName     = "tailnode.log.tailscale.io"
)

type Notify struct {
	Started       *string
	LoginURL      *string
	Authenticated *string
	Stopped       *string
}

// Start starts the server. Listen on readyCh to
func Start(ctx context.Context, logf client.LoggerFunc, readyCh chan<- Notify) {
	opts := ipnserver.Options{
		SocketPath:         paths.DefaultTailscaledSocket(),
		Port:               PORT,
		StatePath:          StateFilePath,
		AutostartStateKey:  autoStartStateKey,
		LegacyConfigPath:   paths.LegacyConfigPath(),
		SurviveDisconnects: true,
		DebugMux:           nil,
	}

	lm := func(format string, args ...interface{}) {

		if strings.HasPrefix(format, startIndicatingLogMsg) {
			readyCh <- Notify{Started: &format}
		} else if strings.HasPrefix(format, authLoginLogMsg) {
			url := args[0].(string)
			readyCh <- Notify{LoginURL: &url}
		} else if len(args) > 0 && strings.HasPrefix(format, authenticatedLogMsg) {
			if v, ok := args[0].(controlclient.State); ok {
				if v == controlclient.StateAuthenticated {
					authState := v.String()
					readyCh <- Notify{Authenticated: &authState}
				}
			}
		}

		if logf != nil {
			logf(format, args...)
		}
	}

	//pol := logpolicy.New(logCollectionName)
	e, _ := wgengine.NewUserspaceEngine(lm, defaultTunName(), 41641)

	err := ipnserver.Run(ctx, lm, "1234", ipnserver.FixedEngine(e), opts)
	if err != nil {
		log.Fatalf("Couldn't start tailscale server : %v", err)
	}
}
