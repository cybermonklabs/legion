package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// and such a credential holds no scope at all. Nothing is lost by letting a machine write it,
// because the record is what that deployment's gate did, not a choice a person makes here.
//
// The write appends no issue event and publishes nothing, whether the set changed or not. The
// record is the daemon's own fact about its own gate, so an event would feed the daemon's intake
// with facts it caused, and it would move the activity feed of every gated issue each time a
// daemon republished what Dispatch already holds.

// issueDesignGate is one step of an issue's gate: the step word and the document that step
// records. The step is one of model.GatedStepsKnown, which design_gates' own check repeats. The
// route takes and answers the same shape, since what the daemon publishes is what Dispatch
// stores.
type issueDesignGate struct {
	Step       string `json:"step" enum:"decisions,criteria,spec"`
	ArtifactID string `json:"artifact_id"`
}

// issueDesignGates is the stored set the route answers: the issue and its gates in the order
// model.GatedStepsKnown names the steps, which is the order a gate runs them.
type issueDesignGates struct {
	Issue string            `json:"issue"`
	Gates []issueDesignGate `json:"gates"`
}

// issueDesignGatesInput is the body PUT /api/v1/issues/{key}/design-gates takes. Gates is
// required, and a pointer for projectGatedStepsInput.Steps' reason: a body that names no list is
// refused (400 INVALID_ISSUE, putIssueDesignGates below) rather than read as the empty one, so a
// malformed publish cannot clear an issue's record, while an explicit `[]` is the publish that
// clears it.
type issueDesignGatesInput struct {
	Gates *[]issueDesignGate `json:"gates" required:"true"`
	Actor *model.Actor       `json:"actor"`
}

// putIssueDesignGates publishes the documents an issue's design-gate steps record. The daemon
// sends the issue's whole set, so the transaction deletes the rows the body does not name and
// upserts the ones it does: the stored set is what was published and nothing older survives
// beside it. A list naming anything but the three steps is refused before the database's own
// check would raise, a step named twice is refused as the project's own publish refuses it (one
// record per step is all the gate keeps), and a gate naming no document is refused because the
// document is the whole of what the row says.
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
	gates := *input.Gates
	for index, gate := range gates {
		if !slices.Contains(model.GatedStepsKnown, gate.Step) {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, fmt.Sprintf(
				"%q is not a design-gate step: gates names %s", gate.Step, strings.Join(model.GatedStepsKnown, ", ")))
			return
		}
		if slices.ContainsFunc(gates[:index], func(earlier issueDesignGate) bool { return earlier.Step == gate.Step }) {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, fmt.Sprintf(
				"%q is named twice: gates names each design-gate step once, with the one document that step records", gate.Step))
			return
		}
		if gate.ArtifactID == "" {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, fmt.Sprintf(
				"%q names no document: artifact_id is the document that step's gate recorded", gate.Step))
			return
		}
	}
	// The order a reader gets the set back in is model.GatedStepsKnown's, whatever order the
	// publish named the steps in: the rows are a set keyed by step, so the answer is sorted
	// where a project's list is stored in the order it was published.
	slices.SortFunc(gates, func(left, right issueDesignGate) int {
		return slices.Index(model.GatedStepsKnown, left.Step) - slices.Index(model.GatedStepsKnown, right.Step)
	})
	steps := make([]string, len(gates))
	documents := make([]string, len(gates))
	for index, gate := range gates {
		steps[index] = gate.Step
		documents[index] = gate.ArtifactID
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The issue is read in the write's own transaction, under the key-share lock the rows'
	// foreign key takes anyway, so the refusal and the write decide on the same row. The code is
	// written here rather than taken from loadIssue, which answers the package's plain
	// NOT_FOUND: the daemon must tell a Dispatch that knows no such issue from a Dispatch that
	// does not serve this route at all, whose answer is the codeless `404 page not found`.
	var issue string
	if err := tx.QueryRow(r.Context(), `
		select key from issues where key = $1 for key share
	`, r.PathValue("key")).Scan(&issue); err != nil {
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
		if _, err := tx.Exec(r.Context(), `
			insert into design_gates (issue, step, artifact_id)
			select $1::text, published.step, published.artifact_id
			from unnest($2::text[], $3::text[]) as published(step, artifact_id)
			on conflict (issue, step) do update set artifact_id = excluded.artifact_id
		`, issue, steps, documents); err != nil {
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
