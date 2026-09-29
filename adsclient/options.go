package adsclient

import (
	"log/slog"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

// DefaultPort is the AMS router's TCP port.
const DefaultPort = 48898

// Option configures Dial.
type Option func(*config)

type config struct {
	port           int
	source         ams.Address
	requestTimeout time.Duration
	logger         *slog.Logger
	onDrop         func()
	onNotify       NotificationHandler
	disableSum     bool
}

// WithPort connects to a TCP port other than DefaultPort, e.g. a NAT forward.
// A port in the host string passed to Dial does the same.
func WithPort(port int) Option { return func(c *config) { c.port = port } }

// WithSource sets the source AMS address. A zero NetID is derived from the
// connection's local IP; a zero Port picks a random one.
func WithSource(source ams.Address) Option { return func(c *config) { c.source = source } }

// WithRequestTimeout bounds each request and the dial. The default is 5s.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.requestTimeout = d
		}
	}
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(logger *slog.Logger) Option { return func(c *config) { c.logger = logger } }

// WithOnDrop calls fn once when the connection drops unexpectedly. It does not
// fire for Close.
func WithOnDrop(fn func()) Option { return func(c *config) { c.onDrop = fn } }

// WithNotificationHandler receives device notification samples. See
// NotificationHandler.
func WithNotificationHandler(fn NotificationHandler) Option {
	return func(c *config) { c.onNotify = fn }
}

// WithoutSumCommands makes SumRead, SumWrite and the Sum notification calls
// send one request per item, as they do automatically on devices that lack
// sum commands. For devices that claim support and misbehave, and for tests.
func WithoutSumCommands() Option { return func(c *config) { c.disableSum = true } }
