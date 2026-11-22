package daemon

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// relaunchStampOf is the relaunch an R6 controller line names (its relaunchedAt, the claim's
// LastRelaunch as the daemon stamped it), read back as recordingLog keeps it; the zero time where
// the line carries none or carries it in a form time.Parse(time.RFC3339Nano, ...) does not read.
// It lives in this file, which w5's negative control lays over the base with the driver that
// calls it.
func relaunchStampOf(rec loggedRecord) time.Time {
	text, _ := rec.Attrs["relaunchedAt"].(string)
	stamped, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}
	}
	return stamped
}

// The R6 controller line names the relaunch it counts from as a time attribute, and w5's
// relaunched-controller check reads it back through the rig's own log recorder
// (recordingLog, decodeAttr, relaunchStampOf). That path reads the stamp the daemon wrote, to the
// nanosecond, so the check counts R6's 20 minutes from it rather than from nothing. A line that
// carries no stamp reads as the zero time, which the check fails.
//
// The control (the pull request records the run): decodeAttr turning a time into
// slog.Value.String's layout, as it did, makes relaunchStampOf read the zero time, and this test
// fails.
func TestTheRelaunchStampOfAnR6LineReadsBackAsTheDaemonWroteIt(t *testing.T) {
	stamp := time.Date(2026, 11, 19, 23, 27, 10, 27_000_123, time.UTC)
	recorder := newRecordingLog(slog.New(slog.NewTextHandler(io.Discard, nil)))
	logger := slog.New(recorder)
	logger.Error("LEGION FLEET ALERT: the project's controller is out of reach", "case", "controller_unreachable",
		"relaunchedAt", stamp)
	logger.Error("LEGION FLEET ALERT: a line with no stamp", "case", "controller_unreachable")

	lines := recorder.matching(func(loggedRecord) bool { return true })
	if len(lines) != 2 {
		t.Fatalf("the recorder kept %d lines, want the two logged", len(lines))
	}
	if got := relaunchStampOf(lines[0]); !got.Equal(stamp) {
		t.Fatalf("the R6 line's relaunch reads back as %s (attribute %#v), want %s as the daemon wrote it",
			got, lines[0].Attrs["relaunchedAt"], stamp)
	}
	if got := relaunchStampOf(lines[1]); !got.IsZero() {
		t.Fatalf("a line with no stamp reads back as %s, want the zero time, which the check fails", got)
	}
}
