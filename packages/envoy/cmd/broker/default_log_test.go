package main

import (
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
)

// keepTheDefaults puts back the default logger and the log package's output and flags when t
// ends, unless the row says the log package's lock is held for good (stuck), when setting its
// output would wait on that lock too.
func keepTheDefaults(t *testing.T) (stuck *bool) {
	stuck = new(bool)
	previous, output, flags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		// slog.SetDefault points the log package at a handler other than slog's own, which
		// takes the same lock.
		if !*stuck {
			slog.SetDefault(previous)
			log.SetOutput(output)
			log.SetFlags(flags)
		}
	})
	return stuck
}

// TestSlogsOwnHandlerWritesThroughTheLogPackage: slogsOwnHandler, read before main, is slog's own
// handler, the one that writes each record through the log package, which is what setDefaultLog
// must leave the log package alone for.
func TestSlogsOwnHandlerWritesThroughTheLogPackage(t *testing.T) {
	keepTheDefaults(t)
	var out logtest.Buffer
	log.SetOutput(&out)
	log.SetFlags(0)
	slog.New(slogsOwnHandler).Info("a line of slog's own handler")
	if !strings.Contains(out.String(), "INFO a line of slog's own handler") {
		t.Fatalf("slogsOwnHandler wrote %q to the log package; want its line there, as slog's own handler writes it", out.String())
	}
}

// TestSetDefaultLogLeavesTheLogPackageAloneForSlogsOwnHandler: given slog's own handler, which
// writes through the log package, setDefaultLog leaves the log package's output as it is, as
// slog.SetDefault does. Pointed at that handler, the log package would write each line into a
// handler that writes it back through the log package, whose output holds its lock while it
// writes: log.Print would wait on its own lock and never return. Here both a log.Print and a line
// of slog return, and each is written once, where the log package writes.
func TestSetDefaultLogLeavesTheLogPackageAloneForSlogsOwnHandler(t *testing.T) {
	stuck := keepTheDefaults(t)
	var out logtest.Buffer
	log.SetOutput(&out)
	log.SetFlags(0)
	setDefaultLog(slogsOwnHandler)
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Print("a line of the log package")
		slog.Info("a line of slog")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		*stuck = true
		t.Fatal("log.Print has not returned after 10 seconds: the log package writes into slog's own handler, which writes back through the log package, and waits on its own lock")
	}
	written := out.String()
	if strings.Count(written, "a line of the log package") != 1 || strings.Count(written, "INFO a line of slog") != 1 {
		t.Fatalf("the log package wrote %q; want each line once", written)
	}
}
