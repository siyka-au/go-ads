package router

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
)

// TestParseIdentifyResponse_RealShape uses the tag set and values observed on
// hardware: a CX reporting TwinCAT 3.1.4024 as netID 5.66.133.203.1.1 on the
// router's own port 10000.
func TestParseIdentifyResponse_RealShape(t *testing.T) {
	const invokeID = 0xAABBCCDD
	resp := fakeplc.IdentifyResponse(invokeID, [6]byte{5, 66, 133, 203, 1, 1}, 10000,
		fakeplc.NameTag("CX-4285CB"),
		fakeplc.VersionTag(3, 1, 4024),
		fakeplc.Tag{ID: 4, Data: []byte{0x14, 0x01, 0, 0, 7, 0, 0, 0}}, // uninterpreted system-info blob
	)

	id, err := parseIdentifyResponse(resp, invokeID)
	if err != nil {
		t.Fatalf("parseIdentifyResponse: %v", err)
	}
	if got, want := id.Address.NetID.String(), "5.66.133.203.1.1"; got != want {
		t.Errorf("NetID = %q, want %q", got, want)
	}
	if id.Address.Port != 10000 {
		t.Errorf("Port = %d, want 10000 (the router's own port)", id.Address.Port)
	}
	if got, want := id.HostName, "CX-4285CB"; got != want {
		t.Errorf("HostName = %q, want %q (trailing NUL must be trimmed)", got, want)
	}
	if got, want := id.Version(), "3.1.4024"; got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
	if got, want := len(id.Tags), 3; got != want {
		t.Errorf("Tags len = %d, want %d (uninterpreted tags must be preserved)", got, want)
	}
	if _, ok := id.Tags[4]; !ok {
		t.Error("tag 4 missing from Tags; callers rely on raw access for platform details")
	}
}

// TestParseIdentifyResponse_Rejects covers every malformed or hostile response
// the parser must refuse rather than return a half-filled identity for.
func TestParseIdentifyResponse_Rejects(t *testing.T) {
	const invokeID = 0x11223344
	good := func() []byte {
		return fakeplc.IdentifyResponse(invokeID, [6]byte{5, 1, 2, 3, 1, 1}, 10000, fakeplc.NameTag("PLC"))
	}

	tests := []struct {
		name string
		data []byte
	}{
		{name: "too short", data: good()[:23]},
		{
			name: "wrong cookie",
			data: func() []byte { b := good(); binary.LittleEndian.PutUint32(b[0:], 0xDEADBEEF); return b }(),
		},
		{
			name: "invokeID mismatch",
			data: func() []byte { b := good(); binary.LittleEndian.PutUint32(b[4:], invokeID+1); return b }(),
		},
		{
			name: "serviceId without response flag",
			data: func() []byte { b := good(); binary.LittleEndian.PutUint32(b[8:], serviceIdentify); return b }(),
		},
		{
			name: "zero NetID",
			data: fakeplc.IdentifyResponse(invokeID, [6]byte{}, 10000),
		},
		{
			name: "tag header truncated",
			data: func() []byte { b := good(); return b[:len(b)-4] }(),
		},
		{
			name: "tag data exceeds response",
			data: func() []byte {
				b := good()
				binary.LittleEndian.PutUint16(b[26:], 0xFFFF) // tag length past the buffer
				return b
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseIdentifyResponse(tt.data, invokeID); err == nil {
				t.Error("err = nil, want rejection")
			}
		})
	}
}

// TestIdentity_RuntimePort: the identify service never reports a runtime port,
// so this is a per-major-version convention.
func TestIdentity_RuntimePort(t *testing.T) {
	tests := []struct {
		name  string
		major uint8
		want  ams.Port
	}{
		{name: "TwinCAT 2 uses 801", major: 2, want: 801},
		{name: "TwinCAT 3 uses 851", major: 3, want: 851},
		{name: "unknown major defaults to 851", major: 0, want: 851},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := Identity{Major: tt.major}
			if got := id.RuntimePort(); got != tt.want {
				t.Errorf("RuntimePort() = %d, want %d (major=%d)", got, tt.want, tt.major)
			}
		})
	}
}

// TestBuildIdentifyPacket pins the request: tag-less, zero source AmsAddr, and
// the identify service id. Every tested TwinCAT answers this, which is what
// makes discovery need no local configuration.
func TestBuildIdentifyPacket(t *testing.T) {
	const invokeID = 0x01020304
	pkt := buildIdentifyPacket(invokeID)
	if len(pkt) != 24 {
		t.Fatalf("len = %d, want 24 (header only, no tags)", len(pkt))
	}
	if got := binary.LittleEndian.Uint32(pkt[0:]); got != routeCookie {
		t.Errorf("cookie = 0x%08X, want 0x%08X", got, routeCookie)
	}
	if got := binary.LittleEndian.Uint32(pkt[4:]); got != invokeID {
		t.Errorf("invokeID = 0x%08X, want 0x%08X", got, invokeID)
	}
	if got := binary.LittleEndian.Uint32(pkt[8:]); got != serviceIdentify {
		t.Errorf("serviceId = %d, want %d", got, serviceIdentify)
	}
	for i, b := range pkt[12:20] {
		if b != 0 {
			t.Errorf("source AmsAddr byte %d = 0x%02X, want 0 (discovery must not need a local NetID)", i, b)
		}
	}
	if got := binary.LittleEndian.Uint32(pkt[20:]); got != 0 {
		t.Errorf("tagCount = %d, want 0", got)
	}
}

