package api

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/rules"
)

// writing_check.go puts the writing check (INFRA-3231) on the server's write path. A route whose
// request carries prose (writing_check_fields.go) calls writingCheck once the request is decoded
// and its writer resolved, and before it opens a transaction, takes a lock or writes anything: the
// check reads the record to decide the write's reader, and no database transaction waits on it.
// The check refuses no write: every write it reads goes on. Every decision the check makes for a
// write that took place is one row of writing_check_events. The check keeps the row on the
// request (keepWritingCheck), and the route's layer (recordTakenWrites) inserts it once the
// handler answered the write 2xx, outside the write's transaction, which has ended by then, and
// before the request ends. A write the route refused after the check - a closed issue, an unknown
// check, a delivery never made - records nothing, as it writes nothing else. A call that a route
// answers 2xx with none of the words the check read written records none either, since the route
// drops its rows first
// (dropWritingCheck): on POST /api/v1/messages/{id}/reply and POST /api/v1/comments/{id}/reply,
// a call on an attempt already answered that posts nothing and an error report repeated on an
// attempt already failed; on the three edit routes, POST /api/v1/artifacts/{id}/edits,
// POST /api/v1/issues/{key}/artifacts/{slug}/edits and
// POST /api/v1/projects/{key}/artifacts/{slug}/edits, a batch that leaves the document as it was;
// and on POST /api/v1/artifacts/{id}/approval-requests, a request whose every document already
// stands approved at its current version. A failed insert is logged and fails nothing.

// The outcomes writingCheck records besides the checker's own (writingcheck.Outcome*) and the
// actor rule's exemptions (writingcheck.Exempt*).
const (
	// writingCheckFindings is a write in which the check found at least one sentence that breaks
	// a rule; the write went on, and the row names the rules and the fields.
	writingCheckFindings = "findings"
	// writingCheckUnreadUpload is an uploaded file the check does not read: any upload that is
	// not stored as a Markdown document.
	writingCheckUnreadUpload = "exempt_unread_upload"
	// writingCheckNoReader is an agent's write whose text no person reads
	// (writingCheckReader): a send to an agent, a broadcast, a reply between two sessions, or a
	// record that names nobody. The check does not read it.
	writingCheckNoReader = "exempt_no_reader"
	// writingCheckGenerated is text a plugin generated rather than an agent wrote, posted on one
	// of generatedTextRoutes with contracts.GeneratedTextHeader. The check does not read it.
	writingCheckGenerated = "exempt_generated"
	// writingCheckPersonTyped is a write a person typed themselves in a session: the write
	// carried contracts.PersonTypedHeader. Neither the check nor the reviewer reads it.
	writingCheckPersonTyped = "exempt_person_typed"
)

// generatedTextRoutes are the routes a plugin posts the text it generates on (a person's
// !command report, a slash command's output, a plugin's error reply): the replies to a delivered
// message and to a delivered mention. The server reads contracts.GeneratedTextHeader on these two
// routes alone.
var generatedTextRoutes = []string{"POST /api/v1/messages/{id}/reply", "POST /api/v1/comments/{id}/reply"}

// generatedText reports whether r is a plugin's generated text: a write on one of
// generatedTextRoutes whose contracts.GeneratedTextHeader is "1".
func generatedText(r *http.Request, pattern string) bool {
	return r.Header.Get(contracts.GeneratedTextHeader) == "1" && slices.Contains(generatedTextRoutes, pattern)
}

// personTypedWrite reports whether r is a write a person typed in a session (Decision 26 of
// INFRA-3231's plan): its contracts.PersonTypedHeader is "1", which the dispatch command sends on
// every write of a command pi-envoy's typed shell ran for a person. The server reads it on every
// write route: the write posts at once, with no hold and no review whatever the reviewer's
// settings say, is recorded exempt_person_typed, and its events carry person_typed
// (markPersonTyped). Like the generated-text header it is the client's to set: a session that
// sends it itself skips the review, and each use is counted with its session.
func personTypedWrite(r *http.Request) bool {
	return r.Method != http.MethodGet && r.Header.Get(contracts.PersonTypedHeader) == "1"
}

