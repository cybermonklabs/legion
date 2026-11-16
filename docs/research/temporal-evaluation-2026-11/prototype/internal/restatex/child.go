package restatex

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// FieldLine is what a cmd/restateservice process tells its driver: one
// `restateservice: key=value` line per thing the driver has to know, the first of them
// always `pid`. A key may be printed again and again, as the service's periodic
// readings of itself are; the driver sees the newest value.
var FieldLine = regexp.MustCompile(`^restateservice: ([a-z0-9-]+)=(.*)$`)

// Field prints one thing a service process's driver is waiting to hear.
func Field(key string, value any) {
	fmt.Printf("restateservice: %s=%v\n", key, value)
	_ = os.Stdout.Sync()
}

// Service is one cmd/restateservice process the driver started: the endpoint it serves
// on, the pid the operating system gave it, the fields it has printed so far, and the
// plumbing that forwards the rest of its output to stderr and reaps it exactly once.
type Service struct {
	name    string
	address string
	cmd     *exec.Cmd
	pid     int
	drained chan struct{}
	once    sync.Once

	mu      sync.Mutex
	fields  map[string]string
	updated chan struct{}
}

// StartService runs a service process and returns once its endpoint answers. Restate
// calls the endpoint rather than the other way round, so a driver that registered a
// deployment before the endpoint was up would have the server fail discovery; waiting
// for the endpoint's own health check is what makes registration reliable.
func StartService(ctx context.Context, name, binary string, args []string, workDir, address string, timeout time.Duration) (*Service, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s: pipe: %w", name, err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = workDir
	cmd.Stdout = writeEnd
	cmd.Stderr = writeEnd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		readEnd.Close()
		writeEnd.Close()
		return nil, fmt.Errorf("%s: start %s: %w", name, binary, err)
	}
	// The parent holds no write end, so the reader sees EOF when the process dies.
	writeEnd.Close()

	s := &Service{
		name:    name,
		address: address,
		cmd:     cmd,
		pid:     cmd.Process.Pid,
		drained: make(chan struct{}),
		fields:  map[string]string{},
		updated: make(chan struct{}),
	}
	go func() {
		defer close(s.drained)
		defer readEnd.Close()
		scanner := bufio.NewScanner(readEnd)
		for scanner.Scan() {
			line := scanner.Text()
			if m := FieldLine.FindStringSubmatch(line); m != nil {
				s.setField(m[1], m[2])
			}
			fmt.Fprintf(os.Stderr, "[%s] %s\n", name, line)
		}
	}()

	reported, err := s.Field(ctx, "pid", timeout)
	if err != nil {
		s.Stop()
		return nil, err
	}
	if reported != strconv.Itoa(s.pid) {
		s.Stop()
		return nil, fmt.Errorf("%s: printed pid %s but was started as pid %d", name, reported, s.pid)
	}
	if err := s.waitHealthy(ctx, timeout); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

// waitHealthy blocks until the SDK endpoint answers its health check. The endpoint is
// served over unencrypted HTTP/2 only -- that is what Restate speaks to it -- so the
// check has to be made with a client that speaks the same thing: a plain HTTP/1.1
// request to it is answered with a reset connection.
func (s *Service) waitHealthy(ctx context.Context, timeout time.Duration) error {
	client := &http.Client{Transport: h2cTransport(), Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL()+"/health", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("health answered %s", resp.Status)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf("%s: endpoint %s was not healthy within %s: %v", s.name, s.URL(), timeout, last)
}

// h2cTransport speaks unencrypted HTTP/2 with prior knowledge, which is the only
// protocol a Restate SDK endpoint serves.
func h2cTransport() *http.Transport {
	transport := &http.Transport{}
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	transport.Protocols = protocols
	return transport
}

// URL is the endpoint Restate is told to call, which is also how a deployment is
// identified: two processes serving the same URL are, to Restate, one deployment.
func (s *Service) URL() string { return "http://" + s.address }

// Address is the HOST:PORT the process listens on.
func (s *Service) Address() string { return s.address }

// PID is the process id the operating system gave the service, which is the pid the
// driver signals and the pid the experiment's facts name.
func (s *Service) PID() int { return s.pid }

// Name is how the experiment's facts refer to this process.
func (s *Service) Name() string { return s.name }

func (s *Service) setField(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fields[key] = value
	close(s.updated)
	s.updated = make(chan struct{})
}

// Field waits until the service has printed key, and returns its newest value. It
// fails when the process exits without printing it, or when timeout passes first.
func (s *Service) Field(ctx context.Context, key string, timeout time.Duration) (string, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		s.mu.Lock()
		value, ok := s.fields[key]
		updated := s.updated
		s.mu.Unlock()
		if ok {
			return value, nil
		}
		select {
		case <-updated:
		case <-s.drained:
			s.mu.Lock()
			value, ok := s.fields[key]
			s.mu.Unlock()
			if ok {
				return value, nil
			}
			return "", fmt.Errorf("%s: exited before printing %s", s.name, key)
		case <-timer.C:
			return "", fmt.Errorf("%s: printed no %s within %s", s.name, key, timeout)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// Latest is the newest value the service printed for key, without waiting. The second
// result is false when the service has not printed that key at all. It is how a driver
// reads a reading the service takes again and again -- how many goroutines it holds,
// how many invocations it has in flight -- at the moment the driver cares about.
func (s *Service) Latest(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.fields[key]
	return value, ok
}

// Alive reports whether the process still exists, by sending it the null signal.
func (s *Service) Alive() bool {
	return syscall.Kill(s.pid, 0) == nil
}

// Signal sends sig to the exact pid the driver recorded, then reaps the process. It is
// what the experiment measures with, so it names the one pid and nothing around it.
func (s *Service) Signal(sig syscall.Signal) error {
	if err := syscall.Kill(s.pid, sig); err != nil {
		return fmt.Errorf("%s: signal %v to pid %d: %w", s.name, sig, s.pid, err)
	}
	s.reap()
	return nil
}

// Stop takes the process group down and reaps it. It is safe to call on a process that
// is already gone, and safe to call more than once, which is what makes a deferred
// Stop on every exit path correct.
func (s *Service) Stop() {
	if s.cmd.Process != nil {
		// Negative pid: the process group started by Setpgid, so nothing survives.
		_ = syscall.Kill(-s.pid, syscall.SIGKILL)
	}
	s.reap()
}

// reap waits for the process exactly once and for its output to run out, so no zombie
// and no goroutine outlives the experiment.
func (s *Service) reap() {
	s.once.Do(func() {
		_ = s.cmd.Wait()
		<-s.drained
	})
}
