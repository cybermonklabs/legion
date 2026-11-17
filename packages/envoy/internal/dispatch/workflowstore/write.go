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
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The forms of a step's names. A task id is a plan's slug, `default` for an issue whose owner gave
// no plan; a role and a type are a word of lowercase letters, digits and hyphens.
var (
	taskForm = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	roleForm = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

var (
	stepKinds   = []string{StepKindStep, StepKindRetro, StepKindMerge}
	stepResults = []string{ResultPassed, ResultFailed, ResultStopped}
)

// PutStep writes one run of one step inside the caller's transaction and answers its node as it
// stands after the write. It sets each field the write states that the node does not hold yet,
// appending one value tuple for each, and leaves each field the node holds as it is, appending
// nothing for it. So a start and an end merge in either order, a write that runs again changes
// nothing, and a later end never replaces an earlier one.
//
// The write that finds no node makes it, whichever of the two it is, and states the `part_of`
// relation to the issue; it must name the step's whole identity (task, kind, step, role and
// generation), or it is refused ErrStepInput and writes nothing. The write that sets the report
// states the `reports` relation to that handoff. A rating is judged against the kind the node
// holds, not the one the write names, so a run made as a step never takes one.
func PutStep(ctx context.Context, tx pgx.Tx, write StepWrite, actor model.Actor, at time.Time) (Step, error) {
	id, stated, err := write.attributes()
	if err != nil {
		return Step{}, err
	}
	writer, err := json.Marshal(actor)
	if err != nil {
		return Step{}, fmt.Errorf("encode the writer of step %s: %w", id, err)
	}
	missing := write.missingIdentity()
	written, err := putNode(ctx, tx, nodeWrite{
		kind: KindStep, id: id, issue: write.Issue, stated: stated, makes: len(missing) == 0,
		writer: writer, at: at,
		check: func(held map[string]json.RawMessage) error {
			if write.Rating == nil {
				return nil
			}
			var kind string
			if raw, holds := held[AttributeKind]; holds {
				if err := json.Unmarshal(raw, &kind); err != nil {
					return fmt.Errorf("decode the kind of step %s: %w", id, err)
				}
			}
			if kind != "" && kind != StepKindRetro {
				return inputError("rating is the rating a retro gives its plan, and step %s is a run of kind %s", id, kind)
			}
			return nil
		},
		relations: func(setting map[string]any) []tuple {
			if report, sets := setting[AttributeReport]; sets {
				return []tuple{{fromID: id, kind: RelationReports, toKind: KindHandoff, toID: report.(string)}}
			}
			return nil
		},
	})
	var other *nodeOfAnotherIssue
	switch {
	case errors.Is(err, errNoNode):
		return Step{}, inputError(
			"step %s holds no node yet, and this write, which would make it, does not name its %s: the first write of a step names its task, kind, step, role and generation",
			id, strings.Join(missing, ", "))
	case errors.As(err, &other):
		return Step{}, fmt.Errorf("%w: step %s is part of %s, and this write names %s", ErrStepOtherIssue, id, other.issue, write.Issue)
	case err != nil:
		return Step{}, err
	}
	return stepFrom(id, written)
}

// nodeWrite is one write of one node of the store: its kind, its id and the issue it is part of,
// the attributes it states, whether it names the node's whole identity and so may make it, and who
// writes it when.
type nodeWrite struct {
	kind, id, issue string
	stated          []attribute
	makes           bool
	writer          []byte
	at              time.Time
	// check judges the write against the attributes the node holds before it ({} for the node
	// this write makes); a refusal is answered as it is. Nil takes every write.
	check func(held map[string]json.RawMessage) error
	// relations are the relation tuples the write states beside its attributes and its
	// `part_of`, from the attributes it sets. Nil states none.
	relations func(setting map[string]any) []tuple
}

// errNoNode is a write that finds no node and does not name the identity that would make one; the
// caller refuses it in its own words.
var errNoNode = errors.New("no node, and the write does not name its identity")

// nodeOfAnotherIssue is a write of an id whose node is part of issue, not of the write's issue;
// the caller refuses it in its own words.
type nodeOfAnotherIssue struct{ issue string }

func (other *nodeOfAnotherIssue) Error() string { return "the node is part of " + other.issue }

// putNode is the one write of a node, a run's or a plan's, inside the caller's transaction, and
// answers the node's attributes as they stand after it. It sets each attribute the write states
// that the node does not hold yet, appending one value tuple for each, and leaves each the node
// holds, appending nothing for it; the write that makes the node states its `part_of` relation to
// the issue. A refusal comes before anything is moved, but the node a refused write made is the
// caller's to roll back with its transaction.
//
// Two writes of one id at one time take turns: the insert that makes the node comes before the
// read of what it holds, and that read locks the row (for update), so the second write waits for
// the first to commit and then reads everything the first wrote.
func putNode(ctx context.Context, tx pgx.Tx, write nodeWrite) ([]byte, error) {
	made := false
	if write.makes {
		var inserted string
		err := tx.QueryRow(ctx, `
			insert into workflow_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			values ($1, $2, $1, '`+VisibilityProject+`', '`+StateLive+`', '{}', $3, $4, $5)
			on conflict (kind, id) do nothing
			returning id
		`, write.kind, write.id, write.at, write.writer, NoEvent).Scan(&inserted)
		switch {
		case err == nil:
			made = true
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, fmt.Errorf("write %s %s: %w", write.kind, write.id, err)
		}
	}

	var heldText []byte
	err := tx.QueryRow(ctx, `
		select attributes from workflow_nodes where kind = $1 and id = $2 for update
	`, write.kind, write.id).Scan(&heldText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoNode
	}
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", write.kind, write.id, err)
	}
	var held map[string]json.RawMessage
	if err := json.Unmarshal(heldText, &held); err != nil {
		return nil, fmt.Errorf("decode the attributes of %s %s: %w", write.kind, write.id, err)
	}
	if !made {
		var issue string
		if raw, holds := held[AttributeIssue]; holds {
			if err := json.Unmarshal(raw, &issue); err != nil {
				return nil, fmt.Errorf("decode the issue of %s %s: %w", write.kind, write.id, err)
			}
		}
		if issue != write.issue {
			return nil, &nodeOfAnotherIssue{issue: issue}
		}
	}
	if write.check != nil {
		if err := write.check(held); err != nil {
			return nil, err
		}
	}

	setting := map[string]any{}
	var tuples []tuple
	for _, attribute := range write.stated {
		if _, holds := held[attribute.name]; holds {
			continue
		}
		setting[attribute.name] = attribute.value
		tuples = append(tuples, tuple{fromID: write.id, kind: attribute.name, toKind: KindValue, toID: attribute.text})
	}
	if made {
		tuples = append(tuples, tuple{fromID: write.id, kind: RelationPartOf, toKind: KindIssue, toID: write.issue})
	}
	if write.relations != nil {
		tuples = append(tuples, write.relations(setting)...)
	}
	if len(setting) == 0 {
		return heldText, nil
	}

	moved, err := json.Marshal(setting)
	if err != nil {
		return nil, fmt.Errorf("encode the attributes of %s %s: %w", write.kind, write.id, err)
	}
	var written []byte
	if err := tx.QueryRow(ctx, `
		update workflow_nodes set attributes = attributes || $3::jsonb
		where kind = $1 and id = $2
		returning attributes
	`, write.kind, write.id, moved).Scan(&written); err != nil {
		return nil, fmt.Errorf("write %s %s: %w", write.kind, write.id, err)
	}
	if err := appendTuples(ctx, tx, write.kind, tuples, write.writer, write.at); err != nil {
		return nil, fmt.Errorf("write %s %s: %w", write.kind, write.id, err)
	}
	return written, nil
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
// in. A text field that is empty states nothing, and a ReturnedTo of 0 states nothing; a rating
// and its reason are stated together, of a retro, or neither is.
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
	moment := func(name string, value *time.Time) {
		if value == nil {
			return
		}
		formatted := value.UTC().Format(time.RFC3339Nano)
		stated = append(stated, attribute{name, formatted, formatted})
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
		case kind == StepKindStep && place < 1:
			return "", nil, inputError("step %d is no place of a step of kind step: a plan's steps count from 1", place)
		case kind != "" && kind != StepKindStep && place != 0:
			return "", nil, inputError("step %d is no place of a %s: no plan names it, so its step is 0", place, kind)
		}
		number(AttributeStep, int64(place))
	}
	if role := text(AttributeRole, write.Role); role != "" && !roleForm.MatchString(role) {
		return "", nil, inputError("role %q is not a role: a lowercase letter, then lowercase letters, digits and hyphens, at most 32", role)
	}
	if typ := text(AttributeType, write.Type); typ != "" && !roleForm.MatchString(typ) {
		return "", nil, inputError("type %q is not a type of work: a lowercase letter, then lowercase letters, digits and hyphens, at most 32, or empty", typ)
	}
	text(AttributeAgent, write.Agent)
	text(AttributeSession, write.Session)
	text(AttributeModel, write.Model)
	moment(AttributeStartedAt, write.StartedAt)

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
	moment(AttributeEndedAt, write.EndedAt)
	if write.ReturnedTo != nil && *write.ReturnedTo != 0 {
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
	rated, err := write.rating(kind)
	if err != nil {
		return "", nil, err
	}
	return id, append(stated, rated...), nil
}

