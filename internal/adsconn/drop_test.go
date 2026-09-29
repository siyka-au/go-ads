package adsconn

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/internal/testlog"

	"github.com/siyka-au/go-ads/v3/ams"
)

func TestDropVerdict_FramesOnEitherSocketMeanEstablished(t *testing.T) {
	tests := []struct {
		name    string
		primary uint64
		peer    uint64
		want    bool
	}{
		{name: "no frames at all", primary: 0, peer: 0, want: false},
		{name: "frames on our own connection", primary: 3, peer: 0, want: true},
		{
			// The case a primary-only counter gets wrong. On TC3.1.4026/RTOS the PLC
			// answers only on the connection IT opens, so a healthy routed session
			// decodes zero frames on its primary for its entire life; judging those
			// drops "never served" would give a whole device class the route-suspect
			// diagnosis and the slow backoff.
			name: "frames only on the connection the PLC opened to us",
			peer: 5, want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c Conn
			c.framesPrimary.Store(tc.primary)
			c.framesPeer.Store(tc.peer)
			if got := c.Established(); got != tc.want {
				t.Errorf("wasEstablished() = %v, want %v (framesPrimary=%d framesPeer=%d)",
					got, tc.want, tc.primary, tc.peer)
			}
		})
	}
}

// TestDropSentinels_AreDistinct: the two verdicts must be mutually exclusive, or
// a consumer branching on them gets both paths.
func TestDropSentinels_AreDistinct(t *testing.T) {
	if errors.Is(ErrRouteNotServed, ErrEstablishedDropped) || errors.Is(ErrEstablishedDropped, ErrRouteNotServed) {
		t.Error("the two drop sentinels match each other; a consumer cannot branch on them")
	}
	wrapped := errors.Join(ErrEstablishedDropped, io.EOF)
	if !errors.Is(wrapped, ErrEstablishedDropped) {
		t.Error("ErrEstablishedDropped does not survive wrapping")
	}
	if errors.Is(wrapped, ErrRouteNotServed) {
		t.Error("an established-drop error also matches ErrRouteNotServed")
	}
}

// TestLogDropVerdict_CarriesTheLocalPortAndFrameCounts.
//
// Every drop investigated in the field needed the ephemeral port to correlate the
// event against a packet capture, and it was only at Debug — which
// `debug_level: true` then rotated away every ~9 seconds. The counts go in the
// same line because they are the evidence behind the verdict.
func TestLogDropVerdict_CarriesTheLocalPortAndFrameCounts(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	logs := &testlog.Handler{}
	c, err := dialTest(srv.Host, srv.Port, ams.Address{}, ams.Address{}, 2*time.Second, slog.New(logs))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	port := c.LocalPort()
	if port == 0 {
		t.Fatal("localPort() returned 0 for a live connection")
	}
	c.logDropVerdict(io.EOF)

	rec := logs.FindByMessage("transport down")
	if rec == nil {
		t.Fatal(`no drop record containing "transport down" — the AST guard and two message-matching tests depend on that substring`)
	}
	if !rec.HasAttr("localPort") {
		t.Error("the drop line carries no localPort: a packet capture cannot be correlated after the fact")
	}
	if !rec.HasAttr("framesPrimary") || !rec.HasAttr("framesPeer") {
		t.Error("the drop line carries no frame counts: the verdict cannot be checked from the log")
	}
}

// TestLogDropVerdict_EstablishedDropDoesNotBlameTheRoute: the whole point of the
// split. A connection that carried frames was being served, so the route is the
// wrong thing to send the reader after.
func TestLogDropVerdict_EstablishedDropDoesNotBlameTheRoute(t *testing.T) {
	logs := &testlog.Handler{}
	var c Conn
	c.logger = slog.New(logs)
	c.ctx = context.Background()
	c.tx = &Transport{}
	c.framesPrimary.Store(7)

	c.logDropVerdict(io.EOF)

	rec := logs.FindByMessage("PLC dropped an established connection, transport down")
	if rec == nil {
		t.Fatal("an established drop was not reported as one")
	}
	if hint := rec.Attr("hint"); hint == "" {
		t.Fatal("the established-drop line carries no hint")
	}
	if got := rec.Attr("hint"); !strings.Contains(got, "per-host TCP slot") {
		t.Errorf("the established-drop hint does not name eviction, which is the top suspect: %q", got)
	}
	if logs.FindByMessage("PLC closed connection, transport down") != nil {
		t.Error("an established drop also produced the route-suspect line")
	}
}
