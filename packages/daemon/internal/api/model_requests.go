package api

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// modelRequests is a host's report of the model requests of the sessions it runs for one launch of
// a claim (claim.ModelRequestsRequest): the role's own session, each subagent's it starts, or the
// controller's. The daemon keeps each request once, by the claim, the launch generation and the
// request's own id, in the minute it ended, for the provider's headroom, and answers
// every id of the batch kept once its fact applied: each is held, new or held already, or ended
// before the hour of minutes any read reaches again, which the store keeps as nothing
// (record.Store.KeepModelRequests), so the host sends none of them again. A host whose reports could
// not reach the daemon for an hour so forgets the requests no read counts any more, rather than
// sending them again for ever.
//
// The body is decoded strictly, so a member the request does not name — a prompt, an answer, an
// error's text, the cache reads, a project — is refused with the whole body: the project is the
// claim's. The caller is authenticated exactly as a turn outcome's is (reportCaller): the Oh My Pi
// plugin by the claim's secret, the Claude Code bridge by its launch's boot token. The controller
// the daemon launches (`controller: daemon`) is a claim like any role's; the operator's
// (`controller: operator`) holds no claim the daemon supervises, and names its own token,
// claim.ControllerToken, with the session and the secret its registration was issued, which are
// checked against the controller record as the controller's grants are (modelRequestsByController).
// The generation is the authenticated launch's: a body naming another is refused as stale.
//
// The batch is one fact (intake.ModelRequests) under an id that names the claim, the generation
// and the batch's ids. The id only names the fact: the fact keeps no processed mark (intake's
// keepsMark), which would be a row for ever for each report, so nothing reads the id for a
// duplicate. A batch sent again applies again and keeps nothing twice, since the store keeps each
// request once by the claim, the generation and its id; a batch carrying an id already held keeps
// that id's first report, but for a later end, to which the store moves the request
// (record.Store.KeepModelRequests): the Claude Code bridge reports a request again with the end of
// a block of it that came after its report.
func (s *server) modelRequests(w http.ResponseWriter, r *http.Request) {
	var req claim.ModelRequestsRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireGeneration(w, r, req.Generation) {
		return
	}
	if problem := modelRequestsProblem(req.Requests); problem != "" {
		s.refuse(w, r, http.StatusBadRequest, problem)
		return
	}
	token, generation, ok := s.modelRequestsCaller(w, r, req)
	if !ok {
		return
	}
	if s.pool == nil || s.records == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	ids := make([]string, len(req.Requests))
	for i, request := range req.Requests {
		ids[i] = request.ID
	}
	result, err := s.apply(r.Context(), "api", modelRequestsFactID(token, generation, ids),
		intake.ModelRequests{Claim: token, Generation: generation, Requests: req.Requests})
	if err != nil {
		s.logFailure("api: keep a report of model requests", "claim", token, "generation", generation, "error", err)
		s.refuse(w, r, http.StatusInternalServerError, "report of model requests failed: the daemon could not keep it")
		return
	}
	if result.Refusal != nil {
		s.writeRefusal(w, r, result.Refusal)
		return
	}
	// A batch sent again keeps what an earlier fact kept the same way, and a request it kept the
	// store deletes only once it ended before the hour a read reaches, so every id is answered kept.
	s.writeAnswer(w, r, AnswerModelRequests, http.StatusOK, claim.ModelRequestsResponse{Kept: ids})
}

