package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// WaitRequest is the body of POST /legion/v1/claims/wait: the claim the declaring session holds,
// that session, the claim's secret, what the worker waits on, and when the wait's time comes, an
// RFC 3339 instant (dispatch://INFRA-2040 R2).
type WaitRequest struct {
	ClaimToken claim.Token `json:"claimToken"`
	SessionID  string      `json:"sessionId"`
	Secret     string      `json:"secret"`
	WaitingOn  string      `json:"waitingOn"`
	WakeAt     string      `json:"wakeAt"`
}

// WaitView is one wait as the daemon recorded it: its id, what it waits on, when its time comes
// and when the daemon recorded the declaration.
type WaitView struct {
	ID         string    `json:"id"`
	WaitingOn  string    `json:"waitingOn"`
	WakeAt     time.Time `json:"wakeAt"`
	DeclaredAt time.Time `json:"declaredAt"`
}

// WaitViewOf is wait as a claim view shows it while it stands (ClaimView.Wait): the same four
// facts the declaration's answer named, read back from the record, which kept both times to the
// microsecond the answer gave.
func WaitViewOf(wait record.ClaimWait) *WaitView {
	return &WaitView{ID: wait.ID, WaitingOn: wait.WaitingOn, WakeAt: wait.WakeAt.UTC(), DeclaredAt: wait.DeclaredAt.UTC()}
}

// WaitResponse is the body POST /legion/v1/claims/wait answers once the wait is recorded.
type WaitResponse struct {
	Wait WaitView `json:"wait"`
}