// markPersonTyped gives a write a person typed in a session the context that marks its events
// (store.WithPersonTyped), so the event the write emits carries person_typed: true.
func markPersonTyped(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if personTypedWrite(r) {
			r = r.WithContext(store.WithPersonTyped(r.Context()))
		}
		handler(w, r)
	}
}

// recordPersonTyped records a write a person typed in a session as exempt_person_typed, with the
// session it was typed in, the person who reads it and the project it stands in, whether or not
// the writing check and the reviewer are on: R8's counts show each use apart. A write that
// carries no prose is not recorded, as the check records none.
func (s *server) recordPersonTyped(r *http.Request, actor model.Actor, input any, project string) {
	if len(proseFieldsOf(input)) == 0 {
		if carrier, ok := input.(unreadUploadCarrier); !ok || carrier.unreadUpload() == "" {
			return
		}
	}
	method, route := routeOf(r)
	event := store.WritingCheckEvent{
		Route: route, Method: method, ActorKind: actor.Kind, ActorID: actor.ID,
		TokenID: s.writingCheckTokenID(r, actor), Outcome: writingCheckPersonTyped, Project: project,
	}
	if actor.Act != nil {
		event.ActSession = actor.Act.Session
	}
	reader, err := s.writingCheckReader(r.Context(), r, method+" "+route, actor, input)
	if err != nil {
		event.Detail = "the reader could not be decided: " + err.Error()
	}
	event.Reader = reader
	s.keepWritingCheck(r, event)
}

// WritingCheck is the writing check one server runs, over the bundle's structure rules
// (writingcheck.EnabledRules): the checker and what the state route answers about it. A nil
// *WritingCheck is the check turned off. The phrase detector does not run in the check: it runs as
// the reviewer's detector rule set (Decision 11 of INFRA-3231's plan).
type WritingCheck struct {
	checker *writingcheck.Checker
}

// NewWritingCheck builds the check, on, over the bundle's structure rules.
func NewWritingCheck() *WritingCheck {
	return &WritingCheck{checker: writingcheck.New(writingcheck.Config{Rules: writingcheck.EnabledRules})}
}

func (c *WritingCheck) on() bool { return c != nil }

// writingCheckReads applies the actor rule: the check reads a write when an agent session that is
// not a project's Legion daemon makes it, or when a person's credential makes it for such a
// session, and reads no other write.
func writingCheckReads(actor model.Actor) (checked bool, exemption string) {
	actSession := ""
	if actor.Act != nil {
		actSession = actor.Act.Session
	}
	return writingcheck.Checks(actor.Kind, actor.ID, actSession)
}

// unreadUploadCarrier is an upload whose file the check does not read: unreadUpload answers the
// file's media type, or "" for an upload the check reads.
type unreadUploadCarrier interface{ unreadUpload() string }

// writingCheck runs the writing check over the prose input carries (proseFieldsOf), for the write
// actor makes, and keeps its decision for the record of the write once it took place
// (keepWritingCheck). It refuses no write, so the write goes on whatever it finds. It reads a
// write only when the actor rule says an agent makes it (writingCheckReads), a person reads it
// (writingCheckReader) and no plugin generated it (generatedText), and it records that person on
// every row. Call it once input is decoded and actor resolved, and before any transaction, lock
// or write.
func (s *server) writingCheck(r *http.Request, actor model.Actor, input any) {
	if personTypedWrite(r) {
		s.recordPersonTyped(r, actor, input, "")
		return
	}
	check := s.deps.WritingCheck
	if !check.on() {
		return
	}
	fields := proseFieldsOf(input)
	unread := ""
	if carrier, ok := input.(unreadUploadCarrier); ok {
		unread = carrier.unreadUpload()
	}
	if len(fields) == 0 && unread == "" {
		// A write that carries no prose (a status change, an empty note) gives the check nothing
		// to read and nothing to record.
		return
	}
	method, route, found := strings.Cut(r.Pattern, " ")
	if !found {
		method, route = r.Method, r.URL.Path
	}
	event := store.WritingCheckEvent{
		Route: route, Method: method, ActorKind: actor.Kind, ActorID: actor.ID,
		TokenID: s.writingCheckTokenID(r, actor),
	}
	if actor.Act != nil {
		event.ActSession = actor.Act.Session
	}
	readerOf, err := s.writingCheckReader(r.Context(), r, method+" "+route, actor, input)
	if err != nil {
		// The write is let through unread: the check never fails closed.
		slog.Warn("dispatch: decide the reader of a write", "route", method+" "+route, "error", err)
		event.Detail = "the reader could not be decided: " + err.Error()
	}
	event.Reader = readerOf
	switch checked, exemption := writingCheckReads(actor); {
	case generatedText(r, method+" "+route):
		event.Outcome = writingCheckGenerated
	case !checked:
		event.Outcome = exemption
	case readerOf == "":
		event.Outcome = writingCheckNoReader
	default:
		checkFields(&event, check.checker.Check(r.Context(), fields), unread)
	}
	s.keepWritingCheck(r, event)
}

