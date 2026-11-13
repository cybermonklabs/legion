package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/rules"
)

// writing_check.go puts the writing check (INFRA-3231) on the server's write path. A route whose
// request carries prose (writing_check_fields.go) calls writingCheck once the request is decoded
// and its writer resolved, and before it opens a transaction, takes a lock or writes anything: the
// check may call a model, and no database transaction waits on it. A write the check refuses is
// answered 422 WRITING_CHECK_REFUSED and writes nothing. Every decision the check makes for a
// write is one row of writing_check_events, inserted outside the write's transaction, after the
// decision and before the answer; a failed insert is logged and fails nothing.

// The values override_writing_check takes besides "".
const (
	// writingCheckOverrideAgent is an agent sending its own text through on its own judgement.
	writingCheckOverrideAgent = "agent"
	// writingCheckOverrideGenerated is a plugin sending text it generated.
	writingCheckOverrideGenerated = "generated"
)

// The outcomes writingCheck records besides the checker's own (writingcheck.Outcome*) and the
// actor rule's exemptions (writingcheck.Exempt*).
const (
	// writingCheckForced is a write the check refused that override_writing_check sent through.
	writingCheckForced = "forced"
	// writingCheckUnreadUpload is an uploaded file the check does not read: any upload that is
	// not stored as a Markdown document.
	writingCheckUnreadUpload = "exempt_unread_upload"
)

// codeWritingCheckRefused is the refusal a write the check refused is answered with.
const codeWritingCheckRefused = "WRITING_CHECK_REFUSED"

// writingCheckOverrideField is the request member that sends a refused write through.
const writingCheckOverrideField = "override_writing_check"

// WritingCheckModel is the model the writing check's reader calls, as GET /api/v1/writing-check
// shows it: no key, token or credential is part of it.
type WritingCheckModel struct {
	EndpointHost    string `json:"endpoint_host"`
	Deployment      string `json:"deployment"`
	APIVersion      string `json:"api_version"`
	TokensPerMinute int    `json:"tokens_per_minute"`
}

// WritingCheckConfig is the writing check as cmd/dispatch configured it (DISPATCH_WRITING_CHECK
// and the settings beside it).
type WritingCheckConfig struct {
	// Enabled turns the check on.
	Enabled bool
	// RulesOff is the rule ids DISPATCH_WRITING_CHECK_RULES_OFF turns off, the detector's
	// writingcheck.DetectorRule among them where it names it.
	RulesOff []string
	// Detector is whether DISPATCH_WRITING_CHECK_DETECTOR leaves the bundle's detector on.
	Detector bool
	// Reader is the model reader the detector calls; nil where no model is configured, and the
	// structure rules then run alone.
	Reader writingcheck.Reader
	// Model describes the model Reader calls, for the state route; nil where Reader is nil.
	Model *WritingCheckModel
}

// WritingCheck is the writing check one server runs: the checker and what the state route
// answers about it. A nil *WritingCheck is the check turned off.
type WritingCheck struct {
	config  WritingCheckConfig
	checker *writingcheck.Checker
}

// NewWritingCheck builds the check cfg describes, over the bundle's rules
// (writingcheck.EnabledRules).
func NewWritingCheck(cfg WritingCheckConfig) *WritingCheck {
	return &WritingCheck{config: cfg, checker: writingcheck.New(writingcheck.Config{
		Enabled:  cfg.Enabled,
		Rules:    writingcheck.EnabledRules,
		RulesOff: cfg.RulesOff,
		Detector: cfg.Detector,
		Reader:   cfg.Reader,
	})}
}

func (c *WritingCheck) on() bool { return c != nil && c.config.Enabled }

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

// writingCheckOverrideCarrier is a request that declares override_writing_check: every request
// type embedding writingCheckOverride.
type writingCheckOverrideCarrier interface{ writingCheckOverrideValue() string }

