package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The settings of admission by signals (S3 of dispatch://INFRA-5899): a person turns admission by
// signals on or off and sets or clears the limit of each series, through the operator's route
// (`legion project signals`) or through the controller's on the person's cited word. Each route
// checks the change whole and answers the reason it refuses one, changing nothing; a change it
// takes is applied as a fact (intake.SignalSettingChanged), which admission writes with its entry
// of the record of changes in the fact's transaction before that fact's pass, so the change is in
// force at once and a restart keeps it.

// The actions of a change of admission by signals.
const (
	SignalsOn         = "on"
	SignalsOff        = "off"
	SignalsSetLimit   = "set-limit"
	SignalsClearLimit = "clear-limit"
)

// signalSettingInvalid is the code of a change of admission by signals the routes refuse.
const signalSettingInvalid = "SIGNAL_SETTING_INVALID"

// ProjectSignalsRequest is the controller's POST /legion/v1/project/signals: its grant, the action
// (on, off, set-limit or clear-limit), for a limit the series by its signal and, for the provider,
// its part (headroom), the limit to set, and the person's cited reason, which every action
// requires. It names no project: the daemon acts on its own.
type ProjectSignalsRequest struct {
	GrantID string   `json:"grantId"`
	Action  string   `json:"action"`
	Signal  string   `json:"signal,omitempty"`
	Part    string   `json:"part,omitempty"`
	Limit   *float64 `json:"limit,omitempty"`
	Reason  string   `json:"reason"`
}

// OperatorSignalsRequest is the operator's POST /legion/v1/operator/project/signals, `legion
// project signals`: the controller's request with no grant.
type OperatorSignalsRequest struct {
	Action string   `json:"action"`
	Signal string   `json:"signal,omitempty"`
	Part   string   `json:"part,omitempty"`
	Limit  *float64 `json:"limit,omitempty"`
	Reason string   `json:"reason"`
}

// SignalSettingsView is the settings of admission by signals as the routes answer them. It is
// store.SignalSettingsView, declared once.
type SignalSettingsView = store.SignalSettingsView

// ProjectSignalsResponse answers a change of admission by signals: whether it moved the setting (a
// change to the value the setting holds already moves nothing), and the project's settings as they
// stand after it, read from the settings themselves.
type ProjectSignalsResponse struct {
	Changed  bool               `json:"changed"`
	Settings SignalSettingsView `json:"settings"`
}

// projectSignals is the controller's change of admission by signals: a controller grant, the
// controller session's or one the operator's bearer minted, acts for the person whose word the
// reason cites, and the change is the operator's when the operator's bearer minted the grant
// (controllerActor). Its answer is a plugin's (AnswerProjectSignals).
func (s *server) projectSignals(w http.ResponseWriter, r *http.Request) {
	var req ProjectSignalsRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) {
		return
	}
	if by, ok := s.controllerActor(w, r, req.GrantID, "a change of admission by signals requires a controller grant"); ok {
		s.changeSignals(w, r, AnswerProjectSignals, by, OperatorSignalsRequest{Action: req.Action, Signal: req.Signal, Part: req.Part,
			Limit: req.Limit, Reason: req.Reason})
	}
}

// operatorProjectSignals is `legion project signals`: the operator's bearer changes admission by
// signals. Its answer is the `legion` command's, which no plugin parses (answerUnparsed).
func (s *server) operatorProjectSignals(w http.ResponseWriter, r *http.Request) {
	var req OperatorSignalsRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	s.changeSignals(w, r, answerUnparsed, store.PauseActor{Kind: store.PauseByOperator}, req)
}

