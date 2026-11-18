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

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The forms of a step's names. A task id is a plan's slug, `default` for an issue whose owner gave
// no plan; a role and a type are a word of lowercase letters, digits and hyphens. The agent, the
// session and the model are printable ASCII with no space, at most 128 characters
// (contracts.StepAgentTextPattern), since `dispatch steps` joins a record's fields with spaces.
var (
	taskForm      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	roleForm      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	agentTextForm = regexp.MustCompile(contracts.StepAgentTextPattern)
)

var (
	stepKinds   = []string{StepKindStep, StepKindRetro, StepKindMerge}
	stepResults = []string{ResultPassed, ResultFailed, ResultStopped}
)

// PutStep writes one run of one step inside the caller's transaction and answers its node as it
// stands after the write. It sets each field the write states that the node does not hold yet,
// appending one value tuple for each, and leaves each field the node holds as it is, appending
// nothing for it. So a start and an end merge in either order, a write that runs again changes
// nothing, and a later end never replaces an earlier one. The one field taken otherwise is
// returned_to: the result of a run and the step it returned to are one fact, the outcome one end
// reported, so only the write that sets the result sets it, and a node that holds a result keeps
// the destination its own end named, none included.
//
// The write that finds no node makes it, whichever of the two it is, and states the `part_of`
// relation to the issue; it must name the step's whole identity (task, kind, step, role and
// generation), or it is refused ErrStepInput and writes nothing. A write that names a type for a
// node of kind retro or merge is refused ErrStepInput too, since neither has one. The write that
// sets the report states the `reports` relation to that handoff.
//
// Two writes of one id at one time take turns: the insert that makes the node comes before the
// read of what it holds, and that read locks the row (for update), so the second write waits for
// the first to commit and then reads everything the first wrote.
func PutStep(ctx context.Context, tx pgx.Tx, write StepWrite, actor model.Actor, at time.Time) (Step, error) {
	id, stated, err := write.attributes()
	if err != nil {
		return Step{}, err
	}
	writer, err := json.Marshal(actor)
	if err != nil {
		return Step{}, fmt.Errorf("encode the writer of step %s: %w", id, err)
	}

	made := false
	if len(write.missingIdentity()) == 0 {
		var inserted string
		err := tx.QueryRow(ctx, `
			insert into workflow_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			values ('`+KindStep+`', $1, '`+KindStep+`', '`+VisibilityProject+`', '`+StateLive+`', '{}', $2, $3, $4)
			on conflict (kind, id) do nothing
			returning id
		`, id, at, writer, NoEvent).Scan(&inserted)
		switch {
		case err == nil:
			made = true
		case !errors.Is(err, pgx.ErrNoRows):
			return Step{}, fmt.Errorf("write step %s: %w", id, err)
		}
	}

	var heldText []byte
	err = tx.QueryRow(ctx, `
		select attributes from workflow_nodes where kind = '`+KindStep+`' and id = $1 for update
	`, id).Scan(&heldText)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, inputError(
			"step %s holds no node yet, and this write, which would make it, does not name its %s: the first write of a step names its task, kind, step, role and generation",
			id, strings.Join(write.missingIdentity(), ", "))
	}
	if err != nil {
		return Step{}, fmt.Errorf("read step %s: %w", id, err)
	}
	var held map[string]json.RawMessage
	if err := json.Unmarshal(heldText, &held); err != nil {
		return Step{}, fmt.Errorf("decode the attributes of step %s: %w", id, err)
	}
	current, err := stepFrom(id, heldText)
	if err != nil {
		return Step{}, err
	}
	if !made {
		if current.Issue != write.Issue {
			return Step{}, fmt.Errorf("%w: step %s is part of %s, and this write names %s", ErrStepOtherIssue, id, current.Issue, write.Issue)
		}
		if write.Type != nil {
			if err := typeOfKind(current.Kind, *write.Type); err != nil {
				return Step{}, err
			}
		}
	}

	setting := map[string]any{}
	var tuples []tuple
	var report string
	_, ended := held[AttributeResult]
	for _, attribute := range stated {
		if _, holds := held[attribute.name]; holds {
			continue
		}
		if attribute.name == AttributeReturnedTo && ended {
			continue
		}
		setting[attribute.name] = attribute.value
		tuples = append(tuples, tuple{fromID: id, kind: attribute.name, toKind: KindValue, toID: attribute.text})
		if attribute.name == AttributeReport {
			report = attribute.text
		}
	}
	if made {
		tuples = append(tuples, tuple{fromID: id, kind: RelationPartOf, toKind: KindIssue, toID: write.Issue})
	}
	if report != "" {
		tuples = append(tuples, tuple{fromID: id, kind: RelationReports, toKind: KindHandoff, toID: report})
	}
	if len(setting) == 0 {
		return current, nil
	}

	moved, err := json.Marshal(setting)
	if err != nil {
		return Step{}, fmt.Errorf("encode the attributes of step %s: %w", id, err)
	}
	var written []byte
	if err := tx.QueryRow(ctx, `
		update workflow_nodes set attributes = attributes || $2::jsonb
		where kind = '`+KindStep+`' and id = $1
		returning attributes
	`, id, moved).Scan(&written); err != nil {
		return Step{}, fmt.Errorf("write step %s: %w", id, err)
	}
	if err := appendTuples(ctx, tx, tuples, writer, at); err != nil {
		return Step{}, fmt.Errorf("write step %s: %w", id, err)
	}
	return stepFrom(id, written)
}