// checkFields records on event what the check found in a write's fields: findings, with each
// rule broken and each field it stands in, or clean; and, for an upload whose file the check does
// not read, that file's type.
func checkFields(event *store.WritingCheckEvent, result writingcheck.Result, unread string) {
	event.Outcome = result.Outcome
	for _, finding := range result.Findings {
		if !slices.Contains(event.Rules, finding.Rule) {
			event.Rules = append(event.Rules, finding.Rule)
		}
		if !slices.Contains(event.Fields, finding.Field) {
			event.Fields = append(event.Fields, finding.Field)
		}
	}
	switch {
	case result.Outcome == writingcheck.OutcomeRefused:
		event.Outcome = writingCheckFindings
		if unread != "" {
			event.Detail = "file of type " + unread + " not read"
		}
	case unread != "":
		event.Outcome = writingCheckUnreadUpload
		event.Detail = "file of type " + unread + " not read; the outcome of its summary: " + result.Outcome
	}
}

// writingCheckTokenID is the personal token a write was made with, "" for any other credential:
// the token an actor's act names, else the one the request's own credential presented, so a
// daemon-named write made with a person's token is recorded under that token. The credential is
// read on a resolution that names nobody on the access-log line (unloggedResolution), which the
// handler's own resolution of actor named already.
func (s *server) writingCheckTokenID(r *http.Request, actor model.Actor) string {
	if actor.Act != nil && actor.Act.TokenID != "" {
		return actor.Act.TokenID
	}
	caller, _, err := s.optionalActor(unloggedResolution(r))
	if err != nil || caller.Credential == nil {
		return ""
	}
	return caller.Credential.TokenID
}

// recordWritingCheck inserts one decision of the check, on the pool and outside any transaction of
// the write. A failure is logged: the record is the check's own, and no write fails for it.
func (s *server) recordWritingCheck(ctx context.Context, event store.WritingCheckEvent) {
	if s.deps.Store == nil {
		return
	}
	if err := s.deps.Store.InsertWritingCheckEvent(ctx, event); err != nil {
		slog.Warn("dispatch: record a writing check decision", "route", event.Method+" "+event.Route,
			"outcome", event.Outcome, "error", err)
	}
}

// writingCheckRowsKey carries a write's writingCheckRows from the route's layer that records them
// (recordTakenWrites) to the check that decides them (keepWritingCheck).
type writingCheckRowsKey struct{}

// writingCheckRows are the rows the writing check decided for one write, kept until its handler
// has answered it, and the id of the write that every row of it records (writeIDOf).
type writingCheckRows struct {
	events []store.WritingCheckEvent
	// write is the write's id: the held text's where the reviewer holds the write (holdWrite),
	// else one made for the first row that is recorded apart from these (writeIDOf), "" while
	// there is none.
	write string
}

// writingCheckRowsFrom is the rows ctx keeps for its write (recordTakenWrites), nil for a request
// served outside that layer.
func writingCheckRowsFrom(ctx context.Context) *writingCheckRows {
	kept, _ := ctx.Value(writingCheckRowsKey{}).(*writingCheckRows)
	return kept
}

// writeIDOf is the id of the write ctx is the request of, which a row the reviewer records apart
// from the check's (the review of the write's text after its post) records as its WriteID, so the
// counts count the text once (store.WritingCheckEvent.WriteID). It is made the first time a row
// asks for it, and is "" for a request served outside recordTakenWrites.
func writeIDOf(ctx context.Context) string {
	kept := writingCheckRowsFrom(ctx)
	if kept == nil {
		return ""
	}
	if kept.write == "" {
		kept.write = uuid.NewString()
	}
	return kept.write
}

