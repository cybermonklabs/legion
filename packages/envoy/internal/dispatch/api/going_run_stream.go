package api

// going_run_stream.go is a command-bar run as its person meets it over HTTP: POST
// /api/v1/going/runs starts one, GET /api/v1/going/runs/{id} reads it, GET
// /api/v1/going/runs/{id}/events streams the frames it stores (going_run_events), POST
// /api/v1/going/runs/{id}/stop stops it, and GET /api/v1/me/going-plans lists the runs whose plan
// waits for their person, which the Inbox shows. Only the run's person reads or stops it; any
// other caller is answered the run's 404 (writeGoingRunNotFound).
//
// A stream serves the record and not the process that runs the run, so any process serves the
// stream of any run, and a reader that drops resumes after the last sequence it holds
// (Last-Event-ID). Each transaction that appends frames announces the run on
// GoingRunEventsChannel (appendGoingRunFrames), which wakes the run's readers on every process.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// GoingRunEventsChannel is the NOTIFY channel each transaction that appends a run's frames
// announces the run on, its id the payload, which cmd/dispatch registers on its listener hub for
// the GoingRunEventSignal it hands Deps.
const GoingRunEventsChannel = "dispatch_going_run_events"

// The bounds of a run's stream (background-runs design, section 5, "Readers of a run's events").
const (
	// goingReadersPerRun is how many streams of one run one process serves at once.
	goingReadersPerRun = 4
	// goingReadersPerPerson is how many streams of one person's runs one process serves at once.
	goingReadersPerPerson = 20
	// goingRunStreamIdle is how long a stream stays open with no frame to write: then it writes
	// a keepalive frame and closes, so a run that waits for its person holds no connection for
	// ever, and the reader opens it again from its last sequence.
	goingRunStreamIdle = 5 * time.Minute
	// goingRunStreamCheck is how often an open stream reads the run again with no announcement:
	// a run another process ended with no frame of its own (an agent killed past its bound) closes
	// its stream by then.
	goingRunStreamCheck = 15 * time.Second
	// goingRunStreamBatch is the most frames one read of a stream answers; a stream behind by more
	// reads again at once.
	goingRunStreamBatch = 256
)

// The run routes' own refusals.
const (
	// codeGoingRunReadersFull refuses a stream past goingReadersPerRun or goingReadersPerPerson.
	codeGoingRunReadersFull = "GOING_RUN_READERS_FULL"
	// codeGoingRunNotLatest refuses a follow-up of a run a later run of its conversation follows.
	codeGoingRunNotLatest = "GOING_RUN_NOT_LATEST"
	// codeGoingRunNotEnded refuses a follow-up of a run that is queued, works or waits.
	codeGoingRunNotEnded = "GOING_RUN_NOT_ENDED"
)

// goingFrameKeepalive is the frame a stream that was idle for goingRunStreamIdle writes before it
// closes. It carries no id, so the reader's Last-Event-ID stays the last frame it was written.
const goingFrameKeepalive = "keepalive"

// goingStoppedByPerson is the stop reason of a run its person stopped.
const goingStoppedByPerson = "STOPPED: its person stopped it"

// GoingRunEventSignal wakes the streams this process serves of one run at each announcement of
// that run on GoingRunEventsChannel: cmd/dispatch registers Notify on its listener hub for it, and
// Wake for each LISTEN the hub makes, so an announcement the hub missed while it had no connection
// wakes every stream. Each stream reads the store again when woken. A nil signal in Deps is a
// server no announcement reaches, which register gives a signal of its own, so its streams read
// again every goingRunStreamCheck.
type GoingRunEventSignal struct {
	mu   sync.Mutex
	runs map[string]*goingRunWaiters
}

// goingRunWaiters is the channel the streams of one run wait on, closed at the run's next
// announcement, and how many of them wait on it.
type goingRunWaiters struct {
	woken   chan struct{}
	waiting int
}

