package fakeplc

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

// PLC is a programmable wire-level ADS PLC stub.
//
// Unlike echoServer (client_test.go), which always returns an empty Read
// response with InvokeID echoed, PLC dispatches each inbound
// AMS frame to a per-test handler keyed by (Command, group). Tests
// register handlers per command (or per WriteRead group) and the server
// constructs a canonical response from the handler's return value.
//
// Default behavior: any (cmd, group) pair without a registered handler
// returns ReturnCodeDeviceServiceNotSupported. Inbound frames are recorded
// for post-test assertions.
//
// All registration helpers are concurrency-safe and may be called BEFORE
// or AFTER StartPLC() returns; the per-conn handle goroutine
// looks up handlers under the server's mutex on every request.

// addNotifRequest mirrors the wire-level AddDeviceNotification payload.
type AddNotifRequest struct {
	Group     uint32
	Offset    uint32
	Length    uint32
	TransMode uint32
	MaxDelay  uint32 // 100ns units
	CycleTime uint32 // 100ns units
}

// addNotifResponse is what the test handler returns for AddDeviceNotification.
type AddNotifResponse struct {
	Handle uint32
	Error  ams.ReturnCode
}

// sumNotifResponse — per-item response for SumAddDeviceNotification.
type SumNotifResponse struct {
	Error  ams.ReturnCode
	Handle uint32
}

type (
	WriteReadHandler func(req []byte) []byte
	WriteHandler     func(group, offset uint32, data []byte) ams.ReturnCode
	ReadHandler      func(group, offset, length uint32) (ams.ReturnCode, []byte)
)

type PLC struct {
	Host string
	Port int
	ln   net.Listener
	wg   sync.WaitGroup

	stopOnce sync.Once
	// quit is closed by stop() so a handler parked in an injected delay leaves
	// immediately. An uninterruptible sleep ignores the socket close, so every test
	// arming a long delayBefore used to pay stop()'s full 2s wait cap — the same
	// mistake as the untracked connections above, on the other path.
	quit chan struct{}

	// dropAlwaysMu guards dropAlways and is deliberately NOT s.mu. dispatch holds
	// s.mu for the whole handler and acceptLoop takes it per accept, so anything
	// evaluated under s.mu can stall new accepts — and the test this injector
	// exists for counts accepts. A separate lock makes that interaction
	// impossible rather than merely unlikely.
	dropAlwaysMu sync.Mutex
	// dropAlways implements dropConnAlways: close the connection instead of
	// answering EVERY occurrence of a command, until stopDroppingConn clears it.
	//
	// dropConnAfter cannot stand in for this: it deletes its own key once it
	// fires, so a probe can only be made to fail at transport level ONCE. A
	// redial loop that dials on every failed probe needs the failure to repeat,
	// which is exactly the storm condition awaitRouteActive's cap exists to bound.
	dropAlways map[ams.Command]bool

	mu sync.Mutex // guards every field below

	// conns are the accepted client connections, tracked so stop() can close
	// them. Closing only the listener leaves every handle() goroutine blocked in
	// io.ReadFull until the client happens to hang up, which cost each test using
	// this stub the full 2s stop() timeout.
	conns  map[net.Conn]struct{}
	closed bool

	// Per-cmd dispatch tables. key (group) used for Read/Write/WriteRead.
	writeReadHandlers map[uint32]WriteReadHandler
	writeHandlers     map[uint32]WriteHandler
	readHandlers      map[uint32]ReadHandler

	addNotifFn    func(req AddNotifRequest) AddNotifResponse
	deleteNotifFn func(handle uint32) ams.ReturnCode

	// Optional artificial latency, keyed by (cmd, group). For commands that
	// don't carry a group (AddDeviceNotification etc.) the group is 0.
	delays map[delayKey]time.Duration

	// dropAfter/dropSeen implement dropConnAfter: close the connection instead
	// of answering the nth occurrence of a command.
	dropAfter map[ams.Command]int
	dropSeen  map[ams.Command]int

	// amsErrAfter/amsErrCode/amsErrSeen implement amsErrorAfter: answer the nth
	// and every later occurrence of a command with an AMS-header ErrorCode
	// instead of a normal reply. Models a TwinCAT system dropping into CONFIG —
	// the router keeps answering, the runtime port stops existing.
	amsErrAfter map[ams.Command]int
	amsErrCode  map[ams.Command]uint32
	amsErrSeen  map[ams.Command]int

	// closeAfterReply implements answerThenClose: answer the nth occurrence of a
	// command normally, then close the connection. Models the PLC behaviour that
	// matters most here — a reply followed immediately by a route-idle close, a
	// runtime restart, or an RST.
	closeAfterReply map[ams.Command]int

	// adsState is what ReadState reports; zero means RUN. Lets a test put the stub
	// "in CONFIG" the way a real system service reports it.
	adsState  ams.State
	replySeen map[ams.Command]int

	// Recorded inbound frames (full bytes including TCP header).
	frameBuf [][]byte

	// acceptCount is outside mu on purpose: a reconnect-storm test reads it while
	// the accept loop is running.
	acceptCount atomic.Int64

	// peerAddr, when set, makes the stub answer on a connection IT opens to that
	// address instead of on the client's connection — the behaviour measured on a
	// TC/RTOS device, which treats a registered route as a peer router. Requests
	// are still accepted on the client's connection.
	peerAddr atomic.Pointer[string]
	peerMu   sync.Mutex
	peerConn net.Conn

	// unanswered counts requests the stub processed but could not reply to
	// because the peer connection was not up yet. It is a counter rather than a
	// t.Logf because this goroutine can outlive the test, and logging after the
	// test returns kills the whole binary.
	unanswered atomic.Int64
}

