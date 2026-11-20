package mcpbridge

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type ServerState int32

const (
	StateStarting ServerState = iota
	StateReady
	StateDead
)

type Server interface {
	Name() string
	Start() error
	Stop()
	State() ServerState
	WaitForExit() error
	ReadResource(uri string) ([]resourceContent, error)
}

func NewServer(cfg ServerConfig, onNotify func(uri string)) Server {
	switch cfg.Transport {
	case "http":
		return NewHTTPServer(cfg, onNotify)
	default:
		return NewManagedServer(cfg, onNotify)
	}
}

type ManagedServer struct {
	cfg       ServerConfig
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	session   *session
	state     atomic.Int32
	onNotify  func(uri string)
	stopCh    chan struct{}
	closeOnce sync.Once
	// mu guards cmd, stdin, exited and stopping between Start and Stop, so a Stop that comes while
	// Start runs finds the process Start started, and a Start after a Stop starts none.
	mu       sync.Mutex
	stopping bool
	// waitOnce, exited and waitErr are the one wait for cmd (wait).
	waitOnce sync.Once
	exited   chan struct{}
	waitErr  error
}

// StopGrace is how long Stop waits for a server to exit once its standard input is closed, the
// stdio transport's own signal to end, before it kills the server: the longest a Stop takes, and
// so the longest a bridge's stop takes, which stops its servers at the same time (Bridge.Stop),
// the one still starting included.
const StopGrace = 5 * time.Second

// errStopped is Start's answer for a server Stop has already stopped.
var errStopped = errors.New("the server was stopped before it started")

func NewManagedServer(cfg ServerConfig, onNotify func(uri string)) *ManagedServer {
	s := &ManagedServer{cfg: cfg, onNotify: onNotify, stopCh: make(chan struct{})}
	s.state.Store(int32(StateStarting))
	return s
}

func (s *ManagedServer) Name() string       { return s.cfg.Name }
func (s *ManagedServer) State() ServerState { return ServerState(s.state.Load()) }

// Start starts the server's process and makes the MCP handshake with it. A Stop while the
// handshake waits ends the wait at once (session.call reads stopCh) and kills the process.
func (s *ManagedServer) Start() error {
	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	for k, v := range s.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("start %s: %w", s.cfg.Name, errStopped)
	}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("start %s: %w", s.cfg.Name, err)
	}
	s.cmd = cmd
	s.stdin = stdin
	s.exited = make(chan struct{})
	s.mu.Unlock()
	s.scanner = bufio.NewScanner(stdout)
	s.scanner.Buffer(make([]byte, 1<<20), 1<<20)
	s.session = newSession(s.cfg.Name, func(data []byte) error {
		data = append(data, '\n')
		_, err := s.stdin.Write(data)
		return err
	}, s.onNotify, s.stopCh)
	go s.readLoop()
	if err := s.session.initialize(); err != nil {
		s.kill()
		return fmt.Errorf("initialize %s: %w", s.cfg.Name, err)
	}
	for _, uri := range s.cfg.Resources {
		if err := s.session.subscribe(uri); err != nil {
			s.kill()
			return fmt.Errorf("subscribe %s to %s: %w", s.cfg.Name, uri, err)
		}
	}
	s.state.Store(int32(StateReady))
	log.Printf("mcp-bridge: server %s ready", s.cfg.Name)
	return nil
}

// Stop ends the server: it closes its standard input, waits up to StopGrace for it to exit, and
// kills it then. A Stop before Start leaves Start nothing to start.
func (s *ManagedServer) Stop() {
	s.closeOnce.Do(func() { close(s.stopCh) })
	s.state.Store(int32(StateDead))
	s.mu.Lock()
	s.stopping = true
	cmd, stdin, exited := s.cmd, s.stdin, s.exited
	s.mu.Unlock()
	if stdin != nil {
		stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		go s.wait()
		select {
		case <-exited:
		case <-time.After(StopGrace):
			cmd.Process.Kill()
		}
	}
}

// wait waits for the server's process once, whoever asks first, and every caller returns once it
// has been waited for, with the wait's error: exec.Cmd.Wait may be called only once, and Stop, a
// failed Start's kill and the bridge's monitor (WaitForExit) can each ask for it at the same time.
func (s *ManagedServer) wait() error {
	s.waitOnce.Do(func() {
		s.waitErr = s.cmd.Wait()
		close(s.exited)
	})
	<-s.exited
	return s.waitErr
}

func (s *ManagedServer) WaitForExit() error {
	if s.cmd == nil {
		return fmt.Errorf("not started")
	}
	return s.wait()
}

func (s *ManagedServer) ReadResource(uri string) ([]resourceContent, error) {
	return s.session.readResource(uri)
}

func (s *ManagedServer) kill() {
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.wait()
	}
	s.state.Store(int32(StateDead))
}

func (s *ManagedServer) readLoop() {
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		s.session.handleMessage(line)
	}
	if err := s.scanner.Err(); err != nil {
		log.Printf("mcp-bridge: %s: read error: %v", s.cfg.Name, err)
	}
	s.state.Store(int32(StateDead))
}