// NewGoingRunEventSignal is a signal nothing has woken yet.
func NewGoingRunEventSignal() *GoingRunEventSignal {
	return &GoingRunEventSignal{runs: map[string]*goingRunWaiters{}}
}

// Notify wakes the streams of the run payload names. It never blocks, as a hub's handler must
// not.
func (g *GoingRunEventSignal) Notify(run string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if waiters := g.runs[run]; waiters != nil {
		close(waiters.woken)
		delete(g.runs, run)
	}
}

// Wake wakes every stream. It never blocks.
func (g *GoingRunEventSignal) Wake() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, waiters := range g.runs {
		close(waiters.woken)
	}
	g.runs = map[string]*goingRunWaiters{}
}

// wait is a channel closed at run's next announcement, and the release a stream calls once it no
// longer waits on it. A stream takes it before it reads the store, so an announcement between its
// read and its wait is not lost.
func (g *GoingRunEventSignal) wait(run string) (<-chan struct{}, func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	waiters := g.runs[run]
	if waiters == nil {
		waiters = &goingRunWaiters{woken: make(chan struct{})}
		g.runs[run] = waiters
	}
	waiters.waiting++
	return waiters.woken, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		waiters.waiting--
		if waiters.waiting == 0 && g.runs[run] == waiters {
			delete(g.runs, run)
		}
	}
}

// goingRunReaders counts the streams this process serves, by run and by person, so the next past
// goingReadersPerRun or goingReadersPerPerson is refused. It counts this process alone: what it
// bounds is what one process holds open.
type goingRunReaders struct {
	mu       sync.Mutex
	byRun    map[string]int
	byPerson map[string]int
}

// admit counts one more stream of login's run, and answers its release; or, past a bound, no
// release and the refusal's words.
func (g *goingRunReaders) admit(run, login string) (func(), string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byRun == nil {
		g.byRun, g.byPerson = map[string]int{}, map[string]int{}
	}
	switch {
	case g.byRun[run] >= goingReadersPerRun:
		return nil, fmt.Sprintf("this run already has %d streams open, the most one run may have: close one of them, or read it from where one stands", goingReadersPerRun)
	case g.byPerson[login] >= goingReadersPerPerson:
		return nil, fmt.Sprintf("you already have %d streams of your runs open, the most one person may have: close one of them", goingReadersPerPerson)
	}
	g.byRun[run]++
	g.byPerson[login]++
	return sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.byRun[run]--; g.byRun[run] == 0 {
			delete(g.byRun, run)
		}
		if g.byPerson[login]--; g.byPerson[login] == 0 {
			delete(g.byPerson, login)
		}
	}), ""
}

// goingRunInput is POST /api/v1/going/runs's body: the words, the context chip, the run's kind
// (empty for a run of the words, new_issue for the line File as a new issue, a Fast run that files
// the words as one new issue at once: going_new_issue.go), the level (Fast, Standard or Deep;
// Standard where it is empty, and Fast alone for a new-issue run), the mode (the person's setting
// where it is empty), and the run the new one follows (empty for the first run of a
// conversation).
type goingRunInput struct {
	Words     string             `json:"words" required:"true"`
	Context   *goingContextInput `json:"context"`
	Kind      string             `json:"kind" enum:"new_issue"`
	Level     string             `json:"level" enum:"fast,standard,deep"`
	Mode      string             `json:"mode" enum:"ask,auto_review"`
	Continues string             `json:"continues"`
}

