package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
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
//   - the listener's main NATS client reports the cut before the hub thaws;
//   - each of the three caches logs a warm-up line after the thaw, so a rewatch replaced each
//     watcher;
//   - an interest written through another NATS connection after the thaw reaches this listener's cache;
//   - the listener logs no ERROR line, so no watcher a rewatch replaced reports its end as a
//     failure of the cache (kvwatch.Watcher.Start).
//
// This listener reaches one of the two branches of kvwatch.Watcher.Start: main opens its stores
// and waits out their warm-ups before it adds the reconnect hook, so a first start of this process
// always ends before a rewatch can replace it, and the ERROR it writes is a live cache's. The
// branch a rewatch replaced is pinned by the unit test of internal/kvwatch. What this test holds
// is the whole cut path, every cache's replacement included, to no ERROR at all.
func TestASilentCutRewatchesTheCachesWithNoErrorLine(t *testing.T) {
	t.Parallel()
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, promptListenerBinary(t), uri, "cut-rewatch")
	listener.waitHealthy(t)

	thaw, output, _ := freezeUntilTheClientReportsTheCut(t, server, listener)
	beforeThaw := len(output())
	thaw()

	// Every cache is rewatched on the reconnect, and each replacement's warm-up line says so.
	for _, warmUp := range cutRewatchCaches {
		testwait.Until(t, fmt.Sprintf("the listener to log %q after the thaw", warmUp), func() (string, bool) {
			logged := output()[beforeThaw:]
			return logged, strings.Contains(logged, `"msg":"`+warmUp+`"`)
		})
	}

	// The listener's cache follows the bucket again. Write a marker through a separate NATS
	// connection so only its watcher, not a local write-through, can make it visible to /v1.
	ctx := testwait.Context(t)
	writer := testnats.Connect(t, uri)
	t.Cleanup(writer.Close)
	js, err := jetstream.New(writer, testnats.RequestTimeout(t))
	if err != nil {
		t.Fatalf("open JetStream for the independent writer: %v", err)
	}
	interests, err := js.KeyValue(ctx, bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket for the independent writer: %v", err)
	}
	const sessionID = "ses_cut_rewatch_writer"
	interest, err := json.Marshal(store.Interest{
		SessionID: sessionID,
		MachineID: "other-listener",
		Topics:    []string{contracts.LegacyAgentSubject(sessionID)},
		UpdatedAt: contracts.NowMillis(),
	})
	if err != nil {
		t.Fatalf("encode the interest marker: %v", err)
	}
	if _, err := interests.Put(ctx, sessionID, interest); err != nil {
		t.Fatalf("write the interest marker after the thaw: %v", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/interests/", listener.port)
	testwait.Until(t, "the listener's cache to list the interest written after the thaw", func() (string, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+listenerTestToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err.Error(), false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return resp.Status, false
		}
		var listed []store.Interest
		if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
			return err.Error(), false
		}
		for _, item := range listed {
			if item.SessionID == sessionID {
				return "", true
			}
		}
		return fmt.Sprintf("%+v", listed), false
	})

	// The marker was written after all three warm-up records. Check the log through its read,
	// not through a quiet window.
	for line := range strings.SplitSeq(output(), "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("a silent cut and its rewatch logged an error: %s", line)
		}
	}
}
