package mcpbridge

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

type Bridge struct {
	cfg     *Config
	client  *bus.Client
	servers []Server
	mu      sync.Mutex
	// stopped is set by Stop, under mu: no server is registered or started after it.
	stopped  bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// errBridgeStopped is startServer's answer once Stop has run.
var errBridgeStopped = errors.New("the bridge is stopping")

func NewBridge(cfg *Config, client *bus.Client) *Bridge {
	return &Bridge{cfg: cfg, client: client, stopCh: make(chan struct{})}
}

// Start starts each configured server in turn (startServer), and stops the bridge where one
// fails. A Stop while it runs ends it: the server starting then is stopped, and none starts
// after it.
func (b *Bridge) Start() error {
	for i := range b.cfg.Servers {
		serverCfg := b.cfg.Servers[i]
		s := NewServer(serverCfg, b.makeHandler(&serverCfg))
		if err := b.startServer(s, nil); err != nil {
			b.Stop()
			return err
		}
		go b.monitor(s, &serverCfg)
	}
	return nil
}

// startServer registers s, in replaced's place where replaced is set, and then starts it. The
// registration comes first, under mu, so a Stop that comes while s starts finds s and stops it -
// its handshake's wait ends at once and its process is killed (ManagedServer.Stop) - and a Stop
// that came before leaves s unregistered and unstarted.
func (b *Bridge) startServer(s, replaced Server) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return errBridgeStopped
	}
	if replaced == nil {
		b.servers = append(b.servers, s)
	} else {
		for i, existing := range b.servers {
			if existing == replaced {
				b.servers[i] = s
				break
			}
		}
	}
	b.mu.Unlock()
	return s.Start()
}

// Stop stops every server the bridge has registered, one still starting included, and ends its
// starts and restarts. The servers are stopped at the same time, so the whole stop takes as long
// as its slowest server's, at most StopGrace however many there are, rather than StopGrace for
// each one that ignores its closed input. A second Stop stops them again, which ends nothing
// more.
func (b *Bridge) Stop() {
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	var stopping sync.WaitGroup
	for _, s := range b.servers {
		stopping.Go(s.Stop)
	}
	stopping.Wait()
}

func (b *Bridge) Healthy() bool {
	if err := b.client.Conn.FlushTimeout(3 * time.Second); err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.servers {
		if s.State() != StateReady {
			return false
		}
	}
	return true
}

func (b *Bridge) makeHandler(cfg *ServerConfig) func(uri string) {
	return func(uri string) { b.handleNotification(cfg, uri) }
}

func (b *Bridge) handleNotification(cfg *ServerConfig, uri string) {
	b.mu.Lock()
	var server Server
	for _, s := range b.servers {
		if s.Name() == cfg.Name {
			server = s
			break
		}
	}
	b.mu.Unlock()
	if server == nil {
		log.Printf("mcp-bridge: no server for %s", cfg.Name)
		return
	}
	if _, err := cfg.RenderTopic(uri); err != nil {
		log.Printf("mcp-bridge: %s: URI does not match pattern: %s", cfg.Name, uri)
		return
	}
	contents, err := server.ReadResource(uri)
	if err != nil {
		log.Printf("mcp-bridge: %s: resources/read failed for %s: %v", cfg.Name, uri, err)
		return
	}
	envelope, err := BuildEnvelope(cfg, uri, contents)
	if err != nil {
		log.Printf("mcp-bridge: %s: envelope construction failed: %v", cfg.Name, err)
		return
	}
	if err := envelope.Validate(); err != nil {
		log.Printf("mcp-bridge: %s: invalid envelope: %v", cfg.Name, err)
		return
	}
	if err := b.client.Publish(envelope); err != nil {
		log.Printf("mcp-bridge: %s: publish failed: %v", cfg.Name, err)
		return
	}
	log.Printf("mcp-bridge: %s: published to %s", cfg.Name, envelope.Topic)
}

func (b *Bridge) monitor(s Server, cfg *ServerConfig) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		err := s.WaitForExit()
		select {
		case <-b.stopCh:
			return
		default:
		}
		if httpServer, ok := s.(*HTTPServer); ok && httpServer.Stopped() {
			return
		}
		log.Printf("mcp-bridge: %s: exited: %v", cfg.Name, err)
		for {
			select {
			case <-b.stopCh:
				return
			case <-time.After(backoff):
			}
			log.Printf("mcp-bridge: %s: restarting (backoff=%v)", cfg.Name, backoff)
			newServer := NewServer(*cfg, b.makeHandler(cfg))
			err := b.startServer(newServer, s)
			if errors.Is(err, errBridgeStopped) {
				return
			}
			// newServer holds s's place now, started or not, so the next restart replaces it.
			s = newServer
			if err != nil {
				log.Printf("mcp-bridge: %s: restart failed: %v", cfg.Name, err)
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			backoff = time.Second
			break
		}
	}
}