type delayKey struct {
	cmd   ams.Command
	group uint32 // 0 for non-group cmds
}

func StartPLC(t *testing.T) *PLC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		t.Fatalf("unexpected addr type: %T", ln.Addr())
	}
	s := &PLC{
		Host:              addr.IP.String(),
		Port:              addr.Port,
		ln:                ln,
		writeReadHandlers: map[uint32]WriteReadHandler{},
		writeHandlers:     map[uint32]WriteHandler{},
		readHandlers:      map[uint32]ReadHandler{},
		delays:            map[delayKey]time.Duration{},
		conns:             map[net.Conn]struct{}{},
		dropAlways:        map[ams.Command]bool{},
		quit:              make(chan struct{}),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	// Belt and braces: most callers `defer srv.stop()`, but a t.Fatalf before that
	// line, or a test that forgets it, would otherwise leave the goroutines running
	// for the rest of the process. stop() is idempotent, so both can fire.
	t.Cleanup(s.Stop)
	return s
}

// stop closes the listener and every accepted connection, then waits for the
// goroutines. Idempotent: callers both `defer srv.stop()` and get a t.Cleanup.
func (s *PLC) Stop() {
	s.stopOnce.Do(func() {
		close(s.quit) // first: releases any handler sitting in an injected delay
		s.peerMu.Lock()
		if s.peerConn != nil {
			_ = s.peerConn.Close()
			s.peerConn = nil
		}
		s.peerMu.Unlock()
		_ = s.ln.Close()
		s.mu.Lock()
		s.closed = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.conns = map[net.Conn]struct{}{}
		s.mu.Unlock()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
}

// closeClientConns closes every accepted client connection and leaves the
// listener up, so the session's next dial is still served.
//
// stop() cannot stand in for this: it closes the listener too, so a session that
// is supposed to redial after the drop has nothing to redial to.
func (s *PLC) CloseClientConns() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// onWriteRead registers a handler for the given group on CommandReadWrite.
func (s *PLC) OnWriteRead(group ams.Group, fn WriteReadHandler) {
	s.mu.Lock()
	s.writeReadHandlers[uint32(group)] = fn
	s.mu.Unlock()
}

// onWrite registers a handler for the given group on CommandWrite.
func (s *PLC) OnWrite(group ams.Group, fn WriteHandler) {
	s.mu.Lock()
	s.writeHandlers[uint32(group)] = fn
	s.mu.Unlock()
}

// onRead registers a handler for the given group on CommandRead.
func (s *PLC) OnRead(group ams.Group, fn ReadHandler) {
	s.mu.Lock()
	s.readHandlers[uint32(group)] = fn
	s.mu.Unlock()
}

// onAddDeviceNotification registers the AddDeviceNotification handler.
func (s *PLC) OnAddDeviceNotification(fn func(req AddNotifRequest) AddNotifResponse) {
	s.mu.Lock()
	s.addNotifFn = fn
	s.mu.Unlock()
}

// onDeleteDeviceNotification registers the DeleteDeviceNotification handler.
func (s *PLC) OnDeleteDeviceNotification(fn func(handle uint32) ams.ReturnCode) {
	s.mu.Lock()
	s.deleteNotifFn = fn
	s.mu.Unlock()
}

// delayBefore injects artificial latency for a particular (cmd, group)
// before the handler runs. Pass group=0 for commands without an index group
// (AddDeviceNotification, DeleteDeviceNotification, ReadDeviceInfo, ReadState).
func (s *PLC) SetADSState(state ams.State) {
	s.mu.Lock()
	s.adsState = state
	s.mu.Unlock()
}

func (s *PLC) DelayBefore(cmd ams.Command, group uint32, d time.Duration) {
	s.mu.Lock()
	s.delays[delayKey{cmd: cmd, group: group}] = d
	s.mu.Unlock()
}

// dropConnAlways closes the connection instead of answering any occurrence of
// cmd, for as long as it stays armed. The listener stays up, so the client's
// next dial is still accepted — which is what makes a redial loop observable
// through accepts().
//
// Sticky on purpose. dropConnAfter disarms itself on its first firing
// (see dropAlways), so it cannot reproduce a probe that keeps failing at
// transport level.
func (s *PLC) DropConnAlways(cmd ams.Command) {
	s.dropAlwaysMu.Lock()
	s.dropAlways[cmd] = true
	s.dropAlwaysMu.Unlock()
}

// stopDroppingConn disarms dropConnAlways for cmd, so a test can let the
// session recover and assert on what it does next.
func (s *PLC) StopDroppingConn(cmd ams.Command) {
	s.dropAlwaysMu.Lock()
	delete(s.dropAlways, cmd)
	s.dropAlwaysMu.Unlock()
}

// droppingAlways reports whether cmd is currently armed for a sticky drop.
func (s *PLC) DroppingAlways(cmd ams.Command) bool {
	s.dropAlwaysMu.Lock()
	defer s.dropAlwaysMu.Unlock()
	return s.dropAlways[cmd]
}

// frames returns a snapshot of every fully-received inbound frame.
func (s *PLC) Frames() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.frameBuf))
	copy(out, s.frameBuf)
	return out
}

