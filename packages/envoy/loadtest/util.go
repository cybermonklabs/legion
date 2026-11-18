package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
)

// finite is false for NaN and the infinities.
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// envOr is the environment variable name's value, or def when it is unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// sleepUntil waits for t on the wall clock; false when ctx ended first.
func sleepUntil(ctx context.Context, t time.Time) bool {
	return sleepUntilOn(ctx, clock.Real(), t)
}

// sleepUntilOn waits for t on c; false when ctx ended first. A t c has already reached returns at
// once, true unless ctx has ended.
func sleepUntilOn(ctx context.Context, c clock.Clock, t time.Time) bool {
	d := t.Sub(c.Now())
	if d <= 0 {
		return ctx.Err() == nil
	}
	return c.Sleep(ctx, d) == nil
}

// fetchTo saves the body of a GET of url to path; a failure is logged, not returned.
func fetchTo(url, path string, timeout time.Duration, lg *log.Logger) {
	resp, err := (&http.Client{Timeout: timeout}).Get(url)
	if err != nil {
		lg.Printf("fetch %s: %v", redactURL(url), err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		lg.Printf("fetch %s: %d %s", redactURL(url), resp.StatusCode, trunc(b))
		return
	}
	f, err := os.Create(path)
	if err != nil {
		lg.Printf("create %s: %v", path, err)
		return
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		lg.Printf("fetch %s: %v", redactURL(url), err)
	}
}

// writeJSONFile writes v to path as indented JSON with the given mode, which it also sets on a
// file that already exists; a failure is logged, not returned.
func writeJSONFile(path string, v any, mode os.FileMode, lg *log.Logger) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = os.WriteFile(path, append(b, '\n'), mode)
	}
	if err == nil {
		err = os.Chmod(path, mode)
	}
	if err != nil {
		lg.Printf("write %s: %v", path, err)
	}
}

// metaMode is meta.json's mode: owner only, since it records the step's command line and the
// stack it ran against. Its tokens and URL passwords are redacted all the same (recordedArgs).
const metaMode os.FileMode = 0o600

// collector gathers the items that many goroutines record during a step.
type collector[T any] struct {
	mu    sync.Mutex
	items []T
}

func newCollector[T any](capacity int) *collector[T] {
	return &collector[T]{items: make([]T, 0, capacity)}
}

func (c *collector[T]) add(x T) {
	c.mu.Lock()
	c.items = append(c.items, x)
	c.mu.Unlock()
}

// snapshot is a copy of the items recorded so far.
func (c *collector[T]) snapshot() []T {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]T(nil), c.items...)
}
