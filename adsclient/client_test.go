package adsclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/adsconn"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
)

func TestDial_ReadAndClose(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{0x42}
	})

	ctx := context.Background()
	c, err := Dial(ctx, srv.Host, ams.Address{NetID: ams.NetID{5, 1, 2, 3, 1, 1}, Port: 851},
		WithPort(srv.Port), WithRequestTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	got, err := c.Read(ctx, ams.GroupSymbolVersion, 0, 1)
	if err != nil || len(got) != 1 || got[0] != 0x42 {
		t.Fatalf("Read = %v, %v; want [0x42]", got, err)
	}
	v, err := c.SymbolVersion(ctx)
	if err != nil || v != 0x42 {
		t.Fatalf("SymbolVersion = %d, %v; want 0x42", v, err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Read(ctx, ams.GroupSymbolVersion, 0, 1); !errors.Is(err, ErrTransportClosed) {
		t.Errorf("Read after Close = %v, want ErrTransportClosed", err)
	}
}

func TestDial_Unreachable(t *testing.T) {
	_, err := Dial(context.Background(), "127.0.0.1", ams.Address{}, WithPort(1), WithRequestTimeout(250*time.Millisecond))
	if err == nil {
		t.Skip("port 1 unexpectedly accepted a connection on this host")
	}
}

func TestBoundClient_FollowsTheCurrentConnection(t *testing.T) {
	// A Client bound to a session reports ErrTransportClosed while there is no
	// connection, rather than holding on to a dead one.
	c := &Client{conn: func() *adsconn.Conn { return nil }}
	if _, err := c.ReadState(context.Background()); !errors.Is(err, ErrTransportClosed) {
		t.Errorf("ReadState with no connection = %v, want ErrTransportClosed", err)
	}
	if err := c.SetNotificationHandler(func(uint32, time.Time, []byte) {}); err == nil {
		t.Error("a session-bound Client accepted a notification handler; the session owns that routing")
	}
}
