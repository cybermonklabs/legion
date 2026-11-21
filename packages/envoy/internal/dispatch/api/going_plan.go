package api

// going_plan.go is a command-bar run's plan (R4): the changes its model proposes, each a row the
// person approves before anything of it is written. The model proposes through a plan call of the
// agent's (agentwire.KindPlan): the server checks each change against its tool spec, reads as the
// person the item it changes - the value now, the holder of its issue's claim - and keeps the plan
// (going_plans, going_plan_rows), whose rows wait, while the run waits for its person. The person
// reads it at GET /api/v1/going/runs/{id}/plan, which reads each waiting row's item again, and
// answers it: approve, with each row's tick and each value they edited; revise, with words the run
// takes to propose another plan; or reject. An approval moves the run's grant to the credential
// that approved, saves the person's answer as a step of the run (agentwire.PersonStep) and makes
// the run ready, and the agent runs the approved rows in order (going_plan_run.go) before its next
// model call. Only the run's own person reads or answers its plan; any other caller is answered
// the run's 404.
//
// The states of a plan (going_plans.state) follow from its rows: waiting while a row waits for the
// person and none is approved or running, approved while an approved row has not started, running
// while a row's write is under way, done once no row waits, and rejected once the person rejected
// it or revised it. A row is waiting (shown, or waiting again after its item changed), approved,
// unticked, running, done, failed or skipped.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The plan routes' own refusals.
const (
	codeGoingPlanNotFound   = "GOING_PLAN_NOT_FOUND"
	codeGoingPlanNotWaiting = "GOING_PLAN_NOT_WAITING"
	codeGoingPlanValues     = "GOING_PLAN_VALUES"
	codeGoingPlanInput      = "GOING_PLAN_INPUT"
	// codeGoingPlanChanged refuses an approval of a plan that changed after the page that sends
	// it read it (goingPlanApproveInput.Version).
	codeGoingPlanChanged = "GOING_PLAN_CHANGED"
)

// goingPlanMostRows is the most changes one plan holds.
const goingPlanMostRows = 50

// goingReviseMostRunes is the most characters the words of a revision hold.
const goingReviseMostRunes = 4000

// goingPlanRejected is the stop reason of a run whose person rejected its plan: it ends done, and
// nothing of the plan ran.
const goingPlanRejected = "PLAN_REJECTED: the person rejected the plan, and nothing of it ran"

// goingFramePlan is the frame a run's events carry each time its plan is made or answered, which
// tells the bar to read the plan again.
const goingFramePlan = "plan"

// goingRowShown is what a plan shows the person of one row's item (going_plan_rows.shown), as the
// server last read it as the person: the item, the issue it is or belongs to, the fields the
// change writes and their value now, the holder of the issue's claim, and why the row waits for
// the person in every mode (goingRowWaits). Changed is a row that waits again because its item
// changed after the person saw it. Token is a document's token as that read answered it
// (goingObservation.token), which the undo of the row's edit holds to the token the edit's route
// answered it found, to know the content read is the content the edit replaced.
type goingRowShown struct {
	Item    string          `json:"item,omitempty"`
	Issue   string          `json:"issue,omitempty"`
	Fields  []string        `json:"fields,omitempty"`
	Now     json.RawMessage `json:"now,omitempty"`
	Token   string          `json:"token,omitempty"`
	Claim   *goingHolder    `json:"claim,omitempty"`
	Waits   []string        `json:"waits,omitempty"`
	Changed bool            `json:"changed,omitempty"`
}

// same reports whether other reads the item as shown does: the same value now and the same holder
// of its claim.
func (shown goingRowShown) same(other goingRowShown) bool {
	return goingJSONEqual(shown.Now, other.Now) && reflect.DeepEqual(shown.Claim, other.Claim)
}

// unchanged reports whether other shows all that shown does: the same item, issue, fields, value,
// claim, reasons to wait and mark of a row that waits again.
func (shown goingRowShown) unchanged(other goingRowShown) bool {
	return shown.same(other) && shown.Item == other.Item && shown.Issue == other.Issue && shown.Changed == other.Changed &&
		slices.Equal(shown.Fields, other.Fields) && slices.Equal(shown.Waits, other.Waits)
}

// goingRowApproval is what let a row run (going_plan_rows.approval): the person, with any values
// they edited, or Auto-review (By), and the read of its item that approval was made over (Seen),
// which the row's item is read against again before its write. Route is the route the create of a
// new-issue run carries beside its arguments (going_new_issue.go), "" for every other row.
type goingRowApproval struct {
	By     string         `json:"by"`
	Edited map[string]any `json:"edited,omitempty"`
	Seen   goingRowShown  `json:"seen"`
	Route  string         `json:"route,omitempty"`
}

// Who approves a row (goingRowApproval.By): the person, by an approval of the plan or by the
// Enter that started a new-issue run, or Auto-review.
const (
	goingApprovedByPerson     = "person"
	goingApprovedByAutoReview = "auto_review"
)

// goingRowRecord is what became of a row's write (going_plan_rows.result): the step and call
// whose key ran it, which a second try of that key is held to, how its upload is sent where it is
// one (goingRowUpload), the status its route answered, or the code and words of its refusal, and,
// for a row that sends a message, what became of it (goingRowDelivery). Resumes is the key of
// each earlier try of the row whose call landed partly before the row waited for its person
// again, oldest first: the row's next try takes over what the newest landed, matched by each
// request's digest, and sends only the rest (AgentListener.adoptLandings), and Undo reads each
// key's landings as the row's (goingLandedWrites), so what landed stays the row's whatever the
// person answers it with. An answer of the row keeps it, and so does every record after.
type goingRowRecord struct {
	Step     int             `json:"step"`
	Call     int             `json:"call"`
	Upload   *goingRowUpload `json:"upload,omitempty"`
	Status   int             `json:"status,omitempty"`
	Code     string          `json:"code,omitempty"`
	Message  string          `json:"message,omitempty"`
	Delivery string          `json:"delivery,omitempty"`
	Resumes  []goingRowKey   `json:"resumes,omitempty"`
}

// goingRowKey is the call key of a try of a row, within its run: the step and the call.
type goingRowKey struct {
	Step int `json:"step"`
	Call int `json:"call"`
}

