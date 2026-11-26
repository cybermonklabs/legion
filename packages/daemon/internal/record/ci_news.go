package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The CI news of a task that takes a fix (PlanTask.FixesCI) keeps three purposes apart, and the
// notices of a fix two of their own: a report of the GitHub listener's new runs and statuses
// (CINewsRuns), a prompt to read Dispatch's state again (CINewsState), the fix rule's one repair
// assignment of a check at a head (CINewsAssignment), the start of a fix told to each other step
// (CINewsFixStarted), and a failed step's report given to the running fix (CINewsStepReport). No
// report and no prompt assigns work, starts a fix, allows a job one more run or allows an
// approval: Dispatch's state of each check decides, and the row tells its reader to read it.

// CINewsPurpose is which of the five a CINews row is.
type CINewsPurpose string

const (
	CINewsRuns       CINewsPurpose = "runs"
	CINewsState      CINewsPurpose = "state"
	CINewsAssignment CINewsPurpose = "assignment"
	CINewsFixStarted CINewsPurpose = "fix-started"
	CINewsStepReport CINewsPurpose = "step-report"
)

// ciNewsPurposes is every CINewsPurpose.
var ciNewsPurposes = []CINewsPurpose{CINewsRuns, CINewsState, CINewsAssignment, CINewsFixStarted, CINewsStepReport}

// CINext is what Legion itself does next for the task a settlement is told of, "" for nothing:
// a fix of the task runs, the task is held before its retro for CI or for its fix, or the testing
// move waits for Dispatch's states.
type CINext string

const (
	CINextNone        CINext = ""
	CINextEndHoldCI   CINext = "end-hold-ci"
	CINextEndHoldFix  CINext = "end-hold-fix"
	CINextFixRuns     CINext = "fix-runs"
	CINextTestingMove CINext = "testing-move"
)

// ciNexts is every CINext.
var ciNexts = []CINext{CINextNone, CINextEndHoldCI, CINextEndHoldFix, CINextFixRuns, CINextTestingMove}

// CIReader is the current reader a CINews row is for, as the row was written: the phase of the
// step the reader runs (a step's reader) or the fix it runs (a fix's reader), and the reader's
// phases-row entry. The owner of an issue in accepting is a step's reader with entry 0, since
// the acceptance step starts no worker. A row whose reader is no longer current is finished
// undelivered (the daemon's ciNews), and EnqueueReadPrompt joins only a pending prompt of the same
// reader.
type CIReader struct {
	Phase phase.Phase `json:"phase,omitempty"`
	Fix   int         `json:"fix,omitempty"`
	Entry int64       `json:"entry"`
}

// CIRun is one CI run a settlement observed at a head, with its GitHub identity and conclusion.
type CIRun struct {
	Name       string `json:"name"`
	ID         int64  `json:"id"`
	Head       string `json:"head"`
	Conclusion string `json:"conclusion"`
}

// CIStatus is one commit status a settlement observed.
type CIStatus struct {
	Context   string    `json:"context"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
}

// CIStateSnapshot is Dispatch's held state of one check when the row was written or last joined,
// at Dispatch's sequence Seq. It is for tests and logs: no text renders it, since a yes queued can
// be overtaken by a no before the row is delivered.
type CIStateSnapshot struct {
	Check  string   `json:"check"`
	State  string   `json:"state"`
	Reason string   `json:"reason"`
	CICase string   `json:"ciCase"`
	Roles  []string `json:"roles"`
	Seq    int64    `json:"seq"`
}

// CIAssigned is the check, the head and the CI case a repair assignment or a fix's start names,
// and the fix that took it (0 for an assignment to the implementer's own step).
type CIAssigned struct {
	Check  string `json:"check"`
	Head   string `json:"head"`
	CICase string `json:"ciCase"`
	Fix    int    `json:"fix"`
}

// CINews is one row of a task's CI news to one role of the issue, on the role-message lane
// (OutboxKindCINews). Role is the claim token the row publishes to, Generation and Task the work
// it is of, Repo, PullRequest and Head where it was observed. Reader is the current reader it is
// for, nil for a row delivered as a role message, which no released claim is woken for. Next is
// what Legion does next for the task (runs only), and Tested the head the tester's completion
// reported where Next is testing-move and that head is not Head. Runs and Statuses are a report's
// new observations, States a prompt's snapshots, and Checks a prompt's replacement of the head's
// set of checks: nil for no change of the set, and present, empty included, for a set that
// changed, so the removal of a head's last check is a present empty set. Assigned is an
// assignment's or a fix's start's check, Branch the receiver's own branch beside the fix
// (fix-started) or the reporting step's (step-report), and Body the text published.
type CINews struct {
	Purpose     CINewsPurpose     `json:"purpose"`
	Role        string            `json:"role"`
	Generation  uint64            `json:"generation"`
	Task        string            `json:"task"`
	Repo        string            `json:"repo"`
	PullRequest int               `json:"pullRequest,omitempty"`
	Head        string            `json:"head"`
	Reader      *CIReader         `json:"reader,omitempty"`
	Next        CINext            `json:"next,omitempty"`
	Runs        []CIRun           `json:"runs,omitempty"`
	Statuses    []CIStatus        `json:"statuses,omitempty"`
	States      []CIStateSnapshot `json:"states,omitempty"`
	Checks      *[]string         `json:"checks,omitempty"`
	Assigned    *CIAssigned       `json:"assigned,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	Tested      string            `json:"tested,omitempty"`
	Body        string            `json:"body"`
}

