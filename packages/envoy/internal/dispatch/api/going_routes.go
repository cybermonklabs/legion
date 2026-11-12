package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/swaggest/jsonschema-go"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The three routes of an answer: the POST that asks a question, the stream that carries the
// answer, and the capability read. They are two requests rather than one because the server
// wraps every POST in its idempotency layer and that layer refuses to stream (keyedWrites), and
// because a deployment of more than one pod spreads the two across pods: everything a reader of
// the stream needs is the record's (going.go), so any pod serves the stream of a run another pod
// is answering, or of a run whose pod is gone.

// codeGoingRequestNotFound is the one answer the stream gives for every run a caller may not
// read: a run the record does not hold, an id that is not a uuid, and a run that is somebody
// else's. One answer for all three is what keeps an id from telling a caller anything it did not
// already know - whether that run exists, and whether it is anyone's.
const codeGoingRequestNotFound = "GOING_REQUEST_NOT_FOUND"

// codeGoingAskerNamesNobody refuses the POST of a credential that names nobody: the
// deployment's shared token, which resolves as {} (server.go). The stream serves a run to the
// credential that asked it and to no other, and comparing identities cannot tell two holders
// of one shared token apart, so such a run would be readable by nobody - the model called, the
// deployment's paced quota spent, and every reader of it answered 404. It is refused where
// that is cheap and plain, before anything is recorded.
const codeGoingAskerNamesNobody = "GOING_ASKER_NAMES_NOBODY"

// goingStreamPoll is how often an open stream reads the record for frames the run has stored
// since its last read. The frames are rows, so a reader learns of one by reading: this is the
// delay a reader may see on a frame, and at a few reads a second over a run's window it is a
// handful of reads per open stream.
const goingStreamPoll = 200 * time.Millisecond

// goingStreamGrace is how long past a run's ends_at a stream and the writes of a run may still
// run: enough for the terminal frame of a run reaching its deadline to be stored and written to
// whoever is reading, and no more, so neither outlives its run by much. A stream of a run
// already past its ends_at - the pod that took the POST is gone - gets that grace from now,
// which is the time this reader needs to end the run itself.
const goingStreamGrace = 5 * time.Second

// Answerer generates one answer from what Dispatch holds. It is declared here, with the methods
// the server calls, rather than taken as *going.Answerer, so a test injects a fake through Deps
// without an HTTP service behind it; nil is a deployment with no model configured
// (cmd/dispatch: DISPATCH_ANSWERER unset), where a run is still recorded and its stream carries
// the error frame contracts.GoingErrorNoModelConfigured.
type Answerer interface {
	// Run answers the request, handing emit each frame of the stream in order with exactly one
	// terminal frame last; the error it answers is emit's own.
	Run(ctx context.Context, request going.Request, emit func(going.Event) error) error
	// Blocks reports whether this process's model answers with blocks, and whether that is
	// known yet: the capability route's answer.blocks (going.Answerer.Blocks).
	Blocks() (blocks, known bool)
}

// createGoingRequestInput is one question as a caller asks it: the words, the project they are
// asked of, and the item the bar's context chip names where they were typed. An empty project
// asks of every project, as search reads its own filter.
type createGoingRequestInput struct {
	Words   string             `json:"words" required:"true"`
	Project string             `json:"project"`
	Context *goingContextInput `json:"context,omitempty"`
}

// goingContextInput is the context chip as the POST carries it: the page's path, and the
// project, issue and document block that page shows, each empty where it shows none. It is a type
// of its own beside goingContext, which the run's record answers it as, because the emitter
// refuses a Go type reflected as a request and a response both.
type goingContextInput struct {
	Path    string `json:"path"`
	Project string `json:"project"`
	Issue   string `json:"issue"`
	Block   string `json:"block"`
}

// goingCancellation is what the cancel route answers: the run and when it was cancelled, the
// first cancel's moment for a run cancelled before.
type goingCancellation struct {
	ID          string    `json:"id"`
	CancelledAt time.Time `json:"cancelled_at"`
}

