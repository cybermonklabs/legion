package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// carryRefusedRemedy is what a 502 adds to the broker's refusal of the release's question: why a
// run again may or may not help.
const carryRefusedRemedy = "the secrets broker refused the question, so nothing was freed. A broker from before this release has no POST /v1/enrollments/carried and answers it 404 or 405: while the broker rolls, a run again may reach a replica that has it; otherwise a run again answers the same until that refusal is fixed"

// releaseRefusedRemedy is what a 502 adds to the broker's refusal to release the claim's session
// names (supervise.ErrReleaseRefused).
const releaseRefusedRemedy = "nothing was freed, and a run again answers the same until that refusal is fixed"

// releaseUnansweredRemedy is what a 503 adds to a release of the claim's session names the broker
// did not answer (supervise.ErrReleaseUnanswered), which a 404 or a 405 is too.
const releaseUnansweredRemedy = "nothing was freed, run it again. A broker from before POST /v1/oauth/exchange/pod-sessions/release answers it 404 or 405: while the broker rolls, a run again may reach a replica that has it; otherwise the broker has to be upgraded first"

// ReleasedNames is the answer of `POST /legion/v1/operator/claims/{token}/release-names` (`legion
// claims release-names`): every name of the claim's other sessions the release freed, in kind and
// then name order, how it knew no grant follows one of them (supervise.CheckedByBroker,
// supervise.CheckedNoBroker, or supervise.CheckedNothing when it had no name to free), and the
// session the claim last abandoned whose names it freed, if any, which the claim's record now
// marks released.
type ReleasedNames struct {
	Claim     claim.Token    `json:"claim"`
	Freed     []ReleasedName `json:"freed"`
	Checked   string         `json:"checked"`
	Abandoned string         `json:"abandoned,omitempty"`
}

// ReleasedName is one name a release freed: its kind (supervise.HoldKind) and the name.
type ReleasedName struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// releaseNames is the operator's release of a claim's names (supervise.Machine.ReleaseNames): it
// frees every name the claim holds but those of its current session (the session it records, and
// the one its live pod-session registration names), once the secrets broker answers that it
// carries nothing of them and, where the daemon registers pod sessions, that it released the
// session names among them. A broker that still carries some is a 409 naming each enrollment and
// when it ends, or saying there are more than its answer lists (a supervise.RefusedError holding
// a supervise.NamesCarriedError, which operatorFailure answers); a broker that does not answer
// either question, or answers with an outage (a 5xx, a 408 or a 429), a 503
// (supervise.ErrCarryUnanswered, supervise.ErrReleaseUnanswered), which a run again can change; a
// broker that refuses the question for good (a 4xx other than a 408 or a 429) a 502
// (supervise.ErrCarryRefused), which a run again answers the same unless the broker is rolling,
// and one that refuses the release for good a 502 too (supervise.ErrReleaseRefused). None frees
// anything, and each 502 and 503 is logged as an operator request that failed, naming the
// broker's reason. It reaches a claim in any state, since the names it frees are of sessions no
// process of the claim runs.
func (s *server) releaseNames(w http.ResponseWriter, r *http.Request) {
	token := claim.Token(r.PathValue("token"))
	m, ok := s.supervisor.Machine(token)
	if !ok {
		s.refuse(w, r, http.StatusNotFound, "no claim "+string(token))
		return
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, r, "release-names") {
		return
	}
	released, err := m.ReleaseNames(ctx)
	switch {
	case errors.Is(err, supervise.ErrCarryRefused):
		s.logFailure("api: an operator request failed", "request", "release-names", "claim", token, "error", err)
		s.refuse(w, r, http.StatusBadGateway, "release-names refused: "+err.Error()+"; "+carryRefusedRemedy)
		return
	case errors.Is(err, supervise.ErrCarryUnanswered):
		s.logFailure("api: an operator request failed", "request", "release-names", "claim", token, "error", err)
		s.refuse(w, r, http.StatusServiceUnavailable, "release-names refused: "+err.Error()+"; nothing was freed, run it again")
		return
	case errors.Is(err, supervise.ErrReleaseRefused):
		s.logFailure("api: an operator request failed", "request", "release-names", "claim", token, "error", err)
		s.refuse(w, r, http.StatusBadGateway, "release-names refused: "+err.Error()+"; "+releaseRefusedRemedy)
		return
	case errors.Is(err, supervise.ErrReleaseUnanswered):
		s.logFailure("api: an operator request failed", "request", "release-names", "claim", token, "error", err)
		s.refuse(w, r, http.StatusServiceUnavailable, "release-names refused: "+err.Error()+"; "+releaseUnansweredRemedy)
		return
	case err != nil:
		s.operatorFailure(w, r, "release-names", token, err)
		return
	}
	answer := ReleasedNames{Claim: token, Freed: make([]ReleasedName, len(released.Freed)), Checked: released.Checked, Abandoned: released.Abandoned}
	for i, h := range released.Freed {
		answer.Freed[i] = ReleasedName{Kind: string(h.Kind), Name: h.Name}
	}
	s.log.Info("api: operator released a claim's names", "claim", token, "freed", len(answer.Freed), "checked", answer.Checked, "abandoned", answer.Abandoned)
	s.operatorDone(r, token)
	s.writeUnparsedAnswer(w, r, http.StatusOK, answer)
}