// attribute is one field a write states: its name, the value the node row holds, and the text its
// value tuple holds.
type attribute struct {
	name  string
	value any
	text  string
}

// attributes checks every field the write states against its form and answers the write's id in
// its canonical spelling with the fields it states, in one order whatever order they were given
// in. A text field that is empty states nothing; a ReturnedTo of 0 is place 0 and states it.
func (write StepWrite) attributes() (string, []attribute, error) {
	parsed, err := uuid.Parse(write.ID)
	if err != nil {
		return "", nil, inputError("id %q is not a uuid: a step record's id is the uuid the daemon derives for the run", write.ID)
	}
	id := parsed.String()
	if strings.TrimSpace(write.Issue) == "" {
		return "", nil, inputError("issue is required: the key of the issue step %s is part of", id)
	}
	stated := []attribute{{AttributeIssue, write.Issue, write.Issue}}
	text := func(name string, value *string) string {
		if value == nil || *value == "" {
			return ""
		}
		stated = append(stated, attribute{name, *value, *value})
		return *value
	}
	number := func(name string, value int64) {
		stated = append(stated, attribute{name, value, strconv.FormatInt(value, 10)})
	}
	moment := func(name string, value *time.Time) error {
		if value == nil {
			return nil
		}
		utc := value.UTC()
		if year := utc.Year(); year < 0 || year > 9999 {
			return inputError("%s cannot be represented in UTC: its year must be between 0 and 9999", name)
		}
		formatted := utc.Format(time.RFC3339Nano)
		stated = append(stated, attribute{name, formatted, formatted})
		return nil
	}

	text(AttributeProject, write.Project)
	if write.Generation != nil {
		if *write.Generation < 1 {
			return "", nil, inputError("generation %d is no run of an issue: an issue's generation counts from 1", *write.Generation)
		}
		number(AttributeGeneration, *write.Generation)
	}
	if task := text(AttributeTask, write.Task); task != "" && !taskForm.MatchString(task) {
		return "", nil, inputError("task %q is not a task id: lowercase letters, digits and hyphens, starting with a letter or a digit, at most 64", task)
	}
	kind := text(AttributeKind, write.Kind)
	if kind != "" && !slices.Contains(stepKinds, kind) {
		return "", nil, inputError("kind %q is not a kind of step: one of %s", kind, strings.Join(stepKinds, ", "))
	}
	if write.Place != nil {
		place := *write.Place
		switch {
		case place < 0:
			return "", nil, inputError("step %d is no place: a step's place is 0 or more", place)
		case kind != "" && !contracts.StepPlaceHolds(kind, place):
			if kind == StepKindStep {
				return "", nil, inputError("step %d is no place of a step of kind step: a plan's steps count from 1", place)
			}
			return "", nil, inputError("step %d is no place of a %s: no plan names it, so its step is 0", place, kind)
		}
		number(AttributeStep, int64(place))
	}
	if role := text(AttributeRole, write.Role); role != "" && !roleForm.MatchString(role) {
		return "", nil, inputError("role %q is not a role: a lowercase letter, then lowercase letters, digits and hyphens, at most 32", role)
	}
	typ := text(AttributeType, write.Type)
	if typ != "" && !roleForm.MatchString(typ) {
		return "", nil, inputError("type %q is not a type of work: a lowercase letter, then lowercase letters, digits and hyphens, at most 32, or empty", typ)
	}
	if err := typeOfKind(kind, typ); err != nil {
		return "", nil, err
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{AttributeAgent, write.Agent}, {AttributeSession, write.Session}, {AttributeModel, write.Model}} {
		if value := text(field.name, field.value); value != "" && !agentTextForm.MatchString(value) {
			return "", nil, inputError("%s %q is not an agent's text: %s", field.name, value, contracts.StepAgentTextRule)
		}
	}
	if err := moment(AttributeStartedAt, write.StartedAt); err != nil {
		return "", nil, err
	}

	result := text(AttributeResult, write.Result)
	if result != "" && !slices.Contains(stepResults, result) {
		return "", nil, inputError("result %q is not a result: one of %s", result, strings.Join(stepResults, ", "))
	}
	if (result != "") != (write.EndedAt != nil) {
		if result == "" {
			return "", nil, inputError("ended_at is written with the result of the run it ends, and this write names none")
		}
		return "", nil, inputError("result %s is written with ended_at, the moment the run ended, and this write names none", result)
	}
	if err := moment(AttributeEndedAt, write.EndedAt); err != nil {
		return "", nil, err
	}
	if write.ReturnedTo != nil {
		returned := *write.ReturnedTo
		if returned < 0 {
			return "", nil, inputError("returned_to %d is no place of a step", returned)
		}
		if result != ResultFailed && result != ResultStopped {
			return "", nil, inputError("returned_to names the step a run returned its task to, which only a run that ended %s or %s does, and this write's result is %q", ResultFailed, ResultStopped, result)
		}
		number(AttributeReturnedTo, int64(returned))
	}
	if write.Report != nil && *write.Report != "" {
		report, err := uuid.Parse(*write.Report)
		if err != nil {
			return "", nil, inputError("report %q is not a uuid: it is the id of the handoff the ending role's completion named", *write.Report)
		}
		canonical := report.String()
		stated = append(stated, attribute{AttributeReport, canonical, canonical})
	}
	return id, stated, nil
}

