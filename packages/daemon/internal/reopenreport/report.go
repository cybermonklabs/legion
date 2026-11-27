// Package reopenreport is the read behind `legion reopened-report`: for each issue the controller
// reopened with a request for new work (D17), the interval from the accepted todo to the end of
// the change the request started, each version of the issue's Verification document that an
// actor other than the daemon wrote in it, whether that version changed a check block, the
// request's withdrawal, and what could not be read.
//
// It classifies a row only by what a program can tell: pending while the change is open, not
// assessable where a read failed, and for an ended row whether such a version wrote a check block
// and whether the request was withdrawn. Whether the checks cover the omitted work is the
// controller's review, and nothing here stores a verdict.
//
// The interval's start is the accepted todo, read from the issue's Dispatch events. Its end is the
// change's merge, or its end with no pull request, read from the versions of close-wait.md that
// the daemon wrote, where each change's end is said once (record.StoredWait.Ended). It is never
// the start of the change's first phase. The report reads those versions in the forms the daemon
// renders them in (record.CloseWaitDocument), and a section of another form fails the read.
package reopenreport

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

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Reader is what the report reads of Dispatch, each read as dispatch.HTTPClient makes it: the
// issue's documents, its event log (EventsAfter, every page of it), a version of close-wait.md,
// what a version of the Verification document changed from the one before it (ArtifactChanges),
// and the typed blocks of that document's first version (PlanDocument).
type Reader interface {
	Artifacts(ctx context.Context, key string) ([]dispatch.Artifact, error)
	EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error)
	DocumentVersion(ctx context.Context, artifactID string, version int) (string, error)
	ArtifactChanges(ctx context.Context, artifactID string, from, to int) (dispatch.ArtifactChanges, error)
	PlanDocument(ctx context.Context, artifactID string, version int) (dispatch.DocumentVersion, error)
}

// Category is what the program can tell of one row (D17, "The measurement").
type Category string

// The categories of a row. ChecksWritten, RequestWithdrawn and NotFollowed are the ended rows that
// could be read (Assessed). ChecksWritten is a version in the interval, by an actor other than the
// daemon, that added or changed a check block. RequestWithdrawn is the request's withdrawal in the
// interval, with no such version. NotFollowed is neither by the change's end. Pending is a change
// still open, and NotAssessable a row with any read that failed, which Row.Unread names.
const (
	ChecksWritten    Category = "checks_written"
	RequestWithdrawn Category = "request_withdrawn"
	NotFollowed      Category = "instruction_not_followed"
	Pending          Category = "pending"
	NotAssessable    Category = "not_assessable"
)

// Categories is every category, in the order the report gives them.
var Categories = []Category{ChecksWritten, RequestWithdrawn, NotFollowed, Pending, NotAssessable}

// Assessed says whether a row of the category is an ended row that could be read: the count of
// NotFollowed is over those rows alone, and a pending or unreadable row is in neither side of it.
func (c Category) Assessed() bool {
	return c == ChecksWritten || c == RequestWithdrawn || c == NotFollowed
}

// Row is one issue's read. Start is the accepted todo, and Change the generation of the change
// the request started, 0 where no version of close-wait.md names one. End is that change's end,
// nil while it is open or where no version says it. Versions is each version of the Verification
// document in the interval by an actor other than the daemon, oldest first. Withdrawal is the
// request's withdrawal, nil for none. Unread names each read that failed: any one makes the row
// NotAssessable.
type Row struct {
	Issue      string
	Category   Category
	Start      *Mark
	Change     uint64
	End        *End
	Versions   []Version
	Withdrawal *Withdrawal
	Unread     []string
}

// Mark is an event of the issue's Dispatch log: its sequence and Dispatch's time of it.
type Mark struct {
	Seq int64
	At  time.Time
}

// End is the end of the request's change as close-wait.md says it: PullRequest is the number of
// the pull request it merged as, 0 for an end with no pull request, and At its time, to the
// second, as the document writes it.
type End struct {
	PullRequest int
	At          time.Time
}