// rating checks the rating a write states and its reason, the rating the agent that did a retro
// gave the plan, and answers their two attributes, or none where the write states neither. They
// come together; the rating is 1 to 5, the reason is not blank and holds at most RatingReasonMost
// code points, and kind, where the write names one, is a retro.
func (write StepWrite) rating(kind string) ([]attribute, error) {
	reasoned := write.RatingReason != nil && *write.RatingReason != ""
	switch {
	case write.Rating == nil && !reasoned:
		return nil, nil
	case write.Rating == nil:
		return nil, inputError("rating and rating_reason are written together, and this write names rating_reason and no rating")
	case !reasoned:
		return nil, inputError("rating and rating_reason are written together, and this write names rating %d and no rating_reason", *write.Rating)
	}
	rating, reason := *write.Rating, *write.RatingReason
	if rating < 1 || rating > 5 {
		return nil, inputError("rating %d is no rating: a plan is rated from 1 (the plan cost the work) to 5 (the plan fit the work)", rating)
	}
	if strings.TrimSpace(reason) == "" {
		return nil, inputError("rating_reason is blank: a rating carries one sentence of why")
	}
	if length := utf8.RuneCountInString(reason); length > RatingReasonMost {
		return nil, inputError("rating_reason holds %d characters, more than the %d a reason holds", length, RatingReasonMost)
	}
	if kind != "" && kind != StepKindRetro {
		return nil, inputError("rating is the rating a retro gives its plan, and this write is of a run of kind %s", kind)
	}
	return []attribute{
		{AttributeRating, int64(rating), strconv.Itoa(rating)},
		{AttributeRatingReason, reason, reason},
	}, nil
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

func inputError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrStepInput, fmt.Sprintf(format, args...))
}