// keepWritingCheck keeps event, the check's decision about r's write, for the route's layer to
// record once the write took place (recordTakenWrites). A write served outside that layer, which
// no route of the table is, has it recorded at once, as no answer of its own is read.
func (s *server) keepWritingCheck(r *http.Request, event store.WritingCheckEvent) {
	if kept := writingCheckRowsFrom(r.Context()); kept != nil {
		kept.events = append(kept.events, event)
		return
	}
	s.recordWritingCheck(r.Context(), event)
}

// dropWritingCheck drops the rows the writing check kept for r (keepWritingCheck), for a route that
// answers 2xx for a call that writes none of the words the check read: the calls the file's
// header names.
func dropWritingCheck(r *http.Request) {
	if kept := writingCheckRowsFrom(r.Context()); kept != nil {
		kept.events = nil
	}
}

// recordTakenWrites is a write route's layer for the writing check's rows: it records the rows
// the check kept for the write (keepWritingCheck) once handler answered it 2xx, each with the
// write's id where a row the reviewer records apart took one (writeIDOf, holdWrite). A route
// answers 2xx for a write that took place, a write the reviewer holds among them, and six routes
// answer it too for a call that writes none of the words the check read, which drops its rows
// first (dropWritingCheck): the two reply routes (POST /api/v1/messages/{id}/reply,
// POST /api/v1/comments/{id}/reply) for a call on an attempt already answered that posts nothing
// or an error report repeated on an attempt already failed, the three edit routes (editArtifact)
// for a batch that leaves the document as it was, and
// POST /api/v1/artifacts/{id}/approval-requests for a request whose every document already stands
// approved at its current version. A write the handler refused leaves no row, as it leaves no
// other: a refusal leaves every table as it found it
// (TestACloseCommittedWhileAWriterWaitsOnTheOwnerLockIsRefused). Every transaction of the handler
// has ended when it returns, so the insert takes a connection of its own. It runs on a context
// the client's going away does not cancel (outlivingTheClient), since the write it records has
// committed whatever the client does next: on a keyed write that is the keyed run's own context,
// which this layer runs inside and which is still live here, so the insert keeps the run's bound
// (keyedRunLimit).
func (s *server) recordTakenWrites(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kept := &writingCheckRows{}
		answer := &answerStatus{ResponseWriter: w}
		handler(answer, r.WithContext(context.WithValue(r.Context(), writingCheckRowsKey{}, kept)))
		if len(kept.events) == 0 || !answer.taken() {
			return
		}
		ctx := outlivingTheClient(r)
		for _, event := range kept.events {
			event.WriteID = cmp.Or(event.WriteID, kept.write)
			s.recordWritingCheck(ctx, event)
		}
	}
}

// answerStatus is a handler's writer that keeps the status the handler answered with.
type answerStatus struct {
	http.ResponseWriter
	// status is the first status the handler gave, 0 until it gives one.
	status int
}

