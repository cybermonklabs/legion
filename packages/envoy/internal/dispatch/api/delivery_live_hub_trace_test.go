package api

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
)

// levelledRecorder keeps the records at level and above, as the server's handler does at the
// level DISPATCH_LOG_LEVEL names, so slog.Default().Enabled answers what it answers there.
type levelledRecorder struct {
	logtest.Recorder
	level slog.Level
}

func (r *levelledRecorder) Enabled(_ context.Context, level slog.Level) bool {
	return level >= r.level
}

// A recompute writes the debug line that times it only while the server logs at debug
// (recomputeTrace): at info the hub recomputes a new view and logs its shared line, and writes no
// line of its times; at debug it writes one, naming the view, the cause and when it began and
// ended.
func TestTheHubTimesARecomputeOnlyAtDebug(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			records := &levelledRecorder{level: level}
			logtest.SetDefault(t, records)
			rig := newHubRig(t, nil)
			seedHubSettings(t, rig.database)
			conn := newFakeLiveConn()
			rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
			conn.snapshot(t, "m")
			// Every recompute has ended, its lines written, once the hub has stopped.
			rig.stop()
			shared, timed := 0, 0
			for _, record := range records.Records() {
				if strings.HasPrefix(record.Message, "dispatch: delivery live recompute view="+deliveryLiveMeasures+" cause="+deliveryCauseSubscribe) {
					shared++
				}
				if record.Message != "dispatch: delivery live hub timed a recompute" {
					continue
				}
				attrs := map[string]slog.Value{}
				record.Attrs(func(attr slog.Attr) bool {
					attrs[attr.Key] = attr.Value
					return true
				})
				if attrs["view"].String() != deliveryLiveMeasures || attrs["cause"].String() != deliveryCauseSubscribe {
					continue
				}
				timed++
				if record.Level != slog.LevelDebug {
					t.Errorf("the recompute's times were logged at %v, want debug", record.Level)
				}
				began, ended := attrs["began_unix_us"].Int64(), attrs["ended_unix_us"].Int64()
				if began <= 0 || ended < began {
					t.Errorf("the recompute's times: began_unix_us=%d ended_unix_us=%d, want a beginning and an end after it", began, ended)
				}
			}
			if shared == 0 {
				t.Fatalf("no recompute of the measures view for its subscribe was logged at %v", level)
			}
			want := 0
			if level == slog.LevelDebug {
				want = 1
			}
			if timed != want {
				t.Errorf("at %v the subscribe's recompute logged %d lines of its times, want %d", level, timed, want)
			}
		})
	}
}
