// Package harness holds what every experiment of the evaluation needs: the command
// line each one is given, a client dialled against the server under test, a wait for
// that server's namespace, and the printing the write-up quotes.
//
// Experiments 1 to 6 measure Temporal and take its address; experiments 7 to 9 measure
// DBOS, which is a library rather than a server, and take a PostgreSQL URL instead;
// experiments 10 to 12 measure Restate, which is a server that calls the application,
// and take its two loopback addresses, the restate-server binary and the binary of the
// service process the server is to call.
//
// Every experiment prints its measured numbers as plain `name: value` lines and then
// exactly one verdict line, `experiment N: PASS` or `experiment N: FAIL`.
package harness

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
)

// Config is the command line of cmd/experiments, shared by every experiment. An
// experiment reads only the fields its own run needs.
type Config struct {
	// Experiment is the number given to -experiment, 1..9.
	Experiment int
	// Address is the server's gRPC host:port.
	Address string
	// Namespace is the Temporal namespace the experiment runs in.
	Namespace string
	// SessionWorker is the absolute path of the built cmd/sessionworker binary,
	// which only experiment 3 starts.
	SessionWorker string
	// MetricsURL is the dev server's Prometheus endpoint, which only experiment 6 reads.
	MetricsURL string
	// WorkDir is a writable scratch directory the experiment may use.
	WorkDir string
	// RepoRoot is the root of the Legion repository, whose own code experiment 1
	// measures. Empty means "find it by walking up from the working directory".
	RepoRoot string
	// PostgresDSN is the URL of the PostgreSQL that experiments 7, 8 and 9 measure
	// DBOS against, which is the only state those three need: DBOS keeps its workflow
	// state in the application's own database.
	PostgresDSN string
	// DBOSWorker is the absolute path of the built cmd/dbosworker binary, which
	// experiments 7, 8 and 9 start as a second process. Empty means "the dbosworker
	// next to the running binary".
	DBOSWorker string
	// RestateIngress and RestateAdmin are the loopback addresses of the Restate
	// server that experiments 10, 11 and 12 measure: the ingress an invocation is
	// started through, and the admin API a deployment is registered with and an
	// invocation's state is read from.
	RestateIngress string
	RestateAdmin   string
	// RestateServer is the absolute path of a restate-server executable. When it
	// names one, the experiment starts the server itself, which is what a runner
	// will do; when it is empty, the experiment measures a server already answering
	// at RestateAdmin and RestateIngress.
	RestateServer string
	// RestateService is the absolute path of the built cmd/restateservice binary,
	// the process holding the handlers that Restate calls. Empty means "the
	// restateservice next to the running binary".
	RestateService string
	// GWWorker is the absolute path of the built cmd/gwworker binary, which
	// experiment 13 starts as a second process so there is a process to kill.
	// Empty means "the gwworker next to the running binary".
	GWWorker string
	// DTWorker is the absolute path of the built cmd/dtworker binary, which
	// experiments 16 and 17 start as a second process. Empty means "the dtworker
	// next to the running binary".
	DTWorker string
}

// WorkDir is the scratch directory the experiment may write in: what -work-dir named,
// or the system temporary directory when it named nothing. The directory exists when
// WorkDir returns without an error.
func WorkDir(cfg Config) (string, error) {
	dir := cfg.WorkDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("-work-dir: %w", err)
	}
	return dir, nil
}

// RequirePostgresDSN refuses an experiment that was given no database. Experiments 7
// to 9 and 13 to 18 measure libraries that keep their workflow state in PostgreSQL,
// so without one there is nothing at all to measure.
func RequirePostgresDSN(cfg Config) error {
	if strings.TrimSpace(cfg.PostgresDSN) == "" {
		return fmt.Errorf("experiment %d measures a library that keeps its workflow state in PostgreSQL: pass -postgres-dsn, or set LEGION_TEST_PG_DSN, to a PostgreSQL URL this process may create a database in", cfg.Experiment)
	}
	return nil
}

// GWWorker is the cmd/gwworker binary experiment 13 starts as a second process: what
// -gw-worker named, or the gwworker beside the running binary, which is where a
// runner that builds both of them puts it.
func GWWorker(cfg Config) (string, error) {
	return executable(cfg.GWWorker, "-gw-worker", "gwworker", "go build -o %s ./cmd/gwworker")
}

// DTWorker is the cmd/dtworker binary experiments 16 and 17 start as a second
// process: what -dt-worker named, or the dtworker beside the running binary.
func DTWorker(cfg Config) (string, error) {
	return executable(cfg.DTWorker, "-dt-worker", "dtworker", "go build -o %s ./cmd/dtworker")
}