// accepts reports how many TCP connections the stub has accepted. A reconnect
// loop that keeps dialing shows up here even when it never gets a reply.
func (s *PLC) Accepts() int {
	return int(s.acceptCount.Load())
}

func (s *PLC) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.acceptCount.Add(1)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handle(c)
	}
}

func (s *PLC) handle(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	for {
		hdr := make([]byte, 6)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		bodyLen := binary.LittleEndian.Uint32(hdr[2:6])
		if bodyLen > 4*1024*1024 {
			return
		}
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		if len(body) < 32 {
			return
		}
		frame := append(append([]byte{}, hdr...), body...)
		s.mu.Lock()
		s.frameBuf = append(s.frameBuf, frame)
		s.mu.Unlock()

		cmd := ams.Command(binary.LittleEndian.Uint16(body[16:18]))
		invokeID := binary.LittleEndian.Uint32(body[28:32])
		payload := body[32:] // ADS request payload

		// Decode group for cmds that carry one. Read/Write/WriteRead all
		// start with Group(4) Offset(4) ...
		var group uint32
		switch cmd {
		case ams.CommandRead, ams.CommandWrite, ams.CommandReadWrite:
			if len(payload) >= 4 {
				group = binary.LittleEndian.Uint32(payload[0:4])
			}
		}

		// Honor any registered delay before dispatching.
		s.mu.Lock()
		if d := s.delays[delayKey{cmd: cmd, group: group}]; d > 0 {
			s.mu.Unlock()
			// Interruptible: tests inject hour-long delays to hold a request in
			// flight, and stop() must not have to outwait them.
			select {
			case <-time.After(d):
			case <-s.quit:
				return
			}
		} else {
			s.mu.Unlock()
		}

		// Sticky drop first, and under its own lock: this is the "the PLC will not
		// serve this route yet" shape, which repeats until the test says otherwise.
		if s.DroppingAlways(cmd) {
			return // deferred c.Close() drops it
		}

		// Drop the connection instead of answering, once the configured number
		// of this command has been seen. Models a PLC/link failure landing
		// mid-operation, which no amount of canned error codes reproduces: the
		// client sees EOF on a request it already sent.
		s.mu.Lock()
		dropAt, armed := s.dropAfter[cmd]
		if armed {
			s.dropSeen[cmd]++
			if s.dropSeen[cmd] >= dropAt {
				delete(s.dropAfter, cmd)
				s.mu.Unlock()
				return // deferred c.Close() drops it
			}
		}
		s.mu.Unlock()

		// Answer with an AMS-header ErrorCode from the nth occurrence of this
		// command onward. Sticky, unlike dropAfter: a system in CONFIG does not
		// come back on its own. The handler still runs, so a test can keep
		// counting the requests that reached the stub.
		s.mu.Lock()
		var amsErr uint32
		if amsErrAt, armed := s.amsErrAfter[cmd]; armed {
			s.amsErrSeen[cmd]++
			if s.amsErrSeen[cmd] >= amsErrAt {
				amsErr = s.amsErrCode[cmd]
			}
		}
		s.mu.Unlock()

		respPayload := s.dispatch(cmd, group, payload)
		out, werr := s.responseWriter(c)
		if werr != nil {
			// Faithful to the measured device: the request was accepted and
			// processed, the answer simply goes somewhere we cannot reach right
			// now. The client connection stays OPEN — closing it would look like a
			// transport drop, which is a different failure entirely.
			s.unanswered.Add(1)
			continue
		}
		if err := writeResponse(out, body, cmd, invokeID, respPayload, amsErr); err != nil {
			// A failed write to the stub's OWN peer connection has to invalidate it,
			// or the cached conn keeps being handed out and every later response goes
			// nowhere — one broken dial-back poisoning the rest of the test.
			s.discardPeerConn(out)
			return
		}

		// Answer-then-close: the reply is already on the wire; dropping the
		// connection now races the client's own response delivery.
		s.mu.Lock()
		closeAt, armed2 := s.closeAfterReply[cmd]
		if armed2 {
			s.replySeen[cmd]++
			if s.replySeen[cmd] >= closeAt {
				delete(s.closeAfterReply, cmd)
				s.mu.Unlock()
				return // deferred c.Close()
			}
		}
		s.mu.Unlock()
	}
}

