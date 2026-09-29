package ads

import (
	"context"
	"fmt"
	"time"

	"github.com/siyka-au/go-ads/v3/router"
)

// discoverTarget fills a missing target NetID or port from the identify response.
// A wrong NetID is the most common ADS misconfiguration and the hardest to see --
// the router accepts the socket then drops every request.
func (sess *Session) discoverTarget(ctx context.Context) error {
	id, err := router.Identify(ctx, sess.ip, sess.routerOptions()...)
	if err != nil {
		return fmt.Errorf("ads: NewSession: target AMS address incomplete and discovery failed "+
			"(set remote.AMS explicitly if the device does not answer the identify service): %w", err)
	}
	return sess.applyDiscoveredIdentity(id)
}

// applyDiscoveredIdentity fills the missing halves of the target address from a
// probe result. Split from the round-trip so the decisions are testable without
// a device.
func (sess *Session) applyDiscoveredIdentity(id router.Identity) error {
	if sess.target.NetID == [6]byte{} {
		sess.target.NetID = id.Address.NetID
	}
	if sess.target.Port == 0 {
		// The port is a per-major-version convention, so with no version reported it
		// is not a convention at all: planting the TC3 default would silently
		// address a runtime that may not exist. Refuse and say what to do.
		if !id.HasVersion() {
			return fmt.Errorf("ads: NewSession: %s (%s) reported no TwinCAT version, so the runtime "+
				"AMS port cannot be inferred; set remote.AMS.Port explicitly (801 on TwinCAT 2, 851 on TwinCAT 3)",
				sess.ip, id.HostName)
		}
		// Logged because a multi-runtime project needs 811/852/... and must
		// override it.
		sess.target.Port = id.RuntimePort()
	}
	sess.logger.Info("discovered target AMS address",
		"host", sess.ip,
		"netID", sess.target.NetID.String(),
		"port", sess.target.Port,
		"hostName", id.HostName,
		"twinCAT", id.Version())
	return nil
}

// targetVerifyTimeout bounds the verification round-trip. Shorter than
// identifyTimeout because verification is optional: a device that does not
// answer must cost a caller who already knows the address almost nothing.
const targetVerifyTimeout = time.Second

// verifyTarget compares the device's own NetID with the caller's, turning ADS's
// worst failure -- socket accepted, every request silently dropped -- into a named
// answer. An unanswered probe is never a failure in any mode: a device can serve
// TCP 48898 with UDP 48899 firewalled off. Only a definite mismatch is reported.
func (sess *Session) verifyTarget(ctx context.Context) error {
	verifyCtx, cancel := context.WithTimeout(ctx, targetVerifyTimeout)
	defer cancel()
	id, err := router.Identify(verifyCtx, sess.ip, sess.routerOptions()...)
	if err != nil {
		// Info, not Debug: the operator needs to know the guard did not run.
		// Silence here would read as "verified" at default log level.
		sess.logger.Info("target NetID not verified — device did not answer the identify service (UDP firewalled?); continuing",
			"host", sess.ip, "router_port", sess.effectiveRouterPort(),
			"target", sess.target.NetID.String(), "error", err)
		return nil
	}
	return sess.applyTargetCheck(id)
}

// applyTargetCheck decides what a verification result means. Split from the
// round-trip so the policy is testable without a device.
func (sess *Session) applyTargetCheck(id router.Identity) error {
	if id.Address.NetID == sess.target.NetID {
		sess.logger.Debug("target NetID confirmed by device",
			"host", sess.ip, "netID", sess.target.NetID.String(),
			"hostName", id.HostName, "twinCAT", id.Version())
		return nil
	}
	// A mismatch is usually a stale NetID, but it is also what a legitimate routed
	// setup looks like: behind a gateway the NetID you want is not the responder's.
	// Nothing separates the two, so the default warns rather than refuses.
	const hint = "usually a wrong or stale target NetID; legitimate when this host is a router and the target sits behind it"
	if sess.targetCheck == TargetCheckError {
		return fmt.Errorf("ads: target NetID %s does not match the NetID %s reported by %s (%s, TwinCAT %s): %s",
			sess.target.NetID.String(), id.Address.NetID.String(), sess.ip, id.HostName, id.Version(), hint)
	}
	sess.logger.Warn("target NetID differs from the NetID this device reports for itself",
		"host", sess.ip,
		"configured", sess.target.NetID.String(),
		"reported", id.Address.NetID.String(),
		"hostName", id.HostName,
		"twinCAT", id.Version(),
		"hint", hint)
	return nil
}
