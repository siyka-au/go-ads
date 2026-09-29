package router

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/logging"
)

// AMS router protocol constants shared by both services.
const (
	routeCookie     = 0x71146603
	responseFlag    = 0x80000000
	serviceAddRoute = 6

	tagResponseError uint16 = 1
	tagPassword      uint16 = 2
	tagComputerName  uint16 = 5
	tagNetID         uint16 = 7
	tagRouteName     uint16 = 12
	tagUsername      uint16 = 13
)

const (
	// routeRegisterAttempts / RouteTimeout mirror identify's retransmit policy.
	// UDP on a plant network drops datagrams, and a single loss here used to fail
	// the whole connect.
	routeRegisterAttempts = 3
	// RouteTimeout is the default budget for AddRoute, retransmits included. A
	// context deadline that is sooner wins.
	RouteTimeout = 6 * time.Second
)

// Route is a route to register on a device: "reach NetID LocalNetID at address
// ComputerName".
type Route struct {
	// Name labels the route in the device's route table.
	Name string
	// LocalNetID is this host's AMS NetID, the one ADS requests will come from.
	LocalNetID ams.NetID
	// ComputerName is the address the device should use to reach this host,
	// normally its IP.
	ComputerName string
	// Username and Password are credentials of an account on the device.
	//
	// Security: the protocol sends them in cleartext and offers no encrypted
	// alternative. Use on trusted networks only.
	Username string
	Password string
}

// LogValue keeps the password out of logs.
func (r Route) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", r.Name),
		slog.String("localNetID", r.LocalNetID.String()),
		slog.String("computerName", r.ComputerName),
		slog.Bool("hasAuth", r.Username != ""),
	)
}

// AddRoute registers r on the device at host over UDP. host may carry the
// router's port ("10.0.0.5:6499") when the device is behind NAT. It gives up
// after RouteTimeout, or at ctx's deadline if that is sooner, and returns the
// device's refusal as an error.
func AddRoute(ctx context.Context, host string, r Route, opts ...Option) error {
	h, cfg, err := resolve(host, opts)
	if err != nil {
		return fmt.Errorf("add route: %w", err)
	}
	return addRoute(ctx, cfg, h, r)
}