func (CINews) OutboxKind() OutboxKind { return OutboxKindCINews }

// Message is what the runner publishes: the row's body, unchanged.
func (n CINews) Message(string) string { return n.Body }

// MarshalJSON encodes n with a present set of checks as an array whatever slice it holds, so a
// replacement by no checks is stored as `[]` and decodes again as a present empty set.
func (n CINews) MarshalJSON() ([]byte, error) {
	type encoded CINews
	if n.Checks != nil && *n.Checks == nil {
		n.Checks = &[]string{}
	}
	return json.Marshal(encoded(n))
}

// Validate refuses a payload whose members its purpose does not take: runs needs Runs or Statuses
// and no States, Checks, Assigned or Branch; state needs States or a present Checks, and a
// Reader; assignment needs Assigned and a Reader; fix-started needs Assigned; step-report needs a
// Reader whose Fix is set. Next is runs' alone, and Tested names the tested head of a testing-move
// that is not Head. Every row names its role, its work and its body; a report and a prompt name
// the repository and the head they are of.
func (n CINews) Validate() error {
	if !slices.Contains(ciNewsPurposes, n.Purpose) {
		return fmt.Errorf("a CI news row is a report, a prompt, an assignment, a fix's start or a step's report; %q is none", n.Purpose)
	}
	if n.Role == "" || n.Generation == 0 || n.Task == "" || n.Body == "" {
		return fmt.Errorf("a CI news row names its role, its generation, its task and its body; it names %q, %d, %q and %d bytes",
			n.Role, n.Generation, n.Task, len(n.Body))
	}
	if !slices.Contains(ciNexts, n.Next) {
		return fmt.Errorf("a CI news row's next act %q is none Legion takes", n.Next)
	}
	if n.Next != CINextNone && n.Purpose != CINewsRuns {
		return fmt.Errorf("only a report names what Legion does next; a %s row names %q", n.Purpose, n.Next)
	}
	if n.Tested != "" && (n.Next != CINextTestingMove || n.Tested == n.Head) {
		return fmt.Errorf("a CI news row names a tested head only for a testing move at another head; it names %q with %q at %q", n.Tested, n.Next, n.Head)
	}
	if r := n.Reader; r != nil && ((r.Phase == "") == (r.Fix == 0) || r.Fix < 0 || r.Entry < 0) {
		return fmt.Errorf("a CI news row's reader runs a step of a phase or a fix, one of them; it is %+v", *r)
	}
	if (n.Purpose == CINewsRuns || n.Purpose == CINewsState) && (n.Repo == "" || n.Head == "") {
		return fmt.Errorf("a %s row names the repository and the head it is of; it names %q at %q", n.Purpose, n.Repo, n.Head)
	}
	if n.Purpose != CINewsRuns && (len(n.Runs) > 0 || len(n.Statuses) > 0) {
		return fmt.Errorf("only a report carries runs and statuses; a %s row carries %d and %d", n.Purpose, len(n.Runs), len(n.Statuses))
	}
	if n.Purpose != CINewsState && (len(n.States) > 0 || n.Checks != nil) {
		return fmt.Errorf("only a prompt to read Dispatch carries states and a set of checks; a %s row carries them", n.Purpose)
	}
	if n.Assigned != nil && n.Purpose != CINewsAssignment && n.Purpose != CINewsFixStarted {
		return fmt.Errorf("only an assignment and a fix's start name an assigned check; a %s row names %+v", n.Purpose, *n.Assigned)
	}
	if n.Branch != "" && n.Purpose != CINewsFixStarted && n.Purpose != CINewsStepReport {
		return fmt.Errorf("only a fix's start and a step's report name a branch beside the fix; a %s row names %q", n.Purpose, n.Branch)
	}
	switch n.Purpose {
	case CINewsRuns:
		if len(n.Runs) == 0 && len(n.Statuses) == 0 {
			return errors.New("a report of the listener carries a run or a status it observed")
		}
	case CINewsState:
		if len(n.States) == 0 && n.Checks == nil {
			return errors.New("a prompt to read Dispatch carries a check's state or the head's set of checks")
		}
		if n.Reader == nil {
			return errors.New("a prompt to read Dispatch is for a current reader")
		}
	case CINewsAssignment:
		if n.Assigned == nil || n.Reader == nil {
			return errors.New("a repair assignment names its check and is for a current reader")
		}
	case CINewsFixStarted:
		if n.Assigned == nil {
			return errors.New("a fix's start names the check the fix is for")
		}
	case CINewsStepReport:
		if n.Reader == nil || n.Reader.Fix == 0 {
			return errors.New("a step's report is for the running fix")
		}
	}
	return nil
}