// modelRequestsCaller is the claim and launch generation a report of model requests is kept
// under: the operator's controller by its controller record (modelRequestsByController), every
// other caller as a turn outcome's (reportCaller), whose generation is its launch's. It writes the
// refusal itself where there is none.
func (s *server) modelRequestsCaller(w http.ResponseWriter, r *http.Request, req claim.ModelRequestsRequest) (claim.Token, uint64, bool) {
	caller, named := req.Caller()
	if agent, ok := caller.(claim.AgentCaller); named && ok && agent.ClaimToken == claim.ControllerToken(s.project) && s.controller != nil {
		if _, supervised := s.supervisor.Machine(agent.ClaimToken); !supervised {
			return s.modelRequestsByController(w, r, agent, req.Generation)
		}
	}
	token, m, ok := s.reportCaller(w, r, modelRequestsReport, caller, named, req.Generation)
	if !ok {
		return "", 0, false
	}
	if generation := m.Claim().Generation; generation != req.Generation {
		s.log.Warn("api: refused a report of model requests reported for another generation", "claim", token,
			"launch", generation, "reported", req.Generation)
		s.writeAnswer(w, r, AnswerRefusal, claim.StaleGeneration.Status, claim.StaleGeneration)
		return "", 0, false
	}
	return token, req.Generation, true
}

// modelRequestsByController authenticates the operator's controller (`legion controller start`),
// which the daemon supervises no claim of, by the one check of a controller session against its
// record (controllerSessionRegistered) that its grants are minted by, and holds it to the record's
// contract by the one staleness check of that record (staleOperatorController), answered as this
// route answers every refusal (claim.StaleControllerContract); its generation is the record's,
// which its registration answered it.
func (s *server) modelRequestsByController(w http.ResponseWriter, r *http.Request, caller claim.AgentCaller, generation uint64) (claim.Token, uint64, bool) {
	if !s.requireFields(w, r, field{"sessionId", caller.SessionID}, field{"secret", caller.Secret}) {
		return "", 0, false
	}
	stored, found, err := s.controller.Controller(r.Context(), s.project)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the controller record for a report of model requests")
		s.refuse(w, r, http.StatusInternalServerError, "report of model requests failed: the daemon could not read its controller record")
		return "", 0, false
	}
	if !found || !controllerSessionRegistered(stored, caller.SessionID, caller.Secret) {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return "", 0, false
	}
	if staleOperatorController(stored) {
		s.logStaleRefusal(r, "session", stored.Session, stored.PluginContract)
		refusal := claim.StaleControllerContract(stored.PluginContract, DaemonAPIVersion())
		s.writeAnswer(w, r, AnswerRefusal, refusal.Status, refusal)
		return "", 0, false
	}
	if generation != stored.Generation {
		s.writeAnswer(w, r, AnswerRefusal, claim.StaleGeneration.Status, claim.StaleGeneration)
		return "", 0, false
	}
	return caller.ClaimToken, generation, true
}

// modelRequestsProblem is why a batch of model requests cannot be read as one, "" where it can:
// it holds 1 to claim.MaxModelRequests requests, each one the daemon can read
// (claim.ModelRequestProblem) and with an id no other request of it has.
func modelRequestsProblem(requests []claim.ModelRequest) string {
	if len(requests) == 0 || len(requests) > claim.MaxModelRequests {
		return fmt.Sprintf("requests holds %d requests; a report carries 1 to %d", len(requests), claim.MaxModelRequests)
	}
	seen := make(map[string]int, len(requests))
	for i, request := range requests {
		if problem := claim.ModelRequestProblem(request); problem != "" {
			return fmt.Sprintf("requests[%d].%s", i, problem)
		}
		if first, twice := seen[request.ID]; twice {
			return fmt.Sprintf("requests[%d].id %q is requests[%d]'s too: an id names one request", i, request.ID, first)
		}
		seen[request.ID] = i
	}
	return ""
}

// modelRequestsFactID names a batch's fact: the claim, the launch generation and a digest of the
// batch's ids. The id only names the fact, in an error about it (intake.ApplyFact): the fact keeps
// no processed mark (intake's keepsMark), so nothing reads the id for a duplicate, and the store
// keeps each request once by its own key.
func modelRequestsFactID(token claim.Token, generation uint64, ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return fmt.Sprintf("model-requests:%s:%d:%x", token, generation, sum)
}
