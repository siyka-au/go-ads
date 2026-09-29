// Example CLI — selector for two demos:
//
//  1. Session demo: interactive REPL over a managed Session — cache,
//     auto-reconnect, online-change handling, persistent notifications,
//     live Stale-flag observation.
//
//  2. Client demo: raw adsclient.Client (no cache, no reconnect) — single-shot
//     protocol-level inspection. Demonstrates ReadDeviceInfo, ReadState,
//     SymbolInfo, and a raw Read by handle.
//
// Connection parameters are read from environment variables — same set
// for both demos:
//
//	ADS_PLC_IP        target PLC IP (default: 192.168.1.100)
//	ADS_TARGET_AMS    target AMS NetID (default: 5.1.2.3.1.1)
//	ADS_TARGET_PORT   target AMS port (default: 851)
//	ADS_LOCAL_AMS     local AMS NetID ("auto" = derive from local IP)
//	ADS_LOCAL_PORT    local AMS port (default: 10500)
//	ADS_SYMBOL_NAME   symbol used for client demo (default: MAIN.bCounter)
//	ADS_ROUTE_USER    optional — register route on PLC if set
//	ADS_ROUTE_PASS    optional — paired with ADS_ROUTE_USER
//	ADS_ROUTE_NAME    optional — route display name (default: go-ads-example)
package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	ads "github.com/siyka-au/go-ads/v3"
	"github.com/siyka-au/go-ads/v3/adsclient"
	"github.com/siyka-au/go-ads/v3/ams"
)

func main() {
	os.Exit(run())
}

// run holds the body of main so deferred cleanups (signal context cancel)
// fire before exit — using os.Exit directly in main bypasses defers.
func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mode := selectMode()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch mode {
	case "session":
		if err := runSessionDemo(ctx, logger); err != nil {
			logger.Error("session demo failed", "error", err)
			return 1
		}
	case "client":
		if err := runClientDemo(ctx, logger); err != nil {
			logger.Error("client demo failed", "error", err)
			return 1
		}
	}
	return 0
}

// selectMode prompts the user to pick a demo. Honours ADS_DEMO=session|client
// to skip the prompt for scripted runs.
func selectMode() string {
	if v := strings.ToLower(os.Getenv("ADS_DEMO")); v == "session" || v == "client" {
		return v
	}
	fmt.Println("go-ads example CLI")
	fmt.Println()
	fmt.Println("  1) Session demo — managed wrapper (cache, auto-reconnect, REPL)")
	fmt.Println("  2) Client demo  — raw RPC (no cache, no reconnect)")
	fmt.Println()
	fmt.Print("Select [1/2]: ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		fmt.Println("read error:", err)
		os.Exit(1)
	}
	switch strings.TrimSpace(line) {
	case "1", "session", "s":
		return "session"
	case "2", "client", "c":
		return "client"
	default:
		fmt.Println("unknown selection")
		os.Exit(1)
		return ""
	}
}

// ----- Session demo (interactive REPL) --------------------------------------

// repl holds shared state for the REPL command loop.
type repl struct {
	sess    *ads.Session
	ctx     context.Context
	logger  *slog.Logger
	updates chan *ads.Update
}

