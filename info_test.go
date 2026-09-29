package ads

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
)

// servingPLC answers sum and single notification registration with fresh
// handles, and deletes.
func servingPLC(t *testing.T) *fakeplc.PLC {
	t.Helper()
	srv := fakeplc.StartPLC(t)
	t.Cleanup(srv.Stop)
	next := uint32(0x2000)
	srv.OnWriteRead(ams.GroupSumupAddDeviceNotification, func(req []byte) []byte {
		items := make([]fakeplc.SumNotifResponse, len(req)/40)
		for i := range items {
			next++
			items[i] = fakeplc.SumNotifResponse{Error: ams.ReturnCodeNoErrors, Handle: next}
		}
		return fakeplc.SumAddNotifPayload(items)
	})
	srv.OnAddDeviceNotification(func(fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		next++
		return fakeplc.AddNotifResponse{Handle: next}
	})
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		return fakeplc.SumDeleteNotifPayload(make([]ams.ReturnCode, len(req)/4))
	})
	srv.OnDeleteDeviceNotification(func(uint32) ams.ReturnCode { return ams.ReturnCodeNoErrors })
	return srv
}

// TestSubscribeAll_ResultsCarryMetadataAndErr: a consumer labels its values from
// the result — no follow-up lookup per symbol — and checks one Err instead of
// pairing Skipped with a ReturnCode.
func TestSubscribeAll_ResultsCarryMetadataAndErr(t *testing.T) {
	srv := servingPLC(t)
	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.a", 0x3001)

	ch := make(chan *Update, 8)
	results, err := sess.SubscribeAll(context.Background(), []NotificationConfig{
		{Symbol: "MAIN.a", Mode: ams.TransModeServerOnChange},
		{Symbol: "MAIN.a", Mode: ams.TransModeServerOnChange}, // duplicate within the batch
	}, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Err != nil || results[0].Handle == 0 {
		t.Errorf("first entry = %+v, want a live handle", results[0])
	}
	if results[0].Symbol.DataType != "INT" || results[0].Symbol.Length != 2 {
		t.Errorf("first entry's metadata = %q/%d, want INT/2", results[0].Symbol.DataType, results[0].Symbol.Length)
	}
	if !errors.Is(results[1].Err, ErrNotificationDuplicate) || results[1].Handle != 0 {
		t.Errorf("duplicate entry = %+v, want ErrNotificationDuplicate and no handle", results[1])
	}
}

// TestUpdate_CarriesTheSubscribedSpelling: TC2 upper-cases names; the Update
// must come back under the name the caller subscribed with.
func TestUpdate_CarriesTheSubscribedSpelling(t *testing.T) {
	srv := servingPLC(t)
	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.COUNTER", 0x3001) // the PLC's casing

	ch := make(chan *Update, 8)
	results, err := sess.SubscribeAll(context.Background(), []NotificationConfig{
		{Symbol: "Main.counter", Mode: ams.TransModeServerOnChange},
	}, ch)
	if err != nil || results[0].Err != nil {
		t.Fatalf("SubscribeAll: %v / %v", err, results[0].Err)
	}
	sess.handleNotification(context.Background(), results[0].Handle, 0, intSample(7))
	select {
	case u := <-ch:
		if u.Symbol != "Main.counter" {
			t.Errorf("Update.Symbol = %q, want the subscribed spelling Main.counter", u.Symbol)
		}
	case <-time.After(time.Second):
		t.Fatal("no update delivered")
	}
}

// TestSubscriptions_ReportsIntentAndRegistration: the declared subscriptions,
// with the handle each is registered under, and no internal heartbeat.
func TestSubscriptions_ReportsIntentAndRegistration(t *testing.T) {
	srv := servingPLC(t)
	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.a", 0x3001)
	preSeedTypedSymbol(sess, "MAIN.b", 0x3002)

	ch := make(chan *Update, 8)
	results, err := sess.SubscribeAll(context.Background(), []NotificationConfig{
		{Symbol: "MAIN.a", Mode: ams.TransModeServerOnChange},
		{Symbol: "MAIN.b", Mode: ams.TransModeServerCycle, CycleTime: time.Second},
	}, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}

	subs := sess.Subscriptions()
	if len(subs) != 2 {
		t.Fatalf("Subscriptions() = %d entries, want 2: %+v", len(subs), subs)
	}
	for i, s := range subs {
		if !s.Active || s.Handle != results[i].Handle {
			t.Errorf("subscription %d = %+v, want active under handle %#x", i, s, results[i].Handle)
		}
	}
	if subs[1].Config.Mode != ams.TransModeServerCycle || subs[1].Config.CycleTime != time.Second {
		t.Errorf("second subscription lost its config: %+v", subs[1].Config)
	}

	if err := sess.Unsubscribe(context.Background(), results[0].Handle); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if subs := sess.Subscriptions(); len(subs) != 1 || subs[0].Config.Symbol != "MAIN.b" {
		t.Errorf("after Unsubscribe, Subscriptions() = %+v, want only MAIN.b", subs)
	}
}

// TestDoneAndErr: a consumer waits on Done rather than polling IsClosed, and
// Err says why the session ended.
func TestDoneAndErr(t *testing.T) {
	sess, err := NewSession(context.Background(), Endpoint{
		Host:   "127.0.0.1",
		Target: ams.Address{NetID: ams.NetID{5, 1, 2, 3, 1, 1}, Port: 851},
	}, WithTargetCheck(TargetCheckOff))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := sess.Err(); err != nil {
		t.Errorf("Err() before Close = %v, want nil", err)
	}
	select {
	case <-sess.Done():
		t.Fatal("Done closed before Close")
	default:
	}

	_ = sess.Close()
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed after Close")
	}
	if !errors.Is(sess.Err(), ErrClosed) {
		t.Errorf("Err() after Close = %v, want ErrClosed", sess.Err())
	}
}

