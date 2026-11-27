package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An issue's design gates are the documents the Legion deployment's gate records on that issue,
// one per step it gates (design_gates, migration 20261110214252_issue_design_gates): `decisions`
// the decisions document the gate recorded, `criteria` the Verification document, `spec` the
// primary document. A project's row says which steps a deployment gates at all
// (PUT /api/v1/projects/{key}/gated-steps); this says which document each of those steps named
// on one issue, so Dispatch's `superseded_approval` advice prints a design-gate clause for a
// document a gate really records and stays quiet about every other document.
//
// The write belongs to the deployment's daemon and to no person, which is why the route declares
// no personal scope — routeScopes names it nowhere (routes_table.go): the daemon presents the
// credential the deployment configured, as it does on PUT /api/v1/projects/{key}/gated-steps,
// and such a credential holds no person scope. The shared token holds no scope at all, and the
// daemon's own broker token holds legion-status alone (brokerTokenActor), which no route's
// scope list names. Nothing is lost by letting a machine write it, because the record is what
// that deployment's gate did, not a choice a person makes here.
//
// The write appends no issue event and publishes nothing, whether the set changed or not. The
// record is the daemon's own fact about its own gate, so an event would feed the daemon's intake
// with facts it caused, and it would move the activity feed of every gated issue each time a
// daemon republished what Dispatch already holds.

// issueDesignGate is one step of an issue's gate: the step word, the document that step records,
// and Holds, whether the deployment's gate holds the issue's tree on that step. The step is one
// of model.GatedStepsKnown, which design_gates' own check repeats. The route answers this shape,
// and the issue read carries it; the route takes issueDesignGateInput, the same fields read as a
// request.
//
// Holds is the daemon's answer and not Dispatch's: which steps a tree is gated on is the daemon's
// rule, and it is not the project's list alone, since a root's architect may register its
// Verification document as `criteria` beside a deployment whose list names `spec` and not
// `criteria`, and that row holds the tree. It is null for an entry that does not carry it, which
// is every entry a daemon older than the field publishes (design_gates.holds, migration
// 20261114020013_design_gates_holds), and the superseded-approval advice answers such a row by the
// project's list (supersededApprovalGates).
type issueDesignGate struct {
	Step       string `json:"step" enum:"decisions,criteria,spec"`
	ArtifactID string `json:"artifact_id"`
	Holds      *bool  `json:"holds"`
}

// issueDesignGates is the stored set the route answers: the issue and its gates in the order
// model.GatedStepsKnown names the steps, which is the order a gate runs them.
type issueDesignGates struct {
	Issue string            `json:"issue"`
	Gates []issueDesignGate `json:"gates"`
}

// issueDesignGateInput is one entry of the body PUT /api/v1/issues/{key}/design-gates takes: an
// issueDesignGate as the daemon publishes it. It is a type of its own because a request body
// requires what its handler refuses the lack of, and a response the fields Go's marshaller always
// writes (openAPIRequestBodyTypes), and the two differ here: the route refuses an entry with no
// step or no document, and takes one with no holds, which every daemon older than the field
// publishes, and stores it as null.
type issueDesignGateInput struct {
	Step       string `json:"step" enum:"decisions,criteria,spec" required:"true"`
	ArtifactID string `json:"artifact_id" required:"true"`
	Holds      *bool  `json:"holds"`
}

// issueDesignGatesInput is the body PUT /api/v1/issues/{key}/design-gates takes. Gates is
// required, and a pointer for projectGatedStepsInput.Steps' reason: a body that names no list is
// refused (400 INVALID_ISSUE, putIssueDesignGates below) rather than read as the empty one, so a
// malformed publish cannot clear an issue's record, while an explicit `[]` is the publish that
// clears it.
type issueDesignGatesInput struct {
	Gates *[]issueDesignGateInput `json:"gates" required:"true"`
	Actor *model.Actor            `json:"actor"`
}

