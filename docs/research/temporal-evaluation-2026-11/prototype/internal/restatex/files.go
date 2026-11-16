package restatex

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// AppendLine appends one line to a file, creating it if it does not exist, and makes
// sure it has reached the disk before returning. It is how a handler performs the side
// effect the experiments count: the line is in the world before anything about the
// attempt is in the server's journal, which is the state a kill has to find.
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

// CountLines is how many lines a file holds, and zero when it does not exist. A
// handler calls it to number its own attempt.
func CountLines(path string) int {
	lines, err := ReadLines(path)
	if err != nil {
		return 0
	}
	return len(lines)
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

// WaitForFile blocks until a file exists, and reports whether it appeared before the
// timeout. A handler uses it to stand for work that is under way and not finished: it
// has written its side effect and is now holding, exactly as a prompt into a live
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

// Touch creates an empty file. It is how a driver releases a handler that is waiting.
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