// unreadUploadCarrier is an upload whose file the check does not read: unreadUpload answers the
// file's media type, or "" for an upload the check reads.
type unreadUploadCarrier interface{ unreadUpload() string }

// writingCheck runs the writing check over the prose input carries (proseFieldsOf), for the write
// actor makes, and reports whether the write goes on. A false answer has been written: 400 for an
// override value the check does not know, 422 WRITING_CHECK_REFUSED for a refusal. Call it once
// input is decoded and actor resolved, and before any transaction, lock or write.
func (s *server) writingCheck(w http.ResponseWriter, r *http.Request, actor model.Actor, input any) bool {
	override := ""
	if carrier, ok := input.(writingCheckOverrideCarrier); ok {
		override = carrier.writingCheckOverrideValue()
	}
	switch override {
	case "", writingCheckOverrideAgent, writingCheckOverrideGenerated:
	default:
		writeError(w, "INVALID_WRITING_CHECK_OVERRIDE", http.StatusBadRequest, fmt.Sprintf(
			"%s must be %q or %q, not %q", writingCheckOverrideField,
			writingCheckOverrideAgent, writingCheckOverrideGenerated, override))
		return false
	}
	check := s.deps.WritingCheck
	if !check.on() {
		return true
	}
	fields := proseFieldsOf(input)
	unread := ""
	if carrier, ok := input.(unreadUploadCarrier); ok {
		unread = carrier.unreadUpload()
	}
	if len(fields) == 0 && unread == "" {
		// A write that carries no prose (a status change, an empty note) gives the check nothing
		// to read and nothing to record.
		return true
	}
	method, route, found := strings.Cut(r.Pattern, " ")
	if !found {
		method, route = r.Method, r.URL.Path
	}
	event := store.WritingCheckEvent{
		Route: route, Method: method, ActorKind: actor.Kind, ActorID: actor.ID,
		TokenID: s.writingCheckTokenID(r, actor), OverrideValue: override,
	}
	if actor.Act != nil {
		event.ActSession = actor.Act.Session
	}
	if checked, exemption := writingCheckReads(actor); !checked {
		event.Outcome = exemption
		s.recordWritingCheck(r.Context(), event)
		return true
	}
	started := time.Now()
	result := check.checker.Check(r.Context(), fields)
	event.Candidates = result.Candidates
	if result.ReaderCalled {
		took := int(time.Since(started) / time.Millisecond)
		event.ReaderMS = &took
	}
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
	case result.Outcome == writingcheck.OutcomeRefused && override != "":
		event.Outcome = writingCheckForced
	case result.Outcome == writingcheck.OutcomeRefused:
		s.recordWritingCheck(r.Context(), event)
		WriteJSON(w, http.StatusUnprocessableEntity, writingCheckRefusalOf(result.Findings))
		return false
	case unread != "":
		event.Outcome = writingCheckUnreadUpload
		event.Detail = "file of type " + unread + " not read; the outcome of its summary: " + result.Outcome
	case result.Outcome != writingcheck.OutcomeClean:
		event.Detail = "the write went through with the structure rules alone"
	}
	if unread != "" && event.Outcome == writingCheckForced {
		event.Detail = "file of type " + unread + " not read"
	}
	s.recordWritingCheck(r.Context(), event)
	return true
}

// writingCheckTokenID is the personal token a write was made with, "" for any other credential:
// the token an actor's act names, else the one the request's own credential presented, so a
// daemon-named write made with a person's token is recorded under that token.
func (s *server) writingCheckTokenID(r *http.Request, actor model.Actor) string {
	if actor.Act != nil && actor.Act.TokenID != "" {
		return actor.Act.TokenID
	}
	caller, _, err := s.optionalActor(r)
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

// writingCheckRefusal is the body of 422 WRITING_CHECK_REFUSED.
type writingCheckRefusal struct {
	Code          string                `json:"code"`
	Error         string                `json:"error"`
	Findings      []writingCheckFinding `json:"findings"`
	OverrideField string                `json:"override_field"`
}

// writingCheckFinding is one rule one sentence of the refused write breaks: the request field it
// stands in, the rule's id, the sentence, and how to fix it.
type writingCheckFinding struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Text    string `json:"text"`
	Rewrite string `json:"rewrite"`
}