func runSessionDemo(ctx context.Context, logger *slog.Logger) error {
	ip, targetAMS, targetPort, localAMS, localPort, _, routeName, routeUser, routePass := readEnv()

	// Buffer chosen large enough to absorb bursts when REPL is mid-input.
	updates := make(chan *ads.Update, 128)

	opts := []ads.Option{
		ads.WithLogger(logger),
		ads.WithAutoReconnect(true),
		ads.WithOnDisconnect(func() {
			fmt.Println("\n[session] transport dropped — auto-reconnect will retry")
		}),
		ads.WithOnReconnect(func() {
			fmt.Println("\n[session] reconnected — cache + notifications restored")
		}),
		ads.WithSymbolVersionStrategy(ads.SymbolVersionAutoReload),
		ads.WithOnSymbolVersionChanged(func(reason ads.Reason) {
			// Surface DP-1 events live so the user sees online-change detection.
			fmt.Printf("\n[online-change] reason=%s\n", reason)
		}),
		ads.WithRequestTimeout(5 * time.Second),
	}
	if routeUser != "" {
		opts = append(opts, ads.WithRoute(routeName, routeUser, routePass))
	}
	target, err := ams.NewAddress(targetAMS, ams.Port(targetPort))
	if err != nil {
		return fmt.Errorf("invalid target AMS: %w", err)
	}
	opts = append(opts, ads.WithLocalAddress(ams.Address{Port: ams.Port(localPort)}))
	if localAMS != "auto" && localAMS != "" {
		local, err := ams.NewAddress(localAMS, ams.Port(localPort))
		if err != nil {
			return fmt.Errorf("invalid local AMS: %w", err)
		}
		opts = append(opts, ads.WithLocalAddress(local))
	}

	sess, err := ads.NewSession(ctx, ads.Endpoint{Host: ip, Target: target}, opts...)
	if err != nil {
		return fmt.Errorf("NewSession: %w", err)
	}
	defer sess.Close()

	if err := sess.Connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	fmt.Printf("[session] connected plc=%s ams=%s\n", ip, targetAMS)

	if err := sess.LoadSymbols(ctx); err != nil {
		// Non-fatal: REPL can still operate via on-demand handle resolve.
		fmt.Printf("[session] LoadSymbols failed: %v (continuing — on-demand resolve still works)\n", err)
	} else {
		syms, _ := sess.Symbols()
		fmt.Printf("[session] symbol cache loaded (%d symbols)\n", len(syms))
	}

	r := &repl{
		sess:    sess,
		ctx:     ctx,
		logger:  logger,
		updates: updates,
	}

	// Notification printer — single goroutine drains the shared channel and
	// prints with a newline prefix to keep the prompt readable.
	notifyDone := make(chan struct{})
	go r.notifyLoop(notifyDone)

	// Graceful shutdown on SIGINT/SIGTERM, or when the session gives up for
	// good: close stdin to unblock ReadString.
	go func() {
		select {
		case <-ctx.Done():
			fmt.Println("\n[session] shutdown signal received")
		case <-sess.Done():
			fmt.Printf("\n[session] ended: %v\n", sess.Err())
		}
		_ = os.Stdin.Close()
	}()

	r.loop()

	close(updates)
	<-notifyDone
	return nil
}

func (r *repl) notifyLoop(done chan struct{}) {
	defer close(done)
	for u := range r.updates {
		if u.Stale != nil {
			fmt.Printf("\n[notify] *STALE* symbol=%s value=%s reason=%s ts=%s\n",
				u.Symbol, formatValue(u.Value), u.Stale.Reason, u.Time.Format(time.RFC3339Nano))
		} else {
			fmt.Printf("\n[notify] %s = %s (ts=%s)\n",
				u.Symbol, formatValue(u.Value), u.Time.Format("15:04:05.000"))
		}
	}
}