// goingContextPathMax and goingContextBlockMax bound the chip's two free-text fields, in UTF-16
// units: a page's path and a block id are each far shorter, and a record holds neither whole past
// these.
const (
	goingContextPathMax  = 2000
	goingContextBlockMax = 200
)

// goingCapability is what the capability route answers, and all it answers: each of the four
// things the command bar needs, apart, so the bar can tell a deployment that answers and cannot
// decide from one that does neither (R8). The models are named, since a name is what an operator
// and the production check compare with the record; no endpoint and no credential has a field
// here, so none can appear in the answer.
type goingCapability struct {
	Answer   goingAnswerCapability   `json:"answer"`
	Decision goingDecisionCapability `json:"decision"`
	Session  goingSettingCapability  `json:"session"`
	Label    goingSettingCapability  `json:"label"`
}

// goingAnswerCapability is the answering model: whether one is configured, the model it was
// configured with ("" with none), and whether it answers with blocks.
type goingAnswerCapability struct {
	Configured bool        `json:"configured"`
	Model      string      `json:"model"`
	Blocks     goingBlocks `json:"blocks"`
}

// goingDecisionCapability is the decision model: whether a measured one is configured (a name
// the record does not hold is none, R11), its name ("" with none), and whether a call to it could
// be made now (Decider.Reachable; false with none).
type goingDecisionCapability struct {
	Configured bool   `json:"configured"`
	Model      string `json:"model"`
	Reachable  bool   `json:"reachable"`
}

// goingSettingCapability is whether one of the new-issue settings is set, as the bar reads it
// now (configuredNewIssue); its value is GET /api/v1/settings/new-issue's.
type goingSettingCapability struct {
	Configured bool `json:"configured"`
}

// goingBlocks is answer.blocks: true once a structured reply was accepted, false once the model
// service refused the structured request, and "unknown" while neither has happened, which is also
// what a deployment with no answering model reads (going.Answerer.Blocks, decision 20).
type goingBlocks struct {
	blocks, known bool
}

// MarshalJSON writes the three states as the document declares them (JSONSchema).
func (b goingBlocks) MarshalJSON() ([]byte, error) {
	switch {
	case !b.known:
		return []byte(`"unknown"`), nil
	case b.blocks:
		return []byte("true"), nil
	}
	return []byte("false"), nil
}

// JSONSchema is answer.blocks as the OpenAPI document declares it: a boolean or the string
// "unknown", which no Go field type reflects into on its own.
func (goingBlocks) JSONSchema() (jsonschema.Schema, error) {
	known := jsonschema.Schema{}
	known.AddType(jsonschema.Boolean)
	unknown := jsonschema.Schema{}
	unknown.AddType(jsonschema.String)
	unknown.WithEnum("unknown")
	schema := jsonschema.Schema{}
	schema.WithDescription(`true once the model accepted a structured reply, false once its ` +
		`service refused one, "unknown" while neither has happened or with no answering model`)
	schema.WithAnyOf(known.ToSchemaOrBool(), unknown.ToSchemaOrBool())
	return schema, nil
}