// Version is one version of the Verification document in the interval: its number, its event's
// sequence, who wrote it and when, and each check block it added, changed or removed, nil for
// none. A first version has no earlier one, so each check block it holds is added.
type Version struct {
	Number int
	Seq    int64
	Actor  dispatch.Actor
	At     time.Time
	Checks []dispatch.BlockChange
}

// wroteChecks says whether the version added or changed a check block: a removal writes no check.
func (v Version) wroteChecks() bool {
	return slices.ContainsFunc(v.Checks, func(check dispatch.BlockChange) bool { return check.Change != dispatch.ChangeRemoved })
}

// Withdrawal is the request's withdrawal as close-wait.md says it: the request's id, the reason
// the owner gave with its evidence, and when it was made.
type Withdrawal struct {
	Request int64
	Reason  string
	At      time.Time
}

// Read reads the row of issue key from Dispatch and classifies it.
func Read(ctx context.Context, reader Reader, key string) Row {
	row := Row{Issue: key}
	row.read(ctx, reader)
	row.Category = row.category()
	return row
}

// category is the row's category from what was read: any read that failed first, then a change
// with no end, then a version that wrote a check block, then the withdrawal.
func (r *Row) category() Category {
	switch {
	case len(r.Unread) > 0:
		return NotAssessable
	case r.End == nil:
		return Pending
	case slices.ContainsFunc(r.Versions, Version.wroteChecks):
		return ChecksWritten
	case r.Withdrawal != nil:
		return RequestWithdrawn
	}
	return NotFollowed
}

// unread records a read that failed, which makes the row NotAssessable.
func (r *Row) unread(format string, a ...any) {
	r.Unread = append(r.Unread, fmt.Sprintf(format, a...))
}

// read reads the row: the issue's documents, its whole event log, the change that carries the
// request from the daemon's versions of close-wait.md, the accepted todo before it, and the
// versions of the Verification document in the interval. It stops where a read leaves the rest
// without meaning, having named that read.
func (r *Row) read(ctx context.Context, reader Reader) {
	artifacts, err := reader.Artifacts(ctx, r.Issue)
	if err != nil {
		r.unread("the issue's documents: %v", err)
		return
	}
	var closeWait, verification string
	for _, artifact := range artifacts {
		switch {
		case artifact.Slug == record.CloseWaitDocumentSlug:
			closeWait = artifact.ID
		case artifact.Role == dispatch.ArtifactRoleVerification:
			verification = artifact.ID
		}
	}
	if closeWait == "" {
		r.unread("the issue holds no %s, where the daemon writes each change's end", record.CloseWaitDocumentName)
		return
	}
	raw, err := reader.EventsAfter(ctx, r.Issue, 0)
	if err != nil {
		r.unread("the issue's events: %v", err)
		return
	}
	log := r.readLog(raw, closeWait, verification)
	opened, known := r.readChange(ctx, reader, closeWait, log.waits)
	if opened == nil {
		return
	}
	for _, todo := range log.todos {
		if todo.Seq < opened.Seq {
			r.Start = &todo
		}
	}
	if r.Start == nil {
		r.unread("no event moved the issue to todo before version %d of %s named change %d open", opened.number, record.CloseWaitDocumentName, r.Change)
		return
	}
	if known {
		r.readVersions(ctx, reader, verification, log.verifications)
	}
}

// todoStatus is Dispatch's status word that a request for new work sets.
const todoStatus = "todo"

// issueEventTypes is each type of Dispatch event that carries the issue as the event leaves it,
// its status among it (intake's issueEventTypes).
var issueEventTypes = []string{"issue.created", "issue.updated", "issue.closed"}