// goingRowUpload is how a row's upload is sent, decided once under its call's key before any of
// it is sent (AgentListener.rowUpload), so every try of that key sends the very same request: over
// the document the upload's read before found (Over), as that document's next version naming the
// token of the version it replaces (Base, "" for a document that has none), or else as a create
// alone.
type goingRowUpload struct {
	Over bool   `json:"over"`
	Base string `json:"base,omitempty"`
}

// goingPlanRowStored is one row of a plan as the store holds it.
type goingPlanRowStored struct {
	row       int
	tool      string
	arguments json.RawMessage
	needs     []int
	state     string
	shown     goingRowShown
	approval  *goingRowApproval
	result    *goingRowRecord
}

// goingPlanStored is a run's plan as the store holds it.
type goingPlanStored struct {
	plan    int
	state   string
	summary string
	rows    []goingPlanRowStored
}

// goingPlanAnswer is a run's plan as its person reads it: its number within the run, its state,
// what it does, the run's mode, each row, and its version, which an approval names
// (goingPlanApproveInput.Version).
type goingPlanAnswer struct {
	Run     string         `json:"run"`
	Plan    int            `json:"plan"`
	State   string         `json:"state" enum:"waiting,approved,rejected,running,done"`
	Summary string         `json:"summary"`
	Mode    string         `json:"mode" enum:"ask,auto_review"`
	Rows    []goingPlanRow `json:"rows"`
	Version string         `json:"version"`
}

// goingPlanRow is one row of a plan as its person reads it: the change (the tool and the arguments
// it runs with, the person's edits included), the rows it needs, its state, its class, the item
// it changes with the fields it writes and their value now, the holder of the item's issue's
// claim, why it waits in every mode, whether it waits again because its item changed after the
// person saw it, and what its write answered.
type goingPlanRow struct {
	Row       int             `json:"row"`
	Tool      string          `json:"tool"`
	Class     string          `json:"class"`
	Arguments json.RawMessage `json:"arguments"`
	Needs     []int           `json:"needs"`
	State     string          `json:"state" enum:"waiting,approved,unticked,running,done,failed,skipped"`
	Item      string          `json:"item"`
	Issue     string          `json:"issue"`
	Fields    []string        `json:"fields"`
	Now       json.RawMessage `json:"now"`
	Claim     *goingHolder    `json:"claim"`
	Waits     []string        `json:"waits"`
	Changed   bool            `json:"changed"`
	Result    *goingRowResult `json:"result"`
}

// goingRowResult is what a row's write answered, as its person reads it: the status its route
// answered, or the code and words of the refusal that stopped it, and for a row that sends a
// message or a broadcast whether it was sent, queued (a delivery not yet confirmed included) or
// refused, null for any other row.
type goingRowResult struct {
	Status   int     `json:"status"`
	Code     string  `json:"code"`
	Message  string  `json:"message"`
	Delivery *string `json:"delivery" enum:"sent,queued,refused"`
}

// answer is the plan of run, in mode, as its person reads it.
func (p goingPlanStored) answer(run, mode string) goingPlanAnswer {
	answer := goingPlanAnswer{Run: run, Plan: p.plan, State: p.state, Summary: p.summary, Mode: mode, Rows: []goingPlanRow{}}
	for _, row := range p.rows {
		read := goingPlanRow{
			Row: row.row, Tool: row.tool, Class: string(row.class()), Arguments: row.arguments,
			Needs: append([]int{}, row.needs...), State: row.state, Item: row.shown.Item, Issue: row.shown.Issue,
			Fields: append([]string{}, row.shown.Fields...), Now: row.shown.Now, Claim: row.shown.Claim,
			Waits: append([]string{}, row.shown.Waits...), Changed: row.shown.Changed,
		}
		if len(read.Now) == 0 {
			read.Now = json.RawMessage("null")
		}
		if row.result != nil && (row.result.Status != 0 || row.result.Code != "") {
			read.Result = &goingRowResult{Status: row.result.Status, Code: row.result.Code, Message: row.result.Message}
			if row.result.Delivery != "" {
				read.Result.Delivery = &row.result.Delivery
			}
		}
		answer.Rows = append(answer.Rows, read)
	}
	answer.Version = goingPlanVersion(answer)
	return answer
}

// goingPlanVersion is the version of a plan as its person reads it: the hex SHA-256 of its state,
// its summary and its rows as answered, so it moves with whatever the page shows of the plan - a
// row read again with another value, a row that waits again, a row approved, unticked or run.
func goingPlanVersion(answer goingPlanAnswer) string {
	shown, _ := json.Marshal(struct {
		State   string         `json:"state"`
		Summary string         `json:"summary"`
		Rows    []goingPlanRow `json:"rows"`
	}{answer.State, answer.Summary, answer.Rows})
	digest := sha256.Sum256(shown)
	return hex.EncodeToString(digest[:])
}

// goingQuerier is what the plan's reads run on: the pool, or a transaction.
type goingQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// readGoingPlan is plan number of run with its rows, its latest plan where number is 0, and nil
// for a plan the run does not hold.
func readGoingPlan(ctx context.Context, q goingQuerier, run string, number int) (*goingPlanStored, error) {
	var plan goingPlanStored
	err := q.QueryRow(ctx, `
		select plan, state, summary from going_plans where run_id = $1::uuid and ($2 = 0 or plan = $2) order by plan desc limit 1
	`, run, number).Scan(&plan.plan, &plan.state, &plan.summary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the run's plan: %w", err)
	}
	rows, err := q.Query(ctx, `
		select `+goingPlanRowColumns+` from going_plan_rows where run_id = $1::uuid and plan = $2 order by row
	`, run, plan.plan)
	if err != nil {
		return nil, fmt.Errorf("read the plan's rows: %w", err)
	}
	plan.rows, err = pgx.CollectRows(rows, func(scanned pgx.CollectableRow) (goingPlanRowStored, error) {
		return scanGoingPlanRow(scanned)
	})
	if err != nil {
		return nil, fmt.Errorf("read the plan's rows: %w", err)
	}
	return &plan, nil
}

// goingPlanRowColumns is the columns of a plan's row as scanGoingPlanRow decodes them.
const goingPlanRowColumns = `row, coalesce(change->>'tool', ''), coalesce(change->'arguments', '{}'::jsonb), needs, state,
	coalesce(shown, '{}'::jsonb), approval, result`