// TestErr_ReportsWhyReconnectionGaveUp: a give-up is not a Close, and the cause
// has to survive a Close that follows it.
func TestErr_ReportsWhyReconnectionGaveUp(t *testing.T) {
	sess, err := NewSession(context.Background(), Endpoint{
		Host:   "127.0.0.1",
		Target: ams.Address{NetID: ams.NetID{5, 1, 2, 3, 1, 1}, Port: 851},
	}, WithTargetCheck(TargetCheckOff))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	cause := errors.New("attempts exhausted")
	_ = sess.giveUpReconnecting(cause)
	_ = sess.Close()
	if err := sess.Err(); !errors.Is(err, cause) || errors.Is(err, ErrClosed) {
		t.Errorf("Err() = %v, want the give-up cause and not ErrClosed", err)
	}
}

// TestInfo_ReportsTheConnection: Info answers what a consumer (or a test) used to
// read from private fields.
func TestInfo_ReportsTheConnection(t *testing.T) {
	srv := servingPLC(t)
	sess, _ := newWiredTestSession(t, srv)
	sess.target = ams.Address{NetID: ams.NetID{5, 1, 2, 3, 1, 1}, Port: 851}
	sess.tx.SetSource(ams.Address{NetID: ams.NetID{10, 0, 0, 9, 1, 1}, Port: 33000})

	info := sess.Info()
	if info.Target != sess.target {
		t.Errorf("Target = %v, want %v", info.Target, sess.target)
	}
	if info.Local.NetID != (ams.NetID{10, 0, 0, 9, 1, 1}) || info.Local.Port != 33000 {
		t.Errorf("Local = %v", info.Local)
	}
	if !info.LocalTCP.IsValid() || info.LocalTCP.Port() == 0 {
		t.Errorf("LocalTCP = %v, want the connection's local end", info.LocalTCP)
	}
	if info.State != sess.State() {
		t.Errorf("State = %v, want %v", info.State, sess.State())
	}
}

// TestClient_UsesTheSessionConnection: raw access goes over the session's own
// connection and fails cleanly while there is none.
func TestClient_UsesTheSessionConnection(t *testing.T) {
	srv := servingPLC(t)
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{0x42}
	})
	sess, _ := newWiredTestSession(t, srv)

	cl := sess.Client()
	v, err := cl.SymbolVersion(context.Background())
	if err != nil || v != 0x42 {
		t.Fatalf("SymbolVersion over the session = %d, %v; want 0x42", v, err)
	}
	accepts := srv.Accepts()
	if accepts != 1 {
		t.Errorf("the fake PLC saw %d connections, want 1: Client opened its own", accepts)
	}

	sess.tx.SetDisconnected(true)
	if _, err := cl.SymbolVersion(context.Background()); !errors.Is(err, ErrTransportClosed) {
		t.Errorf("SymbolVersion while disconnected = %v, want ErrTransportClosed", err)
	}
}

// TestWithoutSumCommands_ReachesEveryConn: the option has to apply to the Conn
// wired on each (re)dial, not only the first.
func TestWithoutSumCommands_ReachesEveryConn(t *testing.T) {
	srv := servingPLC(t)
	sess := newDialableTestSession(t, srv.Host, srv.Port, 0)
	WithoutSumCommands()(sess)
	for i := 0; i < 2; i++ {
		if err := sess.dialAndStart(); err != nil {
			t.Fatalf("dialAndStart: %v", err)
		}
		c := sess.client.Load()
		if c.Capabilities().SumWriteStateLoad() != 2 {
			t.Fatalf("dial %d: sum write not disabled", i)
		}
		sess.tearDownAndReset()
	}
	sess.markClosed()
}
