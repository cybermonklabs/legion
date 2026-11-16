// Package restatex holds what the Restate experiments -- 10, 11 and 12 -- share.
//
// Restate is neither a server the application talks to over gRPC, as Temporal is, nor
// a library inside the application's own process, as DBOS is. It is a separate server
// that calls the application: the handlers run in a process of their own behind an
// HTTP/2 endpoint, the server holds the journal, and the server decides when to invoke,
// retry, suspend or pause. That shape is why the Restate experiments need three things
// the earlier ones did not: a server process started and stopped by the driver, an
// admin client to register the endpoint and read what the server recorded, and an
// ingress client to drive a handler from outside.
//
// The file helpers here are deliberately this package's own rather than dbosx's. The
// two packages measure two different engines and share nothing else; a Restate
// experiment importing the DBOS helpers would tie the Restate results to a package
// that exists to open PostgreSQL connections.
package restatex

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Loopback is the only host the Restate experiments bind or address. The server's
// startup banner prints the host it advertises, so everything -- the node's own
// advertised host included -- is pinned to the loopback address, and no address of the
// machine the experiment runs on can reach the measured output.
const Loopback = "127.0.0.1"

// pollInterval is how often these helpers look again at a file, a port or a row they
// are waiting for.
const pollInterval = 100 * time.Millisecond

// RequireLoopback refuses an address that is not on the loopback interface, naming the
// flag it came from. An experiment that printed a routable address of its host would
// leak it into the write-up, so an address that is not 127.0.0.1 is a configuration
// error rather than something to tolerate.
func RequireLoopback(flag, address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s %q must be HOST:PORT: %w", flag, address, err)
	}
	if host != Loopback {
		return fmt.Errorf("%s %q must be on %s: these experiments bind and address the loopback interface only", flag, address, Loopback)
	}
	if port == "" {
		return fmt.Errorf("%s %q must name a port", flag, address)
	}
	return nil
}

// FreePort is a TCP port nothing is listening on, for a service endpoint the driver
// starts. The port is released before it is returned, so what this reports is a port
// that was free a moment ago rather than one that is held.
func FreePort() (int, error) {
	listener, err := net.Listen("tcp", Loopback+":0")
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("release the free port %d: %w", port, err)
	}
	return port, nil
}

// Address is a loopback HOST:PORT for a port this process chose.
func Address(port int) string {
	return net.JoinHostPort(Loopback, strconv.Itoa(port))
}

// ServerOptions is one restate-server process the driver runs.
type ServerOptions struct {
	// Binary is the restate-server executable.
	Binary string
	// BaseDir is where the server keeps everything it writes. It is emptied before
	// the server starts, so a second run of the same experiment starts from nothing.
	BaseDir string
	// Ingress and Admin are the HOST:PORT the server binds its two user-facing APIs
	// on, and Fabric the port of its internal node-to-node channel.
	Ingress string
	Admin   string
	Fabric  int
	// NodeName is the cluster node name, which the server requires to be stable
	// across restarts of the same data directory.
	NodeName string
	// Partitions is how many logical shards the cluster is provisioned with. One is
	// enough for a single-node experiment and keeps the start quick.
	Partitions int
}

// Server is a restate-server process started by a driver.
type Server struct {
	opts    ServerOptions
	cmd     *exec.Cmd
	logPath string
	stopped bool
}

// StartServer runs restate-server and returns once its admin API answers. Everything
// the server prints goes to a log file under its base directory rather than to the
// experiment's own output: the server's banner names the host it advertises, and the
// measured lines must carry no address of this machine.
func StartServer(ctx context.Context, opts ServerOptions, timeout time.Duration) (*Server, error) {
	if err := RequireLoopback("-restate-ingress", opts.Ingress); err != nil {
		return nil, err
	}
	if err := RequireLoopback("-restate-admin", opts.Admin); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(opts.BaseDir); err != nil {
		return nil, fmt.Errorf("clear the server's base directory %s: %w", opts.BaseDir, err)
	}
	if err := os.MkdirAll(opts.BaseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create the server's base directory %s: %w", opts.BaseDir, err)
	}

	logPath := filepath.Join(opts.BaseDir, "restate-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create the server's log %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(opts.Binary,
		"--node-name", opts.NodeName,
		"--no-logo",
		"--bind-ip", Loopback,
		"--bind-port", fmt.Sprint(opts.Fabric),
		"--advertised-host", Loopback,
		"--advertised-address", fmt.Sprintf("http://%s:%d/", Loopback, opts.Fabric),
		"--default-num-partitions", fmt.Sprint(opts.Partitions),
	)
	cmd.Dir = opts.BaseDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(),
		"RESTATE_BASE_DIR="+opts.BaseDir,
		"RESTATE_ADMIN__BIND_ADDRESS="+opts.Admin,
		"RESTATE_INGRESS__BIND_ADDRESS="+opts.Ingress,
		"RESTATE_DISABLE_TELEMETRY=true",
	)
	// Its own process group, so Stop takes the server down whatever it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", opts.Binary, err)
	}

	server := &Server{opts: opts, cmd: cmd, logPath: logPath}
	if err := server.waitReady(ctx, timeout); err != nil {
		server.Stop()
		return nil, err
	}
	return server, nil
}