// answerViaPeerConnection makes the stub deliver every response over a connection
// it opens to addr, leaving the client's own connection silent.
func (s *PLC) AnswerViaPeerConnection(addr string) {
	a := addr
	s.peerAddr.Store(&a)
}

// responseWriter returns where a response should be written: the client's
// connection normally, or the stub's own connection to the client when
// answerViaPeerConnection is armed.
func (s *PLC) responseWriter(client net.Conn) (net.Conn, error) {
	addr := s.peerAddr.Load()
	if addr == nil {
		return client, nil
	}
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	if s.peerConn != nil {
		return s.peerConn, nil
	}
	c, err := net.DialTimeout("tcp4", *addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	s.peerConn = c
	return c, nil
}

// discardPeerConn drops the cached peer connection if it is the one that just
// failed, so the next response redials instead of writing into a dead socket.
// Compares identity: another goroutine may already have replaced it.
func (s *PLC) discardPeerConn(failed net.Conn) {
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	if s.peerConn != nil && s.peerConn == failed {
		_ = s.peerConn.Close()
		s.peerConn = nil
	}
}

// answerThenClose answers the nth occurrence of cmd (1-based) and then closes
// the connection, then disarms.
func (s *PLC) AnswerThenClose(cmd ams.Command, n int) {
	s.mu.Lock()
	if s.closeAfterReply == nil {
		s.closeAfterReply = map[ams.Command]int{}
		s.replySeen = map[ams.Command]int{}
	}
	s.closeAfterReply[cmd] = n
	s.replySeen[cmd] = 0
	s.mu.Unlock()
}

// dropConnAfter closes the connection without answering the nth occurrence of
// cmd (1-based), then disarms. Use to land a transport failure in the middle of
// a multi-request operation.
func (s *PLC) DropConnAfter(cmd ams.Command, n int) {
	s.mu.Lock()
	if s.dropAfter == nil {
		s.dropAfter = map[ams.Command]int{}
		s.dropSeen = map[ams.Command]int{}
	}
	s.dropAfter[cmd] = n
	s.dropSeen[cmd] = 0
	s.mu.Unlock()
}

// amsErrorAfter answers the nth occurrence of cmd (1-based) and every one after
// it with code in the AMS header's ErrorCode field instead of a reply. Use to
// land a router-level rejection — a TwinCAT system in CONFIG answers 0x06 for
// every request to a runtime port — as opposed to dropConnAfter's dead socket.
//
// Deliberately sticky: unlike dropConnAfter this never disarms, because the
// condition it models does not clear itself.
func (s *PLC) AMSErrorAfter(cmd ams.Command, n int, code ams.ReturnCode) {
	s.mu.Lock()
	if s.amsErrAfter == nil {
		s.amsErrAfter = map[ams.Command]int{}
		s.amsErrCode = map[ams.Command]uint32{}
		s.amsErrSeen = map[ams.Command]int{}
	}
	s.amsErrAfter[cmd] = n
	s.amsErrCode[cmd] = uint32(code)
	s.amsErrSeen[cmd] = 0
	s.mu.Unlock()
}

func (s *PLC) dispatch(cmd ams.Command, group uint32, payload []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch cmd {
	case ams.CommandReadWrite:
		// payload: Group(4) Offset(4) ReadLen(4) WriteLen(4) Data...
		if fn := s.writeReadHandlers[group]; fn != nil {
			if len(payload) < 16 {
				return respondErrorBytes(ams.ReturnCodeDeviceInvalidSize)
			}
			writeLen := binary.LittleEndian.Uint32(payload[12:16])
			if uint64(16+writeLen) > uint64(len(payload)) {
				return respondErrorBytes(ams.ReturnCodeDeviceInvalidSize)
			}
			req := payload[16 : 16+writeLen]
			data := fn(req)
			return buildReadResponse(ams.ReturnCodeNoErrors, data)
		}
		return respondErrorBytes(ams.ReturnCodeDeviceServiceNotSupported)

	case ams.CommandRead:
		// payload: Group(4) Offset(4) Length(4)
		if fn := s.readHandlers[group]; fn != nil {
			if len(payload) < 12 {
				return respondErrorBytes(ams.ReturnCodeDeviceInvalidSize)
			}
			offset := binary.LittleEndian.Uint32(payload[4:8])
			length := binary.LittleEndian.Uint32(payload[8:12])
			rc, data := fn(group, offset, length)
			return buildReadResponse(rc, data)
		}
		return respondErrorBytes(ams.ReturnCodeDeviceServiceNotSupported)

	case ams.CommandWrite:
		// payload: Group(4) Offset(4) Length(4) Data...
		if fn := s.writeHandlers[group]; fn != nil {
			if len(payload) < 12 {
				return respondErrorBytes(ams.ReturnCodeDeviceInvalidSize)
			}
			offset := binary.LittleEndian.Uint32(payload[4:8])
			length := binary.LittleEndian.Uint32(payload[8:12])
			data := []byte{}
			if uint64(12+length) <= uint64(len(payload)) {
				data = payload[12 : 12+length]
			}
			rc := fn(group, offset, data)
			return respondErrorBytes(rc)
		}
		return respondErrorBytes(ams.ReturnCodeDeviceServiceNotSupported)

	case ams.CommandAddDeviceNotification:
		if s.addNotifFn != nil {
			req := decodeAddNotifRequest(payload)
			r := s.addNotifFn(req)
			return buildAddNotifResponse(r.Handle, r.Error)
		}
		return buildAddNotifResponse(0, ams.ReturnCodeDeviceServiceNotSupported)

	case ams.CommandDeleteDeviceNotification:
		if s.deleteNotifFn != nil {
			if len(payload) < 4 {
				return respondErrorBytes(ams.ReturnCodeDeviceInvalidSize)
			}
			h := binary.LittleEndian.Uint32(payload[0:4])
			rc := s.deleteNotifFn(h)
			return respondErrorBytes(rc)
		}
		return respondErrorBytes(ams.ReturnCodeDeviceServiceNotSupported)

	case ams.CommandReadDeviceInfo:
		// 24-byte canned response: 4 errCode + 1 major + 1 minor + 2 version + 16 name
		out := make([]byte, 24)
		copy(out[8:24], []byte("scriptableStub\x00\x00"))
		return out

	case ams.CommandReadState:
		// 8 bytes: 4 errCode + 2 ADSState + 2 DeviceState
		out := make([]byte, 8)
		state := ams.StateRun
		if s.adsState != 0 {
			state = s.adsState
		}
		binary.LittleEndian.PutUint16(out[4:6], uint16(state))
		return out
	}
	return respondErrorBytes(ams.ReturnCodeDeviceServiceNotSupported)
}

// --- response builders ---

// respondErrorBytes returns a 4-byte ReturnCode payload.
func respondErrorBytes(rc ams.ReturnCode) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(rc))
	return out
}