// missingIdentity names each field of a step's identity the write does not state, in the order the
// refusal names them: a write that states them all can make the node.
func (write StepWrite) missingIdentity() []string {
	var missing []string
	if write.Task == nil || *write.Task == "" {
		missing = append(missing, AttributeTask)
	}
	if write.Kind == nil || *write.Kind == "" {
		missing = append(missing, AttributeKind)
	}
	if write.Place == nil {
		missing = append(missing, AttributeStep)
	}
	if write.Role == nil || *write.Role == "" {
		missing = append(missing, AttributeRole)
	}
	if write.Generation == nil {
		missing = append(missing, AttributeGeneration)
	}
	return missing
}

// typeOfKind refuses a type for a run of kind retro or merge: no plan names either, so neither
// has a type, as neither has a place but 0.
func typeOfKind(kind, typ string) error {
	if typ != "" && kind != "" && kind != StepKindStep {
		return inputError("type %q is no type of a %s: no plan names it, so it has none", typ, kind)
	}
	return nil
}

func inputError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrStepInput, fmt.Sprintf(format, args...))
}

// tuple is one fact a write states about one step: an attribute, whose toKind is KindValue and
// whose toID is the value, or a relation to the node toKind and toID address.
type tuple struct {
	fromID, kind, toKind, toID string
}

// appendTuples appends every tuple of one write in one statement, in the order the write states
// them, which is the order their seq is drawn in. Every tuple of the write has one writer, one
// moment and no event, and asserts its fact.
func appendTuples(ctx context.Context, tx pgx.Tx, tuples []tuple, writer []byte, at time.Time) error {
	fromIDs := make([]string, len(tuples))
	kinds := make([]string, len(tuples))
	toKinds := make([]string, len(tuples))
	toIDs := make([]string, len(tuples))
	for index, stated := range tuples {
		fromIDs[index], kinds[index], toKinds[index], toIDs[index] = stated.fromID, stated.kind, stated.toKind, stated.toID
	}
	if _, err := tx.Exec(ctx, `
		insert into workflow_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select '`+KindStep+`', stated.from_id, stated.kind, stated.to_kind, stated.to_id, $5, $6::jsonb, $7, true
		from unnest($1::text[], $2::text[], $3::text[], $4::text[]) with ordinality as stated(from_id, kind, to_kind, to_id, place)
		order by stated.place
	`, fromIDs, kinds, toKinds, toIDs, at, writer, NoEvent); err != nil {
		return fmt.Errorf("append step tuples: %w", err)
	}
	return nil
}

// stepAttributes is the node row's attributes as a step reads them.
type stepAttributes struct {
	Issue      string     `json:"issue"`
	Project    string     `json:"project"`
	Generation int64      `json:"generation"`
	Task       string     `json:"task"`
	Step       int        `json:"step"`
	Kind       string     `json:"kind"`
	Role       string     `json:"role"`
	Type       string     `json:"type"`
	Agent      string     `json:"agent"`
	Session    string     `json:"session"`
	Model      string     `json:"model"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	Result     string     `json:"result"`
	ReturnedTo *int       `json:"returned_to"`
	Report     string     `json:"report"`
}

// stepFrom reads one node row's attributes as the step they hold.
func stepFrom(id string, attributes []byte) (Step, error) {
	var held stepAttributes
	if err := json.Unmarshal(attributes, &held); err != nil {
		return Step{}, fmt.Errorf("decode the attributes of step %s: %w", id, err)
	}
	return Step{
		ID: id, Issue: held.Issue, Project: held.Project, Task: held.Task, Kind: held.Kind,
		Role: held.Role, Type: held.Type, Agent: held.Agent, Session: held.Session, Model: held.Model,
		Result: held.Result, Report: held.Report, Generation: held.Generation, Place: held.Step,
		ReturnedTo: held.ReturnedTo, StartedAt: held.StartedAt, EndedAt: held.EndedAt,
	}, nil
}
