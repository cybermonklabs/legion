// Package harness holds what every experiment of the Temporal evaluation needs: the
// command line each one is given, a client dialled against the server under test, a
// wait for that server's namespace, and the printing the write-up quotes.
//
// Every experiment prints its measured numbers as plain `name: value` lines and then
// exactly one verdict line, `experiment N: PASS` or `experiment N: FAIL`.
package harness

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
)

// Config is the command line of cmd/experiments, shared by every experiment. An
// experiment reads only the fields its own run needs.
type Config struct {
	// Experiment is the number given to -experiment, 1..6.
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