// changeSignals applies req, a change of admission by signals by `by`, and answers the settings as
// they stand after it as answer, the route's own answer id. It refuses, changing nothing, an action
// it does not know, a limit's series its signal and part do not name (store.SignalSeries), a limit
// outside its series' range (signals.CheckLimit, the one range of each), a member the action does
// not take, and a blank reason, each SIGNAL_SETTING_INVALID with the reason; a daemon that runs no
// intake refuses every change FACTS_UNAVAILABLE.
func (s *server) changeSignals(w http.ResponseWriter, r *http.Request, answer AnswerID, by store.PauseActor, req OperatorSignalsRequest) {
	change, problem := signalSettingChangeOf(req)
	if problem != "" {
		s.writeFailure(w, r, http.StatusBadRequest, signalSettingInvalid, problem+"; nothing changed")
		return
	}
	if s.pool == nil {
		s.writeFailure(w, r, http.StatusServiceUnavailable, "FACTS_UNAVAILABLE", "this daemon runs no intake, so it cannot change admission by signals; nothing changed")
		return
	}
	change.Set = store.SignalSet{By: by, At: s.now().UTC(), Reason: req.Reason}
	result, err := s.apply(r.Context(), "api", requestFactID("project/signals"), intake.SignalSettingChanged{Change: change})
	if err != nil {
		s.logRequestFailure(r, err, "api: a change of admission by signals failed", "action", req.Action, "setting", change.Setting,
			"by", string(by.Kind), "by_session", by.Session)
		s.writeFailure(w, r, http.StatusInternalServerError, "SIGNAL_SETTING_FAILED", "the change of admission by signals failed: "+err.Error())
		return
	}
	if result.Refusal != nil {
		s.writeRefusal(w, r, result.Refusal)
		return
	}
	after, err := s.signalSettings(r.Context())
	if err != nil {
		s.logRequestFailure(r, err, "api: read the settings of admission by signals after a change")
		s.writeFailure(w, r, http.StatusInternalServerError, "SIGNAL_SETTING_FAILED", "the change of admission by signals was applied, and the daemon could not read it back")
		return
	}
	// Whether the change moved the setting is the fact's own transaction's word, which no change
	// another fact made before or after it can blur.
	s.writeAnswer(w, r, answer, http.StatusOK, ProjectSignalsResponse{Changed: result.Changed, Settings: after.View()})
}

// signalSettings is the project's settings of admission by signals, read in a transaction of its own.
func (s *server) signalSettings(ctx context.Context) (store.SignalSettings, error) {
	var settings store.SignalSettings
	err := store.TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		settings, err = store.SignalSettingsTx(ctx, tx, s.dispatchProject)
		return err
	})
	return settings, err
}

// signalSettingChangeOf is the change req asks for, its Set left to the caller, or why it cannot
// be taken: on and off take no series and no limit, set-limit a series and a limit in its range,
// clear-limit a series and no limit, and every action a reason that is not blank.
func signalSettingChangeOf(req OperatorSignalsRequest) (store.SignalSettingChange, string) {
	var change store.SignalSettingChange
	if strings.TrimSpace(req.Reason) == "" {
		return change, "a change of admission by signals needs the person's cited reason: say in reason whose word it is and what they said"
	}
	switch req.Action {
	case SignalsOn, SignalsOff:
		if req.Signal != "" || req.Part != "" || req.Limit != nil {
			return change, fmt.Sprintf("%s turns admission by signals %s and takes no signal, part or limit", req.Action, req.Action)
		}
		flag := 0.0
		if req.Action == SignalsOn {
			flag = 1
		}
		return store.SignalSettingChange{Setting: store.SignalSwitchSetting, Value: &flag}, ""
	case SignalsSetLimit, SignalsClearLimit:
		series, named := store.SignalSeries(req.Signal, req.Part)
		if !named {
			return change, fmt.Sprintf("signal %q with part %q names no series: the signals are idle, queue, provider (part headroom), kills and approvals, and only provider takes a part", req.Signal, req.Part)
		}
		change.Setting = series
		if req.Action == SignalsClearLimit {
			if req.Limit != nil {
				return change, "clear-limit clears the limit and takes no limit"
			}
			return change, ""
		}
		if req.Limit == nil {
			return change, "set-limit needs the limit to set"
		}
		if err := signals.CheckLimit(series, *req.Limit); err != nil {
			return change, err.Error()
		}
		change.Value = req.Limit
		return change, ""
	}
	return change, fmt.Sprintf("action %q is none of %s, %s, %s and %s", req.Action, SignalsOn, SignalsOff, SignalsSetLimit, SignalsClearLimit)
}
