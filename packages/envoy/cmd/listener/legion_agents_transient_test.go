package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testwait"
)

// registryRecords counts the records of logs at level whose message is msg.
func registryRecords(logs *lockedBuffer, level, msg string) int {
	n := 0
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["level"] == level && record["msg"] == msg {
			n++
		}
	}
	return n
}

// The first opens of the legion_agents bucket a loaded start makes can fail for now: a request of
// the open unanswered within its wait, or the connection reconnecting (INFRA-4240's CPU-starvation
// control: "open legion agent KV bucket: context deadline exceeded", "... nats: outbound buffer
// limit exceeded"). The registry's retry opens it again a moment later, so each such failure is a
// WARN, not the ERROR an operator reads as a missing grant; the routes answer 503 naming the
// failure meanwhile, as they always did, and serve once an open succeeds. The registry's clock
// stands still here, so the run of failures never outlasts the repair grace.
func TestALegionAgentRegistryOpenThatFailsForNowIsAWarningUntilItOutlastsTheRepairGrace(t *testing.T) {
	for _, transient := range []error{
		fmt.Errorf("open legion agent KV bucket: %w", context.DeadlineExceeded),
		fmt.Errorf("open legion agent KV bucket: open the jetstream handle of KV bucket %s: %w", bus.LegionAgentBucket, natsgo.ErrReconnectBufExceeded),
	} {
		t.Run(transient.Error(), func(t *testing.T) {
			l := newLegionListener(t, setupPublishTestClient(t))
			var failing atomic.Bool
			failing.Store(true)
			var opens atomic.Int32
			logs := &lockedBuffer{}
			registry := startLegionAgentRegistry(l.deps.client.Conn, logging.NewWithWriter("test", logs), func(conn *natsgo.Conn) (*legionagents.Store, error) {
				opens.Add(1)
				if failing.Load() {
					return nil, transient
				}
				return legionagents.Open(conn, legionagents.WithReplicas(1))
			}, 10*time.Millisecond, 40*time.Millisecond, clocktest.New(time.Now()))
			t.Cleanup(func() { registry.close(context.Background()) })
			l.deps.legionAgents = registry

			testwait.Until(t, "the registry's retry to fail a third time", func() (int, bool) {
				warned := registryRecords(logs, "WARN", legionAgentOpenRetried)
				return warned, warned >= 3
			})
			body := l.want(t, http.StatusServiceUnavailable, http.MethodGet, "/v1/legion/agents?issue=INFRA-448", "")
			if !strings.Contains(body, "could not open the legion_agents bucket (a missing grant is the usual cause): "+transient.Error()) {
				t.Fatalf("the 503 while the open fails = %s, want it to name the failed open", body)
			}
			failing.Store(false)
			testwait.Until(t, "the registry to serve once its open recovered", func() (int, bool) {
				code := l.do(t, http.MethodGet, "/v1/legion/agents?issue=INFRA-448", "").Code
				return code, code == http.StatusOK
			})
			if n := registryRecords(logs, "ERROR", legionAgentOpenUnavailable); n != 0 {
				t.Fatalf("%d ERROR records over %d opens that failed for now and recovered, want none:\n%s", n, opens.Load(), logs.String())
			}
			if n := registryRecords(logs, "INFO", "legion agent registry opened"); n != 1 {
				t.Fatalf("%d INFO records say the registry opened, want 1:\n%s", n, logs.String())
			}
		})
	}
}

// The ERROR stays where its operator must see it: an open the server refused under the grant is
// the one ERROR at its first failure, with no WARN, however often the retry fails; and a run of
// transient failures that outlasts bus.RepairGrace on the registry's clock is the one ERROR once it
// has, the WARNs before it.
func TestALegionAgentRegistryOpenThatFailsForRealIsAnError(t *testing.T) {
	t.Run("a refused grant", func(t *testing.T) {
		l := newLegionListener(t, setupPublishTestClient(t))
		var opens atomic.Int32
		logs := &lockedBuffer{}
		refused := fmt.Errorf("open legion agent KV bucket: KV_%s STREAM.INFO: %w to \"$JS.API.STREAM.INFO.KV_%s\": %w",
			bus.LegionAgentBucket, bus.ErrPublishDenied, bus.LegionAgentBucket, context.DeadlineExceeded)
		registry := startLegionAgentRegistry(l.deps.client.Conn, logging.NewWithWriter("test", logs), func(*natsgo.Conn) (*legionagents.Store, error) {
			opens.Add(1)
			return nil, refused
		}, 10*time.Millisecond, 40*time.Millisecond, clocktest.New(time.Now()))
		t.Cleanup(func() { registry.close(context.Background()) })
		testwait.Until(t, "the registry's retry to fail a third time", func() (int32, bool) {
			n := opens.Load()
			return n, n >= 3
		})
		if errs, warns := registryRecords(logs, "ERROR", legionAgentOpenUnavailable), registryRecords(logs, "WARN", legionAgentOpenRetried); errs != 1 || warns != 0 {
			t.Fatalf("a refused grant over %d opens logged %d ERROR and %d WARN records, want 1 ERROR and no WARN:\n%s", opens.Load(), errs, warns, logs.String())
		}
	})

	t.Run("a transient failure that outlasts the repair grace", func(t *testing.T) {
		l := newLegionListener(t, setupPublishTestClient(t))
		var opens atomic.Int32
		logs := &lockedBuffer{}
		fake := clocktest.New(time.Now())
		registry := startLegionAgentRegistry(l.deps.client.Conn, logging.NewWithWriter("test", logs), func(*natsgo.Conn) (*legionagents.Store, error) {
			opens.Add(1)
			return nil, fmt.Errorf("open legion agent KV bucket: %w", context.DeadlineExceeded)
		}, 10*time.Millisecond, 40*time.Millisecond, fake)
		t.Cleanup(func() { registry.close(context.Background()) })
		testwait.Until(t, "the registry's retry to fail a second time", func() (int, bool) {
			warned := registryRecords(logs, "WARN", legionAgentOpenRetried)
			return warned, warned >= 2
		})
		if n := registryRecords(logs, "ERROR", legionAgentOpenUnavailable); n != 0 {
			t.Fatalf("%d ERROR records inside the repair grace, want none:\n%s", n, logs.String())
		}
		fake.Advance(bus.RepairGrace)
		testwait.Until(t, "the ERROR once the failures outlasted the repair grace", func() (int, bool) {
			n := registryRecords(logs, "ERROR", legionAgentOpenUnavailable)
			return n, n >= 1
		})
		after := opens.Load()
		testwait.Until(t, "the retry to fail twice more after the ERROR", func() (int32, bool) {
			n := opens.Load()
			return n, n >= after+2
		})
		if n := registryRecords(logs, "ERROR", legionAgentOpenUnavailable); n != 1 {
			t.Fatalf("%d ERROR records for one run of failures past the repair grace, want 1:\n%s", n, logs.String())
		}
	})
}
