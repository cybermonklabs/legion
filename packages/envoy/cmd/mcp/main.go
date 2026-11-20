package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/mcpbridge"
)

func main() {
	configPath := strings.TrimSpace(os.Getenv("ENVOY_MCP_CONFIG"))
	if configPath == "" {
		log.Fatal("ENVOY_MCP_CONFIG is required")
	}
	if strings.TrimSpace(os.Getenv("ENVOY_MACHINE_ID")) == "" {
		log.Fatal("ENVOY_MACHINE_ID is required")
	}
	natsRaw := strings.TrimSpace(os.Getenv("NATS_URLS"))
	if natsRaw == "" {
		log.Fatal("NATS_URLS is required")
	}
	natsURLs := strings.FieldsFunc(natsRaw, func(r rune) bool { return r == ',' })
	for i, u := range natsURLs {
		natsURLs[i] = strings.TrimSpace(u)
	}

	port := 9012
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("invalid PORT: %v", err)
		}
		port = p
	}

	// Load and validate config.
	cfg, err := mcpbridge.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Connect to NATS. The bridge publishes what its MCP servers report and owns nothing on the
	// bus, so it never reconciles ENVOY_NOTIFICATIONS, and reaches only this machine's NATS
	// unless ENVOY_ALLOW_REMOTE_NATS=1.
	client, err := bus.Connect(natsURLs)
	if err != nil {
		log.Fatalf("nats: %v", err)
	}
	defer client.Close()

	// The shutdown signals (shutdownSignals) are watched from before the bridge starts its MCP
	// servers, not from after: a SIGTERM sent to this process alone (a container's stop, a `kill`)
	// between a server's start and a watch armed later would end this process by the signal's
	// default action and leave that server running. The start runs beside the watch, so a signal
	// during it stops the bridge at once - the server still starting included, its handshake's
	// wait ended and its process killed within mcpbridge.StopGrace - rather than after the start,
	// whose every call may wait 30 s for an answer.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, shutdownSignals()...)
	bridge := mcpbridge.NewBridge(cfg, client)
	started := make(chan error, 1)
	go func() { started <- bridge.Start() }()
	select {
	case err := <-started:
		if err != nil {
			log.Fatalf("bridge start: %v", err)
		}
	case sig := <-sigCh:
		log.Printf("envoy-mcp stopping on %v while the bridge starts", sig)
		bridge.Stop()
		return
	}

	// Health check endpoint.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !bridge.Healthy() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:         ":" + strconv.Itoa(port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("envoy-mcp listening on %d", port)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-sigCh
	log.Printf("envoy-mcp shutting down")
	bridge.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}

// shutdownSignals are the signals that stop envoy-mcp: a container's stop or a `kill` (SIGTERM), a
// Ctrl-C (SIGINT), a closing terminal's SIGHUP and its quit key's SIGQUIT. Each MCP server is a
// child the bridge stops itself (mcpbridge.Bridge.Stop), so a signal this process did not watch
// would end it by its default action, SIGQUIT's with a goroutine dump, and leave the servers
// running. SIGHUP is watched only where this process was not started with it ignored: under
// `nohup envoy-mcp`, a closing terminal leaves it running, as nohup asks, and a watch would undo
// that ignore for good (signal.Notify: the signal "will no longer be ignored"). SIGQUIT is watched
// whatever it was started with, since the Go runtime keeps no inherited ignore of it (only of
// SIGHUP and SIGINT; runtime's sigInstallGoHandler).
func shutdownSignals() []os.Signal {
	signals := []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT}
	if !signal.Ignored(syscall.SIGHUP) {
		signals = append(signals, syscall.SIGHUP)
	}
	return signals
}
