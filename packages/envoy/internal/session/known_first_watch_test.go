package session

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// knownFirstWatchFailed is the WARN the store's watcher logs when its first watch ends before
// the first read (kvwatch.WithFirstWatchRepairedByOwner), and knownWatchUnrepaired the ERROR the
// store logs once that watch is still unrepaired bus.RepairGrace later (reportUnrepaired).
const (
	knownFirstWatchFailed = `"level":"WARN","msg":"known session store first watch failed; its store repairs it"`
	knownWatchUnrepaired  = `"level":"ERROR","msg":"known session store watch failed"`
)

// knownStoreWhoseFirstWatchEnds opens a known-session store whose bucket opens but whose watcher's
// consumer create the grant drops, so its first watch ends unanswered before its first read, as a
// loaded start's did (INFRA-4240's CPU-starvation control: "known session store watch failed ...
// context deadline exceeded"), and returns it, the grant, its timers and its log.
func knownStoreWhoseFirstWatchEnds(t *testing.T) (*KnownSessions, *testnats.NkeyGrant, *clocktest.Fake, *logtest.Buffer) {
	t.Helper()
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, slices.DeleteFunc(knownBucketGrant(), func(subject string) bool {
		return strings.HasPrefix(subject, "$JS.API.CONSUMER.CREATE.")
	})...)
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	timers := clocktest.New(time.Now())
	logs := &logtest.Buffer{}
	known, err := OpenKnownSessions(conn, WithKnownSessionReplicas(1), withKnownSessionTimers(timers),
		WithKnownSessionLogger(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	testwait.Eventually(t, "the store's first watch to end before its first read", func() bool {
		return known.watch() != nil && known.watch().Err() != nil
	})
	testwait.Eventually(t, "the watcher's WARN for its first watch", func() bool {
		return strings.Contains(logs.String(), knownFirstWatchFailed)
	})
	return known, grant, timers, logs
}

// A known-session store whose first watch ends before its first read repairs it itself, so the end
// is one WARN and no ERROR: the store's repair is the one retry, and the repair answers here once
// the grant carries the consumer. The ERROR the store arms for an unrepaired watch finds it repaired
// when the repair grace passes on the store's clock, and writes nothing.
func TestAKnownSessionFirstWatchTheStoreRepairsIsAWarning(t *testing.T) {
	known, grant, timers, logs := knownStoreWhoseFirstWatchEnds(t)
	reportAt := timers.Now().Add(bus.RepairGrace)

	grant.Grant(t, knownBucketGrant()...)
	// The repair's waits are moved through, one at a time, and never the report's: the repair
	// answers inside the grace, so the report must find it repaired.
	testwait.Until(t, "the store to repair its first watch inside the repair grace", func() ([]time.Time, bool) {
		if known.Opened() {
			return nil, true
		}
		armed := timers.Armed()
		for _, at := range armed {
			if at.Before(reportAt) {
				timers.Set(at)
				break
			}
		}
		return armed, false
	})
	timers.Set(reportAt)
	testwait.Eventually(t, "the store's report to find its watch repaired", func() bool {
		return strings.Contains(logs.String(), `"level":"DEBUG","msg":"known session store watch repaired within the grace"`)
	})
	if n := strings.Count(logs.String(), knownFirstWatchFailed); n != 1 {
		t.Fatalf("%d WARN lines for the first watch that ended, want 1:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("a first watch the store repaired logged an ERROR:\n%s", logs.String())
	}
}

// A first watch the store's repair never answers is the one ERROR once the repair grace has passed
// on the store's clock, and not before: the grant never carries the consumer, so no repair can.
func TestAKnownSessionFirstWatchLeftUnrepairedIsAnErrorOnceTheGracePasses(t *testing.T) {
	known, _, timers, logs := knownStoreWhoseFirstWatchEnds(t)
	reportAt := timers.Now().Add(bus.RepairGrace)
	testwait.Until(t, "the store to arm its report of an unrepaired watch", func() ([]time.Time, bool) {
		armed := timers.Armed()
		return armed, slices.ContainsFunc(armed, reportAt.Equal)
	})
	if strings.Contains(logs.String(), knownWatchUnrepaired) {
		t.Fatalf("the store reported its watch unrepaired before the grace passed:\n%s", logs.String())
	}
	timers.Set(reportAt)
	testwait.Eventually(t, "the ERROR for the unrepaired watch", func() bool {
		return strings.Contains(logs.String(), knownWatchUnrepaired)
	})
	if known.Opened() {
		t.Fatal("the store answers over a watch no grant let it repair")
	}
	if n := strings.Count(logs.String(), knownWatchUnrepaired); n != 1 {
		t.Fatalf("%d ERROR lines for one unrepaired watch, want 1:\n%s", n, logs.String())
	}
}