func (r *repl) loop() {
	in := bufio.NewReader(os.Stdin)
	printHelp()
	for {
		fmt.Print("ads> ")
		line, err := in.ReadString('\n')
		if err != nil {
			// EOF / stdin closed (SIGINT) → exit cleanly.
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		cmd := strings.ToLower(fields[0])
		args := fields[1:]

		switch cmd {
		case "help", "?":
			printHelp()
		case "quit", "exit":
			return
		case "state":
			r.cmdState()
		case "list":
			r.cmdList(args)
		case "browse":
			r.cmdBrowse(args)
		case "read":
			r.cmdRead(args)
		case "write":
			r.cmdWrite(args)
		case "info":
			r.cmdInfo(args)
		case "sub":
			r.cmdSub(args)
		case "unsub":
			r.cmdUnsub(args)
		case "reload":
			r.cmdReload()
		case "slow-load":
			r.cmdSlowLoad(args)
		default:
			fmt.Printf("unknown command: %s (type 'help')\n", cmd)
		}
	}
}

func printHelp() {
	fmt.Println("Commands:")
	fmt.Println("  list [prefix]              List cached symbols (optionally filtered)")
	fmt.Println("  browse [path]              Browse symbol hierarchy at path (default: root)")
	fmt.Println("  read <symbol>              Read symbol value (cache-aware)")
	fmt.Println("  write <symbol> <value>     Write value to symbol (library auto-parses)")
	fmt.Println("  info <symbol>              Show DataType/Length/Group/Offset/Comment")
	fmt.Println("  sub <symbol>               Subscribe to symbol on-change notifications")
	fmt.Println("  unsub <handle>             Delete notification by handle")
	fmt.Println("  reload                     RefreshSymbols (manual reload if version changed)")
	fmt.Println("  slow-load [chunk] [delay]  Reload via chunked download (default: 4096 bytes, 100ms)")
	fmt.Println("  state                      Show session state, addresses and subscriptions")
	fmt.Println("  help / ?                   Show this help")
	fmt.Println("  quit / exit                Graceful shutdown")
	fmt.Println()
}

func (r *repl) cmdState() {
	syms, _ := r.sess.Symbols()
	info := r.sess.Info()
	fmt.Printf("  state:          %v\n", info.State)
	fmt.Printf("  target:         %s\n", info.Target)
	fmt.Printf("  local:          %s (tcp %s)\n", info.Local, info.LocalTCP)
	if info.RuntimeKnown {
		fmt.Printf("  runtime:        %v\n", info.Runtime)
	}
	fmt.Printf("  cached symbols: %d\n", len(syms))
	subs := r.sess.Subscriptions()
	fmt.Printf("  subscriptions:  %d\n", len(subs))
	for _, s := range subs {
		fmt.Printf("    handle=%d symbol=%s active=%v\n", s.Handle, s.Config.Symbol, s.Active)
	}
}

func (r *repl) cmdList(args []string) {
	syms, err := r.sess.Symbols()
	if err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	prefix := ""
	if len(args) > 0 {
		prefix = strings.ToLower(args[0])
	}
	names := make([]string, 0, len(syms))
	for name := range syms {
		if prefix == "" || strings.Contains(strings.ToLower(name), prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	fmt.Printf("  symbols (%d shown / %d total):\n", len(names), len(syms))
	for _, n := range names {
		s := syms[n]
		fmt.Printf("    %-50s type=%-20s size=%d\n", n, s.DataType, s.Length)
	}
}

func (r *repl) cmdBrowse(args []string) {
	path := ""
	if len(args) > 0 {
		path = args[0]
	}
	entries, err := r.sess.Browse(path)
	if err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	if path == "" {
		fmt.Printf("  root (%d entries):\n", len(entries))
	} else {
		fmt.Printf("  children of %s (%d entries):\n", path, len(entries))
	}
	for _, e := range entries {
		more := ""
		if e.HasChildren {
			more = " [+]"
		}
		fmt.Printf("    %-45s type=%-20s size=%d%s\n", e.FullName, e.DataType, e.Size, more)
	}
}

func (r *repl) cmdRead(args []string) {
	if len(args) < 1 {
		fmt.Println("  usage: read <symbol>")
		return
	}
	name := args[0]
	v, err := r.sess.ReadValue(r.ctx, name)
	if err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	fmt.Printf("  %s = %s (%T)\n", name, formatValue(v), v)
}

func (r *repl) cmdWrite(args []string) {
	if len(args) < 2 {
		fmt.Println("  usage: write <symbol> <value>")
		return
	}
	name := args[0]
	// Re-join the rest so quoted strings with spaces survive (best-effort).
	text := strings.Join(args[1:], " ")
	// The current value's Go type says how to read the text.
	current, err := r.sess.ReadValue(r.ctx, name)
	if err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	value, err := parseLike(current, text)
	if err != nil {
		fmt.Printf("  error: %q as %T: %v\n", text, current, err)
		return
	}
	if err := r.sess.WriteValue(r.ctx, name, value); err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	fmt.Printf("  wrote %s = %s\n", name, formatValue(value))
	// Read-back confirms what the PLC observed (and exercises invalidation).
	if back, err := r.sess.ReadValue(r.ctx, name); err == nil {
		fmt.Printf("  confirmed: %s = %s\n", name, formatValue(back))
	}
}

func (r *repl) cmdInfo(args []string) {
	if len(args) < 1 {
		fmt.Println("  usage: info <symbol>")
		return
	}
	name := args[0]
	v, err := r.sess.Symbol(r.ctx, name)
	if err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	fmt.Printf("  Name:     %s\n", v.FullName)
	fmt.Printf("  DataType: %s\n", v.DataType)
	fmt.Printf("  Length:   %d\n", v.Length)
	fmt.Printf("  Group:    0x%X\n", v.Group)
	fmt.Printf("  Offset:   0x%X\n", v.Offset)
	fmt.Printf("  Handle:   %d\n", v.Handle)
	fmt.Printf("  BaseType: %s\n", v.BaseTypeName())
	if v.Comment != "" {
		fmt.Printf("  Comment:  %s\n", v.Comment)
	}
}

func (r *repl) cmdSub(args []string) {
	if len(args) < 1 {
		fmt.Println("  usage: sub <symbol>")
		return
	}
	name := args[0]
	h, err := r.sess.Subscribe(
		r.ctx,
		name,
		100*time.Millisecond,
		100*time.Millisecond,
		ams.TransModeServerOnChange,
		r.updates,
	)
	if err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	fmt.Printf("  subscribed: symbol=%s handle=%d\n", name, h)
}

func (r *repl) cmdUnsub(args []string) {
	if len(args) < 1 {
		fmt.Println("  usage: unsub <handle>")
		return
	}
	h, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		fmt.Printf("  invalid handle: %v\n", err)
		return
	}
	if err := r.sess.Unsubscribe(r.ctx, uint32(h)); err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	fmt.Printf("  unsubscribed: handle=%d\n", h)
}

func (r *repl) cmdReload() {
	if err := r.sess.RefreshSymbols(r.ctx); err != nil {
		fmt.Printf("  error: %v\n", err)
		return
	}
	syms, _ := r.sess.Symbols()
	fmt.Printf("  reload complete (%d symbols)\n", len(syms))
}

// cmdSlowLoad invokes Session.LoadSymbolsSlow for chunked symbol download.
// Useful for large symbol tables on slow PLCs where single-shot LoadSymbols
// times out or disrupts the real-time task.
//
// Usage: slow-load [chunkSize] [delayMs]
//
// Defaults: 4096 bytes per chunk, 100ms between chunks.
func (r *repl) cmdSlowLoad(args []string) {
	cfg := ads.SlowDiscoveryConfig{
		ChunkSize:  4096,
		ChunkDelay: 100 * time.Millisecond,
	}
	if len(args) >= 1 {
		if v, err := strconv.ParseUint(args[0], 10, 32); err == nil {
			cfg.ChunkSize = uint32(v)
		} else {
			fmt.Printf("  invalid chunkSize %q: %v\n", args[0], err)
			return
		}
	}
	if len(args) >= 2 {
		if v, err := strconv.Atoi(args[1]); err == nil {
			cfg.ChunkDelay = time.Duration(v) * time.Millisecond
		} else {
			fmt.Printf("  invalid delayMs %q: %v\n", args[1], err)
			return
		}
	}
	fmt.Printf("  loading symbols (chunkSize=%d, delay=%s)...\n", cfg.ChunkSize, cfg.ChunkDelay)
	if err := r.sess.LoadSymbolsSlow(r.ctx, cfg); err != nil {
		fmt.Printf("  slow-load failed: %v\n", err)
		return
	}
	syms, _ := r.sess.Symbols()
	fmt.Printf("  slow-load complete (%d symbols)\n", len(syms))
}

// ----- Client demo (single-shot) --------------------------------------------

func runClientDemo(ctx context.Context, logger *slog.Logger) error {
	ip, targetAMS, targetPort, localAMS, localPort, symbolName, _, _, _ := readEnv()

	target, err := ams.NewAddress(targetAMS, ams.Port(targetPort))
	if err != nil {
		return fmt.Errorf("parse target AMS: %w", err)
	}
	// "auto" / "" leaves the NetID zero, which Dial derives from the local IP.
	source := ams.Address{Port: ams.Port(localPort)}
	if localAMS != "auto" && localAMS != "" {
		if source, err = ams.NewAddress(localAMS, ams.Port(localPort)); err != nil {
			return fmt.Errorf("parse local AMS: %w", err)
		}
	}

	c, err := adsclient.Dial(ctx, ip, target,
		adsclient.WithSource(source),
		adsclient.WithRequestTimeout(5*time.Second),
		adsclient.WithLogger(logger),
		adsclient.WithOnDrop(func() {
			logger.Warn("client: transport dropped (no auto-reconnect — Session does that)")
		}),
	)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	logger.Info("client: dialed", "plc", ip, "ams", targetAMS)

	if info, err := c.ReadDeviceInfo(ctx); err != nil {
		logger.Warn("ReadDeviceInfo failed", "error", err)
	} else {
		logger.Info("client: device info", "name", info.Name, "version", info.Version())
	}

	if state, err := c.ReadState(ctx); err != nil {
		logger.Warn("ReadState failed", "error", err)
	} else {
		logger.Info("client: ads state", "ads", state.State, "device", state.DeviceState)
	}

	sym, err := c.SymbolInfo(ctx, symbolName)
	if err != nil {
		logger.Warn("SymbolInfo failed", "symbol", symbolName, "error", err)
		return nil
	}
	logger.Info("client: symbol info",
		"symbol", sym.Name,
		"group", fmt.Sprintf("0x%X", uint32(sym.Group)),
		"offset", fmt.Sprintf("0x%X", sym.Offset),
		"length", sym.Length,
		"type", sym.DataType,
		"base", sym.BaseType.IECName())

	handle, err := c.Handle(ctx, symbolName)
	if err != nil {
		logger.Warn("resolve handle failed", "symbol", symbolName, "error", err)
		return nil
	}
	defer func() { _ = c.ReleaseHandle(ctx, handle) }()

	data, err := c.Read(ctx, ams.GroupSymbolValueByHandle, handle, sym.Length)
	if err != nil {
		logger.Warn("raw Read failed", "symbol", symbolName, "error", err)
		return nil
	}
	logger.Info("client: raw read",
		"symbol", symbolName,
		"bytes", fmt.Sprintf("% X", data),
		"length", len(data))

	return nil
}

// ----- shared helpers -------------------------------------------------------

func readEnv() (ip, targetAMS string, targetPort int, localAMS string, localPort int, symbolName, routeName, routeUser, routePass string) {
	ip = getEnvOrDefault("ADS_PLC_IP", "192.168.1.100")
	targetAMS = getEnvOrDefault("ADS_TARGET_AMS", "5.1.2.3.1.1")
	targetPort = getEnvIntOrDefault("ADS_TARGET_PORT", 851)
	localAMS = getEnvOrDefault("ADS_LOCAL_AMS", "auto")
	localPort = getEnvIntOrDefault("ADS_LOCAL_PORT", 10500)
	symbolName = getEnvOrDefault("ADS_SYMBOL_NAME", "MAIN.bCounter")
	routeName = getEnvOrDefault("ADS_ROUTE_NAME", "go-ads-example")
	routeUser = os.Getenv("ADS_ROUTE_USER")
	routePass = os.Getenv("ADS_ROUTE_PASS")
	return
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
