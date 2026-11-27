package workflowstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// stepKeyForm is the key of a plan's step after its role (`acceptance:owner`): a lowercase letter,
// then lowercase letters, digits and hyphens. The role before it is a step record's role
// (roleForm), since every run of the step is a record of that role.
var stepKeyForm = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

var planSources = []string{PlanSourceDefault, PlanSourceOwner}

// planFact is the one fact of a plan that a later version replaces whole: its source, its
// document, its version and its tasks.
var planFact = []string{AttributeSource, AttributeArtifactID, AttributeVersion, AttributeTasks}

// PutPlan writes the plan of one run of an issue inside the caller's transaction and answers its
// node as it stands after the write. The Legion daemon writes it under one id for the run, when
// the run's work starts and again at each later version of the owner's plan that Legion takes.
// The plan's source, its document, its version and its tasks are one fact (planFact): a write of
// a version above the one the node holds replaces all four in one write, and a write at or below
// it leaves all four, so a write that arrives late, twice or out of order changes nothing. A plan
// of the source default has no version and counts as version 0, so an owner's version 1 replaces
// it and it replaces nothing. The issue, the project and the generation are each set once, by the
// first write that states them, and the node's first writer stays its maker.
//
// Every write names the plan's generation, its source and its tasks, and an owner's plan its
// document and version, or it is refused ErrPlanInput and writes nothing; so a write that replaces
// the fact states all four of it. A default plan holds its document and version empty, so a write
// at or below its version 0 gives it neither, and an owner's write, whose version is 1 or more,
// replaces the default's fact whole. The write that makes the node states its `part_of` relation
// to the issue; an id whose node is part of another issue is refused ErrPlanOtherIssue.
func PutPlan(ctx context.Context, tx pgx.Tx, write PlanWrite, actor model.Actor, at time.Time) (Plan, error) {
	id, stated, err := write.attributes()
	if err != nil {
		return Plan{}, err
	}
	// A default plan states no document, so its version reads 0 (PlanWrite.Document).
	version := write.Document.Version
	written, _, err := putNode(ctx, tx, nodeWrite{
		kind: KindPlan, id: id, issue: write.Issue, stated: stated, makes: true, actor: actor, at: at,
		replaces: func(held map[string]json.RawMessage) ([]string, error) {
			heldVersion := 0
			if raw, holds := held[AttributeVersion]; holds {
				if err := json.Unmarshal(raw, &heldVersion); err != nil {
					return nil, fmt.Errorf("decode the version of plan %s: %w", id, err)
				}
			}
			if version > heldVersion {
				return planFact, nil
			}
			return nil, nil
		},
	})
	var other *nodeOfAnotherIssue
	switch {
	case errors.As(err, &other):
		// The refusal names the plan and the issue the write names, never the issue the plan is a
		// plan of: a writer of one issue that names another issue's plan id learns nothing of that
		// issue, which may be one of a project it does not see.
		return Plan{}, fmt.Errorf("%w: plan %s is no plan of %s, the issue this write names", ErrPlanOtherIssue, id, write.Issue)
	case err != nil:
		return Plan{}, err
	}
	return planFrom(id, written)
}

