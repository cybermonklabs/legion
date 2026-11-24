package mcpbridge

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
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
	// Words is text the server wrote - a notification's URI, a topic rendered from one - as its
	// words, which print only as the rule for remote text reads them.
	Words(text string) redact.RemoteText
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
}

func NewManagedServer(cfg ServerConfig, onNotify func(uri string)) *ManagedServer {
	s := &ManagedServer{cfg: cfg, onNotify: onNotify, stopCh: make(chan struct{})}
	s.state.Store(int32(StateStarting))
	return s
}

func (s *ManagedServer) Name() string       { return s.cfg.Name }
func (s *ManagedServer) State() ServerState { return ServerState(s.state.Load()) }

// Words is text the child wrote, as its words (childWords).
func (s *ManagedServer) Words(text string) redact.RemoteText { return childWords(text) }

// childWords is text a managed server's child wrote on its standard output - a JSON-RPC error's
// message, a notification's URI - as the server's words: a RemoteText, which prints only as the
// rule reads it. The child answers no request this process sent over HTTP, so there is no request
// record to hold the words with (redact.Carried).
func childWords(text string) redact.RemoteText { return redact.NewRemoteText(text) }

func (s *ManagedServer) Start() error {
	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...)
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
	// The child writes its standard error into a pipe of the server's own, whose every line the
	// server logs as the rule reads it (logStderr). The pipe is not exec's (Cmd.StderrPipe), which
	// Wait closes as soon as the child has exited, before a last line has been read.
	stderr, stderrWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.Stderr = stderrWrite
	err = cmd.Start()
	// The child holds its own copy of the pipe's write end: the read end ends once the child, and
	// every process it handed the pipe to, has closed it.
	stderrWrite.Close()
	if err != nil {
		stderr.Close()
		return fmt.Errorf("start %s: %w", s.cfg.Name, err)
	}
	go logStderr(s.cfg.Name, stderr)
	s.cmd = cmd
	s.stdin = stdin
	s.scanner = bufio.NewScanner(stdout)
	s.scanner.Buffer(make([]byte, 1<<20), 1<<20)
	s.session = newSession(s.cfg.Name, func(data []byte) error {
		data = append(data, '\n')
		_, err := s.stdin.Write(data)
		return err
	}, childWords, s.onNotify, s.stopCh)
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

// logStderr logs each line the managed server name's child writes on its standard error, as the
// rule for remote text reads it (redact.Field), until the pipe r ends; then it closes r. A line
// longer than the rule reads (redact.FieldLimit) prints as redact.WordsNotReadable, and is read to
// its end all the same, so the child is never left blocked on a full pipe.
func logStderr(name string, r io.ReadCloser) {
	defer r.Close()
	lines := bufio.NewReader(r)
	for {
		line, err := stderrLine(lines)
		if len(line) > 0 {
			log.Printf("mcp-bridge: %s: stderr: %s", name, redact.Field(string(line)))
		}
		if err != nil {
			return
		}
	}
}

// stderrLine is the next line of r, without its line ending, holding no more of the line than
// redact.FieldLimit and one byte - enough for the rule to refuse a longer one - while the rest of
// a longer line is read and dropped. err is r's, io.EOF at the end.
func stderrLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if room := redact.FieldLimit + 1 - len(line); room > 0 {
			line = append(line, chunk[:min(len(chunk), room)]...)
		}
		if err != bufio.ErrBufferFull {
			return bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r")), err
		}
	}
}

func (s *ManagedServer) Stop() {
	s.closeOnce.Do(func() { close(s.stopCh) })
	s.state.Store(int32(StateDead))
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		done := make(chan struct{})
		go func() { s.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.cmd.Process.Kill()
		}
	}
}

func (s *ManagedServer) WaitForExit() error {
	if s.cmd == nil {
		return fmt.Errorf("not started")
	}
	return s.cmd.Wait()
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
		s.cmd.Wait()
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
		log.Printf("mcp-bridge: %s: read error: %v", s.cfg.Name, readFailure(err))
	}
	s.state.Store(int32(StateDead))
}

// readFailure is err, what ended a managed server's read of its child's standard output, as a
// message may print it: bufio's own words for a line past the scanner's buffer, and otherwise the
// read and the system's reason (redact.File), never the error's own text.
func readFailure(err error) error {
	if errors.Is(err, bufio.ErrTooLong) {
		return bufio.ErrTooLong
	}
	return redact.File(err)
}