// createGoingRun is POST /api/v1/going/runs: the caller starts a run. The run's grant is the
// request's own credential (goingGrantOf), which must be a person's own sign-in or personal token:
// a broker-issued token, the deployment's shared token and a service-account token are refused
// 403 RUN_NEEDS_PERSON_CREDENTIAL, before anything is written. A follow-up names the run it
// continues, which must be the caller's own, the newest run of its conversation and ended; it
// joins that run's conversation. The run is written queued, with its grant, under the person's
// run lock, which refuses it 429 GOING_RUN_QUEUE_FULL where the person has as many runs queued as
// one may (admitGoingRun), and announced on GoingRunsChannel so a waiting claim takes it.
func (s *server) createGoingRun(w http.ResponseWriter, r *http.Request) {
	grant, err := s.goingGrantOf(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	// Past the grant, the credential is a person's: one that does not act as its person (a token
	// without the person scope) starts nothing, as every route of a person's own record refuses it.
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	var input goingRunInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	words := strings.TrimSpace(input.Words)
	if words == "" {
		writeError(w, codeGoingInput, http.StatusBadRequest, "words is required: what the run is to do")
		return
	}
	if length := itemblocks.Units16(words); length > contracts.GoingWordsMax {
		capExceeded(w, "words", length, contracts.GoingWordsMax)
		return
	}
	chip, refused := goingContextFrom(input.Context)
	if refused != nil {
		s.writeHandlerError(w, refused)
		return
	}
	level := input.Level
	switch {
	case input.Kind != "" && input.Kind != agentwire.RunNewIssue:
		writeError(w, codeGoingInput, http.StatusBadRequest, fmt.Sprintf("kind %q is not %s: leave it out for a run of the words", input.Kind, agentwire.RunNewIssue))
		return
	case input.Kind == agentwire.RunNewIssue && level != "" && level != goingNewIssueLevel:
		writeError(w, codeGoingInput, http.StatusBadRequest, fmt.Sprintf("a new-issue run is Fast, not %s", level))
		return
	case input.Kind == agentwire.RunNewIssue:
		level = goingNewIssueLevel
	case level == "":
		level = "standard"
	}
	if _, known := agent.Levels[level]; !known {
		writeError(w, codeGoingInput, http.StatusBadRequest, fmt.Sprintf("level %q is none of fast, standard and deep", level))
		return
	}
	mode := input.Mode
	switch mode {
	case "":
		settings, err := s.readGoingSettings(r.Context(), grant.login)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		mode = settings.Mode
	case goingModeAsk, goingModeAutoReview:
	default:
		writeError(w, codeGoingInput, http.StatusBadRequest, fmt.Sprintf("mode %q is neither ask nor auto_review", mode))
		return
	}
	run := goingNewRun{id: uuid.NewString(), login: grant.login, words: words, chip: chip, kind: input.Kind, level: level, mode: mode}
	run.conversation = run.id
	var follows string
	if continues := strings.TrimSpace(input.Continues); continues != "" {
		parsed, err := uuid.Parse(continues)
		if err != nil {
			writeGoingRunNotFound(w)
			return
		}
		follows = parsed.String()
		run.continues = &follows
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	if err := admitGoingRun(r.Context(), tx, grant.login); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if run.continues != nil {
		// Under the person's run lock, which every start of a run of theirs takes: two follow-ups
		// of one run cannot both find it the newest of its conversation.
		var state string
		var newest bool
		err := tx.QueryRow(r.Context(), `
			select coalesce(r.conversation_id, r.id)::text, r.state,
			  not exists (select 1 from going_runs n where n.conversation_id = coalesce(r.conversation_id, r.id) and n.created_at > r.created_at)
			from going_runs r
			where r.id = $1::uuid and r.author_login = $2 and r.state is not null and `+goingConversationListed(3),
			follows, grant.login, goingRunsKept.Seconds()).Scan(&run.conversation, &state, &newest)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeGoingRunNotFound(w)
			return
		case err != nil:
			s.writeHandlerError(w, fmt.Errorf("read the run this one follows: %w", err))
			return
		case !newest:
			writeError(w, codeGoingRunNotLatest, http.StatusConflict,
				"a later run of this conversation follows that run: continue the conversation from its newest run")
			return
		case !goingRunEnded(state):
			writeError(w, codeGoingRunNotEnded, http.StatusConflict,
				fmt.Sprintf("that run is %s: stop it, or answer its plan, before you follow it up", state))
			return
		}
	}
	started, err := insertGoingRun(r.Context(), tx, run)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := recordGoingGrant(r.Context(), tx, run.id, grant); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Delivered when the transaction commits, so no claim reads the queue before the run is in it.
	if _, err := tx.Exec(r.Context(), `select pg_notify($1, $2)`, GoingRunsChannel, "ready"); err != nil {
		s.writeHandlerError(w, fmt.Errorf("announce the run: %w", err))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, started)
}

// goingRunEnded reports whether a run in state has ended: done, stopped or failed.
func goingRunEnded(state string) bool {
	switch state {
	case agentwire.StateDone, agentwire.StateStopped, agentwire.StateFailed:
		return true
	}
	return false
}

// readCallersGoingRun is the caller's own run the path's {id} names, with the caller's canonical
// login, or false once it has answered the request: the run's 404 for an id that is no uuid, a run
// of anyone else's, one the record does not hold and one History no longer lists.
func (s *server) readCallersGoingRun(w http.ResponseWriter, r *http.Request) (goingRun, string, bool) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return goingRun{}, "", false
	}
	login := model.CanonicalLogin(caller.ID)
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeGoingRunNotFound(w)
		return goingRun{}, "", false
	}
	run, err := readGoingRun(r.Context(), s.deps.Store.Pool, id.String(), login)
	if errors.Is(err, pgx.ErrNoRows) {
		writeGoingRunNotFound(w)
		return goingRun{}, "", false
	}
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the run: %w", err))
		return goingRun{}, "", false
	}
	return run, login, true
}