// createGoingRequest records one question and starts its answer: POST /api/v1/going/requests. It
// answers the stored run, whose id addresses the stream, as soon as the row is committed, and
// the answer itself runs behind it on s.detached, which shutdown waits for (server.detached): a
// run takes contracts.GoingRunWindow, which is far too long to hold an answer for.
//
// A credential that names nobody is refused here, before anything is recorded
// (codeGoingAskerNamesNobody): the stream would serve its run to no reader at all.
//
// A deployment with no model configured records the question exactly the same way and stores the
// run's error frame instead of answering it (startGoingRun), so a caller learns that from the
// stream, where every other failure of a run is reported, rather than from a status of this
// route's own.
func (s *server) createGoingRequest(w http.ResponseWriter, r *http.Request) {
	// The caller's own credential names the writer: this route takes no `actor`, so a question
	// is recorded for whoever asked it - the person at the browser, the session a broker-issued
	// token proved, or the person a personal token belongs to - and the stream compares the
	// reader with it. Recorded, as every actor a row holds is (model.Actor.Recorded).
	caller, cookie, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	actor := caller.Recorded()
	// A credential that names nobody asks nothing: see codeGoingAskerNamesNobody. Refused here,
	// before the record, so no run is stored and no quota is spent on an answer no reader of it
	// would ever be served.
	if !goingActorNamesSomebody(actor) {
		writeError(w, codeGoingAskerNamesNobody, http.StatusForbidden,
			"this credential names nobody, so the run it asked for would be nobody's to read: ask under a credential that names a session, a person or a workload")
		return
	}
	var input createGoingRequestInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	words := strings.TrimSpace(input.Words)
	if words == "" {
		writeError(w, "GOING_INPUT", http.StatusBadRequest, "words is required: the question to answer")
		return
	}
	// Counted as search counts its own query, after trimming, so a question this route accepts
	// is one the answerer can send whole.
	if length := itemblocks.Units16(words); length > contracts.GoingWordsMax {
		capExceeded(w, "words", length, contracts.GoingWordsMax)
		return
	}
	project := strings.TrimSpace(input.Project)
	if project != "" && !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
		return
	}
	// The project comes from the body, not from the address, so this row is classed
	// scopeProjectInBody and withinProjectScope leaves the check here, as it leaves createIssue's
	// (issue_create.go). It runs before the row is written: a credential that may write one
	// project asks only of that project, and asking of every project is asking outside it.
	covers, scoped := s.callerProject(r)
	if scoped && covers != project {
		refuseWriteOutsideProjectScope(w, covers, project)
		return
	}
	chip, refused := goingContextFrom(input.Context)
	if refused != nil {
		s.writeHandlerError(w, refused)
		return
	}
	// What the run may offer is the caller's to decide, and the caller is this request's: the
	// run is detached from it, and the row keeps no credential (model.Actor.Recorded).
	credential := goingCredentialOf(caller, cookie)
	start := goingRunStart{
		chip:    chip,
		login:   goingRunLogin(caller),
		actions: s.going.runnableBy(credential),
		session: credential.session,
		reads:   s.goingReadsFor(credential, scoped),
	}
	// Detached from the request, as a broadcast's deliveries are (broadcasts.go): SIGTERM
	// cancels the server's lifetime before the HTTP drain begins, so a run bound to either the
	// request or the lifetime would be cut off with frames it had not stored, and a reader would
	// wait out the deadline for frames nothing was writing.
	//
	// On a connection mark of the run's own (store.ForConcurrentRead) and never the request's,
	// which context.WithoutCancel keeps: the run outlives this handler, and its first search
	// holds one of the pool's connections while its cursor is open (store.Pool.Query). This
	// request's own mark is still in use at that moment - keyedWrites settles the
	// Idempotency-Key with an Exec on it the moment this handler returns - and the pool refuses
	// a second connection taken under a held mark (store.ErrNestedAcquire), so a shared mark
	// would lose that settle to the run's first search and the key would store no answer: a
	// repeat would then be recorded as a second question instead of being answered this run's
	// id.
	//
	// Taken before the question is recorded, as every other caller of ForConcurrentRead answers
	// its refusal (comment_create.go): a run that cannot be detached is a run nothing would
	// answer, so recording the question first would leave the caller a 201 and a stream that
	// only runs out its deadline.
	answering, err := store.ForConcurrentRead(context.WithoutCancel(r.Context()))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	request, err := s.recordGoingRequest(r.Context(), actor, project, words)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.detached.goDetached(func() { s.startGoingRun(answering, request, start) })
	WriteJSON(w, http.StatusCreated, request)
}