// scanGoingPlanRow decodes one plan row read as goingPlanRowColumns, after the columns lead holds,
// which the statement selects before them.
func scanGoingPlanRow(scanned pgx.Row, lead ...any) (goingPlanRowStored, error) {
	var row goingPlanRowStored
	var arguments, shown, approval, result []byte
	if err := scanned.Scan(append(lead, &row.row, &row.tool, &arguments, &row.needs, &row.state, &shown, &approval, &result)...); err != nil {
		return row, err
	}
	row.arguments = arguments
	if err := json.Unmarshal(shown, &row.shown); err != nil {
		return row, fmt.Errorf("decode row %d's shown: %w", row.row, err)
	}
	if approval != nil {
		row.approval = &goingRowApproval{}
		if err := json.Unmarshal(approval, row.approval); err != nil {
			return row, fmt.Errorf("decode row %d's approval: %w", row.row, err)
		}
	}
	if result != nil {
		row.result = &goingRowRecord{}
		if err := json.Unmarshal(result, row.result); err != nil {
			return row, fmt.Errorf("decode row %d's result: %w", row.row, err)
		}
	}
	return row, nil
}

// goingPlanKey names one plan: its run and its number within the run.
type goingPlanKey struct {
	run  string
	plan int
}

// readGoingPlanRows is the rows of each plan keys names, in one read, each plan's in row order: a
// page of plans (listMyGoingPlans) reads every plan's rows in one round trip, as readGoingPlan
// reads one plan's.
func readGoingPlanRows(ctx context.Context, q goingQuerier, keys []goingPlanKey) (map[goingPlanKey][]goingPlanRowStored, error) {
	byPlan := make(map[goingPlanKey][]goingPlanRowStored, len(keys))
	if len(keys) == 0 {
		return byPlan, nil
	}
	runs, plans := make([]string, 0, len(keys)), make([]int, 0, len(keys))
	for _, key := range keys {
		runs, plans = append(runs, key.run), append(plans, key.plan)
	}
	rows, err := q.Query(ctx, `
		select p.run_id::text, p.plan, `+goingPlanRowColumns+`
		from unnest($1::uuid[], $2::int[]) k (run_id, plan)
		join going_plan_rows p on p.run_id = k.run_id and p.plan = k.plan
		order by p.run_id, p.plan, p.row
	`, runs, plans)
	if err != nil {
		return nil, fmt.Errorf("read the plans' rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key goingPlanKey
		row, err := scanGoingPlanRow(rows, &key.run, &key.plan)
		if err != nil {
			return nil, fmt.Errorf("read the plans' rows: %w", err)
		}
		byPlan[key] = append(byPlan[key], row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the plans' rows: %w", err)
	}
	return byPlan, nil
}

// goingPlanStateSQL sets the state of plan $2 of run $1 from its rows, and answers it: rejected
// stays rejected, and otherwise it is running while a row runs, approved while a row is approved,
// waiting while a row waits, and done.
const goingPlanStateSQL = `
	update going_plans p set state = case
	  when p.state = 'rejected' then 'rejected'
	  when exists (select 1 from going_plan_rows r where r.run_id = p.run_id and r.plan = p.plan and r.state = 'running') then 'running'
	  when exists (select 1 from going_plan_rows r where r.run_id = p.run_id and r.plan = p.plan and r.state = 'approved') then 'approved'
	  when exists (select 1 from going_plan_rows r where r.run_id = p.run_id and r.plan = p.plan and r.state = 'waiting') then 'waiting'
	  else 'done' end
	where p.run_id = $1::uuid and p.plan = $2
	returning p.state
`

// recordGoingPlanApproval records in tx that by, as going_plans.approved_by holds it, approved
// plan of run now.
func recordGoingPlanApproval(ctx context.Context, tx pgx.Tx, run string, plan int, by any) error {
	encoded, err := json.Marshal(by)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `update going_plans set approved_at = now(), approved_by = $3::jsonb where run_id = $1::uuid and plan = $2`,
		run, plan, string(encoded))
	return err
}

// goingJSONEqual reports whether a and b hold the same JSON value, whatever the order of their
// members: a value Postgres stored as jsonb reads back with its members in an order of its own.
// Two absent values are equal.
func goingJSONEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b) || string(a) == "null" || string(b) == "null"
	}
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

// goingReadRow reads what a row of tool with arguments changes, as the person doer acts for
// (login): the call prepared as its write will be, and what the plan shows of its item - the
// item, the issue it is or belongs to, the fields the change writes and their value now, the
// holder of that issue's claim, and why the row waits in every mode. sendIssue is the issue a
// send to an agent session is posted on (going_messages.go), "" for none. A call the tool's schema
// or translation refuses, and a read the person may not make, answers its error.
func (s *server) goingReadRow(ctx context.Context, doer GoingDoer, login, tool string, arguments map[string]any, sendIssue string) (goingRowShown, error) {
	caller := GoingCaller{Catalogue: s.going, Doer: doer, knownSession: s.goingKnownSession, sendIssue: sendIssue}
	prepared, err := caller.prepare(ctx, tool, arguments)
	if err != nil {
		return goingRowShown{}, err
	}
	observer := goingObserver{read: caller.reader(), s: s}
	before, err := observer.observe(ctx, prepared, nil, 0, goingObservation{})
	if err != nil {
		return goingRowShown{}, err
	}
	shown := goingRowShown{Item: before.target, Issue: before.issue, Fields: before.fields, Token: before.token}
	if shown.Issue == "" && strings.HasPrefix(prepared.route.Pattern, "/api/v1/issues/{key}") {
		shown.Issue = prepared.call.path["key"]
	}
	if before.value != nil {
		if shown.Now, err = json.Marshal(before.value); err != nil {
			return goingRowShown{}, fmt.Errorf("encode the value of %s: %w", shown.Item, err)
		}
	}
	if shown.Issue != "" {
		holder, canonical, err := observer.claimHolder(ctx, shown.Issue)
		if err != nil {
			return goingRowShown{}, err
		}
		shown.Issue, shown.Claim = canonical, holder
	}
	event, err := s.goingRowEvent(ctx, tool, prepared, shown, login)
	if err != nil {
		return goingRowShown{}, err
	}
	if shown.Waits, err = s.goingRowWaits(ctx, tool, event, shown.Claim); err != nil {
		return goingRowShown{}, err
	}
	return shown, nil
}

// goingRowArguments decodes a row's arguments, an object or nothing.
func goingRowArguments(raw json.RawMessage) (map[string]any, error) {
	arguments := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" {
		return arguments, nil
	}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return nil, fmt.Errorf("the arguments are no JSON object: %w", err)
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	return arguments, nil
}