func flakyCX(t *testing.T) *fakeplc.Router {
	r := fakeplc.NewRouter(t)
	r.SetIdentity(fakeplc.Identity{
		NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 10000,
		Tags: []fakeplc.Tag{fakeplc.NameTag("FLAKY-CX"), fakeplc.VersionTag(3, 1, 4024)},
	})
	return r
}

// TestIdentify_RetransmitsOnPacketLoss: one lost request must not fail the
// call. This was observed for real — a dropped datagram failed NewSession
// outright, which is a bad trade for a single packet on a plant network.
func TestIdentify_RetransmitsOnPacketLoss(t *testing.T) {
	r := flakyCX(t)
	r.DropIdentifies.Store(1)
	id, err := Identify(t.Context(), r.Addr())
	if err != nil {
		t.Fatalf("identify with one dropped request: %v", err)
	}
	if got := id.Address.NetID.String(); got != "5.1.2.3.1.1" {
		t.Errorf("NetID = %s, want 5.1.2.3.1.1", got)
	}
}

// TestIdentify_SkipsUnrelatedDatagram: a stray reply on the shared port must be
// discarded, not consume the read and not be reported as malformed.
func TestIdentify_SkipsUnrelatedDatagram(t *testing.T) {
	r := flakyCX(t)
	r.StrayIdentify.Store(true)
	id, err := Identify(t.Context(), r.Host, WithPort(r.Port))
	if err != nil {
		t.Fatalf("identify with a stray datagram present: %v", err)
	}
	if got := id.HostName; got != "FLAKY-CX" {
		t.Errorf("HostName = %q, want FLAKY-CX", got)
	}
}

// TestIdentify_CancelInterruptsTheRead: cancellation must end a call waiting on
// a silent router at once, not at the end of the read window.
func TestIdentify_CancelInterruptsTheRead(t *testing.T) {
	r := fakeplc.NewRouter(t) // no identity: never answers
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := Identify(ctx, r.Addr()); err == nil {
		t.Fatal("identify against a silent router succeeded")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("returned %v after cancellation; the read was not interrupted", waited)
	}
}

// TestIdentifyResponseIsOurs covers the discriminator used to tell our answer
// from someone else's traffic.
func TestIdentifyResponseIsOurs(t *testing.T) {
	const invokeID = 0x11223344
	good := fakeplc.IdentifyResponse(invokeID, [6]byte{5, 1, 2, 3, 1, 1}, 10000)

	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{name: "our answer", data: good, want: true},
		{name: "too short", data: good[:20], want: false},
		{
			name: "another exchange's invokeID",
			data: fakeplc.IdentifyResponse(invokeID+1, [6]byte{5, 1, 2, 3, 1, 1}, 10000),
			want: false,
		},
		{
			name: "wrong cookie",
			data: func() []byte { b := append([]byte(nil), good...); binary.LittleEndian.PutUint32(b[0:], 1); return b }(),
			want: false,
		},
		{
			name: "different service",
			data: func() []byte {
				b := append([]byte(nil), good...)
				binary.LittleEndian.PutUint32(b[8:], responseFlag|serviceAddRoute)
				return b
			}(),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := identifyResponseIsOurs(tt.data, invokeID); got != tt.want {
				t.Errorf("identifyResponseIsOurs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		opts     []Option
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{name: "bare host uses the default port", host: "10.0.0.5", wantHost: "10.0.0.5", wantPort: DefaultPort},
		{name: "port in host", host: "10.0.0.5:6499", wantHost: "10.0.0.5", wantPort: 6499},
		{name: "WithPort", host: "10.0.0.5", opts: []Option{WithPort(6499)}, wantHost: "10.0.0.5", wantPort: 6499},
		{name: "both agree", host: "10.0.0.5:6499", opts: []Option{WithPort(6499)}, wantHost: "10.0.0.5", wantPort: 6499},
		{name: "both disagree", host: "10.0.0.5:6499", opts: []Option{WithPort(48899)}, wantErr: true},
		{name: "non-numeric port", host: "10.0.0.5:abc", wantErr: true},
		{name: "port out of range", host: "10.0.0.5:70000", wantErr: true},
		{name: "empty host", host: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, cfg, err := resolve(tt.host, tt.opts)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolve(%q) = %q:%d, want an error", tt.host, h, cfg.port)
				}
				return
			}
			if err != nil || h != tt.wantHost || cfg.port != tt.wantPort {
				t.Errorf("resolve(%q) = %q, %d, %v; want %q, %d", tt.host, h, cfg.port, err, tt.wantHost, tt.wantPort)
			}
		})
	}
}
