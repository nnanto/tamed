package server

import (
	"context"
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
	LogId = "tamed-log-id"
)

const (
	autoStartStateKey     = "_daemon"
	startIndicatingLogMsg = "Listening on " // log message indicating start of server
	authLoginLogMsg       = "control: AuthURL is "
	authenticatedLogMsg   = "control: authRoutine"
)

type Notify struct {
	Started       *string
	LoginURL      *string
	Authenticated *string
	Stopped       *string
}

type LoggerFunc func(format string, args ...interface{})

// Start starts the server. Listen on readyCh to receive notification from server
func Start(ctx context.Context, logf LoggerFunc, readyCh chan<- Notify) error {
	opts := ipnserver.Options{
		SocketPath:         paths.DefaultTailscaledSocket(),
		Port:               PORT,
		StatePath:          StateFilePath,
		AutostartStateKey:  autoStartStateKey,
		LegacyConfigPath:   paths.LegacyConfigPath(),
		SurviveDisconnects: true,
		DebugMux:           nil,
	}

	lm := logListener(readyCh, logf)

	e, err := wgengine.NewUserspaceEngine(lm, defaultTunName(), 41641)
	if err != nil {
		return err
	}
	err = ipnserver.Run(ctx, lm, LogId, ipnserver.FixedEngine(e), opts)
	if err != nil {
		return err
	}
	return nil
}

func logListener(readyCh chan<- Notify, logf LoggerFunc) func(format string, args ...interface{}) {
	return func(format string, args ...interface{}) {

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
		// send to client logger
		if logf != nil {
			logf(format, args...)
		}
	}
}