// logEvent is one event of the issue's Dispatch log, as far as the report reads it.
type logEvent struct {
	Seq   int64  `json:"seq"`
	Type  string `json:"type"`
	Actor struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"actor"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

// documentVersion is the event of one version of a document: the version's number, the event, and
// who wrote it.
type documentVersion struct {
	number int
	Mark
	actor dispatch.Actor
}

// issueLog is what the report takes of the issue's log: each event that moved the issue to todo,
// each version of close-wait.md the daemon wrote, and each version of the Verification document
// by an actor other than the daemon, each oldest first.
type issueLog struct {
	todos         []Mark
	waits         []documentVersion
	verifications []documentVersion
}

// readLog reads raw, the issue's log as EventsAfter answers it. An event that moves the issue to
// todo is one whose status is todo where the issue event before it left another status. A version
// of close-wait.md counts only where the daemon wrote it: a person's edit stands until the
// daemon's next version replaces it, and says nothing the daemon wrote. An event the report cannot
// place is named as unread.
func (r *Row) readLog(raw []json.RawMessage, closeWait, verification string) issueLog {
	daemon := dispatch.Actor{Kind: "session", ID: dispatch.DaemonSession(r.Issue)}
	var log issueLog
	status := ""
	for _, message := range raw {
		var event logEvent
		if err := json.Unmarshal(message, &event); err != nil {
			r.unread("an event of the issue's log: %v", err)
			continue
		}
		mark := Mark{Seq: event.Seq, At: event.CreatedAt.UTC()}
		actor := dispatch.Actor{Kind: event.Actor.Kind, ID: event.Actor.ID}
		switch {
		case slices.Contains(issueEventTypes, event.Type):
			var issue struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(event.Payload, &issue); err != nil || issue.Status == "" {
				r.unread("event %d (%s) names no status", event.Seq, event.Type)
				continue
			}
			if issue.Status == todoStatus && status != todoStatus {
				log.todos = append(log.todos, mark)
			}
			status = issue.Status
		case event.Type == "artifact.created" || event.Type == "artifact.version":
			document, number, err := versionOf(event, closeWait, verification)
			if err != nil {
				r.unread("event %d (%s): %v", event.Seq, event.Type, err)
				continue
			}
			version := documentVersion{number: number, Mark: mark, actor: actor}
			switch {
			case document == closeWait && actor == daemon:
				log.waits = append(log.waits, version)
			case document == verification && actor != daemon:
				log.verifications = append(log.verifications, version)
			}
		}
	}
	return log
}

// versionOf is the document and version number an artifact event names: artifact.version names
// both, and artifact.created names the document with its first version. It answers "" for an
// event of another document than closeWait and verification, and refuses an event of either that
// names no positive version, or one that names no document at all.
func versionOf(event logEvent, closeWait, verification string) (string, int, error) {
	var document string
	var number int
	if event.Type == "artifact.created" {
		var created struct {
			Artifact struct {
				ID       string `json:"id"`
				Versions []struct {
					Number int `json:"number"`
				} `json:"versions"`
			} `json:"artifact"`
		}
		if err := json.Unmarshal(event.Payload, &created); err != nil {
			return "", 0, err
		}
		document = created.Artifact.ID
		if len(created.Artifact.Versions) > 0 {
			number = created.Artifact.Versions[0].Number
		}
	} else {
		var version struct {
			ArtifactID string `json:"artifact_id"`
			Version    struct {
				Number int `json:"number"`
			} `json:"version"`
		}
		if err := json.Unmarshal(event.Payload, &version); err != nil {
			return "", 0, err
		}
		document, number = version.ArtifactID, version.Version.Number
	}
	switch {
	case document == "":
		return "", 0, errors.New("it names no document")
	case document != closeWait && (verification == "" || document != verification):
		return "", 0, nil
	case number <= 0:
		return "", 0, fmt.Errorf("it names no version of document %s", document)
	}
	return document, number, nil
}

// readChange finds the change the request started in the daemon's versions of close-wait.md,
// waits: the newest change a version names open with a request among its reasons. It sets the
// row's Change, its End where the version after the change's last open one says it, and its
// Withdrawal where a version in that run says the request withdrawn. It answers the first version
// that names the change open, and whether the interval's end is known: the change ended, or it is
// still open. A version after the change's last open one that names no end of it leaves the end
// unknown, and is named as unread. Versions are read newest first, and no older than the change
// needs.
func (r *Row) readChange(ctx context.Context, reader Reader, closeWait string, waits []documentVersion) (*documentVersion, bool) {
	parsed := make([]*waitVersion, len(waits))
	read := func(i int) *waitVersion {
		if parsed[i] != nil {
			return parsed[i]
		}
		markdown, err := reader.DocumentVersion(ctx, closeWait, waits[i].number)
		if err != nil {
			r.unread("version %d of %s: %v", waits[i].number, record.CloseWaitDocumentName, err)
			return nil
		}
		version, err := parseWait(markdown)
		if err != nil {
			r.unread("version %d of %s: %v", waits[i].number, record.CloseWaitDocumentName, err)
			return nil
		}
		parsed[i] = &version
		return parsed[i]
	}
	last := -1
	for i := len(waits) - 1; i >= 0 && last < 0; i-- {
		version := read(i)
		if version == nil {
			return nil, false
		}
		if version.open != nil && len(version.open.requests) > 0 {
			last = i
		}
	}
	if last < 0 {
		r.unread("no version of %s names a change that carries a request", record.CloseWaitDocumentName)
		return nil, false
	}
	r.Change = parsed[last].open.generation
	first := last
	for first > 0 {
		version := read(first - 1)
		if version == nil {
			return nil, false
		}
		if version.open == nil || version.open.generation != r.Change {
			break
		}
		first--
	}
	after := last + 1
	for after < len(waits) && parsed[after].open != nil && parsed[after].open.generation == r.Change {
		after++
	}
	requests := map[int64]bool{}
	for _, version := range parsed[first : last+1] {
		for _, id := range version.open.requests {
			requests[id] = true
		}
	}
	for _, version := range parsed[first:min(after+1, len(waits))] {
		for _, withdrawn := range version.withdrawn {
			if requests[withdrawn.Request] && r.Withdrawal == nil {
				r.Withdrawal = &withdrawn
			}
		}
	}
	if after == len(waits) {
		return &waits[first], true
	}
	ended := parsed[after].ended
	if ended == nil || ended.generation != r.Change {
		r.unread("version %d of %s no longer names change %d open, and names no end of it", waits[after].number, record.CloseWaitDocumentName, r.Change)
		return &waits[first], false
	}
	r.End = &End{PullRequest: ended.pullRequest, At: ended.at}
	return &waits[first], true
}

// readVersions keeps each version of the Verification document after the accepted todo and
// before the change's end, with the check blocks it changed. The end is known to the second, so a
// version in that same second cannot be ordered against it, and is named as unread rather than
// counted either way. While the change is open each version after the todo is in the interval.
func (r *Row) readVersions(ctx context.Context, reader Reader, verification string, versions []documentVersion) {
	for _, version := range versions {
		if version.Seq <= r.Start.Seq {
			continue
		}
		if r.End != nil {
			if !version.At.Before(r.End.At.Add(time.Second)) {
				continue
			}
			if !version.At.Before(r.End.At) {
				r.unread("version %d of the Verification document and the change's end fall in one second, so their order is not known", version.number)
				continue
			}
		}
		checks, err := checksOf(ctx, reader, verification, version.number)
		if err != nil {
			r.unread("version %d of the Verification document: %v", version.number, err)
			continue
		}
		r.Versions = append(r.Versions, Version{Number: version.number, Seq: version.Seq, Actor: version.actor, At: version.At, Checks: checks})
	}
}

// checkBlockType is the type of a check block, as Dispatch's block schema spells it
// (pmdoc/schema/blocks.json, block `check`).
const checkBlockType = "check"

// checksOf is each check block version number of the Verification document changed from the
// version before it, as Dispatch compares the two (ArtifactChanges), and for a first version each
// check block it holds (PlanDocument), as added.
func checksOf(ctx context.Context, reader Reader, verification string, number int) ([]dispatch.BlockChange, error) {
	var checks []dispatch.BlockChange
	if number == 1 {
		document, err := reader.PlanDocument(ctx, verification, 1)
		if err != nil {
			return nil, err
		}
		for _, block := range document.Blocks {
			if block.Type == checkBlockType {
				checks = append(checks, dispatch.BlockChange{ID: block.ID, Type: block.Type, Change: dispatch.ChangeAdded})
			}
		}
		return checks, nil
	}
	changes, err := reader.ArtifactChanges(ctx, verification, number-1, number)
	if err != nil {
		return nil, err
	}
	for _, block := range changes.Blocks {
		if block.Type == checkBlockType {
			checks = append(checks, block)
		}
	}
	return checks, nil
}

// waitVersion is what one version of close-wait.md says that the report reads: the change it
// names open with the ids of the requests among its reasons, the change it says ended, and each
// withdrawal it says.
type waitVersion struct {
	open      *openChange
	ended     *changeEnd
	withdrawn []Withdrawal
}

// openChange is the section "Next change": the change's generation, and each reason of kind
// request it carries, by id.
type openChange struct {
	generation uint64
	requests   []int64
}

// changeEnd is the section "The change that ended": the change's generation, the pull request it
// merged as (0 for none) and the time of its end.
type changeEnd struct {
	generation  uint64
	pullRequest int
	at          time.Time
}

// The headings and lines of close-wait.md the report reads, as record.CloseWaitDocument writes
// them, and the time those lines write (record's waitTime). No mechanism shares them with the
// renderer, so they change together. The report's test renders every version it reads with
// record.CloseWaitDocument, so a change of the renderer alone fails that test.
const (
	endedHeading     = "The change that ended"
	openHeading      = "Next change"
	withdrawnHeading = "Withdrawn defects"
	reasonCutLine    = "  The defect was:"
	agentTextIndent  = "      "
	waitTimeLayout   = "2006-01-02 15:04:05 UTC"
	waitTimePattern  = `(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} UTC)`
)

var (
	mergedLine    = regexp.MustCompile(`^Change (\d+) merged as pull request #(\d+) at ` + waitTimePattern + `\.$`)
	endedLine     = regexp.MustCompile(`^Change (\d+) ended with no pull request at ` + waitTimePattern + `\.$`)
	openLine      = regexp.MustCompile(`^Change (\d+) is open since ` + waitTimePattern + `\. It carries these reasons:$`)
	reasonLine    = regexp.MustCompile(`^- (\S+) (\d+)(?:,| about )`)
	withdrawnLine = regexp.MustCompile(`^- defect (\d+), withdrawn at ` + waitTimePattern + `, because:$`)
)

// knownHeadings is every `##` heading record.CloseWaitDocument writes. A version with any other is
// refused: a section the renderer gained may say what the report must read (a request's
// withdrawal under a heading of its own, say), so it is never passed over in silence.
var knownHeadings = []string{
	endedHeading, withdrawnHeading, "Closed", "Unread", "Unapproved", "Checks", "Production check",
	"Issues beneath", "Open children", "Reported defects", openHeading,
}

// parseWait reads one version of close-wait.md. Every text a role wrote stands under its item as
// an indented code block (record's writeAgentText), so a line that starts `## ` or `- ` is the
// daemon's own. A withdrawal's reason is the block between its item and the line that opens the
// defect's text. A section of the three it reads that holds no line of its form, or an item of one
// in another form, is an error: the renderer changed, and the report does not guess at its words.
func parseWait(markdown string) (waitVersion, error) {
	var version waitVersion
	lines := strings.Split(markdown, "\n")
	section := ""
	seen := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			section = heading
			seen[section] = true
			continue
		}
		switch section {
		case endedHeading:
			if version.ended != nil || line == "" {
				continue
			}
			if m := mergedLine.FindStringSubmatch(line); m != nil {
				end, err := changeEndOf(m[1], m[2], m[3])
				if err != nil {
					return waitVersion{}, err
				}
				version.ended = &end
			} else if m := endedLine.FindStringSubmatch(line); m != nil {
				end, err := changeEndOf(m[1], "0", m[2])
				if err != nil {
					return waitVersion{}, err
				}
				version.ended = &end
			} else {
				return waitVersion{}, fmt.Errorf("its section %q says %q, which is no end the daemon writes", endedHeading, line)
			}
		case openHeading:
			if m := openLine.FindStringSubmatch(line); m != nil && version.open == nil {
				generation, err := strconv.ParseUint(m[1], 10, 64)
				if err != nil {
					return waitVersion{}, err
				}
				version.open = &openChange{generation: generation}
				continue
			}
			if !strings.HasPrefix(line, "- ") {
				continue
			}
			m := reasonLine.FindStringSubmatch(line)
			if m == nil || version.open == nil {
				return waitVersion{}, fmt.Errorf("its section %q lists %q, which is no reason the daemon writes", openHeading, line)
			}
			if m[1] == string(record.DefectKindRequest) {
				id, err := strconv.ParseInt(m[2], 10, 64)
				if err != nil {
					return waitVersion{}, err
				}
				version.open.requests = append(version.open.requests, id)
			}
		case withdrawnHeading:
			if !strings.HasPrefix(line, "- ") {
				continue
			}
			m := withdrawnLine.FindStringSubmatch(line)
			if m == nil {
				return waitVersion{}, fmt.Errorf("its section %q lists %q, which is no withdrawal the daemon writes", withdrawnHeading, line)
			}
			id, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				return waitVersion{}, err
			}
			at, err := time.Parse(waitTimeLayout, m[2])
			if err != nil {
				return waitVersion{}, err
			}
			reason, next, err := agentText(lines, i+1, reasonCutLine)
			if err != nil {
				return waitVersion{}, fmt.Errorf("the withdrawal of %d: %w", id, err)
			}
			i = next
			version.withdrawn = append(version.withdrawn, Withdrawal{Request: id, Reason: reason, At: at})
		}
	}
	switch {
	case seen[endedHeading] && version.ended == nil:
		return waitVersion{}, fmt.Errorf("its section %q says no end", endedHeading)
	case seen[openHeading] && version.open == nil:
		return waitVersion{}, fmt.Errorf("its section %q names no change", openHeading)
	}
	return version, nil
}