func (w *answerStatus) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap is the writer this one wraps, for http.ResponseController.
func (w *answerStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// taken reports whether the handler answered 2xx. A handler that gave no status, having written
// a body or nothing, is answered 200 by net/http, so it is taken too.
func (w *answerStatus) taken() bool {
	return w.status == 0 || (w.status >= 200 && w.status < 300)
}

// writingCheckState is the body of GET /api/v1/writing-check: the check as this process runs it.
type writingCheckState struct {
	Enabled bool `json:"enabled"`
	// Rules is the structure rules the check applies: the bundle's.
	Rules []string `json:"rules"`
	// RulesLeftOut is every rule the check does not apply, with why.
	RulesLeftOut []writingCheckRuleLeftOut `json:"rules_left_out"`
	Detector     writingCheckDetector      `json:"detector"`
	// BundleModelID is the model id the bundle was measured with, "" while it pins none.
	BundleModelID string `json:"bundle_model_id"`
}

// writingCheckRuleLeftOut is one rule the check does not apply.
type writingCheckRuleLeftOut struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// writingCheckDetector is whether the check runs the phrase detector, and why it does not.
type writingCheckDetector struct {
	On     bool   `json:"on"`
	Reason string `json:"reason"`
}

// writingCheckDetectorElsewhere is why the check runs no detector while it is on.
const writingCheckDetectorElsewhere = "the phrase detector runs as the reviewer's detector rule set (DISPATCH_REVIEWER_RULES), not in the check"

// state is what GET /api/v1/writing-check answers.
func (c *WritingCheck) state() writingCheckState {
	state := writingCheckState{
		Enabled: c.on(), Rules: []string{}, RulesLeftOut: []writingCheckRuleLeftOut{},
		Detector: writingCheckDetector{Reason: writingCheckDetectorElsewhere}, BundleModelID: writingcheck.BundleModelID,
	}
	if !state.Enabled {
		state.Detector.Reason = "the check is off"
	}
	for _, rule := range rules.All() {
		measured := slices.IndexFunc(writingcheck.MeasuredOff, func(m writingcheck.RuleMeasurement) bool { return m.ID == rule.ID })
		switch {
		case measured >= 0:
			state.RulesLeftOut = append(state.RulesLeftOut, writingCheckRuleLeftOut{rule.ID, writingcheck.MeasuredOff[measured].Reason})
		case !slices.Contains(writingcheck.EnabledRules, rule.ID):
			state.RulesLeftOut = append(state.RulesLeftOut, writingCheckRuleLeftOut{rule.ID, "the bundle does not enable it"})
		default:
			state.Rules = append(state.Rules, rule.ID)
		}
	}
	for _, omission := range rules.LeftOut {
		state.RulesLeftOut = append(state.RulesLeftOut, writingCheckRuleLeftOut{omission.ID, omission.Reason})
	}
	return state
}

// getWritingCheck is GET /api/v1/writing-check.
func (s *server) getWritingCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	WriteJSON(w, http.StatusOK, s.deps.WritingCheck.state())
}

// writingCheckCountsDays is the counts route's range when its query names none, the last 14 days
// with today, and writingCheckCountsMostDays the longest range it counts: the days the sweep keeps
// a row the counts read (store.WritingCheckCountedRetention), so a range that ends today counts
// its rows whole.
const (
	writingCheckCountsDays     = 14
	writingCheckCountsMostDays = int(store.WritingCheckCountedRetention / (24 * time.Hour))
)

// getWritingCheckCounts is GET /api/v1/writing-check/counts: R8's counts of the UTC days ?first=
// to ?last=, both YYYY-MM-DD and both included, with one page of its rows (store.CountWritingCheck,
// list_page.go). last is today where it is absent, and first the 13 days before last. Any
// signed-in caller reads it (Decision 14 of INFRA-3231's plan).
func (s *server) getWritingCheckCounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	day := func(name string, fallback time.Time) (time.Time, bool) {
		raw := strings.TrimSpace(query.Get(name))
		if raw == "" {
			return fallback, true
		}
		parsed, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, name+" must be one UTC day written YYYY-MM-DD")
			return time.Time{}, false
		}
		return parsed, true
	}
	now := time.Now().UTC()
	last, ok := day("last", time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC))
	if !ok {
		return
	}
	first, ok := day("first", last.AddDate(0, 0, 1-writingCheckCountsDays))
	if !ok {
		return
	}
	switch {
	case first.After(last):
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "first must be no later than last")
		return
	case last.Sub(first) >= store.WritingCheckCountedRetention:
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf("the range from first to last holds at most %d days", writingCheckCountsMostDays))
		return
	}
	page, err := parseListPage(query)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	counts, err := s.deps.Store.CountWritingCheck(r.Context(), first, last, page.limit, page.offset)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, counts)
}

// writingCheckSweepInterval is how often each process sweeps writing_check_events.
const writingCheckSweepInterval = 24 * time.Hour

