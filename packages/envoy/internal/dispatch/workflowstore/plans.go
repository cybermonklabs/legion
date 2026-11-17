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

// stepWordForm is one step of a plan's task: a role, or a role and a key (`acceptance:owner`),
// each a lowercase letter, then lowercase letters, digits and hyphens.
var stepWordForm = regexp.MustCompile(`^[a-z][a-z0-9-]*(:[a-z][a-z0-9-]*)?$`)

var planSources = []string{PlanSourceDefault, PlanSourceOwner}

// PutPlan writes the plan of one run of an issue inside the caller's transaction and answers its
// node as it stands after the write. The Legion daemon writes it when the run's work starts, and
// again where it sends the same plan again; each field is set once, so a write that runs again
// changes nothing, and a later write of other tasks under the same id leaves the first.
//
// Every write names the plan's generation, its source and its tasks, and an owner's plan its
// document and version, or it is refused ErrPlanInput and writes nothing. The write that makes the
// node states its `part_of` relation to the issue; an id whose node is part of another issue is
// refused ErrPlanOtherIssue.
func PutPlan(ctx context.Context, tx pgx.Tx, write PlanWrite, actor model.Actor, at time.Time) (Plan, error) {
	id, stated, err := write.attributes()
	if err != nil {
		return Plan{}, err
	}
	writer, err := json.Marshal(actor)
	if err != nil {
		return Plan{}, fmt.Errorf("encode the writer of plan %s: %w", id, err)
	}
	written, err := putNode(ctx, tx, nodeWrite{
		kind: KindPlan, id: id, issue: write.Issue, stated: stated, makes: true, writer: writer, at: at,
	})
	var other *nodeOfAnotherIssue
	switch {
	case errors.As(err, &other):
		return Plan{}, fmt.Errorf("%w: plan %s is part of %s, and this write names %s", ErrPlanOtherIssue, id, other.issue, write.Issue)
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
		return "", nil, planInputError("id %q is not a uuid: a plan's id is the uuid the daemon derives for the issue's run", write.ID)
	}
	id := parsed.String()
	if strings.TrimSpace(write.Issue) == "" {
		return "", nil, planInputError("issue is required: the key of the issue plan %s is part of", id)
	}
	stated := []attribute{{AttributeIssue, write.Issue, write.Issue}}
	if write.Project != nil && *write.Project != "" {
		stated = append(stated, attribute{AttributeProject, *write.Project, *write.Project})
	}

	if write.Generation == nil {
		return "", nil, planInputError("generation is required: the run of the issue the plan is of")
	}
	if generation := *write.Generation; generation < 1 {
		return "", nil, planInputError("generation %d is no run of an issue: an issue's generation counts from 1", generation)
	}
	stated = append(stated, attribute{AttributeGeneration, *write.Generation, strconv.FormatInt(*write.Generation, 10)})

	if write.Source == nil || *write.Source == "" {
		return "", nil, planInputError("source is required: one of %s", strings.Join(planSources, ", "))
	}
	source := *write.Source
	if !slices.Contains(planSources, source) {
		return "", nil, planInputError("source %q is not a source of a plan: one of %s", source, strings.Join(planSources, ", "))
	}
	stated = append(stated, attribute{AttributeSource, source, source})

	document := write.ArtifactID != nil && *write.ArtifactID != ""
	versioned := write.Version != nil && *write.Version != 0
	switch source {
	case PlanSourceDefault:
		if document {
			return "", nil, planInputError("artifact_id names the document an owner gave, and a plan of source %s has none", source)
		}
		if versioned {
			return "", nil, planInputError("version names the version of the document an owner gave, and a plan of source %s has none", source)
		}
	case PlanSourceOwner:
		if !document {
			return "", nil, planInputError("artifact_id is required of a plan of source %s: the document its owner gave", source)
		}
		artifact, err := uuid.Parse(*write.ArtifactID)
		if err != nil {
			return "", nil, planInputError("artifact_id %q is not a uuid: it is the id of the document the owner gave", *write.ArtifactID)
		}
		if !versioned {
			return "", nil, planInputError("version is required of a plan of source %s: the version of the document Legion took", source)
		}
		if version := *write.Version; version < 1 {
			return "", nil, planInputError("version %d is no version of a document: versions count from 1", version)
		}
		canonical := artifact.String()
		stated = append(stated,
			attribute{AttributeArtifactID, canonical, canonical},
			attribute{AttributeVersion, int64(*write.Version), strconv.Itoa(*write.Version)})
	}

	if write.Tasks == nil {
		return "", nil, planInputError("tasks is required: a list of tasks, each {id, steps}")
	}
	if len(write.Tasks) == 0 {
		return "", nil, planInputError("tasks is empty: a plan has at least one task")
	}
	named := map[string]bool{}
	for _, task := range write.Tasks {
		if !taskForm.MatchString(task.ID) {
			return "", nil, planInputError("task %q is not a task id: lowercase letters, digits and hyphens, starting with a letter or a digit, at most 64", task.ID)
		}
		if named[task.ID] {
			return "", nil, planInputError("task %s is named twice: a task's id is unique in its plan", task.ID)
		}
		named[task.ID] = true
		if len(task.Steps) == 0 {
			return "", nil, planInputError("steps of task %s are empty: a task has at least one step", task.ID)
		}
		for _, step := range task.Steps {
			if !stepWordForm.MatchString(step) {
				return "", nil, planInputError("steps of task %s hold %q, which is no step: a role or role:key, each a lowercase letter, then lowercase letters, digits and hyphens", task.ID, step)
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

func planInputError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPlanInput, fmt.Sprintf(format, args...))
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

// planFrom reads one node row's attributes as the plan they hold.
func planFrom(id string, attributes []byte) (Plan, error) {
	var held planAttributes
	if err := json.Unmarshal(attributes, &held); err != nil {
		return Plan{}, fmt.Errorf("decode the attributes of plan %s: %w", id, err)
	}
	return Plan{
		ID: id, Issue: held.Issue, Project: held.Project, Source: held.Source, ArtifactID: held.ArtifactID,
		Generation: held.Generation, Version: held.Version, Tasks: held.Tasks,
	}, nil
}