// buildReadResponse builds an 8-byte (errCode + length) header followed by data.
// Used for both CommandRead and CommandReadWrite.
func buildReadResponse(rc ams.ReturnCode, data []byte) []byte {
	out := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint32(out[0:4], uint32(rc))
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(data)))
	copy(out[8:], data)
	return out
}

// buildAddNotifResponse builds the 8-byte AddDeviceNotification response payload.
func buildAddNotifResponse(handle uint32, rc ams.ReturnCode) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint32(out[0:4], uint32(rc))
	binary.LittleEndian.PutUint32(out[4:8], handle)
	return out
}

// buildSumAddNotifPayload returns the data section for a SumAddDeviceNotification
// WriteRead response (per-item: 4 errCode + 4 handle).
func SumAddNotifPayload(items []SumNotifResponse) []byte {
	out := make([]byte, 8*len(items))
	for i, it := range items {
		binary.LittleEndian.PutUint32(out[i*8:i*8+4], uint32(it.Error))
		binary.LittleEndian.PutUint32(out[i*8+4:i*8+8], it.Handle)
	}
	return out
}

// buildSumDeleteNotifPayload returns the data section for a SumDeleteDeviceNotification
// WriteRead response (per-item: 4 errCode).
func SumDeleteNotifPayload(codes []ams.ReturnCode) []byte {
	out := make([]byte, 4*len(codes))
	for i, c := range codes {
		binary.LittleEndian.PutUint32(out[i*4:i*4+4], uint32(c))
	}
	return out
}

