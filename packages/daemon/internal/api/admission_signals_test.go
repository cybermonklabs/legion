package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The routes of the settings of admission by signals and the state's readings of it.
const (
	controllerSignalsRoute = "/legion/v1/project/signals"
	operatorSignalsRoute   = "/legion/v1/operator/project/signals"
)

// signalsStartsAt is where a signals harness's clock starts.
var signalsStartsAt = time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

// signalsHarness is the harness whose API applies its facts through the daemon's own handlers,
// admission among them, with the live signals over the store's settings of admission by signals
// registered as the daemon registers them, on a clock the test moves: the routes' clock, the
// passes' and the state's read. Its state route serves admission by signals as the daemon's
// source does (admit.LiveSignals.State, store.SignalChangesTx).
type signalsHarness struct {
	*harness
	admission *admit.Admission
	live      *admit.LiveSignals
	handlers  []intake.Handler
	now       time.Time
}

func newSignalsHarness(t *testing.T) *signalsHarness {
	t.Helper()
	h := &signalsHarness{harness: newHarness(t), now: signalsStartsAt}
	records := record.NewStore()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := func() time.Time { return h.now }
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quiet)
	h.admission = admit.New(records, engine, 2, testDispatchProject, quiet)
	h.admission.UseClock(clock)
	h.live = admit.NewLiveSignals(h.admission, admit.StoreSignalSettings{}, ModelRequestsContract())
	h.admission.UseSignals(h.live.Read)
	h.admission.UseLiveSignals(h.live, func(record.SignalDeadline) {})
	h.handlers = admit.Handlers(records, testDispatchProject, engine, h.admission)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Log: quiet,
		Pool: h.store.Pool(), Record: records, Handlers: h.handlers, Now: clock, State: signalsSource{h}, StateTransactions: h.store.Pool(),
	}).Handler
	return h
}

// signalsSource is the state of the harness's project with admission by signals as the daemon's
// source serves it (daemon's source.State), at the harness's clock.
type signalsSource struct{ h *signalsHarness }

func (s signalsSource) State(ctx context.Context, tx pgx.Tx) (State, []supervise.Claim, error) {
	state := State{Daemon: DaemonInfo{Project: testDispatchProject}, Admission: Admission{Cap: 2}}
	reading, err := s.h.live.State(ctx, tx, s.h.now)
	if err != nil {
		return State{}, nil, err
	}
	state.Admission.Signals = &reading
	state.Admission.SignalChanges, err = store.SignalChangesTx(ctx, tx, testDispatchProject, store.SignalChangesShown)
	return state, nil, err
}

// apply applies fact under id through the harness's handlers, at its clock.
func (h *signalsHarness) apply(source, id string, fact intake.Fact) {
	h.t.Helper()
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), source, id, fact, h.handlers...); err != nil {
		h.t.Fatalf("apply %s/%s: %v", source, id, err)
	}
}

// change sends body to target, the operator's route with its bearer or the controller's with no
// header, and answers the status and the decoded answer.
func (h *signalsHarness) change(target string, body any) (int, ProjectSignalsResponse, string) {
	h.t.Helper()
	var header http.Header
	if target == operatorSignalsRoute {
		header = bearer
	}
	recorder := h.request(http.MethodPost, target, body, header)
	var answer ProjectSignalsResponse
	if recorder.Code == http.StatusOK {
		decodeInto(h.t, recorder, &answer)
	}
	return recorder.Code, answer, recorder.Body.String()
}

// changed sends body to target and requires a 200, answering the answer.
func (h *signalsHarness) changed(target string, body any) ProjectSignalsResponse {
	h.t.Helper()
	status, answer, raw := h.change(target, body)
	if status != http.StatusOK {
		h.t.Fatalf("POST %s %+v = %d, want 200; body %s", target, body, status, raw)
	}
	return answer
}