// agentText reads a role's text that lines holds from from, an indented code block as record's
// writeAgentText writes it under an item, up to the line until: each line agentTextIndent past the
// margin, and blank lines, the block's own first and last blank lines left out. It answers the
// text and the index of the line before until. A line of any other form, or no line until, is an
// error.
func agentText(lines []string, from int, until string) (string, int, error) {
	var text []string
	for i := from; i < len(lines); i++ {
		line := lines[i]
		switch {
		case line == until:
			return strings.Trim(strings.Join(text, "\n"), "\n"), i - 1, nil
		case line == "":
			text = append(text, "")
		case strings.HasPrefix(line, agentTextIndent):
			text = append(text, strings.TrimPrefix(line, agentTextIndent))
		default:
			return "", 0, fmt.Errorf("its text holds %q, which is no line of a block the daemon writes", line)
		}
	}
	return "", 0, fmt.Errorf("its text runs to the end with no line %q", until)
}

// changeEndOf is the end a line of "The change that ended" says: the generation, the pull request
// (0 for none) and the time, each as the line writes it.
func changeEndOf(generation, pullRequest, at string) (changeEnd, error) {
	var end changeEnd
	var err error
	if end.generation, err = strconv.ParseUint(generation, 10, 64); err != nil {
		return changeEnd{}, err
	}
	if end.pullRequest, err = strconv.Atoi(pullRequest); err != nil {
		return changeEnd{}, err
	}
	if end.at, err = time.Parse(waitTimeLayout, at); err != nil {
		return changeEnd{}, err
	}
	return end, nil
}

// Total is one category's rows, by issue, in the order they were read.
type Total struct {
	Category Category
	Issues   []string
}

// Totals is each category's rows, in the order of Categories, a category with none among them.
func Totals(rows []Row) []Total {
	totals := make([]Total, len(Categories))
	for i, category := range Categories {
		totals[i] = Total{Category: category, Issues: []string{}}
	}
	for _, row := range rows {
		if i := slices.Index(Categories, row.Category); i >= 0 {
			totals[i].Issues = append(totals[i].Issues, row.Issue)
		}
	}
	return totals
}