// RunWritingCheckEventSweep deletes the writing_check_events rows past their retention
// (store.SweepWritingCheckEvents): 366 days for a row R8's counts read
// (store.WritingCheckCountedRetention), and 90 for every other row (store.WritingCheckRetention),
// once a day, the first pass at start, on the store's own pool. It takes no lease: the pass is
// two idempotent deletes by age, so every process runs it and a second process's pass the same
// day deletes what the first left, which is nothing, and the sweep holds no connection of the
// lease pool. It stops when ctx is done.
func RunWritingCheckEventSweep(ctx context.Context, database *store.Store) {
	ticker := time.NewTicker(writingCheckSweepInterval)
	defer ticker.Stop()
	for {
		deleted, err := database.SweepWritingCheckEvents(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Warn("dispatch: writing check event sweep failed", "error", err)
		case deleted > 0:
			slog.Info("dispatch: swept writing check events past their retention", "deleted", deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// addedMarkdownBlocks is the Markdown of the blocks of next that previous does not hold, compared
// as sets: what a new version uploaded whole adds to the document, which is all the check reads of
// it. The blocks are split by blank lines, with each list item, heading and table row a block of
// its own and a fenced code block kept whole (markdownBlocks). A table row is given as its cells,
// one paragraph each, so a lone row reads as the cells' text.
func addedMarkdownBlocks(previous, next string) string {
	held := map[string]bool{}
	for _, block := range markdownBlocks(previous) {
		held[block] = true
	}
	var added []string
	for _, block := range markdownBlocks(next) {
		if held[block] {
			continue
		}
		held[block] = true
		if strings.HasPrefix(block, "|") {
			added = append(added, tableRowCells(block)...)
			continue
		}
		added = append(added, block)
	}
	return strings.Join(added, "\n\n")
}

// markdownBlocks splits markdown into its blocks (addedMarkdownBlocks), each with the indentation
// of its first line taken off its every line.
func markdownBlocks(markdown string) []string {
	var blocks, current []string
	fence := ""
	flush := func() {
		if len(current) == 0 {
			return
		}
		indent := len(current[0]) - len(strings.TrimLeft(current[0], " \t"))
		for index, line := range current {
			trim := min(indent, len(line)-len(strings.TrimLeft(line, " \t")))
			current[index] = strings.TrimRight(line[trim:], " \t")
		}
		blocks = append(blocks, strings.Join(current, "\n"))
		current = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			current = append(current, line)
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
				flush()
			}
			continue
		}
		if opener := codeFence(trimmed); opener != "" {
			flush()
			fence = opener
			current = append(current, line)
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		single := strings.HasPrefix(trimmed, "|") || strings.HasPrefix(trimmed, "#")
		if single || opensListItem(trimmed) {
			flush()
		}
		current = append(current, line)
		if single {
			flush()
		}
	}
	flush()
	return blocks
}

// opensListItem reports whether a line, its indentation taken off, opens a list item: a bullet
// (-, *, +) or a number followed by . or ), then a space.
func opensListItem(trimmed string) bool {
	if len(trimmed) >= 2 && strings.ContainsRune("-*+", rune(trimmed[0])) && trimmed[1] == ' ' {
		return true
	}
	digits := len(trimmed) - len(strings.TrimLeft(trimmed, "0123456789"))
	return digits > 0 && digits < 10 && len(trimmed) > digits+1 &&
		(trimmed[digits] == '.' || trimmed[digits] == ')') && trimmed[digits+1] == ' '
}

// tableRowCells is a table row's cells, each trimmed, the delimiter row's none.
func tableRowCells(row string) []string {
	var cells []string
	for _, cell := range strings.Split(strings.Trim(row, "|"), "|") {
		cell = strings.TrimSpace(cell)
		if strings.Trim(cell, "-: ") == "" {
			continue
		}
		cells = append(cells, cell)
	}
	return cells
}

// previousDocumentMarkdown is the Markdown of the newest version a document has saved, "" where
// it has none: what a new version uploaded whole is compared with (addedMarkdownBlocks). It reads
// on the pool, before the write's transaction opens.
func (s *server) previousDocumentMarkdown(ctx context.Context, artifactID string) (string, error) {
	var markdown string
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce((select markdown from artifact_versions
			where artifact_id = $1 and markdown is not null order by number desc limit 1), '')
	`, artifactID).Scan(&markdown)
	if err != nil {
		return "", fmt.Errorf("read the document's newest version for the writing check: %w", err)
	}
	return markdown, nil
}
