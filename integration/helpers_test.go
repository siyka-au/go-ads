//go:build integration

package integration

import (
	"testing"

	"github.com/siyka-au/go-ads/v3"

	"github.com/siyka-au/go-ads/v3/ams"
)

// liveHandles returns the handles of the subscriptions currently registered on
// the PLC.
func liveHandles(s *ads.Session) []uint32 {
	var out []uint32
	for _, sub := range s.Subscriptions() {
		if sub.Active {
			out = append(out, sub.Handle)
		}
	}
	return out
}

// liveCount is how many subscriptions are currently registered on the PLC.
func liveCount(s *ads.Session) int { return len(liveHandles(s)) }

// hasLiveHandle reports whether handle is one of the session's live
// subscriptions.
func hasLiveHandle(s *ads.Session, handle uint32) bool {
	for _, h := range liveHandles(s) {
		if h == handle {
			return true
		}
	}
	return false
}

// sumAddress is how a sum request addresses a top-level symbol: by handle when
// the session holds one, else by index group and offset.
func sumAddress(v ads.SymbolView) (group, offset uint32) {
	if v.Handle != 0 {
		return uint32(ams.GroupSymbolValueByHandle), v.Handle
	}
	return v.Group, v.Offset
}

// setupConnectionWithoutSum is setupConnection with every sum command forced onto
// its per-item fallback.
func setupConnectionWithoutSum(t *testing.T) *ads.Session {
	t.Helper()
	return setupConnectionWithDefaults(t, connDefaults{
		ip:        "192.168.3.224",
		targetAMS: "5.154.236.19.1.1",
		routeName: "go-ads-test",
	}, ads.WithoutSumCommands())
}

// defaultConn is the lab PLC setupConnection targets.
var defaultConn = connDefaults{
	ip:        "192.168.3.224",
	targetAMS: "5.154.236.19.1.1",
	routeName: "go-ads-test",
}