// goingContextFrom is the chip the POST carried, each field trimmed, and nil where it carried
// none or named nothing; a project or an issue that is no key, or a path or block past its bound,
// is refused with the field it is.
func goingContextFrom(input *goingContextInput) (*goingContext, error) {
	if input == nil {
		return nil, nil
	}
	chip := goingContext{
		Path:    strings.TrimSpace(input.Path),
		Project: strings.TrimSpace(input.Project),
		Issue:   strings.TrimSpace(input.Issue),
		Block:   strings.TrimSpace(input.Block),
	}
	switch {
	case chip == goingContext{}:
		return nil, nil
	case chip.Project != "" && !projectKeyPattern.MatchString(chip.Project):
		return nil, errorf(http.StatusBadRequest, "INVALID_PROJECT", "%s", projectKeyProblem("context.project", chip.Project))
	case chip.Issue != "" && !issueKeyPattern.MatchString(chip.Issue):
		return nil, errorf(http.StatusBadRequest, "GOING_INPUT", "context.issue must be an issue key such as DEMO-12, or empty for none")
	case itemblocks.Units16(chip.Path) > goingContextPathMax:
		return nil, errorf(http.StatusBadRequest, "GOING_INPUT", "context.path is longer than %d characters", goingContextPathMax)
	case itemblocks.Units16(chip.Block) > goingContextBlockMax:
		return nil, errorf(http.StatusBadRequest, "GOING_INPUT", "context.block is longer than %d characters", goingContextBlockMax)
	}
	return &chip, nil
}

// goingRunLogin is the login a caller's run is kept under in going_runs: the person the
// credential acts as, read as actingPerson reads it, so the run is one GET /api/v1/me/going-runs
// answers that same caller. A credential that does not act as a person - a session's own token,
// a workload's - reads no such route, so its run keeps no record ("").
func goingRunLogin(caller model.Actor) string {
	if !caller.Credential.Holds(model.ScopePerson) {
		return ""
	}
	person := caller.ID
	if caller.Owner != nil {
		person = *caller.Owner
	}
	return model.CanonicalLogin(person)
}

