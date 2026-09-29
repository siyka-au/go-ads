package ads

import (
	"errors"

	"github.com/siyka-au/go-ads/v3/internal/adsconn"
)

// ErrDisconnected indicates the underlying TCP connection is not available —
// either Close() has been called or a reconnect has failed. Callers should use
// errors.Is(err, ErrDisconnected) to detect this case.
var ErrDisconnected = errors.New("connection is disconnected")

// ErrRuntimeNotRunning is returned by symbol and subscription calls when the
// system service reports the runtime is not in RUN. A refusal, not a retry: in
// CONFIG the runtime port does not exist, so the call cannot succeed and attempting
// it only yields a misleading AMS "port not found". The session keeps polling.
var ErrRuntimeNotRunning = errors.New("ads: PLC runtime is not in RUN")

// Connection-level errors, shared with adsclient so errors.Is works whichever
// layer returned them.
var (
	// ErrTransportClosed is returned by a request made while the connection is
	// down (between a drop and the reconnect that replaces it).
	ErrTransportClosed = adsconn.ErrTransportClosed
	// ErrRouteNotServed reports a connection that was dropped without ever
	// carrying an AMS frame: the addressing or the route is the thing to look at.
	ErrRouteNotServed = adsconn.ErrRouteNotServed
	// ErrRouterUnresponsive reports a PLC whose AMS router answered nothing for
	// long enough that the route could not be checked. Nothing to fix: retry.
	ErrRouterUnresponsive = adsconn.ErrRouterUnresponsive
	// ErrEstablishedDropped reports a connection that had been carrying frames and
	// was then dropped by the PLC or the network, so the route existed.
	ErrEstablishedDropped = adsconn.ErrEstablishedDropped
)