// buildSymbolInfoPayload encodes a GetSymbolInfoByName response: symbolEntry
// struct followed by name+0, datatype+0, comment+0.
func SymbolInfoPayload(name, dataType, comment string, group, offset, size uint32, baseType ams.DataType, flags ams.SymbolFlag) []byte {
	// The wire layout, spelled out here so the fake checks the parser against
	// the protocol rather than against the parser's own struct.
	type symbolEntry struct {
		EntryLength   uint32
		IGroup        uint32
		IOffs         uint32
		Size          uint32
		DataType      uint32
		Flags         uint32
		NameLength    uint16
		TypeLength    uint16
		CommentLength uint16
	}
	entry := symbolEntry{
		IGroup:        group,
		IOffs:         offset,
		Size:          size,
		DataType:      uint32(baseType),
		Flags:         uint32(flags),
		NameLength:    uint16(len(name)),
		TypeLength:    uint16(len(dataType)),
		CommentLength: uint16(len(comment)),
	}
	// EntryLength = sizeof(symbolEntry) + name+0 + dt+0 + comment+0
	entry.EntryLength = uint32(30 /*symbolEntry*/) + uint32(len(name)+1+len(dataType)+1+len(comment)+1)

	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, entry)
	buf.WriteString(name)
	buf.WriteByte(0)
	buf.WriteString(dataType)
	buf.WriteByte(0)
	buf.WriteString(comment)
	buf.WriteByte(0)
	return buf.Bytes()
}