// startGoingRun answers one recorded run and stores its frames. The run itself is bounded by the
// window the record gave it (goingRequest.EndsAt), which is what every reader of the run reads
// too; the writes are bounded by that window and goingStreamGrace, because the terminal frame of
// a run reaching its deadline is stored at the moment the deadline passes and a context that
// ended with the run could not store it.
//
// A run with a decider decides first and stores the decision frame before the answer is asked
// anything (decision 1), so the answer is told the actions the decision offered; once the answer
// has written, the proposal frame carries their arguments, before the frame that ends the stream.
// The run's record (going_runs) is written as the run starts and filled as it goes: a run its
// author cancels keeps what it reached and reads unfinished.
func (s *server) startGoingRun(ctx context.Context, request goingRequest, start goingRunStart) {
	storing, endStoring := context.WithDeadline(ctx, request.EndsAt.Add(goingStreamGrace))
	defer endStoring()
	// The run's own cancel, which a cancel landing on this process calls (cancelGoingRequest):
	// the decision and the answer stop at once rather than at their next frame. The record's
	// writes are on storing, so a cancelled run still keeps what it reached.
	running, cancel := context.WithCancel(storing)
	s.goingCancels.Store(request.ID, cancel)
	defer func() {
		s.goingCancels.Delete(request.ID)
		cancel()
	}()

	recorded := start.login != ""
	if recorded {
		if err := s.recordGoingRun(storing, request.ID, start.login, request.Words, start.chip); err != nil {
			slog.Warn("dispatch going: record the run", "request", request.ID, "error", err)
			recorded = false
		}
	}
	fill := func(parts goingRunParts) {
		if !recorded {
			return
		}
		if err := s.fillGoingRun(storing, request.ID, parts); err != nil {
			slog.Warn("dispatch going: fill the run's record", "request", request.ID, "error", err)
		}
	}

	if s.deps.Answerer == nil {
		if _, err := s.endGoingRun(storing, request.ID, going.Fault(contracts.GoingErrorNoModelConfigured)); err != nil {
			slog.Warn("dispatch going: store the no-model-configured frame", "request", request.ID, "error", err)
		}
		fill(goingRunParts{Finished: true})
		return
	}
	answering, endAnswering := context.WithDeadline(running, request.EndsAt)
	defer endAnswering()
	emit := func(event going.Event) error {
		if event.Terminal() {
			_, err := s.endGoingRun(storing, request.ID, event)
			return err
		}
		_, err := s.appendGoingEvent(storing, request.ID, event)
		return err
	}

	decision, decided := s.goingDecide(answering, request.Words, start)
	if running.Err() != nil {
		// Cancelled while it decided: nothing is asked of the answer.
		return
	}
	var offer going.Offer
	if decided {
		if err := emit(going.Event{Name: contracts.GoingEventDecision, Data: decision}); err != nil {
			goingRunStopped(request.ID, err)
			return
		}
		encoded, err := json.Marshal(decision)
		if err == nil {
			fill(goingRunParts{Decision: encoded})
		}
		offer = decision.offer(start)
	}

	// Every hit the answer's searches found, for the record: the search tool runs on
	// agentloop's goroutines, and the record is written from the answer's own.
	var found struct {
		sync.Mutex
		hits []going.Hit
	}
	search := s.goingSearch(request.Project)
	hits := func() []going.Hit {
		found.Lock()
		defer found.Unlock()
		return append([]going.Hit{}, found.hits...)
	}
	// The read tools the caller may call, each made as the run's author (decision 22); the
	// block's fill makes its read through the same functions, and the roster read is what the
	// roster frame reports.
	reads := s.goingRunReads(request, start)
	ended := false
	err := s.deps.Answerer.Run(answering, going.Request{
		Words: request.Words,
		Search: func(ctx context.Context, q string) ([]going.Hit, error) {
			result, err := search(ctx, q)
			found.Lock()
			found.hits = append(found.hits, result...)
			found.Unlock()
			return result, err
		},
		Bodies: s.goingBodies,
		Offer:  offer,
		Reads:  reads.funcs,
		OnOutcome: func(outcome going.Outcome) error {
			if running.Err() != nil {
				// Cancelled under the answer on this process: the answer's ending is the
				// cancel's, so the run stores no ending and no proposal and its record reads
				// unfinished, as a cancel landing on another process leaves it.
				return running.Err()
			}
			parts := goingRunParts{Hits: hits(), Finished: true}
			if outcome.StructuredOutput != "" {
				parts.StructuredOutput = &outcome.StructuredOutput
			}
			if outcome.StructuredOutput != "" {
				// After every text frame and before the proposal and the ending (R2): the block
				// the structured reply wrote, filled from its read, and how fresh the roster the
				// run read was (decision 18). Neither counts against the answer's word cut.
				if block, drawn := reads.block(answering, outcome.Final, request.Words, decision.Display.Chart); drawn {
					if err := emit(going.Event{Name: contracts.GoingEventBlock, Data: block}); err != nil {
						return err
					}
				}
				if roster, read := reads.rosterRead(); read {
					if err := emit(going.Event{Name: contracts.GoingEventRoster, Data: roster}); err != nil {
						return err
					}
				}
			}
			// A run whose answer came to nothing - it failed before the model answered or
			// refused - proposes nothing either.
			if decided && outcome.StructuredOutput != "" {
				proposal := goingPropose(decision, outcome.Final, start)
				if err := emit(going.Event{Name: contracts.GoingEventProposal, Data: proposal}); err != nil {
					return err
				}
				if encoded, err := json.Marshal(proposal); err == nil {
					parts.Proposal = encoded
				}
			}
			fill(parts)
			ended = true
			return nil
		},
	}, emit)
	if !ended {
		// Cancelled, ended under the answer, or failed before it was told anything: the record
		// keeps what the run found and reads unfinished.
		fill(goingRunParts{Hits: hits()})
	}
	goingRunStopped(request.ID, err)
}