// ReadPromptBody is the body of a prompt to read Dispatch for issue key at head: the checks whose
// state changed, or the head's set of checks where checks is empty, then what the reader does.
// It takes no state, so no body can render a verdict, which a later state may overtake before the
// row is delivered.
func ReadPromptBody(key, head string, checks []string) string {
	read := "Read it now with dispatch results --issue " + key + ", and act on what you read."
	switch len(checks) {
	case 0:
		return "Dispatch's set of checks at " + head + " changed. " + read
	case 1:
		return "Dispatch's state of check " + checks[0] + " at " + head + " changed. " + read
	default:
		return "Dispatch's state of checks " + strings.Join(checks, ", ") + " at " + head + " changed. " + read
	}
}

// readPromptCandidate is EnqueueReadPrompt's read of the pending prompt a change joins: the oldest
// unleased state row of the issue that names the complete work identity of the incoming one, its
// generation, its task, its role, its repository, its head and its reader.
const readPromptCandidate = `select id, payload from outbox
	where issue = $1 and kind = 'ci_news'
	  and payload->>'purpose' = 'state'
	  and payload->>'generation' = $2
	  and payload->>'task' = $3
	  and payload->>'role' = $4
	  and payload->>'repo' = $5
	  and payload->>'head' = $6
	  and payload->'reader' = $7::jsonb
	  and (lease_until is null or lease_until <= $8)
	order by created_at, id limit 1 for update`