// addRoute sends the registration. The UDP source matters on a multi-homed
// host: TC3 records the route against the UDP SOURCE IP, not the computerName
// tag, so letting the OS choose registers whichever NIC wins the metric and a
// session on the other one is reset despite a successful registration.
func addRoute(ctx context.Context, cfg config, remoteHost string, r Route) error {
	logger := cfg.logger
	logger.Info("registering route", "remoteHost", remoteHost, "route", r)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("add route: %w", err)
	}
	addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(remoteHost, fmt.Sprint(cfg.port)))
	if err != nil {
		return fmt.Errorf("failed to resolve remote host: %w", err)
	}

	var laddr *net.UDPAddr
	if cfg.localIP != nil {
		laddr = &net.UDPAddr{IP: cfg.localIP}
	}
	conn, err := net.DialUDP("udp4", laddr, addr)
	if err != nil {
		return fmt.Errorf("failed to dial UDP: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := interruptOnDone(ctx, conn)
	defer stop()

	// Random invokeID, checked against the echo in the response. Defends against
	// UDP spoofing on the local network — an attacker would need to predict the
	// per-call value to inject a fake "success" response.
	var invokeIDBuf [4]byte
	if _, err := cryptorand.Read(invokeIDBuf[:]); err != nil {
		return fmt.Errorf("generate invokeID: %w", err)
	}
	invokeID := binary.LittleEndian.Uint32(invokeIDBuf[:])

	packet := buildRoutePacket(r, invokeID)

	// Retransmit, like identify does, and for the measured reason: a single dropped
	// datagram was seen failing NewSession outright, and this runs on the same plant
	// networks over the same shared port 48899. Registration is the costlier failure
	// of the two, because Connect aborts on it.
	//
	// Same invokeID across attempts, so a late answer to an earlier one still counts;
	// and unrelated datagrams on the shared port are skipped rather than parsed,
	// which previously turned somebody else's identify reply into a registration
	// failure.
	respBuf := make([]byte, 2048)
	deadline := time.Now().Add(RouteTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	var lastErr error
	for attempt := 1; attempt <= routeRegisterAttempts; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		window := remaining / time.Duration(routeRegisterAttempts-attempt+1)
		if attempt == routeRegisterAttempts {
			window = remaining
		}
		if _, err = conn.Write(packet); err != nil {
			return fmt.Errorf("failed to send route request: %w", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
			return fmt.Errorf("failed to set read deadline: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("add route: %w", err)
		}
		for {
			n, rerr := conn.Read(respBuf)
			if rerr != nil {
				if cerr := ctx.Err(); cerr != nil {
					return fmt.Errorf("add route: %w", cerr)
				}
				lastErr = rerr
				if !errors.Is(rerr, os.ErrDeadlineExceeded) {
					// Not the window closing. On a connected UDP socket an ICMP
					// port-unreachable arrives as an immediate error, so retransmitting
					// would fire all three attempts in milliseconds and report a
					// timeout for what is really "nothing is listening there".
					return fmt.Errorf("route registration: %w", rerr)
				}
				break // window expired: retransmit
			}
			if n == len(respBuf) {
				return fmt.Errorf("route response of %d bytes filled the read buffer", n)
			}
			if !routeResponseIsOurs(respBuf[:n], invokeID) {
				// Not an answer to this request: keep reading inside the window.
				// Rare on a connected socket, which only receives from the address we
				// dialled, but a late duplicate of an earlier attempt lands here.
				logger.Debug("route registration: ignoring a datagram that is not our answer",
					"bytes", n)
				continue
			}
			perr := parseRouteResponse(logger, respBuf[:n], invokeID)
			if perr == nil {
				if attempt > 1 {
					logger.Info("route registered after a retransmit", "attempts", attempt)
				}
				return nil
			}
			// Ours, and it says no. Retransmitting cannot change that answer, and
			// doing so put the route password on the wire twice more and made the
			// caller wait out the whole budget for a verdict already in hand.
			return perr
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no answer")
	}
	return fmt.Errorf("route registration got no usable answer in %v over %d attempts: %w",
		RouteTimeout, routeRegisterAttempts, lastErr)
}

// routeResponseIsOurs reports whether a datagram is the answer to OUR registration
// request, independent of whether that answer is success or refusal.
//
// Identify keeps this split deliberately: ownership is a header question
// (cookie, invokeID, service id), and only once a datagram is ours does its
// content decide the outcome. Conflating the two meant a refusal — a wrong
// password, say — was treated as somebody else's traffic and retransmitted until
// the budget ran out.
func routeResponseIsOurs(data []byte, invokeID uint32) bool {
	if len(data) < 12 {
		return false
	}
	if binary.LittleEndian.Uint32(data[0:4]) != routeCookie {
		return false
	}
	if binary.LittleEndian.Uint32(data[4:8]) != invokeID {
		return false
	}
	// The RESPONSE flag, as parseRouteResponse below also checks.
	return binary.LittleEndian.Uint32(data[8:12]) == (responseFlag | serviceAddRoute)
}

// buildRoutePacket constructs a UDP route registration packet. invokeID
// identifies the request and is echoed in the response; use a random value
// from crypto/rand to defend against UDP spoofing on the local network.
func buildRoutePacket(r Route, invokeID uint32) []byte {
	tags := [][]byte{
		buildTag(tagNetID, r.LocalNetID[:]),
		buildTag(tagPassword, appendNull([]byte(r.Password))),
		buildTag(tagComputerName, appendNull([]byte(r.ComputerName))),
		buildTag(tagRouteName, appendNull([]byte(r.Name))),
		buildTag(tagUsername, appendNull([]byte(r.Username))),
	}

	var tagsData []byte
	for _, tag := range tags {
		tagsData = append(tagsData, tag...)
	}

	// Header: cookie(4) + invokeID(4) + serviceId(4) + AmsAddr(8) + tagCount(4)
	header := make([]byte, 24)
	binary.LittleEndian.PutUint32(header[0:], routeCookie)
	binary.LittleEndian.PutUint32(header[4:], invokeID)
	binary.LittleEndian.PutUint32(header[8:], serviceAddRoute)
	// AmsAddr: NetID(6) + Port(2) — port is 0 per Beckhoff spec
	copy(header[12:18], r.LocalNetID[:])
	binary.LittleEndian.PutUint16(header[18:], 0)
	binary.LittleEndian.PutUint32(header[20:], uint32(len(tags)))

	return append(header, tagsData...)
}

// buildTag creates a single tag: tagID(2) + length(2) + data.
func buildTag(tagID uint16, data []byte) []byte {
	tag := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint16(tag[0:], tagID)
	binary.LittleEndian.PutUint16(tag[2:], uint16(len(data)))
	copy(tag[4:], data)
	return tag
}

// appendNull appends a null terminator to a byte slice.
func appendNull(data []byte) []byte {
	return append(data, 0)
}

// parseRouteResponse validates the route registration response.
// Response format: cookie(4) + invokeID(4) + serviceId(4) + AmsAddr(8) + tagCount(4) + tags...
//
// expectedInvokeID is the value sent in the request; the device echoes it and a
// mismatch is rejected as a possible spoof.
func parseRouteResponse(logger *slog.Logger, data []byte, expectedInvokeID uint32) error {
	logger = logging.Or(logger)
	logger.Debug("route response raw bytes", logging.HexAttr("response", data), "length", len(data))

	if len(data) < 24 {
		return fmt.Errorf("route response too short: %d bytes", len(data))
	}

	cookie := binary.LittleEndian.Uint32(data[0:])
	if cookie != routeCookie {
		return fmt.Errorf("unexpected route response cookie: 0x%08X", cookie)
	}

	gotInvokeID := binary.LittleEndian.Uint32(data[4:])
	if gotInvokeID != expectedInvokeID {
		return fmt.Errorf("route response invokeID mismatch: got 0x%08X, expected 0x%08X (possible spoof or PLC misbehavior)", gotInvokeID, expectedInvokeID)
	}

	serviceID := binary.LittleEndian.Uint32(data[8:])
	if serviceID != (responseFlag | serviceAddRoute) {
		return fmt.Errorf("unexpected route response serviceId: 0x%08X", serviceID)
	}

	// Skip AmsAddr (8 bytes at offset 12), tagCount is at offset 20
	tagCount := binary.LittleEndian.Uint32(data[20:])
	logger.Debug("route response tags", "tagCount", tagCount)
	offset := 24
	for i := uint32(0); i < tagCount; i++ {
		if offset+4 > len(data) {
			return fmt.Errorf("route response truncated: incomplete tag %d header", i)
		}
		tid := binary.LittleEndian.Uint16(data[offset:])
		tlen := binary.LittleEndian.Uint16(data[offset+2:])
		offset += 4
		if offset+int(tlen) > len(data) {
			return fmt.Errorf("route response truncated: tag %d data exceeds response", tid)
		}
		logger.Debug("route response tag", "tagID", tid, "tagLen", tlen, logging.HexAttr("tagData", data[offset:offset+int(tlen)]))
		if tid == tagResponseError && tlen >= 4 {
			errCode := binary.LittleEndian.Uint32(data[offset:])
			if errCode != 0 {
				return fmt.Errorf("route registration failed with error code: %d", errCode)
			}
			logger.Info("route registration successful")
			return nil
		}
		offset += int(tlen)
	}

	// No tagResponseError observed. Beckhoff's documented happy-path always
	// includes the tag (errCode=0 on success). Treating its absence as
	// success masked real failures where the PLC sent a malformed or truncated
	// response — caller would observe Connect() succeed and then every
	// subsequent ADS command fail with ReturnCodeGlobalTargetNotFound.
	return fmt.Errorf("route registration response had no error tag (response truncated or malformed)")
}
