package evals

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestEverySignalEntryHasItsSeriesColAndUnit is the inventory of the signal entries the daemon's
// record `admission.signalChanges` holds at dispatch://INFRA-5899's version 14, after
// dispatch://INFRA-3199's version 52: the value and the limit of each of the five signals, the hold
// of each, and the switch, 16 kinds, each with the series, `col` and unit root R39's Design part
// 'The admission eval' gives it (signalSeries). It fails for a kind with no mapping. A `provider`
// entry of the part `refusals`, which that version took out of the record, an entry whose `what`
// is `other`, one of a signal the record never names and a switch that names a signal are each
// skipped by the writer's list of entries (admissionEntries) and logged with their row key, and the
// 16 others are each in the list once.
func TestEverySignalEntryHasItsSeriesColAndUnit(t *testing.T) {
	t.Parallel()
	kinds := []struct {
		signal, part            *string
		what, series, col, unit string
	}{
		{new("idle"), nil, signalValue, "admission-idle", "value", "share"},
		{new("idle"), nil, signalLimit, "admission-idle", "limit", "share"},
		{new("queue"), nil, signalValue, "admission-queue", "value", "hours"},
		{new("queue"), nil, signalLimit, "admission-queue", "limit", "hours"},
		{new("provider"), new("headroom"), signalValue, "admission-headroom", "value", "share of limit"},
		{new("provider"), new("headroom"), signalLimit, "admission-headroom", "limit", "share of limit"},
		{new("kills"), nil, signalValue, "admission-kills", "value", "count"},
		{new("kills"), nil, signalLimit, "admission-kills", "limit", "count"},
		{new("approvals"), nil, signalValue, "admission-approvals", "value", "count"},
		{new("approvals"), nil, signalLimit, "admission-approvals", "limit", "count"},
		{new("idle"), nil, signalHolds, "admission-holds", "idle", ""},
		{new("queue"), nil, signalHolds, "admission-holds", "queue", ""},
		{new("provider"), new("headroom"), signalHolds, "admission-holds", "provider.headroom", ""},
		{new("kills"), nil, signalHolds, "admission-holds", "kills", ""},
		{new("approvals"), nil, signalHolds, "admission-holds", "approvals", ""},
		{nil, nil, signalSwitch, "admission-switch", "", ""},
	}
	if len(kinds) != 16 {
		t.Fatalf("the inventory lists %d kinds, want the 16 of the daemon's record", len(kinds))
	}
	at := time.Date(2026, 11, 26, 7, 0, 0, 0, time.UTC)
	var changes []legion.SignalChange
	for index, kind := range kinds {
		change := legion.SignalChange{ID: int64(index + 1), At: at, Signal: kind.signal, Part: kind.part, What: kind.what, Value: new(1.0)}
		series, col, unit, known := signalSeries(change)
		if !known || series != kind.series || col != kind.col || unit != kind.unit {
			t.Errorf("the %s of %s/%s is in series %q col %q unit %q (known %t), want series %q col %q unit %q",
				kind.what, textOrNone(kind.signal), textOrNone(kind.part), series, col, unit, known, kind.series, kind.col, kind.unit)
		}
		changes = append(changes, change)
	}
	unknown := []legion.SignalChange{
		{Signal: new("provider"), Part: new("refusals"), What: signalValue},
		{Signal: new("provider"), Part: new("refusals"), What: signalLimit},
		{Signal: new("provider"), Part: new("refusals"), What: signalHolds},
		{Signal: new("provider"), What: signalValue},
		{Signal: new("idle"), What: "other"},
		{Signal: new("memory"), What: signalValue},
		{Signal: new("idle"), What: signalSwitch},
	}
	for index := range unknown {
		change := &unknown[index]
		change.ID, change.At, change.Value = int64(len(kinds)+index+1), at, new(1.0)
		if series, col, unit, known := signalSeries(*change); known {
			t.Errorf("the %s of %s/%s is in series %q col %q unit %q, want it unknown", change.What, textOrNone(change.Signal), textOrNone(change.Part), series, col, unit)
		}
	}

	logs := &logtest.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	record, err := json.Marshal(map[string]any{"cap": 3, "signalChanges": append(changes, unknown...)})
	if err != nil {
		t.Fatalf("encode the admission block: %v", err)
	}
	var block legion.Admission
	if err := json.Unmarshal(record, &block); err != nil {
		t.Fatalf("decode the admission block: %v", err)
	}
	entries := admissionEntries(legion.AdmissionState{Project: "EVAL", StateID: "store", Seq: 9, At: at, Blocks: []*legion.Admission{&block, &block}}, newLoggedEntries(), logger)
	if len(entries) != len(kinds) {
		t.Fatalf("the writer's list holds %d entries %v, want the %d known kinds once each", len(entries), entries, len(kinds))
	}
	for index, entry := range entries {
		kind := kinds[index]
		if entry.RowKind != rowKindSignalChange || entry.RowKey != fmt.Sprintf("store:%d", index+1) ||
			entry.Series != kind.series || entry.Col != kind.col || entry.Unit != kind.unit {
			t.Errorf("entry %d is %+v, want the signal_change of store:%d in series %q col %q unit %q", index, entry, index+1, kind.series, kind.col, kind.unit)
		}
	}
	logged := map[string]int{}
	for _, line := range logtest.JSONLines(t, logs.String()) {
		if line["msg"] == AdmissionUnknownLine && line["level"] == "WARN" && line["project"] == "EVAL" {
			logged[fmt.Sprint(line["row_key"])]++
		}
	}
	for _, change := range unknown {
		if key := fmt.Sprintf("store:%d", change.ID); logged[key] != 1 {
			t.Errorf("the unknown entry %s (%s of %s/%s) is logged %d times, want once at WARN: %v",
				key, change.What, textOrNone(change.Signal), textOrNone(change.Part), logged[key], logged)
		}
	}
	if len(logged) != len(unknown) {
		t.Errorf("the log names the unknown entries %v, want the %d unknown entries alone", logged, len(unknown))
	}
}

