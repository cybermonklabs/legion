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

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The forms of a step's names. A task id is a plan's slug, `default` for an issue whose owner gave
// no plan (taskRule); a role and a type are a word of lowercase letters, digits and hyphens
// (roleRule), and so is the role a plan's step names before its key. The agent, the session and
// the model are printable ASCII with no space, at most 128 characters
// (contracts.StepAgentTextPattern), since `dispatch steps` joins a record's fields with spaces. So
// are the set model and each model of the answers. An experiment and its variant are a name the
// daemon's settings declare, under the daemon's rule for those names.
var (
	taskForm      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	roleForm      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	agentTextForm = regexp.MustCompile(contracts.StepAgentTextPattern)
	nameForm      = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// taskRule and roleRule are taskForm and roleForm as a refusal states them.
const (
	taskRule = "lowercase letters, digits and hyphens, starting with a letter or a digit, at most 64"
	roleRule = "a lowercase letter, then lowercase letters, digits and hyphens, at most 32"
)

var (
	stepKinds   = []string{StepKindStep, StepKindRetro, StepKindMerge}
	stepResults = []string{ResultPassed, ResultFailed, ResultStopped}
)

// RunsChannel is the Postgres notification channel a write of a run signals its issue on, with the
// issue's key as its payload. A run's write appends no event (NoEvent), so no event a page follows
// comes with a run: the live hub listens on this channel to mark an open view of the issue's Legion
// state owed, whose body carries the count IssueRunsRevision reads. PutStep sends it inside the
// write's own transaction, so Postgres delivers it at the commit, to every Dispatch process that
// listens, and never for a write rolled back; and only where the write appended a tuple, so a write
// sent again, which sets nothing, sends nothing, and each signal comes with a rise of that count. A
// plan's write (PutPlan) sends nothing: a plan is no run.
const RunsChannel = "dispatch_workflow_runs"

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
// sets the report states the `reports` relation to that handoff. A type and a rating are judged
// against the kind the write names and against the kind the node holds (typeOfKind,
// ratingOfKind), so a run made as a step never takes a rating, whatever kind a later write calls
// it.
//
// A write that appends a tuple signals its issue on RunsChannel inside tx, so the signal goes at
// the commit and a write rolled back sends none; a write that sets nothing sends nothing.
func PutStep(ctx context.Context, tx pgx.Tx, write StepWrite, actor model.Actor, at time.Time) (Step, error) {
	id, stated, err := write.attributes()
	if err != nil {
		return Step{}, err
	}
	missing := write.missingIdentity()
	written, appended, err := putNode(ctx, tx, nodeWrite{
		kind: KindStep, id: id, issue: write.Issue, stated: stated, makes: len(missing) == 0,
		actor: actor, at: at,
		check: func(held map[string]json.RawMessage) error {
			if write.Type == nil && write.Rating == nil {
				return nil
			}
			kind, err := heldString(held, AttributeKind)
			if err != nil {
				return fmt.Errorf("decode the kind of step %s: %w", id, err)
			}
			if write.Type != nil {
				if err := typeOfKind(kind, *write.Type); err != nil {
					return err
				}
			}
			if write.Rating != nil {
				if err := ratingOfKind(id, kind); err != nil {
					return err
				}
			}
			return nil
		},
		// A run's result and the step it returned to are one fact, so the write takes returned_to
		// only where the node holds no result. attributes() admits returned_to only beside a
		// failed or stopped result, so the write that states it states a result too.
		sets: func(held map[string]json.RawMessage, name string) bool {
			if name != AttributeReturnedTo {
				return true
			}
			_, ended := held[AttributeResult]
			return !ended
		},
		relations: func(setting map[string]string) []tuple {
			if report, sets := setting[AttributeReport]; sets {
				return []tuple{{kind: RelationReports, toKind: KindHandoff, toID: report}}
			}
			return nil
		},
	})
	var other *nodeOfAnotherIssue
	switch {
	case errors.Is(err, errNoNode):
		return Step{}, refusal(ErrStepInput, "step %s holds no node yet, and this write, which would make it, does not name its %s: the first write of a step names its task, kind, step, role and generation",
			id, strings.Join(missing, ", "))
	case errors.As(err, &other):
		return Step{}, fmt.Errorf("%w: step %s is part of %s, and this write names %s", ErrStepOtherIssue, id, other.issue, write.Issue)
	case err != nil:
		return Step{}, err
	}
	if appended {
		if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, RunsChannel, write.Issue); err != nil {
			return Step{}, fmt.Errorf("signal the runs of %s: %w", write.Issue, err)
		}
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
	actor           model.Actor
	at              time.Time
	// check judges the write against the attributes the node holds before it ({} for the node
	// this write makes); a refusal is answered as it is. Nil takes every write.
	check func(held map[string]json.RawMessage) error
	// replaces judges the write against the attributes the node holds before it and answers the
	// attributes of its own it sets whether the node holds them or not, one fact the write
	// replaces whole; an error is answered as it is. Nil, or an answer of none, replaces nothing.
	replaces func(held map[string]json.RawMessage) ([]string, error)
	// sets says whether the write sets the attribute called name, which the node does not hold
	// yet or the write replaces, given the attributes the node holds before it. Nil sets each.
	sets func(held map[string]json.RawMessage, name string) bool
	// relations are the relation tuples the write states beside its attributes and its
	// `part_of`, from the attributes it sets: each one's text, as its value tuple holds it, by its
	// name. Nil states none.
	relations func(setting map[string]string) []tuple
}

// errNoNode is a write that finds no node and does not name the identity that would make one; the
// caller refuses it in its own words.
var errNoNode = errors.New("no node, and the write does not name its identity")

// nodeOfAnotherIssue is a write of an id whose node is part of issue, not of the write's issue;
// the caller refuses it in its own words.
type nodeOfAnotherIssue struct{ issue string }

func (other *nodeOfAnotherIssue) Error() string { return "the node is part of " + other.issue }

// putNode is the one write of a node, a run's or a plan's, inside the caller's transaction, and
// answers the node's attributes as they stand after it, and whether it appended a tuple, which a
// write that sets nothing does not. It sets each attribute the write states that the node does not
// hold yet or that the write replaces (nodeWrite.replaces), where the write sets it
// (nodeWrite.sets), appending one value tuple for each, and leaves every other the node holds,
// appending nothing for it; the write that makes the node states its `part_of` relation to the
// issue. A refusal comes before anything is moved, but the node a refused write made is the
// caller's to roll back with its transaction.
//
// Two writes of one id at one time take turns: the insert that makes the node comes before the
// read of what it holds, and that read locks the row (for update), so the second write waits for
// the first to commit and then reads everything the first wrote.
func putNode(ctx context.Context, tx pgx.Tx, write nodeWrite) ([]byte, bool, error) {
	writer, err := json.Marshal(write.actor)
	if err != nil {
		return nil, false, fmt.Errorf("encode the writer of %s %s: %w", write.kind, write.id, err)
	}
	made := false
	if write.makes {
		var inserted string
		err := tx.QueryRow(ctx, `
			insert into workflow_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			values ($1, $2, $1, '`+VisibilityProject+`', '`+StateLive+`', '{}', $3, $4, $5)
			on conflict (kind, id) do nothing
			returning id
		`, write.kind, write.id, write.at, writer, NoEvent).Scan(&inserted)
		switch {
		case err == nil:
			made = true
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, false, fmt.Errorf("write %s %s: %w", write.kind, write.id, err)
		}
	}

	var heldText []byte
	err = tx.QueryRow(ctx, `
		select attributes from workflow_nodes where kind = $1 and id = $2 for update
	`, write.kind, write.id).Scan(&heldText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, errNoNode
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s %s: %w", write.kind, write.id, err)
	}
	var held map[string]json.RawMessage
	if err := json.Unmarshal(heldText, &held); err != nil {
		return nil, false, fmt.Errorf("decode the attributes of %s %s: %w", write.kind, write.id, err)
	}
	if !made {
		issue, err := heldString(held, AttributeIssue)
		if err != nil {
			return nil, false, fmt.Errorf("decode the issue of %s %s: %w", write.kind, write.id, err)
		}
		if issue != write.issue {
			return nil, false, &nodeOfAnotherIssue{issue: issue}
		}
	}
	if write.check != nil {
		if err := write.check(held); err != nil {
			return nil, false, err
		}
	}
	var replaced []string
	if write.replaces != nil {
		if replaced, err = write.replaces(held); err != nil {
			return nil, false, err
		}
	}

	setting := map[string]any{}
	texts := map[string]string{}
	var tuples []tuple
	for _, attribute := range write.stated {
		if _, holds := held[attribute.name]; holds && !slices.Contains(replaced, attribute.name) {
			continue
		}
		if write.sets != nil && !write.sets(held, attribute.name) {
			continue
		}
		setting[attribute.name] = attribute.value
		texts[attribute.name] = attribute.text
		tuples = append(tuples, tuple{kind: attribute.name, toKind: KindValue, toID: attribute.text})
	}
	if made {
		tuples = append(tuples, tuple{kind: RelationPartOf, toKind: KindIssue, toID: write.issue})
	}
	if write.relations != nil {
		tuples = append(tuples, write.relations(texts)...)
	}
	if len(setting) == 0 {
		return heldText, false, nil
	}

	moved, err := json.Marshal(setting)
	if err != nil {
		return nil, false, fmt.Errorf("encode the attributes of %s %s: %w", write.kind, write.id, err)
	}
	var written []byte
	if err := tx.QueryRow(ctx, `
		update workflow_nodes set attributes = attributes || $3::jsonb
		where kind = $1 and id = $2
		returning attributes
	`, write.kind, write.id, moved).Scan(&written); err != nil {
		return nil, false, fmt.Errorf("write %s %s: %w", write.kind, write.id, err)
	}
	if err := appendTuples(ctx, tx, write.kind, write.id, tuples, writer, write.at); err != nil {
		return nil, false, fmt.Errorf("write %s %s: %w", write.kind, write.id, err)
	}
	return written, true, nil
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
// in. A text field that is empty states nothing; a ReturnedTo of 0 is place 0 and states it; a
// rating and its reason are stated together, of a retro, or neither is; an experiment and its
// variant are stated together, or neither is. An empty list of answers states a run whose agent
// made no call.
func (write StepWrite) attributes() (string, []attribute, error) {
	parsed, err := uuid.Parse(write.ID)
	if err != nil {
		return "", nil, refusal(ErrStepInput, "id %q is not a uuid: a step record's id is the uuid the daemon derives for the run", write.ID)
	}
	id := parsed.String()
	if strings.TrimSpace(write.Issue) == "" {
		return "", nil, refusal(ErrStepInput, "issue is required: the key of the issue step %s is part of", id)
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
			return refusal(ErrStepInput, "%s cannot be represented in UTC: its year must be between 0 and 9999", name)
		}
		formatted := utc.Format(time.RFC3339Nano)
		stated = append(stated, attribute{name, formatted, formatted})
		return nil
	}

	text(AttributeProject, write.Project)
	if write.Generation != nil {
		if *write.Generation < 1 {
			return "", nil, refusal(ErrStepInput, "generation %d is no run of an issue: an issue's generation counts from 1", *write.Generation)
		}
		number(AttributeGeneration, *write.Generation)
	}
	if task := text(AttributeTask, write.Task); task != "" && !taskForm.MatchString(task) {
		return "", nil, refusal(ErrStepInput, "task %q is not a task id: %s", task, taskRule)
	}
	kind := text(AttributeKind, write.Kind)
	if kind != "" && !slices.Contains(stepKinds, kind) {
		return "", nil, refusal(ErrStepInput, "kind %q is not a kind of step: one of %s", kind, strings.Join(stepKinds, ", "))
	}
	if write.Place != nil {
		place := *write.Place
		switch {
		case place < 0:
			return "", nil, refusal(ErrStepInput, "step %d is no place: a step's place is 0 or more", place)
		case kind != "" && !contracts.StepPlaceHolds(kind, place):
			if kind == StepKindStep {
				return "", nil, refusal(ErrStepInput, "step %d is no place of a step of kind step: a plan's steps count from 1", place)
			}
			return "", nil, refusal(ErrStepInput, "step %d is no place of a %s: no plan names it, so its step is 0", place, kind)
		}
		number(AttributeStep, int64(place))
	}
	if role := text(AttributeRole, write.Role); role != "" && !roleForm.MatchString(role) {
		return "", nil, refusal(ErrStepInput, "role %q is not a role: %s", role, roleRule)
	}
	typ := text(AttributeType, write.Type)
	if typ != "" && !roleForm.MatchString(typ) {
		return "", nil, refusal(ErrStepInput, "type %q is not a type of work: %s, or empty", typ, roleRule)
	}
	if err := typeOfKind(kind, typ); err != nil {
		return "", nil, err
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{AttributeAgent, write.Agent}, {AttributeSession, write.Session}, {AttributeModel, write.Model}, {AttributeSetModel, write.SetModel}} {
		if value := text(field.name, field.value); value != "" && !agentTextForm.MatchString(value) {
			return "", nil, refusal(ErrStepInput, "%s %q is not an agent's text: %s", field.name, value, contracts.StepAgentTextRule)
		}
	}
	if write.Answers != nil {
		answers, err := answersAttribute(*write.Answers)
		if err != nil {
			return "", nil, err
		}
		stated = append(stated, answers)
	}
	if err := moment(AttributeStartedAt, write.StartedAt); err != nil {
		return "", nil, err
	}

	result := text(AttributeResult, write.Result)
	if result != "" && !slices.Contains(stepResults, result) {
		return "", nil, refusal(ErrStepInput, "result %q is not a result: one of %s", result, strings.Join(stepResults, ", "))
	}
	if (result != "") != (write.EndedAt != nil) {
		if result == "" {
			return "", nil, refusal(ErrStepInput, "ended_at is written with the result of the run it ends, and this write names none")
		}
		return "", nil, refusal(ErrStepInput, "result %s is written with ended_at, the moment the run ended, and this write names none", result)
	}
	if err := moment(AttributeEndedAt, write.EndedAt); err != nil {
		return "", nil, err
	}
	if write.ReturnedTo != nil {
		returned := *write.ReturnedTo
		if returned < 0 {
			return "", nil, refusal(ErrStepInput, "returned_to %d is no place of a step", returned)
		}
		if result != ResultFailed && result != ResultStopped {
			return "", nil, refusal(ErrStepInput, "returned_to names the step a run returned its task to, which only a run that ended %s or %s does, and this write's result is %q", ResultFailed, ResultStopped, result)
		}
		number(AttributeReturnedTo, int64(returned))
	}
	if write.Report != nil && *write.Report != "" {
		report, err := uuid.Parse(*write.Report)
		if err != nil {
			return "", nil, refusal(ErrStepInput, "report %q is not a uuid: it is the id of the handoff the ending role's completion named", *write.Report)
		}
		canonical := report.String()
		stated = append(stated, attribute{AttributeReport, canonical, canonical})
	}
	rated, err := write.rating(id, kind)
	if err != nil {
		return "", nil, err
	}
	drawn, err := write.experiment()
	if err != nil {
		return "", nil, err
	}
	return id, append(append(stated, rated...), drawn...), nil
}

// answersAttribute checks the models that answered a run's agent against the rules of a step
// record's answers. It answers the attribute that holds them, in the order the write gives them.
// A record names at most contracts.StepAnswersMax models, each in the form of an agent's text,
// each once and with 1 call or more. Its value tuple holds the list as JSON text, an empty list as
// [], which is a run whose agent made no call.
func answersAttribute(answers []StepAnswer) (attribute, error) {
	if len(answers) > contracts.StepAnswersMax {
		return attribute{}, refusal(ErrStepInput, "%s holds %d entries, more than the %d a step record holds",
			AttributeAnswers, len(answers), contracts.StepAnswersMax)
	}
	named := make(map[string]bool, len(answers))
	for index, answer := range answers {
		entry := index + 1
		switch {
		case !agentTextForm.MatchString(answer.Model):
			return attribute{}, refusal(ErrStepInput, "%s entry %d names %q, which is not an agent's text: %s",
				AttributeAnswers, entry, answer.Model, contracts.StepAgentTextRule)
		case answer.Calls < 1:
			return attribute{}, refusal(ErrStepInput, "%s entry %d (%s) has %d calls: a model that answered has 1 or more",
				AttributeAnswers, entry, answer.Model, answer.Calls)
		case named[answer.Model]:
			return attribute{}, refusal(ErrStepInput, "%s names %s twice: each model is listed once with all its calls",
				AttributeAnswers, answer.Model)
		}
		named[answer.Model] = true
	}
	// A pointer to a nil list states the empty list, as a pointer to an empty one does, and JSON
	// writes a nil list as null.
	if answers == nil {
		answers = []StepAnswer{}
	}
	text, err := json.Marshal(answers)
	if err != nil {
		return attribute{}, fmt.Errorf("encode the answers: %w", err)
	}
	return attribute{AttributeAnswers, json.RawMessage(text), string(text)}, nil
}

// rating checks the rating a write states with its reason, the rating the agent that did a retro
// gave the plan, and answers their two attributes, or none where the write states no rating. The
// rating and its reason are held to the one rule of a rating (contracts.CheckPlanRating), which
// the daemon's completion route holds a retro's completion to as well, through steprecord, so
// every rating the daemon takes is one Dispatch takes, and kind, where the write names one, is a
// retro (ratingOfKind).
func (write StepWrite) rating(id, kind string) ([]attribute, error) {
	if write.Rating == nil {
		return nil, nil
	}
	rating, reason := write.Rating.Rating, write.Rating.Reason
	switch contracts.CheckPlanRating(rating, reason) {
	case contracts.PlanRatingOffScale:
		return nil, refusal(ErrStepInput, "rating %d is no rating: a plan is rated from %d (the plan cost the work) to %d (the plan fit the work)", rating, contracts.PlanRatingLeast, contracts.PlanRatingMost)
	case contracts.PlanReasonBlank:
		return nil, refusal(ErrStepInput, "rating_reason is left out or blank: a rating carries one sentence of why")
	case contracts.PlanReasonLong:
		return nil, refusal(ErrStepInput, "rating_reason holds %d characters, more than the %d a reason holds", utf8.RuneCountInString(reason), contracts.PlanReasonMax)
	}
	if err := ratingOfKind(id, kind); err != nil {
		return nil, err
	}
	return []attribute{
		{AttributeRating, int64(rating), strconv.Itoa(rating)},
		{AttributeRatingReason, reason, reason},
	}, nil
}

// experiment checks the experiment and the variant a write states, and answers their two
// attributes, or none where the write states no experiment. Both are stated, and each is a name of
// the daemon's rule (nameForm).
func (write StepWrite) experiment() ([]attribute, error) {
	if write.Experiment == nil {
		return nil, nil
	}
	experiment, variant := write.Experiment.Experiment, write.Experiment.Variant
	if experiment == "" || variant == "" {
		return nil, refusal(ErrStepInput, "experiment and variant are stated together or not at all: a run names the experiment its issue was drawn into with the variant drawn")
	}
	if !nameForm.MatchString(experiment) {
		return nil, refusal(ErrStepInput, "experiment %q must match %s", experiment, nameForm)
	}
	if !nameForm.MatchString(variant) {
		return nil, refusal(ErrStepInput, "variant %q must match %s", variant, nameForm)
	}
	return []attribute{
		{AttributeExperiment, experiment, experiment},
		{AttributeVariant, variant, variant},
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

// typeOfKind refuses a type for a run of kind retro or merge: no plan names either, so neither
// has a type, as neither has a place but 0.
func typeOfKind(kind, typ string) error {
	if typ != "" && kind != "" && kind != StepKindStep {
		return refusal(ErrStepInput, "type %q is no type of a %s: no plan names it, so it has none", typ, kind)
	}
	return nil
}

// ratingOfKind refuses a rating for run id of kind step or merge: a rating is the rating a retro
// gives its plan.
func ratingOfKind(id, kind string) error {
	if kind != "" && kind != StepKindRetro {
		return refusal(ErrStepInput, "rating is the rating a retro gives its plan, and step %s is a run of kind %s", id, kind)
	}
	return nil
}

// refusal is a write refused as sentinel, its text the rule format and args state.
func refusal(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...))
}

// heldString is the text attribute name of a node's held attributes, "" where the node holds none.
func heldString(held map[string]json.RawMessage, name string) (string, error) {
	var text string
	if raw, holds := held[name]; holds {
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
	}
	return text, nil
}

// tuple is one fact a write states about its node: an attribute, whose toKind is KindValue and
// whose toID is the value, or a relation to the node toKind and toID address.
type tuple struct {
	kind, toKind, toID string
}

// appendTuples appends every tuple of one write of the node fromKind fromID in one statement, in
// the order the write states them, which is the order their seq is drawn in. Every tuple of the
// write has one writer, one moment and no event, and asserts its fact.
func appendTuples(ctx context.Context, tx pgx.Tx, fromKind, fromID string, tuples []tuple, writer []byte, at time.Time) error {
	kinds := make([]string, len(tuples))
	toKinds := make([]string, len(tuples))
	toIDs := make([]string, len(tuples))
	for index, stated := range tuples {
		kinds[index], toKinds[index], toIDs[index] = stated.kind, stated.toKind, stated.toID
	}
	if _, err := tx.Exec(ctx, `
		insert into workflow_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select $7, $8, stated.kind, stated.to_kind, stated.to_id, $4, $5::jsonb, $6, true
		from unnest($1::text[], $2::text[], $3::text[]) with ordinality as stated(kind, to_kind, to_id, place)
		order by stated.place
	`, kinds, toKinds, toIDs, at, writer, NoEvent, fromKind, fromID); err != nil {
		return fmt.Errorf("append %s tuples: %w", fromKind, err)
	}
	return nil
}

// stepAttributes is the node row's attributes as a step reads them. Answers is nil where the node
// holds none, and points to an empty list where it holds [].
type stepAttributes struct {
	Issue        string        `json:"issue"`
	Project      string        `json:"project"`
	Generation   int64         `json:"generation"`
	Task         string        `json:"task"`
	Step         int           `json:"step"`
	Kind         string        `json:"kind"`
	Role         string        `json:"role"`
	Type         string        `json:"type"`
	Agent        string        `json:"agent"`
	Session      string        `json:"session"`
	Model        string        `json:"model"`
	SetModel     string        `json:"set_model"`
	Answers      *[]StepAnswer `json:"answers"`
	StartedAt    *time.Time    `json:"started_at"`
	EndedAt      *time.Time    `json:"ended_at"`
	Result       string        `json:"result"`
	ReturnedTo   *int          `json:"returned_to"`
	Report       string        `json:"report"`
	Rating       int           `json:"rating"`
	RatingReason string        `json:"rating_reason"`
	Experiment   string        `json:"experiment"`
	Variant      string        `json:"variant"`
}

// stepFrom reads one node row's attributes as the step they hold. A rating of 0 is a run that
// holds none, since a write sets a rating and its reason together or neither, and an experiment of
// "" is a run that holds none, since a write sets an experiment and its variant together or
// neither.
func stepFrom(id string, attributes []byte) (Step, error) {
	var held stepAttributes
	if err := json.Unmarshal(attributes, &held); err != nil {
		return Step{}, fmt.Errorf("decode the attributes of step %s: %w", id, err)
	}
	step := Step{
		ID: id, Issue: held.Issue, Project: held.Project, Task: held.Task, Kind: held.Kind,
		Role: held.Role, Type: held.Type, Agent: held.Agent, Session: held.Session, Model: held.Model,
		SetModel: held.SetModel, Answers: held.Answers, Result: held.Result, Report: held.Report,
		Generation: held.Generation, Place: held.Step, ReturnedTo: held.ReturnedTo,
		StartedAt: held.StartedAt, EndedAt: held.EndedAt,
	}
	if held.Rating != 0 {
		step.Rating = &StepRating{Rating: held.Rating, Reason: held.RatingReason}
	}
	if held.Experiment != "" {
		step.Experiment = &StepExperiment{Experiment: held.Experiment, Variant: held.Variant}
	}
	return step, nil
}