// goingRunStopped says why a run's frames stopped short, where that is worth an operator's
// reading: nil is a run that stored its ending, and pgx.ErrNoRows a frame the record stored none
// of because a reader at the deadline or the sweep ended the run under the answer, or its author
// cancelled it (going.go). Either way the run has the one ending every reader of it is written,
// or none that whoever reads it past its ends_at writes, exactly as for a run whose pod died.
func goingRunStopped(requestID string, err error) {
	switch {
	case err == nil, errors.Is(err, pgx.ErrNoRows), errors.Is(err, context.Canceled):
	default:
		slog.Warn("dispatch going: the answer's frames could not be stored", "request", requestID, "error", err)
	}
}

// goingSearch is the one search the answerer makes, within the project the question was asked
// of: the route supplies the project, the limit and the offset, so the model cannot widen the
// search past what the caller asked of.
func (s *server) goingSearch(project string) func(context.Context, string) ([]going.Hit, error) {
	return func(ctx context.Context, q string) ([]going.Hit, error) {
		// No display tree: the answerer reads a hit's title and its href, never its markdown, so
		// reading each hit for display would be work no reader of this answer ever sees, which is
		// what these parameters leaving trees unset says.
		parameters := searchParameters{text: q, project: project, limit: searchDefaultLimit, offset: 0}
		found, err := s.runSearch(ctx, parameters)
		if err != nil {
			return nil, err
		}
		hits := make([]going.Hit, 0, len(found.Results))
		for _, result := range found.Results {
			hits = append(hits, going.Hit{Kind: result.Kind, ID: result.ID, Title: goingHitTitle(result), Href: result.Href})
		}
		return hits, nil
	}
}

// goingHitTitle names one hit to the model and to the reader: an issue's own title for a hit the
// issue owns, and the document's name for every other, which is what search shows each of them
// under.
func goingHitTitle(result model.SearchResult) string {
	if result.Owner.Kind == "issue" {
		return result.Owner.Title
	}
	return result.Owner.Name
}

// goingBodies is each hit's own stored text, the text search ranked, cut to
// contracts.GoingHitRuneMax runes a hit and keyed as going.Hit.Key keys it (goingItemText).
func (s *server) goingBodies(ctx context.Context, hits []going.Hit) (map[string]string, error) {
	return goingItemText(ctx, s.deps.Store.Pool, hits, contracts.GoingHitRuneMax)
}

// streamGoingAnswer writes one run's answer as server-sent events: GET
// /api/v1/going/requests/{id}/stream. It serves the record and not the process that took the
// POST, so the stream of a run this pod never ran is the same stream, and a reader that drops
// resumes from the frame it holds (Last-Event-ID).
//
// A run past its ends_at with no terminal frame stored is ended by this reader, with the
// deadline's error, and the stream ends on whatever terminal frame that write answers
// (endGoingRun settles which one). That is how a stream ends when the pod that took the POST is
// gone: nothing else would ever store that run's ending until the sweep ran.
func (s *server) streamGoingAnswer(w http.ResponseWriter, r *http.Request) {
	request, ok := s.readCallersGoingRequest(w, r)
	if !ok {
		return
	}
	id := request.ID
	after := goingResumeFrom(r)

	// Past this point the answer is a stream: its status and headers are written before the
	// first frame is read, and a failure after them can only end the stream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	stream := http.NewResponseController(w)
	if err := stream.Flush(); err != nil {
		return
	}

	// The stream lives as long as the run and goingStreamGrace, and at least that grace from
	// now, so a run already past its ends_at has the time this reader needs to end it; the
	// caller's own going away ends it sooner.
	until := request.EndsAt.Add(goingStreamGrace)
	if soonest := time.Now().Add(goingStreamGrace); until.Before(soonest) {
		until = soonest
	}
	ctx, done := context.WithDeadline(r.Context(), until)
	defer done()
	for {
		frames, state, err := s.readGoingStream(ctx, id, after)
		if err != nil {
			// pgx.ErrNoRows is the sweep having deleted the run under this reader
			// (contracts.GoingRequestTTL); either way there is nothing left to read.
			return
		}
		for _, frame := range frames {
			if err := writeGoingFrame(w, frame); err != nil {
				return
			}
			after = frame.Sequence
			if err := stream.Flush(); err != nil {
				return
			}
			if frame.Event.Terminal() {
				return
			}
		}
		switch {
		case state.Terminal > 0:
			// The run has stored its one ending. This reader stops on having written that
			// frame, never on the run merely holding one: a terminal frame committed between
			// the read of the frames and the read of this state is not in hand yet, and one
			// more poll answers it, with every frame before it. A reader already past that
			// sequence is at the end of a stream that is over.
			if after >= state.Terminal {
				return
			}
		case state.PastDeadline:
			if _, err := s.endGoingRun(ctx, id, going.Fault(contracts.GoingErrorAnswerDidNotArrive)); err != nil {
				slog.Warn("dispatch going: end the run at its deadline", "request", id, "error", err)
				return
			}
			// Read again rather than writing that frame here: the answer may have stored frames
			// between this reader's last read and its ending, and they belong to the stream
			// before the frame that ends it.
		default:
			select {
			case <-ctx.Done():
				return
			case <-time.After(goingStreamPoll):
			}
		}
	}
}