// planCall is a plan call (agentwire.KindPlan): the run's model proposes its plan, which the
// server keeps with its rows waiting. Each change must name a write tool, need only changes before
// it, and hold arguments its tool's schema and translation take, over an item the person may read;
// the first that does not refuses the plan CALL_REFUSED, naming the change and why, which the
// model is told. A send to an agent session is kept naming the session by its id and showing the
// issue it is posted on (goingPlaceSend). A run whose latest plan still waits for its person, or
// has rows to run, is refused another. A new-issue run's plan is one create of an issue, with the
// deployment's settings for a new issue put on it, and the Enter that started the run approved
// it, so it is kept approved and answered as the row to run now (going_new_issue.go); such a run
// keeps one plan. A second try of the call's step answers the plan its first try made. The plan
// is kept in one transaction that holds the run's lease, with a plan frame on the run's events.
func (a *AgentListener) planCall(ctx context.Context, request agentwire.CallRequest, run goingCallRun, doer GoingDoer) (agentwire.CallAnswer, *agentwire.Error) {
	proposal := request.Proposal
	if proposal == nil || len(proposal.Changes) == 0 {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused, "a plan holds at least one change")
	}
	if len(proposal.Changes) > goingPlanMostRows {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused,
			"a plan holds at most %d changes, and this one holds %d: propose the first of them, and the rest once they ran", goingPlanMostRows, len(proposal.Changes))
	}
	newIssue := run.kind == agentwire.RunNewIssue
	if newIssue {
		if refusal := goingNewIssueProposal(proposal); refusal != nil {
			return agentwire.CallAnswer{}, refusal
		}
	}
	if made, err := a.planOfStep(ctx, run.id, request.Step); err != nil || made != nil {
		return made.callAnswer(), goingAgentFailedIf(err)
	}
	if open, err := readGoingPlan(ctx, a.s.deps.Store.Pool, run.id, 0); err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	} else if open != nil && newIssue {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused,
			"this run files one new issue, and plan %d is its plan: propose no other plan", open.plan)
	} else if open != nil && open.state != "rejected" && open.state != "done" {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused,
			"plan %d of this run still waits for the person or has changes to run: propose no other plan until they answer it", open.plan)
	}
	rows := make([]goingPlanRowStored, 0, len(proposal.Changes))
	for index, change := range proposal.Changes {
		if _, writes := goingWriteTools[change.Tool]; !writes {
			return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused,
				"change %d names %q, which is no tool of Dispatch that changes something; find_tools names them", index, change.Tool)
		}
		if _, held := a.s.going.Lookup(change.Tool); !held {
			return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused, "change %d names %q, which this server does not run", index, change.Tool)
		}
		needs := slices.Clone(change.Needs)
		slices.Sort(needs)
		needs = slices.Compact(needs)
		for _, need := range needs {
			if need < 0 || need >= index {
				return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused,
					"change %d needs change %d, which is not a change before it: list each change after the changes it needs", index, need)
			}
		}
		arguments, err := goingRowArguments(change.Arguments)
		if err != nil {
			return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused, "change %d (%s): %v", index, change.Tool, err)
		}
		var route string
		if newIssue {
			route = a.s.goingNewIssueSettings(arguments)
		}
		var sendIssue string
		if change.Tool == goingSendToAgent {
			sendIssue, err = a.s.goingPlaceSend(ctx, doer, run.id, arguments)
		}
		var shown goingRowShown
		if err == nil {
			shown, err = a.s.goingReadRow(ctx, doer, run.login, change.Tool, arguments, sendIssue)
		}
		if err != nil {
			refusal := goingCallRefusal(change.Tool, err)
			refusal.Message = fmt.Sprintf("change %d: %s", index, refusal.Message)
			return agentwire.CallAnswer{}, refusal
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
		}
		if needs == nil {
			needs = []int{}
		}
		row := goingPlanRowStored{row: index, tool: change.Tool, arguments: encoded, needs: needs, state: "waiting", shown: shown}
		if newIssue {
			row.state, row.approval = "approved", &goingRowApproval{By: goingApprovedByPerson, Seen: shown, Route: route}
		}
		rows = append(rows, row)
	}
	var approvedBy map[string]any
	if newIssue {
		approvedBy = goingNewIssueApprovedBy(run.login)
	}
	plan, refusal := a.keepPlan(ctx, request, run.id, proposal.Summary, rows, approvedBy)
	if refusal != nil {
		return agentwire.CallAnswer{}, refusal
	}
	return plan.callAnswer(), nil
}

// goingAgentFailedIf is goingAgentFailed of err, nil for none.
func goingAgentFailedIf(err error) *agentwire.Error {
	if err == nil {
		return nil
	}
	return goingAgentFailed(agentwire.CallCall, err)
}

