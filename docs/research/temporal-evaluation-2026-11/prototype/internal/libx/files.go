package libx

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// pollInterval is how often the experiments look at a file they are waiting for. The
// file stands for work an activity did or a release a driver gave, and both are seen
// within a tick of it.
const pollInterval = 100 * time.Millisecond

// WorkerConfig is the command line of cmd/gwworker and cmd/dtworker: everything a
// second process needs to join the same workflow state, in the same schema, as the
// driver that started it.
type WorkerConfig struct {
	// Mode names the experiment whose workflow this process hosts.
	Mode string
	// DSN is the PostgreSQL the library keeps its state in, and Schema is the schema
	// of this run inside the evaluation database.
	DSN    string
	Schema string
	// WorkerName is the identity the library records on the rows this process locks.
	WorkerName string
	// InstanceID is the workflow instance this process starts, or the one it is
	// expected to take over when it starts nothing.
	InstanceID string
	// WorkDir is the scratch directory whose files the workflow writes its side
	// effects into and reads its release from.
	WorkDir string
	// Start distinguishes the process that starts the workflow from the replacement
	// process, which starts nothing and only runs the library.
	Start bool
}

// Field prints one thing the driver is waiting to hear from a worker process. prefix
// is the worker's own name, which the driver matches on; everything else a worker
// prints is just forwarded to stderr.
func Field(prefix, key string, value any) {
	fmt.Printf("%s: %s=%v\n", prefix, key, value)
	_ = os.Stdout.Sync()
}

// AppendLine appends one line to a file, creating it if it does not exist. It is how
// an activity performs the side effect the experiments count.
func AppendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("append to %s: %w", path, err)
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		f.Close()
		return fmt.Errorf("append to %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("append to %s: %w", path, err)
	}
	return f.Close()
}

// ReadLines is every line of a file, and no lines at all when the file does not exist.
func ReadLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// WaitForLines blocks until a file holds at least want lines, and returns the lines it
// holds. It fails when timeout passes first, naming how many lines it did see.
func WaitForLines(ctx context.Context, path string, want int, timeout time.Duration) ([]string, error) {
	deadline := time.Now().Add(timeout)
	for {
		lines, err := ReadLines(path)
		if err != nil {
			return nil, err
		}
		if len(lines) >= want {
			return lines, nil
		}
		if !time.Now().Before(deadline) {
			return lines, fmt.Errorf("%s held %d lines, not %d, after %s", path, len(lines), want, timeout)
		}
		select {
		case <-ctx.Done():
			return lines, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Touch creates an empty file. It is how a driver releases an activity that is
// waiting.
func Touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return f.Close()
}

// Exists reports whether a path is there.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// WaitForFile blocks until a file exists, and reports whether it appeared before the
// timeout. An activity uses it to stand for work that is under way and not finished:
// it has written its side effect and is now holding, exactly as a prompt into a live
// session holds while the session thinks.
func WaitForFile(ctx context.Context, path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if Exists(path) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pollInterval):
		}
	}
}

// RemoveAll removes files a previous run of the same experiment left in the scratch
// directory, so a second run starts from nothing.
func RemoveAll(paths ...string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// Sleep waits for d and reports whether the context ended first.
func Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