// putIssueDesignGates publishes the documents an issue's design-gate steps record. The daemon
// sends the issue's whole set, so the transaction deletes the rows the body does not name and
// upserts the ones it does: the stored set is what was published and nothing older survives
// beside it. A list naming anything but the three steps is refused before the database's own
// check would raise, a step named twice is refused as the project's own publish refuses it (one
// record per step is all the gate keeps), and a gate naming no document is refused because the
// document is what the row is about. A gate naming a document its writer may not see is refused
// in the same words (refuseUnseenGateDocuments). Each entry's holds is stored as it was given,
// and an entry that gives none is stored as null.
func (s *server) putIssueDesignGates(w http.ResponseWriter, r *http.Request) {
	var input issueDesignGatesInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The actor is resolved as every write's is, so a caller that names none is turned away
	// here; no row and no event records it, since what the record holds is the deployment's
	// gate and not who published it.
	if _, ok := s.requireActor(w, r, input.Actor); !ok {
		return
	}
	if input.Gates == nil {
		writeError(w, "INVALID_ISSUE", http.StatusBadRequest, fmt.Sprintf(
			"gates is required: the document each design-gate step (%s) records on this issue, or [] for an issue whose gates record none",
			strings.Join(model.GatedStepsKnown, ", ")))
		return
	}
	gates := make([]issueDesignGate, 0, len(*input.Gates))
	for _, entry := range *input.Gates {
		gate := issueDesignGate(entry)
		if !slices.Contains(model.GatedStepsKnown, gate.Step) {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, fmt.Sprintf(
				"%q is not a design-gate step: gates names %s", gate.Step, strings.Join(model.GatedStepsKnown, ", ")))
			return
		}
		if slices.ContainsFunc(gates, func(earlier issueDesignGate) bool { return earlier.Step == gate.Step }) {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, fmt.Sprintf(
				"%q is named twice: gates names each design-gate step once, with the one document that step records", gate.Step))
			return
		}
		if gate.ArtifactID == "" {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, gateNamesNoDocument(gate.Step))
			return
		}
		gates = append(gates, gate)
	}
	// The order a reader gets the set back in is model.GatedStepsKnown's, whatever order the
	// publish named the steps in: the rows are a set keyed by step, so the answer is sorted
	// where a project's list is stored in the order it was published.
	slices.SortFunc(gates, func(left, right issueDesignGate) int {
		return slices.Index(model.GatedStepsKnown, left.Step) - slices.Index(model.GatedStepsKnown, right.Step)
	})
	steps := make([]string, len(gates))
	documents := make([]string, len(gates))
	holds := make([]*bool, len(gates))
	for index, gate := range gates {
		steps[index] = gate.Step
		documents[index] = gate.ArtifactID
		holds[index] = gate.Holds
	}

	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	refused, err := s.refuseUnseenGateDocuments(r.Context(), who, gates)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if refused != "" {
		writeError(w, "INVALID_ISSUE", http.StatusBadRequest, gateNamesNoDocument(refused))
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The issue is read in the write's own transaction, under its owner's advisory lock, which
	// every writer of the issue takes (store.LockOwner): the lock and the read are one batch,
	// the read is a plain select in the statement after the lock, and the refusal and the write
	// decide on the same row (INFRA-636: while no earlier build is in the fleet, no row lock of an
	// owner table, which would give the transaction its id before the reads; beside an earlier
	// build LockOwnerAndRead also takes the issue's row, as that build's writers do). The lock is
	// exclusive and held to the commit, so a second publish of the same issue waits for it and the
	// two replace the set one after the other: the stored set is always one whole set, the set of
	// the one that finishes last (TestTwoDesignGatePublishesOfOneIssueAtOnceLeaveOneWholeSet). The
	// row is read as the caller reads it: an issue of a project it does not see is one no row
	// holds, and it is read so once before the lock is asked for (lockOwnerOnceFound), so a hidden
	// issue is refused without waiting behind its project's writers, as a key no issue holds is.
	// The code is written here rather than taken from loadIssue, which answers the package's
	// plain NOT_FOUND: the daemon must tell a Dispatch that knows no such issue from a Dispatch that
	// does not serve this route at all, and that one answers the router's own
	// `404 {"code":"NOT_FOUND"}` (routes/router.go), which is the plain code loadIssue would give
	// here too. The daemon ends a publication on this code alone, so the two must differ: with
	// loadIssue's code every publication against a Dispatch older than the route would be dropped
	// as a refusal no retry repairs.
	var issue string
	if err := lockOwnerOnceFound(r.Context(), tx, store.IssueOwner(r.PathValue("key")), `
		select key from issues where key = $1 and `+access.ProjectClause("project_key", "$2")+`
	`, []any{r.PathValue("key"), refs.VisibilityArg(who)}, &issue); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "ISSUE_NOT_FOUND", http.StatusNotFound, fmt.Sprintf("no issue %s", r.PathValue("key")))
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		delete from design_gates where issue = $1 and not (step = any($2::text[]))
	`, issue, steps); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The delete above stands for the publish that names no gate — the one that clears a record,
	// and the one the daemon sends for every issue a generation clear empties — so the insert is
	// run only when there is a row to write: an insert selecting from empty arrays writes nothing
	// and asks the database for it anyway.
	if len(gates) > 0 {
		// holds is written whatever it was, null included: the publish is the whole of what the
		// row says, so an entry that carries no answer replaces the one an earlier publish gave
		// rather than leaving it standing beside a document it may not have been given for.
		if _, err := tx.Exec(r.Context(), `
			insert into design_gates (issue, step, artifact_id, holds)
			select $1::text, published.step, published.artifact_id, published.holds
			from unnest($2::text[], $3::text[], $4::boolean[]) as published(step, artifact_id, holds)
			on conflict (issue, step) do update set artifact_id = excluded.artifact_id, holds = excluded.holds
		`, issue, steps, documents, holds); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, issueDesignGates{Issue: issue, Gates: gates})
}

// gateNamesNoDocument is the refusal of a design gate whose step names no document: an empty
// artifact_id, or one naming a document its writer may not see (refuseUnseenGateDocuments).
func gateNamesNoDocument(step string) string {
	return fmt.Sprintf("%q names no document: artifact_id is the document that step's gate recorded", step)
}

// gateDocumentsSeen answers, of the document ids $1, those the viewer $2 binds may see
// (access.DocumentClause): an id no document holds, and one that is no uuid, is none of them.
var gateDocumentsSeen = `
	select named.id from unnest($1::text[]) as named(id)
	where exists (
		select 1 from artifacts a
		where a.id = ` + store.CastableUUID("named.id") + ` and ` + access.DocumentClause("a", "$2") + `
		offset 0)`

// refuseUnseenGateDocuments answers the step of the first of gates, in the order they are stored,
// whose document who may not see, or "" when it sees every one: a gate naming a document its
// writer may not see is refused as one naming no document, so the refusal says nothing of whether
// the document exists, and no row records a document of a project its writer cannot read. A
// writer that sees every object (seesEveryObject) - an open one of a deployment that holds no
// private project - reads nothing, and the route stores what it names as before (R9), whether or
// not a document holds the id. The read runs on the pool before the transaction opens, so it
// holds no lock of the issue.
func (s *server) refuseUnseenGateDocuments(ctx context.Context, who model.Actor, gates []issueDesignGate) (string, error) {
	if len(gates) == 0 || seesEveryObject(who) {
		return "", nil
	}
	named := make([]string, len(gates))
	for index, gate := range gates {
		named[index] = gate.ArtifactID
	}
	rows, err := s.deps.Store.Pool.Query(ctx, gateDocumentsSeen, named, refs.VisibilityArg(who))
	if err != nil {
		return "", err
	}
	seen, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	for _, gate := range gates {
		if !slices.Contains(seen, gate.ArtifactID) {
			return gate.Step, nil
		}
	}
	return "", nil
}