// waitReady blocks until the admin API answers and the ingress port accepts a
// connection, or until the server dies or the timeout passes.
func (s *Server) waitReady(ctx context.Context, timeout time.Duration) error {
	admin := NewAdmin(s.opts.Admin)
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if s.cmd.ProcessState != nil {
			return fmt.Errorf("restate-server exited before it was ready; its log is %s", s.logPath)
		}
		if _, err := admin.Version(ctx); err != nil {
			last = err
		} else if conn, err := net.DialTimeout("tcp", s.opts.Ingress, time.Second); err != nil {
			last = err
		} else {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf("restate-server did not answer on %s within %s (%v); its log is %s", s.opts.Admin, timeout, last, s.logPath)
}

// PID is the server process's id.
func (s *Server) PID() int { return s.cmd.Process.Pid }

// LogPath is the file the server's own output went to, named in a fact so a run that
// went wrong can be read afterwards.
func (s *Server) LogPath() string { return s.logPath }

// Stop takes the server's process group down and reaps it. It is safe to call more
// than once, which is what makes a deferred Stop on every exit path correct.
func (s *Server) Stop() {
	if s.stopped {
		return
	}
	s.stopped = true
	if s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_, _ = s.cmd.Process.Wait()
	}
}

// Defaults are the few settings of the server's own effective configuration that the
// experiments quote: a measured run shows what the server did, and these say what it
// was configured to do.
type Defaults struct {
	// JournalRetention, IdempotencyRetention and WorkflowCompletionRetention are how
	// long the server keeps a completed invocation's journal, its idempotency record
	// and a workflow's result.
	JournalRetention            string
	IdempotencyRetention        string
	WorkflowCompletionRetention string
	// InactivityTimeout is how long a handler may hold a connection open without
	// making progress before the server asks it to suspend; AbortTimeout is how long
	// after that the server waits before it aborts the attempt.
	InactivityTimeout string
	AbortTimeout      string
	// RetryInitialInterval, RetryFactor, RetryMaxInterval, RetryMaxAttempts and
	// RetryOnMaxAttempts are the server-wide invocation retry policy.
	RetryInitialInterval string
	RetryFactor          string
	RetryMaxInterval     string
	RetryMaxAttempts     string
	RetryOnMaxAttempts   string
}

// ReadDefaults asks the server binary to print its effective configuration and picks
// out the settings the experiments quote. It runs the binary with --dump-config, which
// prints and exits without touching any data.
//
// It is given a base directory of its own, empty and thrown away afterwards: the
// server refuses to start against a directory holding another node's data unless it is
// told that node's name, and it derives a default node name from this machine's
// hostname, which these experiments never let near their output.
func ReadDefaults(ctx context.Context, binary, workDir string) (Defaults, error) {
	probeDir, err := os.MkdirTemp(workDir, "config-probe-")
	if err != nil {
		return Defaults{}, fmt.Errorf("make a directory to read the server's configuration in: %w", err)
	}
	defer os.RemoveAll(probeDir)

	cmd := exec.CommandContext(ctx, binary, "--dump-config", "--node-name", "config-probe")
	cmd.Env = append(os.Environ(), "RESTATE_BASE_DIR="+probeDir)
	out, err := cmd.Output()
	if err != nil {
		return Defaults{}, fmt.Errorf("%s --dump-config: %w", binary, err)
	}
	values := map[string]string{}
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			continue
		}
		key, value, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		values[section+"/"+key] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return Defaults{
		JournalRetention:            values["/default-journal-retention"],
		IdempotencyRetention:        values["/default-idempotency-retention"],
		WorkflowCompletionRetention: values["/default-workflow-completion-retention"],
		InactivityTimeout:           values["worker.invoker/inactivity-timeout"],
		AbortTimeout:                values["worker.invoker/abort-timeout"],
		RetryInitialInterval:        values["default-retry-policy/initial-interval"],
		RetryFactor:                 values["default-retry-policy/exponentiation-factor"],
		RetryMaxInterval:            values["default-retry-policy/max-interval"],
		RetryMaxAttempts:            values["default-retry-policy/max-attempts"],
		RetryOnMaxAttempts:          values["default-retry-policy/on-max-attempts"],
	}, nil
}