// tuple is one fact a write states about one node: an attribute, whose toKind is KindValue and
// whose toID is the value, or a relation to the node toKind and toID address.
type tuple struct {
	fromID, kind, toKind, toID string
}

// appendTuples appends every tuple of one write of a node of fromKind in one statement, in the
// order the write states them, which is the order their seq is drawn in. Every tuple of the write
// has one writer, one moment and no event, and asserts its fact.
func appendTuples(ctx context.Context, tx pgx.Tx, fromKind string, tuples []tuple, writer []byte, at time.Time) error {
	if len(tuples) == 0 {
		return nil
	}
	fromIDs := make([]string, len(tuples))
	kinds := make([]string, len(tuples))
	toKinds := make([]string, len(tuples))
	toIDs := make([]string, len(tuples))
	for index, stated := range tuples {
		fromIDs[index], kinds[index], toKinds[index], toIDs[index] = stated.fromID, stated.kind, stated.toKind, stated.toID
	}
	if _, err := tx.Exec(ctx, `
		insert into workflow_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select $8, stated.from_id, stated.kind, stated.to_kind, stated.to_id, $5, $6::jsonb, $7, true
		from unnest($1::text[], $2::text[], $3::text[], $4::text[]) with ordinality as stated(from_id, kind, to_kind, to_id, place)
		order by stated.place
	`, fromIDs, kinds, toKinds, toIDs, at, writer, NoEvent, fromKind); err != nil {
		return fmt.Errorf("append %s tuples: %w", fromKind, err)
	}
	return nil
}

// stepAttributes is the node row's attributes as a step reads them.
type stepAttributes struct {
	Issue        string     `json:"issue"`
	Project      string     `json:"project"`
	Generation   int64      `json:"generation"`
	Task         string     `json:"task"`
	Step         int        `json:"step"`
	Kind         string     `json:"kind"`
	Role         string     `json:"role"`
	Type         string     `json:"type"`
	Agent        string     `json:"agent"`
	Session      string     `json:"session"`
	Model        string     `json:"model"`
	StartedAt    *time.Time `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at"`
	Result       string     `json:"result"`
	ReturnedTo   int        `json:"returned_to"`
	Report       string     `json:"report"`
	Rating       int        `json:"rating"`
	RatingReason string     `json:"rating_reason"`
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
		Result: held.Result, Report: held.Report, RatingReason: held.RatingReason, Generation: held.Generation,
		Place: held.Step, ReturnedTo: held.ReturnedTo, Rating: held.Rating, StartedAt: held.StartedAt,
		EndedAt: held.EndedAt,
	}, nil
}