// goingActorNamesSomebody reports whether an actor names any caller at all: a session or person
// id, the person a personal token belongs to, or the workload a service-account token proved. A
// credential that names none of them - the deployment's shared token, which resolves as {} -
// is nobody: comparing identities cannot tell two holders of that one token apart, so a run it
// asked for would be a run every holder could claim and none is served.
//
// It is asked twice, of the asker and of the run's author. The POST refuses an asker that
// names nobody (codeGoingAskerNamesNobody) before anything is recorded, so no row this route
// writes has such an author. The stream asks it of the author it read all the same, because
// model.Actor.SameCredential reports two credentials that name nobody as one: that comparison
// alone would serve a row authored by {} to every holder of the shared token, and the stream
// serves a run to the credential that asked it and to no other.
func goingActorNamesSomebody(actor model.Actor) bool {
	return actor.ID != "" || actor.Owner != nil || actor.Service != nil
}

// goingResumeFrom is the sequence a reader holds, from the Last-Event-ID header the stream's own
// `id:` lines set: the stream answers the frames past it. Anything that is not a sequence this
// stream could have written is no resumption point, and the whole stream is answered, which is
// what a reader with nothing in hand is answered too.
func goingResumeFrom(r *http.Request) int {
	resume, err := strconv.Atoi(strings.TrimSpace(r.Header.Get("Last-Event-ID")))
	// Past math.MaxInt32 is no sequence this stream could have written either: going_events
	// holds its sequence as an integer, so a larger value cannot bind against that column and
	// the read that carries the stream would fail rather than answer the frames past it.
	if err != nil || resume < 0 || resume > math.MaxInt32 {
		return 0
	}
	return resume
}

