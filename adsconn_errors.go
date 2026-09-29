package ads

import (
	"errors"
)

// ErrTransportClosed is returned by every Client method after the underlying
// TCP transport has been closed (Close called, drop detected, dial failed).
// Callers reconstruct a new *Client to re-establish.
var ErrTransportClosed = errors.New("ads: client transport closed")

// ErrRouteNotServed reports a connection that was dropped without ever carrying
// an AMS frame: the addressing or the route is the thing to look at. Distinct
// from ErrEstablishedDropped so a consumer can branch without matching strings.
var ErrRouteNotServed = errors.New("connection dropped before the PLC served any frame")

// ErrRouterUnresponsive reports a PLC whose AMS router answered nothing, on TCP
// or UDP 48899, for long enough that the route could not be checked. Nothing to
// fix: retry. Measured at ~8s after a client restart on a TC3 4024.
var ErrRouterUnresponsive = errors.New("the PLC's AMS router answered nothing; it is briefly unreachable, not misconfigured")

// ErrEstablishedDropped reports a connection that had been carrying AMS frames
// and was then dropped by the PLC or something on the path. The route
// demonstrably existed, so route tables are the wrong place to look; eviction by
// another client on this host IP, a runtime restart, or the network path are the
// candidates.
var ErrEstablishedDropped = errors.New("established connection dropped by the PLC or the network")
