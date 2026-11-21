package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ClaimApprovalRequest is the body of POST /legion/v1/claims/approval: the claim the reading
// session holds, that session, and the claim's secret, as the plugin's other claim calls send them.
type ClaimApprovalRequest struct {
	ClaimToken claim.Token `json:"claimToken"`
	SessionID  string      `json:"sessionId"`
	Secret     string      `json:"secret"`
}

// ClaimApprovalResponse is the body POST /legion/v1/claims/approval answers: the approval that
// stands for the claim, or null where none does. Approval is never absent, so a reader tells none
// from a daemon that left the key out.
type ClaimApprovalResponse struct {
	Approval *ClaimApproval `json:"approval"`
}

// ClaimApproval is a person's approval that stands for a root's architect: the spec's document as
// the message names it (`dispatch://<KEY>/spec`, or the document's id where a carried copy's
// hand-over names no root), the version approved, and the message with both of them filled in,
// which the plugin places as it is (packages/daemon/internal/prompts/approval.md).
type ClaimApproval struct {
	Document string `json:"document"`
	Version  int    `json:"version"`
	Message  string `json:"message"`
}

// claimApproval is a session reading whether a person's approval of a spec stands for its claim,
// which pi-legion reads before a model call of a root architect's own conversation, because the
// state route is too large to read there. It answers for the claim's own issue alone, as the
// secret authenticates it, in one read-only transaction.
//
// It refuses as its siblings do: a blank field by its name with 400, a claim this daemon does not
// hold or a wrong secret with claim.InvalidSecret, a stale plugin with the stale-contract 409. No
// approval stands for a child's architect, a phase worker, the controller, or under a deployment
// that gates no spec step, so each of them is answered 200 with `approval: null`, as a root with
// none is: a caller that asks is told how things stand, not refused (record.StandingSpecApproval).
// A carried copy whose hand-over row names no root is answered with the document's id, and logged
// once for each such answer, since its message cannot name the root whose spec it is.
func (s *server) claimApproval(w http.ResponseWriter, r *http.Request) {
	var req ClaimApprovalRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireFields(w, r,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) {
		return
	}
	m, held, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	if s.pool == nil || s.records == nil {
		s.refuse(w, r, http.StatusInternalServerError, "the daemon keeps no records to read an approval from")
		return
	}
	var standing *record.StandingApproval
	err := pgx.BeginTxFunc(r.Context(), s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		standing, err = standingApproval(r.Context(), tx, s.records, s.steps, held.Issue, held.Role)
		return err
	})
	var answer ClaimApprovalResponse
	if err == nil {
		answer, err = approvalAnswer(standing)
	}
	if err != nil {
		s.logRequestFailure(r, err, "api: the approval of a claim could not be read", "claim", req.ClaimToken, "session", req.SessionID)
		s.refuse(w, r, http.StatusInternalServerError, "read the approval: "+err.Error())
		return
	}
	if standing != nil && standing.Carried && standing.Spec == "" {
		s.log.Info("api: the approval route names a carried spec by its document, since its hand-over row names no root",
			"claim", req.ClaimToken, "issue", standing.Root, "document", standing.ArtifactID, "version", standing.Version)
	}
	s.writeAnswer(w, r, AnswerClaimApproval, http.StatusOK, answer)
}

// ClaimApprovalOf is what POST /legion/v1/claims/approval answers a claim of role on issue, read in
// the caller's transaction: the route's own answer, for a caller that must read it at the commit
// of the transaction that told an architect of an approval. steps are the deployment's gated
// steps.
func ClaimApprovalOf(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, issue string, role claim.Role) (ClaimApprovalResponse, error) {
	standing, err := standingApproval(ctx, tx, records, steps, issue, role)
	if err != nil {
		return ClaimApprovalResponse{}, err
	}
	return approvalAnswer(standing)
}

// standingApproval is the approval that stands for a claim of role on issue, nil for none: only an
// architect is read for, since no approval stands for any other role.
func standingApproval(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, issue string, role claim.Role) (*record.StandingApproval, error) {
	if role != claim.RoleArchitect {
		return nil, nil
	}
	return record.StandingSpecApproval(ctx, tx, records, steps, issue)
}

// approvalAnswer is standing on the wire, with the message in approval.md's words.
func approvalAnswer(standing *record.StandingApproval) (ClaimApprovalResponse, error) {
	if standing == nil {
		return ClaimApprovalResponse{}, nil
	}
	if standing.Root == "" || standing.ArtifactID == "" || standing.Version < 1 || (!standing.Carried && standing.Spec != standing.Root) {
		return ClaimApprovalResponse{}, errors.New("the approval names no root, document or version, or names another root's spec without a carried gate")
	}
	message, err := prompts.ApprovalMessage(prompts.Approval{Tree: standing.Root, Spec: standing.Spec, Document: standing.ArtifactID,
		Version: standing.Version, Carried: standing.Carried})
	if err != nil {
		return ClaimApprovalResponse{}, err
	}
	document := standing.ArtifactID
	if standing.Spec != "" {
		document = "dispatch://" + standing.Spec + "/spec"
	}
	return ClaimApprovalResponse{Approval: &ClaimApproval{Document: document, Version: standing.Version, Message: message}}, nil
}