// EnqueueReadPrompt writes n, a prompt to read Dispatch (CINewsState), for issue in the fact's
// transaction: it joins the issue's pending prompt of the same complete work identity, which no
// lane leases, and answers true, or queues n as a row of its own, due now, and answers false. So
// the checks of one run that change before the prompt is delivered give one row and one wake. A
// join keeps each check's snapshot of the higher sequence, replaces the set of checks only where
// n names one, an empty one included, and builds the body again from the head and the names of
// the checks it holds (ReadPromptBody). A row a lane leases is in delivery and joins nothing, and
// a pending row of another generation, task or reader stays apart: a stale row cannot take in a
// current reader's change.
func (s *Postgres) EnqueueReadPrompt(ctx context.Context, tx pgx.Tx, issue string, n CINews, now time.Time) (bool, error) {
	describe := fmt.Sprintf("queue a prompt to read Dispatch at %s for %s of %s", n.Head, n.Role, issue)
	if n.Purpose != CINewsState {
		return false, fmt.Errorf("%s: the row is a %s row, not a prompt", describe, n.Purpose)
	}
	n.Body = ReadPromptBody(issue, n.Head, snapshotChecks(n.States))
	if err := n.Validate(); err != nil {
		return false, fmt.Errorf("%s: %w", describe, err)
	}
	reader, err := json.Marshal(n.Reader)
	if err != nil {
		return false, fmt.Errorf("%s: %w", describe, err)
	}
	var id int64
	var stored []byte
	err = tx.QueryRow(ctx, readPromptCandidate, issue, strconv.FormatUint(n.Generation, 10), n.Task, n.Role, n.Repo, n.Head, reader, now).Scan(&id, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err := NewOutboxRow(issue, n, now)
		if err != nil {
			return false, fmt.Errorf("%s: %w", describe, err)
		}
		return false, s.Enqueue(ctx, tx, row)
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", describe, err)
	}
	decoded, err := DecodeOutboxPayload(OutboxRow{ID: id, Kind: OutboxKindCINews, Issue: issue, Payload: stored})
	if err != nil {
		return false, fmt.Errorf("%s: %w", describe, err)
	}
	pending, ok := decoded.(CINews)
	if !ok {
		return false, fmt.Errorf("%s: row %d decodes as %T", describe, id, decoded)
	}
	pending.States = joinSnapshots(pending.States, n.States)
	if n.Checks != nil {
		checks := slices.Clone(*n.Checks)
		pending.Checks = &checks
	}
	pending.Body = ReadPromptBody(issue, pending.Head, snapshotChecks(pending.States))
	if err := validateOutboxPayload(issue, pending); err != nil {
		return false, fmt.Errorf("%s: join row %d: %w", describe, id, err)
	}
	payload, err := json.Marshal(pending)
	if err != nil {
		return false, fmt.Errorf("%s: %w", describe, err)
	}
	if _, err := tx.Exec(ctx, `update outbox set payload = $2 where id = $1`, id, payload); err != nil {
		return false, fmt.Errorf("%s: join row %d: %w", describe, id, err)
	}
	return true, nil
}

// joinSnapshots is pending's snapshots with each of incoming in, by check, the higher sequence
// keeping its check's place, in the order of the checks' names.
func joinSnapshots(pending, incoming []CIStateSnapshot) []CIStateSnapshot {
	byCheck := map[string]CIStateSnapshot{}
	for _, snapshot := range slices.Concat(pending, incoming) {
		held, ok := byCheck[snapshot.Check]
		if !ok || snapshot.Seq > held.Seq {
			byCheck[snapshot.Check] = snapshot
		}
	}
	joined := make([]CIStateSnapshot, 0, len(byCheck))
	for _, snapshot := range byCheck {
		joined = append(joined, snapshot)
	}
	slices.SortFunc(joined, func(a, b CIStateSnapshot) int { return strings.Compare(a.Check, b.Check) })
	return joined
}

// snapshotChecks is the names of the checks of snapshots, in order.
func snapshotChecks(snapshots []CIStateSnapshot) []string {
	names := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		names = append(names, snapshot.Check)
	}
	return names
}

// BesideBranchesDelete deletes on GitHub the branches beside fix Fix of task Task at Generation
// (claim.BesideFixPrefix), or, where Fix is 0, beside each fix of the task at that generation from
// fix 1 to fix Through, the newest the task held when the row was queued (claim.BesideFixPrefix of
// each), once no step can push to them again or the fix took their commits
// (OutboxKindBesideBranchesDelete). A fix the task opens after the row was queued keeps its
// branches, whatever the row's retries. The daemon is its one writer, on the role-message lane, so
// no start of the issue waits behind it.
type BesideBranchesDelete struct {
	Generation uint64 `json:"generation"`
	Task       string `json:"task"`
	Fix        int    `json:"fix,omitempty"`
	// Through is, for a row of Fix 0, the newest fix the task held when the row was queued, and
	// 0 for a row of one fix. A fix the task opens later in the generation, once a later version
	// adds the task again or a move back takes it out of its retro, numbers above Through
	// (PutFix numbers each fix after the task's latest), so a row queued before it never names it.
	Through int `json:"through,omitempty"`
}

func (BesideBranchesDelete) OutboxKind() OutboxKind { return OutboxKindBesideBranchesDelete }