// wait is a phase worker declaring that it waits on something outside its own work — a person's
// answer, a CI run, another issue — until a time it names, so the daemon's own wakes leave it be
// until then (dispatch://INFRA-2040 R2). The route authenticates the claim as activity does — a
// blank field refused by its name, a claim this daemon does not hold or a wrong secret with
// claim.InvalidSecret, a stale plugin with the stale-contract 409 — and holds the call to the
// session the claim records, as ready-for-update's fence does (claim.SameAgentRefusal), since a
// wait is the declaring session's own.
//
// The claim is read once, with the secret (authenticated), and every claim field of the fact
// comes from that read: the issue and role, the run the worker is on (supervise.Claim.ServingRun)
// and the entry of the task it had taken (takenEntry), so no two fields straddle a decision. The
// route mints the wait's id and applies intake.WaitDeclared under `wait-declared:<id>`, through
// intake.ApplyFact, so the declaration takes the lock every fact applies under, the design gate's
// closing among them. The engine decides it (workflow's waitDeclared): a refusal is written as
// the workflow routes write theirs, its code and sentence (400 for the text and the two times, 409
// for the record's), and a recorded wait answers 200 with its id, its text, its time and its
// declaration time. Both times are kept to the microsecond the record keeps, so the answer is the
// record.
func (s *server) wait(w http.ResponseWriter, r *http.Request) {
	var req WaitRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireFields(w, r,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
		field{"wakeAt", req.WakeAt},
	) {
		return
	}
	wakeAt, err := time.Parse(time.RFC3339, req.WakeAt)
	if err != nil {
		s.refuse(w, r, http.StatusBadRequest, "wakeAt must be an RFC 3339 time: "+err.Error())
		return
	}
	_, held, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, held) {
		return
	}
	if req.SessionID != held.Session {
		s.log.Info("api: refused a claim request", "request", "wait", "claim", req.ClaimToken,
			"reason", "the claim resumes another session", "session", req.SessionID, "recorded", held.Session)
		s.writeAnswer(w, r, AnswerRefusal, claim.SameAgentRefusal.Status, claim.SameAgentRefusal)
		return
	}
	if s.pool == nil || len(s.handlers) == 0 {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "this daemon runs no workflow, so it records no wait")
		return
	}
	fact := intake.WaitDeclared{
		Token: req.ClaimToken, Issue: held.Issue, Role: held.Role, Session: req.SessionID,
		Generation: held.ServingRun(), WaitID: uuid.NewString(), WaitingOn: strings.TrimSpace(req.WaitingOn),
		WakeAt: wakeAt.UTC().Truncate(time.Microsecond), TakenEntry: takenEntry(held),
		At: s.now().UTC().Truncate(time.Microsecond),
	}
	result, err := s.apply(r.Context(), "api", "wait-declared:"+fact.WaitID, fact)
	if err != nil {
		s.log.Error("wait: could not apply a declaration", "claim", req.ClaimToken, "wait", fact.WaitID, "error", err)
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply the wait")
		return
	}
	if result.Refusal != nil {
		s.writeFailure(w, r, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	s.writeAnswer(w, r, AnswerWait, http.StatusOK, WaitResponse{Wait: WaitView{
		ID: fact.WaitID, WaitingOn: fact.WaitingOn, WakeAt: fact.WakeAt, DeclaredAt: fact.At,
	}})
}

// WaitNewsRequest is the body of POST /legion/v1/claims/wait-news: the claim the reporting session
// holds, that session, the claim's secret, the id of the wait that news reached the session after
// (dispatch://INFRA-2040 R3's second end), as the wait route's answer named it, and the reported
// delivery's own dedupe key, verbatim, which a person's message has none of.
type WaitNewsRequest struct {
	ClaimToken  claim.Token `json:"claimToken"`
	SessionID   string      `json:"sessionId"`
	Secret      string      `json:"secret"`
	WaitID      string      `json:"waitId"`
	DeliveryKey string      `json:"deliveryKey,omitempty"`
}

// WaitNewsResponse is the body POST /legion/v1/claims/wait-news answers: whether the report ended
// the wait it names, and whether the delivery it reported is the claim's own update ask, which
// is no news. False keeps the wait standing in the plugin.
type WaitNewsResponse struct {
	Ended     bool `json:"ended"`
	UpdateAsk bool `json:"updateAsk,omitempty"`
}

// waitNews is a phase worker's plugin reporting that news reached its session after it declared
// the wait the report names — an Envoy delivery, a message to its role, or a person's message —
// so the daemon ends that wait (dispatch://INFRA-2040 R3's second end). The route authenticates
// the claim and holds the call to the session the claim records exactly as the wait route does,
// since the wait is the declaring session's own.
//
// The claim is read once, with the secret (authenticated). A report whose delivery key is a key
// of the claim's own update ask (record.IsUpdateAskKey, bare or behind the listener's role-forward
// mark) ends nothing: an update round and its ask end no wait (R3), and the ask reaches the
// session as a message to its role, which the plugin reports as it reports every other delivery
// (dispatch://INFRA-2040 D9). The route then answers `ended: false` and `updateAsk: true`,
// records nothing, and logs it with the key. The key names the claim and not the round, so a
// report of the ask sent again after its round has ended is still the ask. Every other report,
// with any other key or none, is news, whether or not an update round is open.
//
// News is applied as intake.WaitNews under `wait-news:<claimToken>:<waitId>`, through the
// package's one apply (apply, intake.ApplyFact), and the route answers whether that fact ended
// the wait (workflow's waitNews). The event id names the claim beside the wait, since a wait's id
// is in the record
// every role reads (read_record): a report of another claim's wait id changes no wait (EndWait
// matches the claim too) and marks no event the wait's own claim reports under. A report naming
// a wait a newer declaration replaced, or one already ended, answers `ended: false` and changes
// nothing; so does a second report of the same wait, which ApplyFact answers as a duplicate
// before any handler runs, since the first report is the one that ended it.
func (s *server) waitNews(w http.ResponseWriter, r *http.Request) {
	var req WaitNewsRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireFields(w, r,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
		field{"waitId", req.WaitID},
	) {
		return
	}
	_, held, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, held) {
		return
	}
	if req.SessionID != held.Session {
		s.log.Info("api: refused a claim request", "request", "wait-news", "claim", req.ClaimToken,
			"reason", "the claim resumes another session", "session", req.SessionID, "recorded", held.Session)
		s.writeAnswer(w, r, AnswerRefusal, claim.SameAgentRefusal.Status, claim.SameAgentRefusal)
		return
	}
	if s.pool == nil || len(s.handlers) == 0 {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "this daemon runs no workflow, so it records no wait")
		return
	}
	if record.IsUpdateAskKey(req.ClaimToken, req.DeliveryKey) {
		s.log.Info("wait: the update ask is no news", "claim", req.ClaimToken, "issue", held.Issue,
			"role", held.Role, "wait", req.WaitID, "key", req.DeliveryKey)
		s.writeAnswer(w, r, AnswerWaitNews, http.StatusOK, WaitNewsResponse{UpdateAsk: true})
		return
	}
	result, err := s.apply(r.Context(), "api", "wait-news:"+string(req.ClaimToken)+":"+req.WaitID, intake.WaitNews{
		Token: req.ClaimToken, Issue: held.Issue, Role: held.Role, WaitID: req.WaitID,
	})
	if err != nil {
		s.log.Error("wait: could not apply a news report", "claim", req.ClaimToken, "wait", req.WaitID, "error", err)
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply the news report")
		return
	}
	if result.Refusal != nil {
		s.writeFailure(w, r, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	s.writeAnswer(w, r, AnswerWaitNews, http.StatusOK, WaitNewsResponse{Ended: result.WaitEnded})
}

// takenEntry is the phase entry of the task c's worker had taken (dispatch://INFRA-2040 D4): the
// entry of the confirmed delivery the claim holds, when that delivery carries one, else the entry
// of the last confirmed delivery it retired (supervise.Claim.ServedEntry). It is not
// Claim.ServingEntry, which for a claim that has retired no confirmed task of an entry answers
// the entry of a held task its worker never confirmed (supervise's serving), a task it had not
// taken.
func takenEntry(c supervise.Claim) int64 {
	if p := c.Pending; p != nil && !p.ConfirmedAt.IsZero() && p.Entry != 0 {
		return p.Entry
	}
	return c.ServedEntry
}