// init gives every route whose request carries prose (requestCarriesProse) the check's refusal
// beside its own, 422 WRITING_CHECK_REFUSED, so the OpenAPI document reflected out of routeShapes
// names it on each route the check reads and on no other. It reads the registry, so a route that
// starts to carry prose is given the refusal with no line of its own.
func init() {
	for key, shape := range routeShapes {
		if requestCarriesProse(shape.Request) {
			shape.Refusals = append(slices.Clip(shape.Refusals), routeAnswer{
				Status: http.StatusUnprocessableEntity, Body: writingCheckRefusal{},
			})
			routeShapes[key] = shape
		}
	}
}

// writingCheckRefusalOf words a refusal of findings. The error text is written in ASD-STE100
// English and passes the check itself when an agent posts it as a body: each rewrite comes
// first, with every phrase it quotes set as inline code, then the rule id, the field and the
// sentence, each in inline code, and last the way through.
func writingCheckRefusalOf(findings []writingcheck.Finding) writingCheckRefusal {
	refusal := writingCheckRefusal{
		Code:          codeWritingCheckRefused,
		Findings:      make([]writingCheckFinding, len(findings)),
		OverrideField: writingCheckOverrideField,
	}
	var text strings.Builder
	text.WriteString("The writing check refused this write. Fix each sentence below and send the write again.\n\n")
	for index, finding := range findings {
		refusal.Findings[index] = writingCheckFinding(finding)
		fmt.Fprintf(&text, "%d. %s Rule %s, field %s: %s.\n", index+1, quotesAsCode(finding.Rewrite),
			inlineCode(finding.Rule), inlineCode(finding.Field), inlineCode(finding.Text))
	}
	text.WriteString("\nIf the text is correct as it is, send the write again with the flag " +
		inlineCode("--override-writing-check") + ", or with the JSON field " +
		inlineCode(writingCheckOverrideField+`: "`+writingCheckOverrideAgent+`"`) + ".")
	refusal.Error = text.String()
	return refusal
}

// inlineCode sets text as one Markdown code span on one line, with a fence longer than any run of
// backticks it holds.
func inlineCode(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	fence := "`"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}
	return fence + text + fence
}

// quotesAsCode sets every phrase a rewrite quotes in double quotes as inline code, so the phrase
// it names - a phrasal verb, a nominalization, a conjunction - is read as a token and not as a
// sentence of the refusal.
func quotesAsCode(rewrite string) string {
	parts := strings.Split(rewrite, `"`)
	if len(parts)%2 == 0 {
		return rewrite
	}
	var out strings.Builder
	for index, part := range parts {
		if index%2 == 1 {
			out.WriteString(inlineCode(part))
			continue
		}
		out.WriteString(part)
	}
	return out.String()
}

// writingCheckState is the body of GET /api/v1/writing-check: the check as this process runs it.
type writingCheckState struct {
	Enabled bool `json:"enabled"`
	// Rules is the structure rules the check applies: the bundle's, less those turned off.
	Rules []string `json:"rules"`
	// RulesLeftOut is every rule the check does not apply, with why.
	RulesLeftOut []writingCheckRuleLeftOut `json:"rules_left_out"`
	Detector     writingCheckDetector      `json:"detector"`
	// Model is the model the reader calls; null where none is configured.
	Model *WritingCheckModel `json:"model"`
	// ReaderCalls is how many calls this process's reader has made since it started.
	ReaderCalls int64 `json:"reader_calls"`
	// BundleModelID is the model id the bundle was measured with, "" while it pins none.
	BundleModelID string `json:"bundle_model_id"`
}

