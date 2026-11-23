package logmask

import (
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/logtest"
)

// slogsOwnHandler is the handler slog's default logger has until something sets one, which writes
// each record through the log package. It is read as the test binary starts, and no package it
// imports sets the default logger as it is initialized.
var slogsOwnHandler = slog.Default().Handler()

// logLockHeld is whether a row left the log package's lock held for good: every later row would
// wait on it too, so each says so and stops.
var logLockHeld bool

// keepTheDefaults puts back the default logger and the log package's output and flags when t
// ends, unless a row left the log package's lock held for good (logLockHeld), when setting its
// output would wait on that lock too.
func keepTheDefaults(t *testing.T) {
	if logLockHeld {
		t.Fatal("an earlier row left the log package's lock held for good, so this row cannot run")
	}
	previous, output, flags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		if !logLockHeld {
			slog.SetDefault(previous)
			log.SetOutput(output)
			log.SetFlags(flags)
		}
	})
}

// TestSlogsOwnHandlerWritesThroughTheLogPackage: slogsOwnHandler is slog's own handler, the one
// that writes each record through the log package, which is what SetDefault must leave the log
// package alone for.
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

// TestSetDefaultLeavesTheLogPackageAloneForSlogsOwnHandler: given slog's own handler, which writes
// through the log package, or a copy of it from WithAttrs or WithGroup, SetDefault leaves the log
// package's output as it is, as slog.SetDefault does. Pointed at that handler, the log package
// would write each line into a handler that writes it back through the log package, whose output
// holds its lock while it writes: log.Print would wait on its own lock and never return. Here both
// a log.Print and a line of slog return, and each is written once, where the log package writes.
// A guard that knew slog's own handler by its identity would miss each copy, which is a handler of
// its own; SetDefault reads what slog.SetDefault did to the log package instead.
func TestSetDefaultLeavesTheLogPackageAloneForSlogsOwnHandler(t *testing.T) {
	for _, c := range []struct {
		name    string
		handler slog.Handler
	}{
		{"slog's own handler", slogsOwnHandler},
		{"a copy of it from WithAttrs", slogsOwnHandler.WithAttrs(nil)},
		{"a copy of it from WithGroup", slogsOwnHandler.WithGroup("g")},
	} {
		t.Run(c.name, func(t *testing.T) {
			keepTheDefaults(t)
			var out logtest.Buffer
			log.SetOutput(&out)
			log.SetFlags(0)
			SetDefault(c.handler)
			done := make(chan struct{})
			go func() {
				defer close(done)
				log.Print("a line of the log package")
				slog.Info("a line of slog")
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				logLockHeld = true
				t.Fatal("log.Print has not returned after 10 seconds: the log package writes into slog's own handler, which writes back through the log package, and waits on its own lock")
			}
			written := out.String()
			if strings.Count(written, "a line of the log package") != 1 || strings.Count(written, "INFO a line of slog") != 1 {
				t.Fatalf("the log package wrote %q; want each line once", written)
			}
		})
	}
}

// TestSetDefaultPointsTheLogPackageAtTheHandler: given any other handler, SetDefault makes it the
// default logger's and points the log package at it with no flags through StandardLog. slog's own
// writer for the log package, which slog.SetDefault alone installs, writes each write as one INFO
// line of the same handler too, so the rows read what only StandardLog does: the bytes a line
// quotes from a peer hidden, a first line cut to answer.FieldBytes, and the rest of a write its
// stack, each with no date of the log package's own before its text.
func TestSetDefaultPointsTheLogPackageAtTheHandler(t *testing.T) {
	keepTheDefaults(t)
	var out logtest.Buffer
	log.SetFlags(log.LstdFlags)
	SetDefault(slog.NewJSONHandler(&out, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	log.Printf("starting with %q", realValue)
	log.Print(strings.Repeat("y", 300) + "\nsecond line")
	slog.Info("a line of slog")
	want := `{"level":"INFO","msg":"starting with \"[service bytes not shown]\""}` + "\n" +
		`{"level":"INFO","msg":"` + strings.Repeat("y", answer.FieldBytes) + `","stack":"second line"}` + "\n" +
		`{"level":"INFO","msg":"a line of slog"}` + "\n"
	if out.String() != want {
		t.Fatalf("the handler wrote %q; want %q", out.String(), want)
	}
}