// getGoingRun is GET /api/v1/going/runs/{id}: the caller's own run.
func (s *server) getGoingRun(w http.ResponseWriter, r *http.Request) {
	run, _, ok := s.readCallersGoingRun(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, run)
}

// stopMyGoingRun is POST /api/v1/going/runs/{id}/stop: the caller stops their own run. A run that
// is queued, works or waits is set stopped with its lease freed, and its agent's waiting renew is
// answered at once, so the agent cancels the run's model call in flight (stopGoingRun); what the
// run did stays. It answers the run as it then stands: a run that had ended already is answered as
// it ended, unchanged.
func (s *server) stopMyGoingRun(w http.ResponseWriter, r *http.Request) {
	run, login, ok := s.readCallersGoingRun(w, r)
	if !ok {
		return
	}
	if _, err := s.stopGoingRun(r.Context(), run.ID, goingStoppedByPerson); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	stopped, err := readGoingRun(r.Context(), s.deps.Store.Pool, run.ID, login)
	if errors.Is(err, pgx.ErrNoRows) {
		writeGoingRunNotFound(w)
		return
	}
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the run stopped: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, stopped)
}

// goingRunFrame is one stored frame of a run's events: its sequence, its name and its data, the
// bytes the record holds.
type goingRunFrame struct {
	sequence int64
	name     string
	data     []byte
}