// standing is everything a change could have moved: the settings, the record of changes and the
// committed deadline, as text.
func (h *signalsHarness) standing() string {
	h.t.Helper()
	var settings store.SignalSettings
	var entries []store.SignalChange
	var deadline record.SignalDeadline
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if settings, err = store.SignalSettingsTx(ctx, tx, testDispatchProject); err != nil {
			return err
		}
		if entries, err = store.SignalChangesTx(ctx, tx, testDispatchProject, 1000); err != nil {
			return err
		}
		deadline, err = record.NewStore().SignalDeadline(ctx, tx, testDispatchProject)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		Settings store.SignalSettingsView
		Entries  []store.SignalChange
		Deadline record.SignalDeadline
	}{settings.View(), entries, deadline})
	if err != nil {
		h.t.Fatal(err)
	}
	return string(encoded)
}

// limitOf is the answer's limit of series, by its signal and part.
func limitOf(t *testing.T, settings SignalSettingsView, series string) store.SignalLimitView {
	t.Helper()
	signal, part := store.SignalPart(series)
	for _, limit := range settings.Limits {
		if limit.Signal == signal && (limit.Part == nil) == (part == "") && (limit.Part == nil || *limit.Part == part) {
			return limit
		}
	}
	t.Fatalf("the settings %+v name no limit of %s", settings, series)
	return store.SignalLimitView{}
}

// operatorChange is the operator's request of action on series (signal and part by
// store.SignalPart, none for ""), at limit, for reason.
func operatorChange(action, series string, limit *float64, reason string) OperatorSignalsRequest {
	req := OperatorSignalsRequest{Action: action, Limit: limit, Reason: reason}
	if series != "" {
		req.Signal, req.Part = store.SignalPart(series)
	}
	return req
}