// writingCheckRuleLeftOut is one rule the check does not apply.
type writingCheckRuleLeftOut struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// writingCheckDetector is whether the detector runs, and why where it does not run or runs
// with no model.
type writingCheckDetector struct {
	On     bool   `json:"on"`
	Reason string `json:"reason"`
}

// state is what GET /api/v1/writing-check answers.
func (c *WritingCheck) state() writingCheckState {
	var cfg WritingCheckConfig
	if c != nil {
		cfg = c.config
	}
	state := writingCheckState{
		Enabled: cfg.Enabled, Rules: []string{}, RulesLeftOut: []writingCheckRuleLeftOut{},
		Model: cfg.Model, BundleModelID: writingcheck.BundleModelID,
	}
	for _, rule := range rules.All() {
		switch {
		case !slices.Contains(writingcheck.EnabledRules, rule.ID):
			state.RulesLeftOut = append(state.RulesLeftOut, writingCheckRuleLeftOut{rule.ID, "the bundle does not enable it"})
		case slices.Contains(cfg.RulesOff, rule.ID):
			state.RulesLeftOut = append(state.RulesLeftOut, writingCheckRuleLeftOut{rule.ID, "DISPATCH_WRITING_CHECK_RULES_OFF names it"})
		default:
			state.Rules = append(state.Rules, rule.ID)
		}
	}
	for _, omission := range rules.LeftOut {
		state.RulesLeftOut = append(state.RulesLeftOut, writingCheckRuleLeftOut{omission.ID, omission.Reason})
	}
	switch {
	case !cfg.Enabled:
		state.Detector.Reason = "the check is off (DISPATCH_WRITING_CHECK)"
	case !cfg.Detector:
		state.Detector.Reason = "DISPATCH_WRITING_CHECK_DETECTOR is off"
	case slices.Contains(cfg.RulesOff, writingcheck.DetectorRule):
		state.Detector.Reason = "DISPATCH_WRITING_CHECK_RULES_OFF names " + writingcheck.DetectorRule
	case cfg.Reader == nil:
		state.Detector = writingCheckDetector{On: true, Reason: "no model is configured: the structure rules run alone, and each check records no_model"}
	default:
		state.Detector.On = true
	}
	if cfg.Reader != nil {
		state.ReaderCalls = cfg.Reader.Calls()
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

// getWritingCheckCounts is GET /api/v1/writing-check/counts: one UTC day of writing_check_events
// counted by outcome, by override value and by route; ?day= is that day, YYYY-MM-DD, and today
// where it is absent.
func (s *server) getWritingCheckCounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	day := time.Now().UTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("day")); raw != "" {
		parsed, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, "day must be one UTC day written YYYY-MM-DD")
			return
		}
		day = parsed
	}
	counts, err := s.deps.Store.CountWritingCheckEvents(r.Context(), day)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, counts)
}

// writingCheckSweepLease names the lease the daily sweep of writing_check_events runs under.
const writingCheckSweepLease = "writing-check-events"

// RunWritingCheckEventSweep deletes the writing_check_events rows older than
// store.WritingCheckRetention once a day, under writingCheckSweepLease, so one process sweeps a
// day however many serve the database. The first pass runs at start. It stops when ctx is done.
func RunWritingCheckEventSweep(ctx context.Context, database *store.Store, leases duty.Leases) {
	duty.RunLeased(ctx, leases, writingCheckSweepLease, 24*time.Hour, duty.TickAtStart, func(ctx context.Context) {
		deleted, err := database.SweepWritingCheckEvents(ctx)
		if err != nil {
			slog.Warn("dispatch: writing check event sweep failed", "error", err)
			return
		}
		if deleted > 0 {
			slog.Info("dispatch: swept writing check events past their retention", "deleted", deleted)
		}
	})
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