// readGoingRunFrames is the run's state and the frames it stored after sequence after, at most
// goingRunStreamBatch of them, in one statement, so a state that has ended is read with every frame
// the transaction that ended it stored. pgx.ErrNoRows is a run the record no longer holds.
func (s *server) readGoingRunFrames(ctx context.Context, run string, after int64) (string, []goingRunFrame, error) {
	// Check c25's mutant: a run's events are its going_requests row's, gone once that row is.
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select r.state, e.sequence, e.name, e.data
		from going_runs r
		left join lateral (
			select sequence, name, data from going_run_events
			where run_id = r.id and sequence > $2 and exists (select 1 from going_requests q where q.id = r.id)
			order by sequence
			limit $3
		) e on true
		where r.id = $1::uuid
		order by e.sequence
	`, run, after, goingRunStreamBatch)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var state string
	var frames []goingRunFrame
	read := false
	for rows.Next() {
		var sequence *int64
		var name *string
		var data []byte
		if err := rows.Scan(&state, &sequence, &name, &data); err != nil {
			return "", nil, err
		}
		read = true
		if sequence != nil {
			frames = append(frames, goingRunFrame{sequence: *sequence, name: *name, data: data})
		}
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if !read {
		return "", nil, pgx.ErrNoRows
	}
	return state, frames, nil
}

// streamGoingRunEvents is GET /api/v1/going/runs/{id}/events: the frames of the caller's own run
// as server-sent events, each `id:` its sequence, `event:` its name and `data:` its stored JSON,
// from the sequence after the reader's Last-Event-ID. It writes each frame as the run stores it,
// woken by the run's announcement on GoingRunEventsChannel, and ends once the run has ended and
// every frame it stored is written. A stream with no frame to write for goingRunStreamIdle writes
// a keepalive frame and ends, and the reader opens it again from its last sequence. Past
// goingReadersPerRun streams of the run, or goingReadersPerPerson of the person's runs, on this
// process, the next is refused 429 GOING_RUN_READERS_FULL.
func (s *server) streamGoingRunEvents(w http.ResponseWriter, r *http.Request) {
	run, login, ok := s.readCallersGoingRun(w, r)
	if !ok {
		return
	}
	release, refusal := s.goingReaders.admit(run.ID, login)
	if refusal != "" {
		writeError(w, codeGoingRunReadersFull, http.StatusTooManyRequests, refusal)
		return
	}
	defer release()
	after := int64(goingResumeFrom(r))

	// Past this point the answer is a stream: its status and headers are written before the first
	// frame is read, and a failure after them can only end the stream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	stream := http.NewResponseController(w)
	if err := stream.Flush(); err != nil {
		return
	}
	idle := time.NewTimer(goingRunStreamIdle)
	defer idle.Stop()
	check := time.NewTicker(goingRunStreamCheck)
	defer check.Stop()
	// pass is one read of the run's frames and one wait for its next: false once the stream ends.
	// The pass's wait is released as it returns, whichever way it does.
	pass := func() bool {
		woken, done := s.goingRunEvents.wait(run.ID)
		defer done()
		state, frames, err := s.readGoingRunFrames(r.Context(), run.ID, after)
		if err != nil {
			// pgx.ErrNoRows is the sweep having deleted the run's conversation under this
			// reader; either way there is nothing left to read.
			return false
		}
		for _, frame := range frames {
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", frame.sequence, frame.name, frame.data); err != nil {
				return false
			}
			after = frame.sequence
		}
		if len(frames) > 0 {
			if err := stream.Flush(); err != nil {
				return false
			}
			idle.Reset(goingRunStreamIdle)
		}
		if len(frames) == goingRunStreamBatch {
			return true
		}
		if goingRunEnded(state) {
			return false
		}
		select {
		case <-woken:
		case <-check.C:
		case <-idle.C:
			_, _ = fmt.Fprintf(w, "event: %s\ndata: {}\n\n", goingFrameKeepalive)
			_ = stream.Flush()
			return false
		case <-r.Context().Done():
			return false
		case <-s.lifetime.Done():
			return false
		}
		return true
	}
	for pass() {
	}
}

// goingWaitingPlan is one run whose plan waits for its person, as the Inbox shows it: the run, its
// conversation and words, the plan's number and summary, how many of its rows wait, when the plan
// was made, and its version as the plan's own answer names it (goingPlanAnswer.Version), which an
// approval from the Inbox names. The plan itself is GET /api/v1/going/runs/{id}/plan.
type goingWaitingPlan struct {
	Run          string    `json:"run"`
	Conversation string    `json:"conversation"`
	Words        string    `json:"words"`
	Plan         int       `json:"plan"`
	Summary      string    `json:"summary"`
	Waiting      int       `json:"waiting"`
	CreatedAt    time.Time `json:"created_at"`
	Version      string    `json:"version"`
}

// goingWaitingPlansPage is a page of the caller's waiting plans, the newest first, paged as GET
// /api/v1/me/answers is.
type goingWaitingPlansPage struct {
	Rows   []goingWaitingPlan `json:"rows"`
	Total  int                `json:"total"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

// listMyGoingPlans is GET /api/v1/me/going-plans: the caller's own runs whose latest plan waits
// for them, while History lists their conversation, the newest plan first. It is the Inbox's read
// of the plans; each change of what it lists appends going_plan.updated naming the person
// (appendGoingPlanEvent), on which the Inbox reads it again. The page is two round trips: the list,
// and every listed plan's rows in one read (readGoingPlanRows), from which each row counts the
// plan's waiting rows and names its version as the plan's own answer does, the version POST
// /api/v1/going/runs/{id}/plan/approve compares.
func (s *server) listMyGoingPlans(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select r.id::text, coalesce(r.conversation_id, r.id)::text, r.words, coalesce(r.mode, $5), p.plan, p.state, p.summary,
		  p.created_at, count(*) over ()
		from going_runs r
		join lateral (select plan, summary, state, created_at from going_plans where run_id = r.id order by plan desc limit 1) p on true
		where r.author_login = $1 and r.state = 'waiting' and p.state = 'waiting' and `+goingConversationListed(2)+`
		order by p.created_at desc, r.id desc
		limit $3 offset $4
	`, model.CanonicalLogin(caller.ID), goingRunsKept.Seconds(), limit, offset, goingModeAsk)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my waiting plans: %w", err))
		return
	}
	type listed struct {
		plan   goingWaitingPlan
		mode   string
		stored goingPlanStored
		total  int64
	}
	plans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (listed, error) {
		var one listed
		err := row.Scan(&one.plan.Run, &one.plan.Conversation, &one.plan.Words, &one.mode, &one.stored.plan, &one.stored.state,
			&one.stored.summary, &one.plan.CreatedAt, &one.total)
		return one, err
	})
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my waiting plans: %w", err))
		return
	}
	keys := make([]goingPlanKey, 0, len(plans))
	for _, one := range plans {
		keys = append(keys, goingPlanKey{run: one.plan.Run, plan: one.stored.plan})
	}
	planRows, err := readGoingPlanRows(r.Context(), s.deps.Store.Pool, keys)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my waiting plans: %w", err))
		return
	}
	page := goingWaitingPlansPage{Rows: make([]goingWaitingPlan, 0, len(plans)), Limit: limit, Offset: offset}
	for _, one := range plans {
		one.stored.rows = planRows[goingPlanKey{run: one.plan.Run, plan: one.stored.plan}]
		for _, row := range one.stored.rows {
			if row.state == "waiting" {
				one.plan.Waiting++
			}
		}
		one.plan.Plan, one.plan.Summary = one.stored.plan, one.stored.summary
		one.plan.Version = one.stored.answer(one.plan.Run, one.mode).Version
		page.Rows, page.Total = append(page.Rows, one.plan), int(one.total)
	}
	WriteJSON(w, http.StatusOK, page)
}

// goingPlanActor is the actor of a going_plan.updated of login's run: the person, through the
// command bar, for that run.
func goingPlanActor(login, run string) model.Actor {
	return model.Actor{Kind: "user", ID: login, Act: &model.ActorAct{Agent: model.ActAgentCommandBar, Run: run}}
}

// appendGoingPlanEvent appends going_plan.updated naming login in tx, the transaction that changed
// what login's plans wait on - a run of theirs began to wait, or a waiting run was answered or
// stopped - after it wrote the run's row. The caller publishes the event once tx commits.
func (s *server) appendGoingPlanEvent(ctx context.Context, tx pgx.Tx, login, run string) (model.Event, error) {
	event, err := s.appendEvent(ctx, tx, model.Event{
		Type: "going_plan.updated", Actor: goingPlanActor(login, run), Payload: model.GoingPlanEventPayload{Login: login},
	})
	if err != nil {
		return model.Event{}, fmt.Errorf("append the plan's event: %w", err)
	}
	return event, nil
}