// TestAdmissionSignalSettings: a person turns admission by signals on and off and sets and clears
// each limit through the operator's route and through the controller's (S3); a fresh project is
// off with no limit; a caller that is neither is refused; each refusal answers the reason and
// changes nothing, no setting, no entry and no deadline; and a limit is held to its series' range
// (signals.CheckLimit).
func TestAdmissionSignalSettings(t *testing.T) {
	operator := store.PauseActor{Kind: store.PauseByOperator}
	t.Run("defaults", func(t *testing.T) {
		h := newSignalsHarness(t)
		answer := h.changed(operatorSignalsRoute, operatorChange(SignalsClearLimit, signals.Kills, nil, "the owner asked whether a limit stands"))
		if answer.Changed || answer.Settings.On || answer.Settings.Switch != nil || len(answer.Settings.Limits) != len(signals.Names()) {
			t.Fatalf("a fresh project's settings = %+v, want off, no switch's change, and the six series", answer)
		}
		for i, series := range signals.Names() {
			signal, part := store.SignalPart(series)
			got := answer.Settings.Limits[i]
			if got.Signal != signal || (got.Part == nil) != (part == "") || got.Limit != nil || got.Set != nil {
				t.Errorf("a fresh project's limit %d = %+v, want %s/%s with no limit and no change", i, got, signal, part)
			}
		}
	})
	t.Run("operator", func(t *testing.T) {
		h := newSignalsHarness(t)
		on := h.changed(operatorSignalsRoute, operatorChange(SignalsOn, "", nil, "the owner turned it on"))
		if !on.Changed || !on.Settings.On || on.Settings.Switch == nil ||
			*on.Settings.Switch != (store.SignalSet{By: operator, At: signalsStartsAt, Reason: "the owner turned it on"}) {
			t.Fatalf("on = %+v, want it on, set by the operator now for the reason", on)
		}
		h.now = h.now.Add(time.Minute)
		set := h.changed(operatorSignalsRoute, operatorChange(SignalsSetLimit, signals.Queue, new(2.5), "the owner set the queue's limit"))
		if got := limitOf(t, set.Settings, signals.Queue); got.Limit == nil || *got.Limit != 2.5 || got.Set == nil || got.Set.By != operator || !got.Set.At.Equal(h.now) {
			t.Fatalf("the queue's limit = %+v, want 2.5 set by the operator now", got)
		}
		if again := h.changed(operatorSignalsRoute, operatorChange(SignalsSetLimit, signals.Queue, new(2.5), "again")); again.Changed {
			t.Fatalf("the same limit again = %+v, want nothing changed", again)
		}
		off := h.changed(operatorSignalsRoute, operatorChange(SignalsOff, "", nil, "the owner turned it off"))
		if !off.Changed || off.Settings.On {
			t.Fatalf("off = %+v, want it off", off)
		}
	})
	t.Run("controller", func(t *testing.T) {
		h := newSignalsHarness(t)
		grant := h.controllerSession()
		answer := h.changed(controllerSignalsRoute, ProjectSignalsRequest{GrantID: grant, Action: SignalsSetLimit, Signal: signals.Provider,
			Part: store.PartRefusals, Limit: new(3.0), Reason: "the owner's word in the weekly plan"})
		want := store.PauseActor{Kind: store.PauseByController, Session: controllerSession}
		if got := limitOf(t, answer.Settings, signals.Refusals); got.Limit == nil || *got.Limit != 3 || got.Set == nil || got.Set.By != want {
			t.Fatalf("the refusals' limit through the controller = %+v, want 3 set by %+v", got, want)
		}
		operatorGrant := controllerGrant(t, h.harness).GrantID
		on := h.changed(controllerSignalsRoute, ProjectSignalsRequest{GrantID: operatorGrant, Action: SignalsOn, Reason: "the operator's grant"})
		if on.Settings.Switch == nil || on.Settings.Switch.By != operator {
			t.Fatalf("on through the operator's grant = %+v, want it the operator's", on)
		}
	})
	t.Run("not the controller", func(t *testing.T) {
		h := newSignalsHarness(t)
		before := h.standing()
		worker := newLiveClaim(t, h.harness, "LEGION-208", claim.RoleImplementer)
		recorder := h.request(http.MethodPost, controllerSignalsRoute, ProjectSignalsRequest{GrantID: worker.grant(t), Action: SignalsOn, Reason: "a worker's word"}, nil)
		refusalServed(t, recorder, controllerSignalsRoute, http.StatusForbidden, "CONTROLLER_REQUIRED")
		for _, header := range []http.Header{nil, {"Authorization": {"Bearer not-the-operator"}}} {
			wantRefusal(t, h.request(http.MethodPost, operatorSignalsRoute, operatorChange(SignalsOn, "", nil, "x"), header), http.StatusForbidden, invalidOperatorToken)
		}
		if after := h.standing(); after != before {
			t.Fatalf("the refused callers changed %s into %s", before, after)
		}
	})
	// ranges: each series refuses 0 and a limit above its top and takes its top; the three that
	// count events refuse a fraction; every refusal answers SIGNAL_SETTING_INVALID with the reason
	// and changes nothing.
	t.Run("ranges", func(t *testing.T) {
		h := newSignalsHarness(t)
		tops := map[string]float64{signals.Idle: 1, signals.Queue: 168, signals.Provider: 10, signals.Refusals: 1_000_000, signals.Kills: 1_000_000, signals.Approvals: 1_000_000}
		whole := []string{signals.Refusals, signals.Kills, signals.Approvals}
		for _, series := range signals.Names() {
			top, held := tops[series]
			if !held {
				t.Fatalf("the series %s has no top in this test", series)
			}
			refused := []float64{0, top + 0.5, -1}
			if slices.Contains(whole, series) {
				refused = append(refused, 2.5, top+1)
			} else {
				refused = append(refused, top*1.0001)
			}
			for _, limit := range refused {
				before := h.standing()
				status, _, raw := h.change(operatorSignalsRoute, operatorChange(SignalsSetLimit, series, &limit, "out of range"))
				if status != http.StatusBadRequest || !json.Valid([]byte(raw)) || !containsCode(raw, signalSettingInvalid) {
					t.Errorf("%s at %v = %d %s, want 400 %s", series, limit, status, raw, signalSettingInvalid)
				}
				if after := h.standing(); after != before {
					t.Errorf("%s at %v, refused, changed %s into %s", series, limit, before, after)
				}
			}
			answer := h.changed(operatorSignalsRoute, operatorChange(SignalsSetLimit, series, &top, "the top of the range"))
			if got := limitOf(t, answer.Settings, series); got.Limit == nil || *got.Limit != top {
				t.Errorf("%s at its top %v = %+v, want it set", series, top, got)
			}
		}
	})
	// bad names and actions: a provider limit with no part or another, a part on another signal, a
	// signal that is none, an action that is none, an on or off naming a limit, a set with no limit,
	// a clear with one and a blank reason are each refused, changing nothing.
	t.Run("bad requests", func(t *testing.T) {
		h := newSignalsHarness(t)
		for _, tc := range []struct {
			name string
			req  OperatorSignalsRequest
		}{
			{"a provider limit with no part", OperatorSignalsRequest{Action: SignalsSetLimit, Signal: signals.Provider, Limit: new(0.5), Reason: "r"}},
			{"a provider limit of another part", OperatorSignalsRequest{Action: SignalsSetLimit, Signal: signals.Provider, Part: "tokens", Limit: new(0.5), Reason: "r"}},
			{"a part on another signal", OperatorSignalsRequest{Action: SignalsSetLimit, Signal: signals.Kills, Part: store.PartHeadroom, Limit: new(2.0), Reason: "r"}},
			{"a signal that is none", OperatorSignalsRequest{Action: SignalsSetLimit, Signal: "memory", Limit: new(2.0), Reason: "r"}},
			{"the refusals by their own name", OperatorSignalsRequest{Action: SignalsClearLimit, Signal: signals.Refusals, Reason: "r"}},
			{"an action that is none", OperatorSignalsRequest{Action: "raise", Reason: "r"}},
			{"on naming a limit", OperatorSignalsRequest{Action: SignalsOn, Signal: signals.Kills, Limit: new(2.0), Reason: "r"}},
			{"a set with no limit", OperatorSignalsRequest{Action: SignalsSetLimit, Signal: signals.Kills, Reason: "r"}},
			{"a clear with a limit", OperatorSignalsRequest{Action: SignalsClearLimit, Signal: signals.Kills, Limit: new(2.0), Reason: "r"}},
			{"a blank reason", OperatorSignalsRequest{Action: SignalsOn, Reason: " \n"}},
		} {
			before := h.standing()
			status, _, raw := h.change(operatorSignalsRoute, tc.req)
			if status != http.StatusBadRequest || !containsCode(raw, signalSettingInvalid) {
				t.Errorf("%s = %d %s, want 400 %s", tc.name, status, raw, signalSettingInvalid)
			}
			if after := h.standing(); after != before {
				t.Errorf("%s, refused, changed %s into %s", tc.name, before, after)
			}
		}
	})
	t.Run("clear while on", func(t *testing.T) {
		h := newSignalsHarness(t)
		h.changed(operatorSignalsRoute, operatorChange(SignalsOn, "", nil, "on"))
		h.changed(operatorSignalsRoute, operatorChange(SignalsSetLimit, signals.Kills, new(5.0), "five kills"))
		cleared := h.changed(operatorSignalsRoute, operatorChange(SignalsClearLimit, signals.Kills, nil, "the owner cleared it"))
		if got := limitOf(t, cleared.Settings, signals.Kills); !cleared.Changed || !cleared.Settings.On || got.Limit != nil || got.Set == nil || got.Set.Reason != "the owner cleared it" {
			t.Fatalf("the clear while on = %+v (kills %+v), want it changed, still on, with no limit and the clear's reason", cleared, got)
		}
	})
}

