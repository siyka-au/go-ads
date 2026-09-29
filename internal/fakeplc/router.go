// Package fakeplc provides in-process stand-ins for a TwinCAT device, for tests:
// PLC is a scriptable AMS/TCP server whose responses each test sets up, and
// Router answers the AMS router's UDP services (identify and route
// registration). Wire layouts are spelled out here rather than borrowed from the
// library's parsers, so the fakes check the library against the protocol, not
// against itself.
package fakeplc

import (
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	routeCookie     = 0x71146603
	serviceIdentify = 1
	serviceAddRoute = 6
	responseFlag    = 0x80000000

	// TagResponseError carries the 4-byte AddRoute result.
	TagResponseError uint16 = 1
	// TagSystemVersion carries major(1) + minor(1) + build(2).
	TagSystemVersion uint16 = 3
	// TagComputerName carries a NUL-terminated host name.
	TagComputerName uint16 = 5
)

// Tag is one id/data pair of an AMS router response.
type Tag struct {
	ID   uint16
	Data []byte
}

// Identity is what Router reports to an identify request.
type Identity struct {
	NetID [6]byte
	Port  uint16
	Tags  []Tag
}

// Router is a UDP stub of the AMS router service on port 48899. It answers
// AddRoute always and identify only once SetIdentity has been called, so a test
// can model a router that is deaf to discovery.
type Router struct {
	// DropAdds and DropIdentifies silently swallow that many requests of each
	// kind before answering, modelling lost datagrams on a plant network.
	DropAdds       atomic.Int32
	DropIdentifies atomic.Int32
	// NoiseFirst sends this many junk datagrams before the next AddRoute reply.
	// It has to come from the router itself: the client's UDP socket is connected
	// and only ever receives from the address it dialled.
	NoiseFirst atomic.Int32
	// StrayIdentify sends an identify answer with the wrong invokeID before each
	// real one, as a late reply to another exchange would.
	StrayIdentify atomic.Bool
	// Reply is the AddRoute result code (0 = success).
	Reply atomic.Int64

	Host string
	Port int

	pc   *net.UDPConn
	done chan struct{}
	wg   sync.WaitGroup
	adds atomic.Int64
	ids  atomic.Int64

	mu       sync.Mutex
	identity *Identity
	netIDs   [][6]byte
}

// NewRouter starts a Router on an ephemeral loopback port and stops it when the
// test ends.
func NewRouter(t testing.TB) *Router {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("fake router listen: %v", err)
	}
	addr := pc.LocalAddr().(*net.UDPAddr)
	r := &Router{Host: addr.IP.String(), Port: addr.Port, pc: pc, done: make(chan struct{})}
	r.wg.Add(1)
	go r.serve()
	t.Cleanup(r.Close)
	return r
}

// Addr returns "host:port" of the router.
func (r *Router) Addr() string { return net.JoinHostPort(r.Host, strconv.Itoa(r.Port)) }

// Close stops the router. Safe to call more than once.
func (r *Router) Close() {
	select {
	case <-r.done:
		return
	default:
	}
	close(r.done)
	_ = r.pc.Close()
	r.wg.Wait()
}

// SetIdentity makes the router answer identify requests with id.
func (r *Router) SetIdentity(id Identity) {
	r.mu.Lock()
	r.identity = &id
	r.mu.Unlock()
}

// Registrations is the number of AddRoute requests received, dropped ones included.
func (r *Router) Registrations() int64 { return r.adds.Load() }

// Identifies is the number of identify requests received, dropped ones included.
func (r *Router) Identifies() int64 { return r.ids.Load() }

// RegisteredNetIDs returns the source NetID carried by every AddRoute request.
func (r *Router) RegisteredNetIDs() [][6]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][6]byte, len(r.netIDs))
	copy(out, r.netIDs)
	return out
}

func (r *Router) serve() {
	defer r.wg.Done()
	buf := make([]byte, 2048)
	for {
		select {
		case <-r.done:
			return
		default:
		}
		_ = r.pc.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, from, err := r.pc.ReadFromUDP(buf)
		if err != nil || n < 24 {
			continue
		}
		invokeID := binary.LittleEndian.Uint32(buf[4:8])
		switch binary.LittleEndian.Uint32(buf[8:12]) {
		case serviceAddRoute:
			r.adds.Add(1)
			var netID [6]byte
			copy(netID[:], buf[12:18])
			r.mu.Lock()
			r.netIDs = append(r.netIDs, netID)
			r.mu.Unlock()
			for r.NoiseFirst.Load() > 0 {
				r.NoiseFirst.Add(-1)
				_, _ = r.pc.WriteToUDP([]byte("junk on the shared AMS router port"), from)
			}
			if r.DropAdds.Load() > 0 {
				r.DropAdds.Add(-1)
				continue
			}
			result := make([]byte, 4)
			binary.LittleEndian.PutUint32(result, uint32(r.Reply.Load()))
			_, _ = r.pc.WriteToUDP(Response(invokeID, serviceAddRoute, [6]byte{}, 0, Tag{TagResponseError, result}), from)
		case serviceIdentify:
			r.ids.Add(1)
			r.mu.Lock()
			id := r.identity
			r.mu.Unlock()
			if id == nil {
				continue
			}
			if r.DropIdentifies.Load() > 0 {
				r.DropIdentifies.Add(-1)
				continue
			}
			if r.StrayIdentify.Load() {
				stray := Response(invokeID^0xFFFF, serviceIdentify, [6]byte{9, 9, 9, 9, 1, 1}, 10000)
				_, _ = r.pc.WriteToUDP(stray, from)
			}
			_, _ = r.pc.WriteToUDP(Response(invokeID, serviceIdentify, id.NetID, id.Port, id.Tags...), from)
		}
	}
}

// Response builds an AMS router response datagram: cookie + invokeID +
// (RESPONSE|service) + AmsAddr + tagCount, then each tag as id(2)+len(2)+data.
func Response(invokeID uint32, service uint32, netID [6]byte, port uint16, tags ...Tag) []byte {
	out := make([]byte, 24)
	binary.LittleEndian.PutUint32(out[0:], routeCookie)
	binary.LittleEndian.PutUint32(out[4:], invokeID)
	binary.LittleEndian.PutUint32(out[8:], responseFlag|service)
	copy(out[12:18], netID[:])
	binary.LittleEndian.PutUint16(out[18:], port)
	binary.LittleEndian.PutUint32(out[20:], uint32(len(tags)))
	for _, tg := range tags {
		hdr := make([]byte, 4)
		binary.LittleEndian.PutUint16(hdr[0:], tg.ID)
		binary.LittleEndian.PutUint16(hdr[2:], uint16(len(tg.Data)))
		out = append(out, hdr...)
		out = append(out, tg.Data...)
	}
	return out
}

// IdentifyResponse builds an identify answer.
func IdentifyResponse(invokeID uint32, netID [6]byte, port uint16, tags ...Tag) []byte {
	return Response(invokeID, serviceIdentify, netID, port, tags...)
}

// VersionTag encodes a TwinCAT version as the identify service reports it.
func VersionTag(major, minor uint8, build uint16) Tag {
	b := []byte{major, minor, 0, 0}
	binary.LittleEndian.PutUint16(b[2:], build)
	return Tag{TagSystemVersion, b}
}

// NameTag encodes a NUL-terminated computer name.
func NameTag(name string) Tag {
	return Tag{TagComputerName, append([]byte(name), 0)}
}
