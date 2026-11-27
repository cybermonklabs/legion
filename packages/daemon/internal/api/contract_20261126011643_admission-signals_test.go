package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The rows admission by signals adds to this package's tests: the report of model requests a host
// sends and its answer, which names every id of the batch the daemon holds; the controller's change
// of admission by signals and its answer, the settings as they stand, one series of each shape (a
// limit with its change, and none); and, in the populated state golden (populatedState), the
// state's readings and record of changes (signalsReadingFixture, signalChangesFixture).
func init() {
	registerEntryTestRows(entryTestRows{
		entry: admissionSignalsContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/claims/model-requests": reflect.TypeFor[claim.ModelRequestsRequest](),
			"POST /legion/v1/project/signals":       reflect.TypeFor[ProjectSignalsRequest](),
		},
		answers: func(*testing.T) map[AnswerID]answerFixture {
			return map[AnswerID]answerFixture{
				AnswerModelRequests: {roots: typesOf(claim.ModelRequestsResponse{}), bodies: []any{claim.ModelRequestsResponse{
					Kept: []string{"req-1", "req-2"},
				}}},
				AnswerProjectSignals: {roots: typesOf(ProjectSignalsResponse{}), bodies: []any{ProjectSignalsResponse{
					Changed: true, Settings: signalSettingsFixture(),
				}}},
			}
		},
	})
}

// signalsFixtureAt is the moment the fixtures of admission by signals are read at.
var signalsFixtureAt = time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)

// operatorSet and controllerSet are the two setters of a setting the fixtures name: the operator,
// and the controller from its session.
var (
	operatorSet   = store.SignalSet{By: store.PauseActor{Kind: store.PauseByOperator}, At: signalsFixtureAt.Add(-3 * time.Hour), Reason: "the owner turned admission by signals on"}
	controllerSet = store.SignalSet{By: store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"}, At: signalsFixtureAt.Add(-2 * time.Hour),
		Reason: "the owner set the limit in the weekly plan"}
)

// signalSettingsFixture is the settings a change answers: admission by signals on, a limit set by
// the controller on the idle share, one by the operator on the headroom, and none on the rest.
func signalSettingsFixture() SignalSettingsView {
	limits := map[string]*float64{signals.Idle: new(0.5), signals.Provider: new(0.9)}
	sets := map[string]store.SignalSet{signals.Idle: controllerSet, signals.Provider: operatorSet}
	view := SignalSettingsView{On: true, Switch: &operatorSet}
	for _, series := range store.LiveSeries() {
		limit := store.SignalLimitView{Limit: limits[series]}
		limit.Signal, limit.Part = signalNamesOf(series)
		if set, held := sets[series]; held {
			limit.Set = &set
		}
		view.Limits = append(view.Limits, limit)
	}
	return view
}

// signalNamesOf is series' signal and part as a view carries them, the part nil where it has none.
func signalNamesOf(series string) (string, *string) {
	signal, part := store.SignalPart(series)
	if part == "" {
		return signal, nil
	}
	return signal, &part
}

// signalsReadingFixture is the state's `admission.signals` of every shape a series takes: a value
// under its limit dated by the hold entry that says so, a value the daemon cannot compute with no
// limit, a value under its limit no pass dated yet, and values with no limit, beside two minutes
// of the provider's record, one held and one not.
func signalsReadingFixture() *SignalsReading {
	since := signalsFixtureAt.Add(-time.Hour)
	reading := &SignalsReading{On: true, Switch: &operatorSet}
	settings := signalSettingsFixture()
	values := map[string]*float64{signals.Idle: new(0.25), signals.Provider: new(0.42), signals.Kills: new(0.0), signals.Approvals: new(1.0)}
	for i, series := range store.LiveSeries() {
		row := store.SignalSeriesReading{Value: values[series], Limit: settings.Limits[i].Limit, LimitSet: settings.Limits[i].Set}
		row.Signal, row.Part = signalNamesOf(series)
		switch series {
		case signals.Queue:
			row.Absent = new("the daemon keeps the merge queue's events from 2026-11-26T06:00:00Z, so the span this signal counts is whole from 2026-11-26T12:00:00Z")
		case signals.Idle:
			row.Since = &since
		}
		reading.Series = append(reading.Series, row)
	}
	reading.Provider.Minutes = []store.ProviderMinuteReading{
		{At: signalsFixtureAt.Add(-2 * time.Minute), Held: true, UncachedInputTokens: 2520, TokenLimit: 6000},
		{At: signalsFixtureAt.Add(-time.Minute), Held: false, TokenLimit: 6000},
	}
	return reading
}

// signalChangesFixture is the state's `admission.signalChanges` newest first: a value the daemon
// cannot compute with its reason, and the switch with who set it and why.
func signalChangesFixture() []SignalChange {
	queue, _ := signalNamesOf(signals.Queue)
	operator := operatorSet.By
	return []SignalChange{
		{ID: 2, At: operatorSet.At, Signal: &queue, What: store.SignalChangeValue, Absent: new("the daemon has not begun keeping the merge queue's events")},
		{ID: 1, At: operatorSet.At, What: store.SignalChangeSwitch, Value: new(1.0), SetBy: &operator, Reason: &operatorSet.Reason},
	}
}
