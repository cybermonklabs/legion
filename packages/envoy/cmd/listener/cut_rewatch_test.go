package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// cutRewatchCaches are the warm-up lines of the three caches the listener keeps. The reconnect
// after a cut rewatches every one of them (rewatchListenerKVWatchers), and each replacement writes
// one warm-up line, so all three say the caches came back.
var cutRewatchCaches = []string{
	"interest registry cache warm-up",
	"role claims cache warm-up",
	"session registry cache warm-up",
}

// A hub that stops answering runs every KV cache of the listener through kvwatch's replacement
// path, and a watcher a rewatch replaced is no operator's page. The listener declares such a hub
// lost about two seconds into the silence (bus.WithSilentCutDetection in listenerStreamOptions),
// and the reconnect that follows rewatches every cache. The freeze is what reaches this path now:
// on nats.go's default pings the same silence left the listener reading connected for four to six
// minutes, so no reconnect and no rewatch ran at all.
//
// The test asserts four things over the real binary, each read from the output the freeze and the
// thaw wrote:
//   - both of the listener's clients report the cut, so the run is in the rewatch path;
//   - each of the three caches logs a warm-up line after the thaw, so a rewatch replaced each
//     watcher;
//   - an interest another listener writes after the thaw reaches this listener's cache;
//   - the listener logs no ERROR line, so no watcher a rewatch replaced reports its end as a
//     failure of the cache (kvwatch.Watcher.Start).
//
// This listener reaches one of the two branches of kvwatch.Watcher.Start: main opens its stores
// and waits out their warm-ups before it adds the reconnect hook, so a first start of this process
// always ends before a rewatch can replace it, and the ERROR it writes is a live cache's. The
// branch a rewatch replaced is pinned by the unit test of internal/kvwatch. What this test holds
// is the whole cut path, every cache's replacement included, to no ERROR at all.
func TestASilentCutRewatchesTheCachesWithNoErrorLine(t *testing.T) {
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "cut-rewatch")
	listener.waitHealthy(t)

	thaw, output := freezeUntilTheClientReportsTheCut(t, server, listener)
	thaw()

	// Every cache is rewatched on the reconnect, and each replacement's warm-up line says so.
	for _, warmUp := range cutRewatchCaches {
		testwait.Until(t, fmt.Sprintf("the %q line a rewatch's replacement writes after the thaw", warmUp),
			func() (string, bool) {
				logs := output()
				return logs, strings.Contains(logs, warmUp)
			})
	}

	// The cache follows the bucket again: an interest another listener writes after the thaw is
	// listed by this listener, which answers /v1/interests/ from its cache alone.
	writer := testnats.Connect(t, uri)
	t.Cleanup(writer.Close)
	registry, err := store.Open(writer)
	if err != nil {
		t.Fatalf("open the interest registry as another listener: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessionID := "ses_cut_rewatch_writer"
	if _, err := registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "other-listener"}, []string{"notifications.agent." + sessionID}); err != nil {
		t.Fatalf("subscribe through another listener: %v", err)
	}
	testwait.Until(t, "the listener's cache to list an interest another listener wrote after the thaw",
		func() (string, bool) { return output(), listsInterest(t, listener.port, sessionID) })

	for line := range strings.SplitSeq(output(), "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("a silent cut and its rewatch logged an error: %s", line)
		}
	}
}