// containsCode says whether raw, a refusal's body, carries code.
func containsCode(raw, code string) bool {
	var failure Failure
	return json.Unmarshal([]byte(raw), &failure) == nil && failure.Code == code && failure.Error != ""
}

// served is the harness's state as the state route serves it, decoded.
func (h *signalsHarness) served() map[string]any {
	h.t.Helper()
	recorder := h.request(http.MethodGet, "/legion/v1/state", nil, nil)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("GET /legion/v1/state = %d; body %s", recorder.Code, recorder.Body)
	}
	var body map[string]any
	decodeInto(h.t, recorder, &body)
	return body
}

// servedSignals is the served state's `admission.signals` and `admission.signalChanges`.
func (h *signalsHarness) servedSignals() (map[string]any, []any) {
	h.t.Helper()
	admission, _ := h.served()["admission"].(map[string]any)
	reading, _ := admission["signals"].(map[string]any)
	changes, held := admission["signalChanges"].([]any)
	if reading == nil || !held {
		h.t.Fatalf("the state's admission %v carries no signals or no signalChanges", admission)
	}
	return reading, changes
}

// seriesServed is each series of a served reading, by "<signal>/<part>".
func seriesServed(t *testing.T, reading map[string]any) map[string]map[string]any {
	t.Helper()
	rows, _ := reading["series"].([]any)
	byName := map[string]map[string]any{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		part, _ := row["part"].(string)
		byName[fmt.Sprintf("%s/%s", row["signal"], part)] = row
	}
	return byName
}