// buildHandlePayload builds a 4-byte handle response (for GetHandleByName).
func HandlePayload(handle uint32) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, handle)
	return out
}

// decodeAddNotifRequest parses the 40-byte request payload of AddDeviceNotification.
func decodeAddNotifRequest(payload []byte) AddNotifRequest {
	var r AddNotifRequest
	if len(payload) < 24 {
		return r
	}
	r.Group = binary.LittleEndian.Uint32(payload[0:4])
	r.Offset = binary.LittleEndian.Uint32(payload[4:8])
	r.Length = binary.LittleEndian.Uint32(payload[8:12])
	r.TransMode = binary.LittleEndian.Uint32(payload[12:16])
	r.MaxDelay = binary.LittleEndian.Uint32(payload[16:20])
	r.CycleTime = binary.LittleEndian.Uint32(payload[20:24])
	return r
}

// writeResponse builds and writes a complete response frame.
// reqBody is the original 32-byte AMS header (used to swap target/source).
// respPayload is the post-header response data.
//
// amsErr, when non-zero, goes into the AMS header's ErrorCode field and the body
// is dropped: an AMS rejection never carries a response, and adsconn rejects
// the frame outright unless Length matches the body actually written.
func writeResponse(c net.Conn, reqBody []byte, cmd ams.Command, invokeID uint32, respPayload []byte, amsErr uint32) error {
	if amsErr != 0 {
		respPayload = nil
	}
	respBody := make([]byte, 32+len(respPayload))
	// Swap source/target so addressing looks right.
	copy(respBody[0:8], reqBody[8:16]) // new Target = old Source
	copy(respBody[8:16], reqBody[0:8]) // new Source = old Target
	binary.LittleEndian.PutUint16(respBody[16:18], uint16(cmd))
	binary.LittleEndian.PutUint16(respBody[18:20], 5) // State = response
	binary.LittleEndian.PutUint32(respBody[20:24], uint32(len(respPayload)))
	binary.LittleEndian.PutUint32(respBody[24:28], amsErr) // ErrorCode
	binary.LittleEndian.PutUint32(respBody[28:32], invokeID)
	copy(respBody[32:], respPayload)

	respHdr := make([]byte, 6)
	binary.LittleEndian.PutUint32(respHdr[2:6], uint32(len(respBody)))

	if _, err := c.Write(append(respHdr, respBody...)); err != nil {
		return err
	}
	return nil
}