// writeGoingFrame writes one stored frame as server-sent events: its sequence as the frame's id,
// so a reader that drops resumes after it, its name as the event, and the stored JSON as the
// data. The payload is the bytes the record holds (scanGoingEvent keeps them), so the frame a
// reader is written is the frame that was stored.
func writeGoingFrame(w http.ResponseWriter, frame goingEvent) error {
	data, err := json.Marshal(frame.Event.Data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", frame.Sequence, frame.Event.Name, data)
	return err
}

// readCallersGoingRequest is the run the path's {id} names when it is the caller's own, and
// false once it has answered the request: 404 GOING_REQUEST_NOT_FOUND for a run the record does
// not hold, an id that is not a uuid, and a run that is another caller's, which are one answer
// (codeGoingRequestNotFound). The stream and the cancel read a run through it alike.
func (s *server) readCallersGoingRequest(w http.ResponseWriter, r *http.Request) (goingRequest, bool) {
	// Resolved once rather than through requireAuthenticated and a second resolution: the actor
	// is what a run's author is compared with, and a credential resolves at a cost - a token
	// row, the sign-in issuer's confirmation - worth paying once.
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return goingRequest{}, false
	}
	id := r.PathValue("id")
	// Decided before the read, not after: readGoingRequest casts $1::uuid, so an id that is not
	// a uuid comes back as a Postgres error rather than pgx.ErrNoRows, and an id nobody could
	// hold is answered exactly as one nobody does.
	if _, err := uuid.Parse(id); err != nil {
		writeGoingRequestNotFound(w)
		return goingRequest{}, false
	}
	request, err := s.readGoingRequest(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeGoingRequestNotFound(w)
		return goingRequest{}, false
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return goingRequest{}, false
	}
	// The whole credential, through model.Actor.SameCredential and never SameAs alone, which
	// would make any two personal tokens one writer: a run is the credential's that asked it.
	// A run whose author names nobody is nobody's, and that is a test of its own because
	// SameCredential calls two credentials that name nobody the one credential: without it,
	// such a row would be served to every holder of the deployment's shared token, which
	// resolves as {} exactly as that author does.
	if !goingActorNamesSomebody(request.Author) || !request.Author.SameCredential(actor) {
		writeGoingRequestNotFound(w)
		return goingRequest{}, false
	}
	return request, true
}

// cancelGoingRequest cancels one run: POST /api/v1/going/requests/{id}/cancel (decision 10). The
// bar sends it on the first keystroke after a run began and when it closes, so a stale answer
// stops rather than arriving later. Only the run's author may cancel it, and every other caller
// is answered as the stream answers a stranger. The record marks the run cancelled, so whichever
// process is answering it stores no further frame and its answer ends at its next delta; a run
// this process is answering is ended at once besides. Cancelling a run that has already ended,
// or one cancelled before, changes nothing and answers the first cancel's moment.
func (s *server) cancelGoingRequest(w http.ResponseWriter, r *http.Request) {
	request, ok := s.readCallersGoingRequest(w, r)
	if !ok {
		return
	}
	cancelled, err := s.cancelGoingRun(r.Context(), request.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The sweep deleted the run between the read and the cancel: there is nothing left to
		// stop, and nothing of it to read.
		writeGoingRequestNotFound(w)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if cancel, running := s.goingCancels.Load(request.ID); running {
		cancel.(context.CancelFunc)()
	}
	WriteJSON(w, http.StatusOK, goingCancellation{ID: request.ID, CancelledAt: cancelled})
}

// writeGoingRequestNotFound is the stream's one refusal; see codeGoingRequestNotFound for why
// every run a caller may not read is answered with it.
func writeGoingRequestNotFound(w http.ResponseWriter) {
	writeError(w, codeGoingRequestNotFound, http.StatusNotFound, "no answer run of that id is yours to read")
}

// getGoingCapability answers what the command bar can do here, each part apart:
// GET /api/v1/going/capability. Any caller Dispatch admits may read it, which is why the answer
// names the models and nothing of where they are served - no endpoint and no credential. It calls
// no model: answer.blocks is what this process's runs have learned so far, and
// decision.reachable one read of the decision model's token file.
func (s *server) getGoingCapability(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var capability goingCapability
	if s.deps.Answerer != nil {
		blocks, known := s.deps.Answerer.Blocks()
		capability.Answer = goingAnswerCapability{Configured: true, Model: s.deps.AnswerModel, Blocks: goingBlocks{blocks: blocks, known: known}}
	}
	if decider := s.deps.Decider; decider != nil {
		capability.Decision = goingDecisionCapability{Configured: true, Model: decider.Model(), Reachable: decider.Reachable(r.Context())}
	}
	newIssue := s.configuredNewIssue()
	capability.Session.Configured = newIssue.Session != ""
	capability.Label.Configured = newIssue.Label != ""
	WriteJSON(w, http.StatusOK, capability)
}