// seriesKey is series as seriesServed names it.
func seriesKey(series string) string {
	signal, part := store.SignalPart(series)
	return signal + "/" + part
}

// The members of a series of `admission.signals` and of an entry of `admission.signalChanges`,
// each written whether null or not.
var (
	seriesMembers = []string{"absent", "holds", "limit", "limitSet", "part", "signal", "since", "value"}
	entryMembers  = []string{"absent", "at", "id", "part", "reason", "setBy", "signal", "value", "what"}
)

// TestAdmissionSignalState: `legion state --json` gives, for each of the six series, its value or
// why it cannot be computed, its limit or none, whether it holds admission and since when (S2),
// computed at the read from what the daemon keeps of the inputs; a series that cannot be computed
// shows no value and holds no start; a hold no committed pass dated is told from a dated one by a
// null since; and `admission.signalChanges` is `[]` before the first change and then the newest
// 50 newest first, each with exactly the nine members of S6 (S6).
func TestAdmissionSignalState(t *testing.T) {
	h := newSignalsHarness(t)
	reading, changes := h.servedSignals()
	if len(changes) != 0 || reading["on"] != false || reading["switch"] != nil {
		t.Fatalf("a fresh project's state = %v and changes %v, want off, no switch's change and []", reading, changes)
	}
	// series-inventory: every series of signals.Names() is served under its signal and part, with
	// every member, the provider's two parts apart.
	t.Run("series-inventory", func(t *testing.T) {
		byName := seriesServed(t, reading)
		for _, series := range signals.Names() {
			row, held := byName[seriesKey(series)]
			if !held {
				t.Errorf("the state serves no series %s", seriesKey(series))
				continue
			}
			if got := slices.Sorted(maps.Keys(row)); !slices.Equal(got, seriesMembers) {
				t.Errorf("the series %s carries %v, want exactly %v", seriesKey(series), got, seriesMembers)
			}
		}
		if len(byName) != len(signals.Names()) {
			t.Errorf("the state serves %d series %v, want the %d of signals.Names()", len(byName), slices.Collect(maps.Keys(byName)), len(signals.Names()))
		}
	})
	// absence: before the daemon keeps an input its series has no value, a reason, and no hold, a
	// limit set below what it would read notwithstanding; the idle share is the daemon's own.
	h.changed(operatorSignalsRoute, operatorChange(SignalsOn, "", nil, "on"))
	h.changed(operatorSignalsRoute, operatorChange(SignalsSetLimit, signals.Queue, new(0.25), "a quarter hour"))
	byName := seriesServed(t, func() map[string]any { r, _ := h.servedSignals(); return r }())
	if queue := byName[seriesKey(signals.Queue)]; queue["value"] != nil || queue["absent"] == nil || queue["absent"] == "" || queue["holds"] != false || queue["limit"] != 0.25 {
		t.Errorf("the queue with no input kept = %v, want no value, a reason, no hold, and its limit", queue)
	}
	if idle := byName[seriesKey(signals.Idle)]; idle["value"] != 0.0 || idle["absent"] != nil {
		t.Errorf("the idle share with no holder = %v, want 0 and no reason", idle)
	}
	// holds and since: a kill reported for a minute after the pass's moment holds at the read a
	// minute later, with no pass that dated it (since null); the next pass dates it.
	h.apply("daemon", "boot", intake.DaemonBooted{Boot: 1})
	h.now = h.now.Add(2 * time.Hour)
	h.changed(operatorSignalsRoute, operatorChange(SignalsSetLimit, signals.Kills, new(1.0), "one kill"))
	h.apply("launcher", "kill-1", intake.MemoryGuardKill{Issue: "LEGION-9", Token: claim.Token("legion-legion-legion-9-tester"), PodUID: "pod-1",
		Report: shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: 1, PID: 4242, Executable: "python3", LimitBytes: 4 << 30, At: h.now.Add(time.Minute)}})
	h.now = h.now.Add(2 * time.Minute)
	kills := seriesServed(t, func() map[string]any { r, _ := h.servedSignals(); return r }())[seriesKey(signals.Kills)]
	if kills["value"] != 1.0 || kills["limit"] != 1.0 || kills["holds"] != true || kills["since"] != nil || kills["limitSet"] == nil {
		t.Errorf("the kills at their limit with no pass since = %v, want 1 at limit 1, holding, undated, with the limit's change", kills)
	}
	h.apply("pause", "resume", intake.ProjectResumed{})
	kills = seriesServed(t, func() map[string]any { r, _ := h.servedSignals(); return r }())[seriesKey(signals.Kills)]
	if kills["holds"] != true || kills["since"] != h.now.Format(time.RFC3339) {
		t.Errorf("the kills after the pass that dated their hold = %v, want holding since %s", kills, h.now.Format(time.RFC3339))
	}
	// entries: exactly the nine members each, the switch's signal and part null, a limit's setter
	// and reason set, a hold's value 1 with neither, a value the daemon cannot compute null with its
	// reason; newest first.
	_, changes = h.servedSignals()
	var whats []string
	for i, raw := range changes {
		entry := raw.(map[string]any)
		if got := slices.Sorted(maps.Keys(entry)); !slices.Equal(got, entryMembers) {
			t.Fatalf("entry %v carries %v, want exactly %v", entry, got, entryMembers)
		}
		if i > 0 && entry["id"].(float64) != changes[i-1].(map[string]any)["id"].(float64)-1 {
			t.Fatalf("the entries' ids %v are not newest first with no gap", changes)
		}
		what := entry["what"].(string)
		whats = append(whats, what)
		setter := what == "limit" || what == "switch"
		if (entry["setBy"] != nil) != setter || (entry["reason"] != nil) != setter {
			t.Errorf("entry %v: want a setter and a reason exactly for a limit and the switch", entry)
		}
		switch {
		case what == "switch" && (entry["signal"] != nil || entry["part"] != nil || entry["value"] != 1.0):
			t.Errorf("the switch's entry %v, want no signal, no part and 1", entry)
		case what == "holds" && entry["value"] != 1.0 && entry["value"] != 0.0:
			t.Errorf("a hold's entry %v, want 1 or 0", entry)
		case what == "value" && (entry["value"] == nil) != (entry["absent"] != nil):
			t.Errorf("a value's entry %v, want a value or a reason, never both", entry)
		case what != "value" && entry["absent"] != nil:
			t.Errorf("entry %v carries a reason a value alone carries", entry)
		}
	}
	for _, what := range []string{"switch", "limit", "holds", "value"} {
		if !slices.Contains(whats, what) {
			t.Errorf("the entries %v hold no %s entry", whats, what)
		}
	}
	// newest 50: with more than 50 entries the state carries the newest 50, newest first.
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		entries := make([]store.SignalChange, 60)
		for i := range entries {
			entries[i] = store.SeriesValueChange(signals.Approvals, h.now, new(float64(i)), "")
		}
		return store.AppendSignalChangesTx(ctx, tx, testDispatchProject, entries)
	}); err != nil {
		t.Fatal(err)
	}
	_, changes = h.servedSignals()
	if len(changes) != store.SignalChangesShown || changes[0].(map[string]any)["value"] != 59.0 {
		t.Fatalf("the state carries %d entries, the first %v, want the newest %d, newest first", len(changes), changes[0], store.SignalChangesShown)
	}
}