// TestTheLoggedEntriesHoldTheNewerHalfPastTheirBound: the set of the entries a writer has logged as
// skipped or unknown (loggedEntries) answers first once for a key, of eight goroutines asking at
// once, and again for the same row under another code. Once it holds loggedEntryBound keys, the
// next key drops the older half: a key of that half is logged again, and a key of the newer half,
// and the key that passed the bound, are not.
func TestTheLoggedEntriesHoldTheNewerHalfPastTheirBound(t *testing.T) {
	t.Parallel()
	logged := newLoggedEntries()
	key := func(id int) loggedEntry {
		return loggedEntry{project: "EVAL", rowKind: rowKindCapChange, rowKey: admissionRowKey("store", int64(id)), code: CodeMeasuredAtOutOfRange}
	}
	var firsts atomic.Int64
	var all sync.WaitGroup
	for range 8 {
		all.Go(func() {
			if logged.first(key(0)) {
				firsts.Add(1)
			}
		})
	}
	all.Wait()
	if n := firsts.Load(); n != 1 {
		t.Fatalf("eight goroutines asking for one key were answered first %d times, want once", n)
	}
	otherCode := key(0)
	otherCode.code = CodeEvalNotFound
	if !logged.first(otherCode) || logged.first(otherCode) {
		t.Fatal("the same row under another code is not logged once")
	}
	for id := 1; id < loggedEntryBound-1; id++ {
		if !logged.first(key(id)) {
			t.Fatalf("key %d is answered logged before, want its first time", id)
		}
	}
	if logged.first(key(0)) || logged.first(key(loggedEntryBound-2)) {
		t.Fatal("a key the set holds is answered first again before the bound")
	}
	if !logged.first(key(loggedEntryBound)) {
		t.Fatalf("the key past the bound is answered logged before, want its first time")
	}
	// The set held key 0, its other code and keys 1 to loggedEntryBound-2: the older half is key 0,
	// its other code and keys 1 to loggedEntryBound/2-2.
	for _, id := range []int{0, loggedEntryBound/2 - 2} {
		if !logged.first(key(id)) {
			t.Errorf("key %d of the older half is answered logged before, want it dropped and logged again", id)
		}
	}
	for _, id := range []int{loggedEntryBound/2 - 1, loggedEntryBound - 2, loggedEntryBound} {
		if logged.first(key(id)) {
			t.Errorf("key %d of the newer half is answered first again, want it held", id)
		}
	}
}