// planOfStep is the plan the call of step made, nil for none.
func (a *AgentListener) planOfStep(ctx context.Context, run string, step int) (*goingPlanStored, error) {
	var plan int
	err := a.s.deps.Store.Pool.QueryRow(ctx, `select plan from going_plans where run_id = $1::uuid and step = $2`, run, step).Scan(&plan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return readGoingPlan(ctx, a.s.deps.Store.Pool, run, plan)
}

// keepPlan keeps rows as the run's next plan, proposed by request's step, in one transaction that
// holds the run's lease, and appends the plan frame to the run's events. Each row is kept in its
// state with its approval: every row waits, but for the create of a new-issue run, which is kept
// approved. approvedBy, nil for a plan nobody approved yet, is who approved the plan as
// going_plans.approved_by holds it, which keeps the plan in the state its rows give it.
func (a *AgentListener) keepPlan(ctx context.Context, request agentwire.CallRequest, run, summary string, rows []goingPlanRowStored,
	approvedBy map[string]any,
) (*goingPlanStored, *agentwire.Error) {
	tx, err := a.s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if held, err := goingLeaseHeld(ctx, tx, run, request.LeaseToken); err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, err)
	} else if !held {
		return nil, goingStaleLease(run)
	}
	plan := goingPlanStored{state: "waiting", summary: summary, rows: rows}
	if err := tx.QueryRow(ctx, `
		insert into going_plans (run_id, plan, state, step, summary)
		select $1::uuid, coalesce(max(plan), 0) + 1, 'waiting', $2, $3 from going_plans where run_id = $1::uuid
		returning plan
	`, run, request.Step, summary).Scan(&plan.plan); err != nil {
		if isUniqueViolation(err) {
			made, readErr := a.planOfStep(ctx, run, request.Step)
			if readErr != nil || made == nil {
				return nil, goingAgentFailed(agentwire.CallCall, errors.Join(err, readErr))
			}
			return made, nil
		}
		return nil, goingAgentFailed(agentwire.CallCall, err)
	}
	for _, row := range rows {
		change, err := json.Marshal(map[string]any{"tool": row.tool, "arguments": row.arguments})
		if err != nil {
			return nil, goingAgentFailed(agentwire.CallCall, err)
		}
		shown, err := json.Marshal(row.shown)
		if err != nil {
			return nil, goingAgentFailed(agentwire.CallCall, err)
		}
		approval, err := nullableJSON(row.approval)
		if err != nil {
			return nil, goingAgentFailed(agentwire.CallCall, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into going_plan_rows (run_id, plan, row, change, needs, state, shown, approval)
			values ($1::uuid, $2, $3, $4::jsonb, $5, $6, $7::jsonb, $8::jsonb)
		`, run, plan.plan, row.row, string(change), row.needs, row.state, string(shown), approval); err != nil {
			return nil, goingAgentFailed(agentwire.CallCall, err)
		}
	}
	if approvedBy != nil {
		if err := recordGoingPlanApproval(ctx, tx, run, plan.plan, approvedBy); err != nil {
			return nil, goingAgentFailed(agentwire.CallCall, err)
		}
		if err := tx.QueryRow(ctx, goingPlanStateSQL, run, plan.plan).Scan(&plan.state); err != nil {
			return nil, goingAgentFailed(agentwire.CallCall, err)
		}
	}
	if err := appendGoingPlanFrame(ctx, tx, run, plan.plan, plan.state); err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, err)
	}
	return &plan, nil
}

// appendGoingPlanFrame appends a plan frame to run's events in tx: plan's number and state.
func appendGoingPlanFrame(ctx context.Context, tx pgx.Tx, run string, plan int, state string) error {
	data, err := json.Marshal(map[string]any{"plan": plan, "state": state})
	if err != nil {
		return err
	}
	return appendGoingRunFrames(ctx, tx, run, []agentwire.Frame{{Name: goingFramePlan, Data: data}})
}

// callAnswer is the plan as a plan call answers it (agentwire.PlanAnswer): its rows, and each row
// approved and not yet run as a row to run now, as a claim of the run would answer it
// (goingPendingRows), with the step of a call that already began it.
func (p *goingPlanStored) callAnswer() agentwire.CallAnswer {
	if p == nil {
		return agentwire.CallAnswer{}
	}
	answer := agentwire.PlanAnswer{Plan: p.plan, Rows: make([]agentwire.PlanRowRead, 0, len(p.rows))}
	for _, row := range p.rows {
		answer.Rows = append(answer.Rows, agentwire.PlanRowRead{
			Row: row.row, Tool: row.tool, Arguments: row.arguments, Item: row.shown.Item, Fields: row.shown.Fields,
			Now: row.shown.Now, Waits: row.shown.Waits,
		})
		if row.state == "approved" || row.state == "running" {
			run := agentwire.PlanRun{PlanRow: agentwire.PlanRow{Plan: p.plan, Row: row.row}, Tool: row.tool, Arguments: row.arguments}
			if row.result != nil {
				run.Step = row.result.Step
			}
			answer.Run = append(answer.Run, run)
		}
	}
	body, _ := json.Marshal(answer)
	return agentwire.CallAnswer{Status: http.StatusOK, Body: body, ContentType: "application/json"}
}

// goingPendingRowsSQL reads the rows of run $1's latest plan the agent runs before its next model
// call: each approved or running row, and each row whose key ran it but whose step the run has not
// saved, in order. A second try of that key answers what the first made of it.
const goingPendingRowsSQL = `
	select r.plan, r.row, coalesce(r.change->>'tool', ''), coalesce(r.change->'arguments', '{}'::jsonb),
	  coalesce((r.result->>'step')::int, 0)
	from going_plan_rows r
	where r.run_id = $1::uuid and r.plan = (select max(plan) from going_plans where run_id = $1::uuid)
	  and (r.state in ('approved', 'running')
	       or (r.result ? 'step' and r.state in ('done', 'failed', 'skipped', 'waiting')
	           and not exists (select 1 from going_run_steps s where s.run_id = r.run_id and s.step = (r.result->>'step')::int)))
	order by r.row
`

// goingPendingRows is run's rows to run (goingPendingRowsSQL).
func goingPendingRows(ctx context.Context, q goingQuerier, run string) ([]agentwire.PlanRun, error) {
	rows, err := q.Query(ctx, goingPendingRowsSQL, run)
	if err != nil {
		return nil, fmt.Errorf("read the rows run %s runs next: %w", run, err)
	}
	pending, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (agentwire.PlanRun, error) {
		var pending agentwire.PlanRun
		var arguments []byte
		err := row.Scan(&pending.Plan, &pending.Row, &pending.Tool, &arguments, &pending.Step)
		pending.Arguments = arguments
		return pending, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the rows run %s runs next: %w", run, err)
	}
	return pending, nil
}

// goingOwnRun is the caller's run a plan route names: its id, its state and its mode.
type goingOwnRun struct {
	id, state, mode string
}

// readOwnGoingRun is the run the path names, where it is login's own, or the run's 404 written: an
// id that is no uuid, a run of anyone else's and one the record does not hold are one answer.
func (s *server) readOwnGoingRun(w http.ResponseWriter, r *http.Request, login string) (goingOwnRun, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeGoingRunNotFound(w)
		return goingOwnRun{}, false
	}
	run := goingOwnRun{id: id.String()}
	err = s.deps.Store.Pool.QueryRow(r.Context(), `
		select state, coalesce(mode, $3) from going_runs where id = $1::uuid and author_login = $2 and state is not null
	`, run.id, login, goingModeAsk).Scan(&run.state, &run.mode)
	if errors.Is(err, pgx.ErrNoRows) {
		writeGoingRunNotFound(w)
		return goingOwnRun{}, false
	}
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the run: %w", err))
		return goingOwnRun{}, false
	}
	return run, true
}

// planRoute is what every plan route starts with: the caller, their run the path names, and its
// latest plan, or the answer written: the run's 404, or GOING_PLAN_NOT_FOUND for a run that
// proposed none.
func (s *server) planRoute(w http.ResponseWriter, r *http.Request) (string, goingOwnRun, *goingPlanStored, bool) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return "", goingOwnRun{}, nil, false
	}
	login := model.CanonicalLogin(caller.ID)
	run, ok := s.readOwnGoingRun(w, r, login)
	if !ok {
		return "", goingOwnRun{}, nil, false
	}
	plan, err := readGoingPlan(r.Context(), s.deps.Store.Pool, run.id, 0)
	if err != nil {
		s.writeHandlerError(w, err)
		return "", goingOwnRun{}, nil, false
	}
	if plan == nil {
		writeError(w, codeGoingPlanNotFound, http.StatusNotFound, "this run has proposed no plan")
		return "", goingOwnRun{}, nil, false
	}
	return login, run, plan, true
}

// getGoingPlan is GET /api/v1/going/runs/{id}/plan: the run's latest plan. While the run waits for
// its person, each waiting row's item is read again as the caller, and its value now, its claim
// and why it waits are what the plan shows from then on.
func (s *server) getGoingPlan(w http.ResponseWriter, r *http.Request) {
	login, run, plan, ok := s.planRoute(w, r)
	if !ok {
		return
	}
	if run.state == "waiting" {
		doer, err := s.goingBarDoer(r, run.id)
		if err != nil {
			s.writeAuthenticationError(w, err)
			return
		}
		for index, row := range plan.rows {
			if row.state != "waiting" {
				continue
			}
			arguments, err := goingRowArguments(row.arguments)
			if err != nil {
				continue
			}
			now, err := s.goingReadRow(r.Context(), doer, login, row.tool, arguments, row.sendIssue())
			if err != nil {
				// A read the person may no longer make shows the row as last read; its write
				// answers the refusal.
				continue
			}
			now.Changed = row.shown.Changed
			if now.unchanged(row.shown) {
				continue
			}
			if err := s.storeRowShown(r.Context(), run.id, plan.plan, row.row, now); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			plan.rows[index].shown = now
		}
	}
	WriteJSON(w, http.StatusOK, plan.answer(run.id, run.mode))
}

// storeRowShown keeps shown as what row of plan of run shows.
func (s *server) storeRowShown(ctx context.Context, run string, plan, row int, shown goingRowShown) error {
	encoded, err := json.Marshal(shown)
	if err != nil {
		return err
	}
	if _, err := s.deps.Store.Pool.Exec(ctx, `
		update going_plan_rows set shown = $4::jsonb where run_id = $1::uuid and plan = $2 and row = $3
	`, run, plan, row, string(encoded)); err != nil {
		return fmt.Errorf("keep what the plan shows: %w", err)
	}
	return nil
}

// goingPlanApproveInput is POST /api/v1/going/runs/{id}/plan/approve's body: the plan approved,
// the person's tick and edited values of each row of it they changed, and the version of the plan
// the page they approve on read (goingPlanAnswer.Version), which the approval holds the plan to. A
// waiting row the body does not name is approved as shown.
type goingPlanApproveInput struct {
	Plan    int             `json:"plan" required:"true"`
	Rows    []goingPlanTick `json:"rows"`
	Version *string         `json:"version"`
}

// goingPlanTick is the person's word on one row: whether it runs (an untick also unticks every row
// that needs it), and the values they edited, each an argument of the row's tool put over the
// arguments the plan shows.
type goingPlanTick struct {
	Row    int            `json:"row" required:"true"`
	Tick   *bool          `json:"tick" required:"true"`
	Values map[string]any `json:"values"`
}

// goingPlanReviseInput is POST /api/v1/going/runs/{id}/plan/revise's body: the plan, and the
// person's words of what to change.
type goingPlanReviseInput struct {
	Plan  int    `json:"plan" required:"true"`
	Words string `json:"words" required:"true"`
}

// goingPlanRejectInput is POST /api/v1/going/runs/{id}/plan/reject's body: the plan rejected.
type goingPlanRejectInput struct {
	Plan int `json:"plan" required:"true"`
}

// approveGoingPlan is POST /api/v1/going/runs/{id}/plan/approve: the person approves the run's
// waiting plan, with each row's tick and each value they edited. A row they unticked is unticked,
// and so is each row that needs it. Each ticked row is read again as the caller with its values: a
// row whose item changed after the plan showed - another value now, or another holder of its
// issue's claim - shows the new value and waits for approval again; every other ticked row is
// approved, over that read. A send to an agent session takes the issue it is posted on as the
// value `issue`, which its arguments do not hold (goingEditSend). The approval moves the run's
// grant to the caller's credential, which must be a person's own sign-in or personal token holding
// the approve scope; it is saved as a step of the run (agentwire.PersonStep), and the run, with a
// row to run, is ready again for the agent, which runs them. A plan that is not the run's latest is
// 409 GOING_PLAN_NOT_CURRENT, and a run or a plan that does not wait 409 GOING_PLAN_NOT_WAITING; a
// version that is not the plan's now - another page read the plan again since, and the rows it
// shows moved - is 409 GOING_PLAN_CHANGED, with nothing written, so a person approves only what
// their page showed; a row the body names that is no waiting row of the plan, or a value its tool's
// schema refuses, 400 GOING_PLAN_VALUES.
func (s *server) approveGoingPlan(w http.ResponseWriter, r *http.Request) {
	login, run, plan, ok := s.planRoute(w, r)
	if !ok {
		return
	}
	var input goingPlanApproveInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !s.planAnswerable(w, run, plan, input.Plan) {
		return
	}
	if input.Version != nil && *input.Version != plan.answer(run.id, run.mode).Version {
		writeError(w, codeGoingPlanChanged, http.StatusConflict,
			"This plan changed after this page read it. Read it again before you approve it.")
		return
	}
	grant, err := s.goingGrantOf(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	doer, err := s.goingBarDoer(r, run.id)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	ticks := map[int]goingPlanTick{}
	for _, tick := range input.Rows {
		at := slices.IndexFunc(plan.rows, func(row goingPlanRowStored) bool { return row.row == tick.Row })
		if at < 0 || plan.rows[at].state != "waiting" {
			writeError(w, codeGoingPlanValues, http.StatusBadRequest, fmt.Sprintf("row %d is no row of plan %d that waits", tick.Row, plan.plan))
			return
		}
		if tick.Tick == nil {
			writeError(w, codeGoingPlanInput, http.StatusBadRequest, fmt.Sprintf("row %d names no tick: true runs it, false leaves it out", tick.Row))
			return
		}
		ticks[tick.Row] = tick
	}
	unticked := map[int]bool{}
	for _, row := range plan.rows {
		if tick, named := ticks[row.row]; named && !*tick.Tick {
			unticked[row.row] = true
		}
		for _, need := range row.needs {
			if unticked[need] {
				unticked[row.row] = true
			}
		}
	}
	var approvals []goingRowAnswer
	var approved []agentwire.PlanRun
	var left []int
	for _, row := range plan.rows {
		if row.state != "waiting" {
			continue
		}
		if unticked[row.row] {
			approvals = append(approvals, goingRowAnswer{row: row.row, tool: row.tool, arguments: row.arguments, shown: row.shown, state: "unticked"})
			left = append(left, row.row)
			continue
		}
		arguments, err := goingRowArguments(row.arguments)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		edited := ticks[row.row].Values
		sendIssue := row.sendIssue()
		if row.tool == goingSendToAgent {
			sendIssue, err = s.goingEditSend(r.Context(), doer, run.id, arguments, edited, sendIssue)
		} else {
			maps.Copy(arguments, edited)
		}
		var now goingRowShown
		if err == nil {
			now, err = s.goingReadRow(r.Context(), doer, login, row.tool, arguments, sendIssue)
		}
		var refused GoingArgumentError
		switch {
		case errors.As(err, &refused):
			writeError(w, codeGoingPlanValues, http.StatusBadRequest, fmt.Sprintf("%s: %s", goingRowLabel(row), refused.Reason))
			return
		case err != nil:
			s.writeGoingPlanReadError(w, goingRowLabel(row), err)
			return
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// An edit of a value reads the fields the plan showed; one that names another field
		// reads another, which the person saw no value of.
		if slices.Equal(row.shown.Fields, now.Fields) && !row.shown.same(now) {
			now.Changed = true
			approvals = append(approvals, goingRowAnswer{row: row.row, tool: row.tool, arguments: encoded, shown: now, state: "waiting"})
			continue
		}
		approvals = append(approvals, goingRowAnswer{row: row.row, tool: row.tool, arguments: encoded, shown: now, state: "approved",
			approval: &goingRowApproval{By: goingApprovedByPerson, Edited: edited, Seen: now}})
		approved = append(approved, agentwire.PlanRun{PlanRow: agentwire.PlanRow{Plan: plan.plan, Row: row.row}, Tool: row.tool, Arguments: encoded})
	}
	err = s.answerGoingPlan(r.Context(), run.id, plan.plan, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if err := recordGoingGrant(ctx, tx, run.id, grant); err != nil {
			return false, err
		}
		for _, row := range approvals {
			if err := row.store(ctx, tx, run.id, plan.plan); err != nil {
				return false, err
			}
		}
		if err := recordGoingPlanApproval(ctx, tx, run.id, plan.plan, map[string]string{"by": goingApprovedByPerson, "kind": grant.kind, "login": grant.login}); err != nil {
			return false, err
		}
		if len(approved) == 0 {
			return false, nil
		}
		note := fmt.Sprintf("the person approved plan %d: %d of its changes run", plan.plan, len(approved))
		if len(left) > 0 {
			note += fmt.Sprintf(", and they left out rows %v", left)
		}
		return true, goingPersonStep(ctx, tx, run.id, agentwire.PersonStep{Plan: plan.plan, Approved: approved, Unticked: left}, note)
	})
	s.writePlanAnswered(w, r, run, err)
}

// goingRowAnswer is what the person's answer makes of one waiting row: its state, the arguments
// it runs with, what it shows, and the approval it runs under.
type goingRowAnswer struct {
	row       int
	tool      string
	arguments json.RawMessage
	approval  *goingRowApproval
	shown     goingRowShown
	state     string
}

// store writes the answer of a row of plan of run in tx, while the row still waits.
func (a goingRowAnswer) store(ctx context.Context, tx pgx.Tx, run string, plan int) error {
	change, err := json.Marshal(map[string]any{"tool": a.tool, "arguments": a.arguments})
	if err != nil {
		return err
	}
	shown, err := json.Marshal(a.shown)
	if err != nil {
		return err
	}
	approval, err := nullableJSON(a.approval)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		update going_plan_rows set state = $4, change = $5::jsonb, shown = $6::jsonb, approval = $7::jsonb,
		  result = nullif(jsonb_strip_nulls(jsonb_build_object('resumes', result->'resumes')), '{}'::jsonb)
		where run_id = $1::uuid and plan = $2 and row = $3 and state = 'waiting'
	`, run, plan, a.row, a.state, string(change), string(shown), approval); err != nil {
		return fmt.Errorf("answer row %d: %w", a.row, err)
	}
	return nil
}

// writeGoingPlanReadError answers a read of a row's item that the person may not make, or that
// failed: a route's refusal as that route answered it, naming the row as the page names it
// (label, goingRowLabel), and anything else as the server's own failure.
func (s *server) writeGoingPlanReadError(w http.ResponseWriter, label string, err error) {
	var refused goingRefusal
	if errors.As(err, &refused) {
		writeError(w, refused.Code, refused.Status, fmt.Sprintf("%s: %s", label, refused.Message))
		return
	}
	s.writeHandlerError(w, err)
}

// planAnswerable writes the refusal of an answer to plan number of run, and reports whether it may
// be answered: the run waits for its person, and number is its latest plan, which waits.
func (s *server) planAnswerable(w http.ResponseWriter, run goingOwnRun, plan *goingPlanStored, number int) bool {
	switch {
	case number != plan.plan:
		writeError(w, codeGoingPlanNotCurrent, http.StatusConflict,
			fmt.Sprintf("plan %d is not this run's latest plan, which is plan %d", number, plan.plan))
		return false
	case run.state != "waiting" || plan.state != "waiting":
		writeError(w, codeGoingPlanNotWaiting, http.StatusConflict,
			fmt.Sprintf("plan %d does not wait for an answer: the run is %s and the plan %s", plan.plan, run.state, plan.state))
		return false
	}
	return true
}

// codeGoingPlanNotCurrent refuses an answer to a plan that is not its run's latest.
const codeGoingPlanNotCurrent = "GOING_PLAN_NOT_CURRENT"

// errGoingPlanMoved is an answer whose run or plan no longer waits once its transaction holds the
// run: another answer came first.
var errGoingPlanMoved = errorf(http.StatusConflict, codeGoingPlanNotWaiting, "the plan was answered meanwhile, and no longer waits")

// answerGoingPlan answers plan of run in one transaction: it holds the run's row, which must still
// wait with plan its latest, waiting plan, runs write, sets the plan's state from its rows, and
// makes the run ready again where write reports that the person's answer asks the run for more -
// a row to run, a plan to propose - with a plan frame on its events and going_plan.updated naming
// the run's person, whose Inbox shows the plan while it waits. A run made ready is announced on
// GoingRunsChannel.
func (s *server) answerGoingPlan(ctx context.Context, run string, plan int, write func(context.Context, pgx.Tx) (bool, error)) error {
	tx, err := s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state, login string
	if err := tx.QueryRow(ctx, `select state, author_login from going_runs where id = $1::uuid for update`, run).Scan(&state, &login); err != nil {
		return fmt.Errorf("hold the run: %w", err)
	}
	var latest int
	var planState string
	if err := tx.QueryRow(ctx, `select plan, state from going_plans where run_id = $1::uuid order by plan desc limit 1`, run).Scan(&latest, &planState); err != nil {
		return fmt.Errorf("read the run's plan: %w", err)
	}
	if state != "waiting" || latest != plan || planState != "waiting" {
		return errGoingPlanMoved
	}
	ready, err := write(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, goingPlanStateSQL, run, plan).Scan(&planState); err != nil {
		return fmt.Errorf("set the plan's state: %w", err)
	}
	if ready {
		if _, err := tx.Exec(ctx, `update going_runs set state = 'queued', finished_at = null, stop_reason = null where id = $1::uuid`, run); err != nil {
			return fmt.Errorf("make the run ready: %w", err)
		}
	}
	if err := appendGoingPlanFrame(ctx, tx, run, plan, planState); err != nil {
		return err
	}
	event, err := s.appendGoingPlanEvent(ctx, tx, login, run)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.publish(ctx, event)
	if ready {
		if err := s.notifyGoingRuns(ctx, "ready"); err != nil {
			return fmt.Errorf("announce the run: %w", err)
		}
	}
	return nil
}

// goingPersonStep saves the person's answer to a plan as run's next step in tx, with note as its
// one line.
func goingPersonStep(ctx context.Context, tx pgx.Tx, run string, step agentwire.PersonStep, note string) error {
	body, err := json.Marshal(step)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into going_run_steps (run_id, step, kind, body, note)
		select $1::uuid, coalesce(max(step), 0) + 1, $2, $3::jsonb, $4 from going_run_steps where run_id = $1::uuid
	`, run, agentwire.StepPerson, string(body), note); err != nil {
		return fmt.Errorf("save the person's answer as a step of the run: %w", err)
	}
	return nil
}

// writePlanAnswered answers a plan route whose answer err ended: its refusal, or the plan as it now
// stands.
func (s *server) writePlanAnswered(w http.ResponseWriter, r *http.Request, run goingOwnRun, err error) {
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	plan, err := readGoingPlan(r.Context(), s.deps.Store.Pool, run.id, 0)
	if err != nil || plan == nil {
		s.writeHandlerError(w, errors.Join(errors.New("read the plan answered"), err))
		return
	}
	WriteJSON(w, http.StatusOK, plan.answer(run.id, run.mode))
}

// reviseGoingPlan is POST /api/v1/going/runs/{id}/plan/revise: the person asks, in words, for
// another plan in place of the run's waiting plan. The plan is rejected, nothing of it runs, and
// the words are saved as a step of the run (agentwire.PersonStep), which the run's model reads
// beside the plan's rows when it proposes the next. The revision moves the run's grant to the
// caller's credential, as an approval does, and makes the run ready again.
func (s *server) reviseGoingPlan(w http.ResponseWriter, r *http.Request) {
	_, run, plan, ok := s.planRoute(w, r)
	if !ok {
		return
	}
	var input goingPlanReviseInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	words := strings.TrimSpace(input.Words)
	if words == "" || len([]rune(words)) > goingReviseMostRunes {
		writeError(w, codeGoingPlanInput, http.StatusBadRequest, fmt.Sprintf("words say what to change in 1 to %d characters", goingReviseMostRunes))
		return
	}
	if !s.planAnswerable(w, run, plan, input.Plan) {
		return
	}
	grant, err := s.goingGrantOf(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	err = s.answerGoingPlan(r.Context(), run.id, plan.plan, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if err := recordGoingGrant(ctx, tx, run.id, grant); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `update going_plans set state = 'rejected' where run_id = $1::uuid and plan = $2`, run.id, plan.plan); err != nil {
			return false, err
		}
		rows := make([]agentwire.PlanRun, 0, len(plan.rows))
		for _, row := range plan.rows {
			rows = append(rows, agentwire.PlanRun{PlanRow: agentwire.PlanRow{Plan: plan.plan, Row: row.row}, Tool: row.tool, Arguments: row.arguments})
		}
		return true, goingPersonStep(ctx, tx, run.id, agentwire.PersonStep{Plan: plan.plan, Revise: words, Rows: rows},
			fmt.Sprintf("the person asked for another plan in place of plan %d: %s", plan.plan, words))
	})
	s.writePlanAnswered(w, r, run, err)
}

// rejectGoingPlan is POST /api/v1/going/runs/{id}/plan/reject: the person rejects the run's
// waiting plan. Nothing of it runs, and the run ends done with goingPlanRejected.
func (s *server) rejectGoingPlan(w http.ResponseWriter, r *http.Request) {
	_, run, plan, ok := s.planRoute(w, r)
	if !ok {
		return
	}
	var input goingPlanRejectInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !s.planAnswerable(w, run, plan, input.Plan) {
		return
	}
	err := s.answerGoingPlan(r.Context(), run.id, plan.plan, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if _, err := tx.Exec(ctx, `update going_plans set state = 'rejected' where run_id = $1::uuid and plan = $2`, run.id, plan.plan); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `
			update going_runs set state = 'done', stop_reason = $2, finished_at = now() where id = $1::uuid
		`, run.id, goingPlanRejected); err != nil {
			return false, err
		}
		frame, err := json.Marshal(agentwire.EndFrame{State: agentwire.StateDone, Reason: goingPlanRejected, Offers: []string{}})
		if err != nil {
			return false, err
		}
		return false, appendGoingRunFrames(ctx, tx, run.id, []agentwire.Frame{{Name: agentwire.FrameEnd, Data: frame}})
	})
	s.writePlanAnswered(w, r, run, err)
}