// DBOSWorker is the cmd/dbosworker binary the DBOS experiments start as a second
// process: what -dbos-worker named, or the dbosworker beside the running binary, which
// is where a runner that builds both of them puts it.
func DBOSWorker(cfg Config) (string, error) {
	return executable(cfg.DBOSWorker, "-dbos-worker", "dbosworker", "go build -o %s ./cmd/dbosworker")
}

// RestateService is the cmd/restateservice binary the Restate experiments start as a
// separate process: what -restate-service named, or the restateservice beside the
// running binary, which is where a runner that builds both of them puts it.
func RestateService(cfg Config) (string, error) {
	return executable(cfg.RestateService, "-restate-service", "restateservice", "go build -o %s ./cmd/restateservice")
}

// RestateServer is the restate-server executable the Restate experiments start, and
// the empty string when none was given and none is beside the running binary: that is
// not an error, because an experiment may instead be pointed at a server somebody
// else runs.
func RestateServer(cfg Config) (string, error) {
	if cfg.RestateServer == "" {
		self, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("-restate-server is empty and this binary's own path is unknown: %w", err)
		}
		beside := filepath.Join(filepath.Dir(self), "restate-server")
		if info, err := os.Stat(beside); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return "", nil
		}
		return beside, nil
	}
	return executable(cfg.RestateServer, "-restate-server", "restate-server", "")
}

// executable resolves one binary flag: an absolute path that exists and can be run,
// defaulting to a name beside the running binary. build, when it is not empty, is the
// command that produces it, named in the error so a failed run says what to do.
func executable(path, flag, beside, build string) (string, error) {
	if path == "" {
		self, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("%s is empty and this binary's own path is unknown: %w", flag, err)
		}
		path = filepath.Join(filepath.Dir(self), beside)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s %q must be an absolute path", flag, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		if build != "" {
			return "", fmt.Errorf("%s: %w (build it with: %s)", flag, err, fmt.Sprintf(build, path))
		}
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s %q is not an executable file", flag, path)
	}
	return path, nil
}

// Logger is the SDK logger the experiments give their clients and workers: warnings and
// errors on stderr, so the SDK's chatter never reaches the measured lines on stdout.
func Logger() sdklog.Logger {
	return sdklog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))
}

// Dial connects to the server and waits for the namespace to answer, which a freshly
// started dev server does only once the namespace it registered has propagated. It
// retries until timeout because the server may still be starting.
func Dial(ctx context.Context, cfg Config, timeout time.Duration) (client.Client, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		c, err := client.DialContext(ctx, client.Options{
			HostPort:  cfg.Address,
			Namespace: cfg.Namespace,
			Logger:    Logger(),
		})
		if err == nil {
			if lastErr = WaitForNamespace(ctx, c, cfg.Namespace, time.Until(deadline)); lastErr == nil {
				return c, nil
			}
			c.Close()
		} else {
			lastErr = err
		}
		if err := sleep(ctx, deadline, 250*time.Millisecond); err != nil {
			return nil, fmt.Errorf("dial %s namespace %s: %w (last error: %v)", cfg.Address, cfg.Namespace, err, lastErr)
		}
	}
}

// WaitForNamespace blocks until DescribeNamespace answers for namespace, or timeout passes.
func WaitForNamespace(ctx context.Context, c client.Client, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		_, lastErr = c.WorkflowService().DescribeNamespace(ctx, &workflowservice.DescribeNamespaceRequest{
			Namespace: namespace,
		})
		if lastErr == nil {
			return nil
		}
		if err := sleep(ctx, deadline, 250*time.Millisecond); err != nil {
			return fmt.Errorf("namespace %s not ready: %w (last error: %v)", namespace, err, lastErr)
		}
	}
}

// sleep waits for d, and reports why it stopped early: the context ended, or the
// deadline passed.
func sleep(ctx context.Context, deadline time.Time, d time.Duration) error {
	if !time.Now().Add(d).Before(deadline) {
		return fmt.Errorf("timed out")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Fact prints one measured value as a plain line of the experiment's output.
func Fact(name string, value any) {
	fmt.Printf("%s: %v\n", name, value)
}

// Verdict prints the experiment's single last line and returns the process exit code:
// 0 when err is nil, 1 otherwise. A failure's reason is printed as a fact before it, so
// a failing run says what it saw rather than only that it failed.
func Verdict(experiment int, err error) int {
	if err != nil {
		Fact("failure", err)
		fmt.Printf("experiment %d: FAIL\n", experiment)
		return 1
	}
	fmt.Printf("experiment %d: PASS\n", experiment)
	return 0
}