// attributes checks every field the plan write states against its form and answers the write's
// id in its canonical spelling with the fields it states, in one order.
func (write PlanWrite) attributes() (string, []attribute, error) {
	parsed, err := uuid.Parse(write.ID)
	if err != nil {
		return "", nil, refusal(ErrPlanInput, "id %q is not a uuid: a plan's id is the uuid the daemon derives for the issue's run", write.ID)
	}
	id := parsed.String()
	if strings.TrimSpace(write.Issue) == "" {
		return "", nil, refusal(ErrPlanInput, "issue is required: the key of the issue plan %s is part of", id)
	}
	stated := []attribute{{AttributeIssue, write.Issue, write.Issue}}
	if write.Project != nil && *write.Project != "" {
		stated = append(stated, attribute{AttributeProject, *write.Project, *write.Project})
	}

	if write.Generation == nil {
		return "", nil, refusal(ErrPlanInput, "generation is required: the run of the issue the plan is of")
	}
	if generation := *write.Generation; generation < 1 {
		return "", nil, refusal(ErrPlanInput, "generation %d is no run of an issue: an issue's generation counts from 1", generation)
	}
	stated = append(stated, attribute{AttributeGeneration, *write.Generation, strconv.FormatInt(*write.Generation, 10)})

	if write.Source == nil || *write.Source == "" {
		return "", nil, refusal(ErrPlanInput, "source is required: one of %s", strings.Join(planSources, ", "))
	}
	source := *write.Source
	if !slices.Contains(planSources, source) {
		return "", nil, refusal(ErrPlanInput, "source %q is not a source of a plan: one of %s", source, strings.Join(planSources, ", "))
	}
	stated = append(stated, attribute{AttributeSource, source, source})

	document := write.Document
	switch source {
	case PlanSourceDefault:
		if document.ArtifactID != "" {
			return "", nil, refusal(ErrPlanInput, "artifact_id names the document an owner gave, and a plan of source %s has none", source)
		}
		if document.Version != 0 {
			return "", nil, refusal(ErrPlanInput, "version names the version of the document an owner gave, and a plan of source %s has none", source)
		}
		stated = append(stated,
			attribute{AttributeArtifactID, "", ""},
			attribute{AttributeVersion, int64(0), "0"})
	case PlanSourceOwner:
		if document.ArtifactID == "" {
			return "", nil, refusal(ErrPlanInput, "artifact_id is required of a plan of source %s: the document its owner gave", source)
		}
		artifact, err := uuid.Parse(document.ArtifactID)
		if err != nil {
			return "", nil, refusal(ErrPlanInput, "artifact_id %q is not a uuid: it is the id of the document the owner gave", document.ArtifactID)
		}
		if document.Version == 0 {
			return "", nil, refusal(ErrPlanInput, "version is required of a plan of source %s: the version of the document Legion took", source)
		}
		if document.Version < 1 {
			return "", nil, refusal(ErrPlanInput, "version %d is no version of a document: versions count from 1", document.Version)
		}
		canonical := artifact.String()
		stated = append(stated,
			attribute{AttributeArtifactID, canonical, canonical},
			attribute{AttributeVersion, int64(document.Version), strconv.Itoa(document.Version)})
	}

	if write.Tasks == nil {
		return "", nil, refusal(ErrPlanInput, "tasks is required: a list of tasks, each {id, steps}")
	}
	if len(write.Tasks) == 0 {
		return "", nil, refusal(ErrPlanInput, "tasks is empty: a plan has at least one task")
	}
	named := map[string]bool{}
	for _, task := range write.Tasks {
		if !taskForm.MatchString(task.ID) {
			return "", nil, refusal(ErrPlanInput, "task %q is not a task id: %s", task.ID, taskRule)
		}
		if named[task.ID] {
			return "", nil, refusal(ErrPlanInput, "task %s is named twice: a task's id is unique in its plan", task.ID)
		}
		named[task.ID] = true
		if len(task.Steps) == 0 {
			return "", nil, refusal(ErrPlanInput, "steps of task %s are empty: a task has at least one step", task.ID)
		}
		for _, step := range task.Steps {
			role, key, keyed := strings.Cut(step, ":")
			if !roleForm.MatchString(role) {
				return "", nil, refusal(ErrPlanInput, "steps of task %s hold %q, whose role %q is not a role: %s, as a step record's role", task.ID, step, role, roleRule)
			}
			if keyed && !stepKeyForm.MatchString(key) {
				return "", nil, refusal(ErrPlanInput, "steps of task %s hold %q, whose key %q is not a key: a lowercase letter, then lowercase letters, digits and hyphens", task.ID, step, key)
			}
		}
	}
	text, err := json.Marshal(write.Tasks)
	if err != nil {
		return "", nil, fmt.Errorf("encode the tasks of plan %s: %w", id, err)
	}
	stated = append(stated, attribute{AttributeTasks, json.RawMessage(text), string(text)})
	return id, stated, nil
}

// planAttributes is the node row's attributes as a plan reads them.
type planAttributes struct {
	Issue      string     `json:"issue"`
	Project    string     `json:"project"`
	Generation int64      `json:"generation"`
	Source     string     `json:"source"`
	ArtifactID string     `json:"artifact_id"`
	Version    int        `json:"version"`
	Tasks      []PlanTask `json:"tasks"`
}

// planFrom reads one node row's attributes as the plan they hold. An artifact id of "" is a plan
// that holds no document, the source default's, since a write sets the document and its version
// together or neither.
func planFrom(id string, attributes []byte) (Plan, error) {
	var held planAttributes
	if err := json.Unmarshal(attributes, &held); err != nil {
		return Plan{}, fmt.Errorf("decode the attributes of plan %s: %w", id, err)
	}
	plan := Plan{
		ID: id, Issue: held.Issue, Project: held.Project, Source: held.Source,
		Generation: held.Generation, Tasks: held.Tasks,
	}
	if held.ArtifactID != "" {
		plan.Document = &PlanDocument{ArtifactID: held.ArtifactID, Version: held.Version}
	}
	return plan, nil
}
