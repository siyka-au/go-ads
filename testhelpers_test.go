package ads

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// --- Notification packet builders ---

// buildNotificationPacket constructs a valid ADS DeviceNotification payload
// with one stamp containing one sample.
func buildNotificationPacket(handle uint32, timestamp uint64, data []byte) []byte {
	buf := new(bytes.Buffer)
	// NotificationStream: Length + Stamps
	streamLen := uint32(8 + 12 + 8 + len(data)) // stream header + stamp header + sample header + data
	binary.Write(buf, binary.LittleEndian, streamLen)
	binary.Write(buf, binary.LittleEndian, uint32(1)) // 1 stamp

	// StampHeader: Timestamp + Samples
	binary.Write(buf, binary.LittleEndian, timestamp)
	binary.Write(buf, binary.LittleEndian, uint32(1)) // 1 sample

	// NotificationSample: Handle + Size
	binary.Write(buf, binary.LittleEndian, handle)
	binary.Write(buf, binary.LittleEndian, uint32(len(data)))

	// Data
	buf.Write(data)
	return buf.Bytes()
}

func buildNotificationPacketMultiSample(stamps []struct {
	timestamp uint64
	samples   []struct {
		handle uint32
		data   []byte
	}
},
) []byte {
	// Calculate total length
	buf := new(bytes.Buffer)
	totalLen := uint32(8) // stream header
	for _, s := range stamps {
		totalLen += 12 // stamp header
		for _, samp := range s.samples {
			totalLen += 8 + uint32(len(samp.data)) // sample header + data
		}
	}
	binary.Write(buf, binary.LittleEndian, totalLen)
	binary.Write(buf, binary.LittleEndian, uint32(len(stamps)))

	for _, s := range stamps {
		binary.Write(buf, binary.LittleEndian, s.timestamp)
		binary.Write(buf, binary.LittleEndian, uint32(len(s.samples)))
		for _, samp := range s.samples {
			binary.Write(buf, binary.LittleEndian, samp.handle)
			binary.Write(buf, binary.LittleEndian, uint32(len(samp.data)))
			buf.Write(samp.data)
		}
	}
	return buf.Bytes()
}

// testEndpoint returns the conventional AMSEndpoint used by unit tests:
// loopback IP, TwinCAT TCP default port, fixed AMS NetID 1.2.3.4.1.1,
// AMS port 851 (PortR0PlcTc3). Tests that need a different target should
// build their own AMSEndpoint inline.
func testEndpoint() AMSEndpoint {
	return AMSEndpoint{
		IP:   "127.0.0.1",
		Port: 48898,
		AMS:  ams.Address{NetID: [6]byte{1, 2, 3, 4, 1, 1}, Port: 851},
	}
}

// newTestConnection creates a minimal Session for unit testing notification parsing.
// The Session has a synthetic *Client wired with its handleNotification installed
// so packet-level tests can drive `conn.client.Load().deviceNotification(ctx, packet)`
// and exercise the cache-aware handler.
func newTestConnection() *Session {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &Session{
		lifecycle:     &sessionLifecycle{ctx: ctx, shutdown: cancel},
		notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		logger:        slog.Default(),
	}
	conn.client.Store(&Client{
		logger: conn.logger,
		ctx:    ctx,
		cancel: cancel,
	})
	conn.client.Load().SetNotificationHandler(conn.handleNotification)
	return conn
}

// drivePacket feeds a wire-format DeviceNotification packet into the
// Client.deviceNotification decoder, which then dispatches to
// Session.handleNotification via the installed callback.
func (conn *Session) drivePacket(ctx context.Context, packet []byte) error {
	return conn.client.Load().deviceNotification(ctx, packet)
}

// --- helpers for the AMS peer-listener tests ---

// freeLocalPort reserves and releases a loopback port, returning its number. The
// small race (something else could take it) is acceptable in tests and keeps the
// stub and the listener agreeing on one number.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		t.Fatalf("unexpected addr type %T", ln.Addr())
	}
	_ = ln.Close()
	return addr.Port
}

func localAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}
