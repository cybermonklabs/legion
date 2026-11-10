package ghautomerge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// ReadFailingTests fills in, for each of runs, what the log of the Actions job it ran as says
// failed (jobFailures): the tests the end of that log states as failed with the first line
// stating one, or which way the daemon could not read them. A run GitHub named no Actions job
// for has no log at all, and is left as it stands.
//
// runs is the failing runs a removal names, which its caller takes off the removal it is about
// to tell of: a *MergeFailed's Runs, a *Removed's, or the Runs of the Removal a
// *ConflictRemoval carries, since whether those are ever told is the daemon's decision
// (its conflictRemoval) and not this package's. The runs are filled where they lie, so the
// removal that holds them says what failed once this has run.
//
// This is the one read of GitHub this package makes outside a submission, and it is made after
// the removal is decided and only where the row is about to tell of it — the fact a failed merge
// sends the issue back with, the notice a removal ends the row with, the message a further
// submission is posted with (the daemon's readyMerge) — so a run that only waits reads no log,
// and a decision already told is never read for twice. logs is the installation's repository
// REST base (githubrest.RepositoryAPI), the one route a job's log lives on, and ctx the context
// the reads run under, which its caller bounds as the whole of one removal's reads rather than
// leaving each read to its own bound (the daemon's removalLogReads): it is deliberately not the
// context the submission's own calls ran under, since no read of a 54 MB log finishes inside a
// merge row's 30 seconds. No failure of a read ever fails the removal: each is told in its own
// run's line and nowhere else, the end of that budget and the daemon's own stop among them
// (jobFailures), which the caller reads off its context rather than off any line.
//
// The logs are read at once, jobLogReads at a time, each into the run it belongs to, so the runs
// read in the order they came whatever order the reads finish in.
func ReadFailingTests(ctx context.Context, logs githubrest.Client, runs []FailingRun) {
	var group errgroup.Group
	group.SetLimit(jobLogReads)
	for i := range runs {
		run := &runs[i]
		if run.JobID == 0 {
			continue
		}
		group.Go(func() error {
			run.Log = jobFailures(ctx, logs, run.JobID)
			return nil
		})
	}
	_ = group.Wait() // no log read ever fails a removal: each failure of one is told in its own line
}

// jobLogReads is how many of a removal's failing jobs have their logs read at once. One read is
// bounded at the log client's own time bound (githubrest.Client.JobLogTimeBound), a minute in
// production, and the whole of a removal's reads by the one deadline its caller gives them
// (ReadFailingTests), since a 54 MB log cannot be read inside a merge row's 30 seconds (the
// daemon's readyMerge). Read one after another, three failing jobs — an ordinary matrix shape —
// would hold the row three minutes; read at once they cost about one read instead, and the limit
// is what holds a merge group with dozens of failing jobs to eight reads of GitHub open at a
// time, and so to eight rings of githubrest.JobLogTail, rather than one per job. It bounds this
// one removal: the reads of every removal running at once are counted together by githubrest's
// own jobLogReadsAtOnce, which a read of this eight waits for its place among.
const jobLogReads = 8

// jobFailures is what the log of Actions job id says failed, written as the end of that run's own
// line: the tests the log names as failed and the first line naming one (failingTests), or, when
// the daemon could not read them, which way it could not — the end of the log states no test
// name, the body passed the byte bound, the read passed the time bound, the deadline the
// removal's reads share ran out, the daemon itself stopped, the fetch failed, or GitHub refused
// the read for want of the Actions grant, a deployment state and no defect. The run link the
// line already carries is what is left to read in every one of those. No failure of this read
// ever fails the removal it belongs to: each is told in the line and nowhere else.
//
// The two the caller's own context ends — its deadline and the daemon's stop — are told apart
// from a fetch that failed (readsEnded), since a read nothing was wrong with says neither that
// GitHub refused it nor that it could not be reached. A caller that stops its reads decides for
// itself whether the removal is told at all: the daemon retries the row after its own stop
// rather than telling anyone these lines (its readyMerge).
//
// The rule this line is held to, rather than a bound on each string in turn: every string it
// carries that came out of GitHub passes through cutLine exactly once, at one place each, and a
// branch that comes to carry a new one passes it through one of those places too. There are two.
// Why a read failed goes through the unread closure, which cuts whatever it is given, so the one
// reason with no length of its own — the error a failed fetch carries, which holds the body
// GitHub's answer had, up to githubrest's jobLogRefusalBody of it, 64 KiB, or twice the whole
// block a Dispatch post is held to — is cut by the same line as the fixed sentences beside it.
// The names and the first failure line go through failingTests, which cuts each name and the
// line as it reads them, and the branch stating them is not cut again: see it below.
//
// At most failingTestsNamed names are stated, with the rest counted after them ("and 12 more"),
// each cut to failureLineUnits (cutLine) and quoted where it is not plain text (quoteName),
// since a name is a log's own bytes on their way into an implementer's prompt and an issue's
// markdown, and one generated name can be as long as a log line.
func jobFailures(ctx context.Context, logs githubrest.Client, id int64) string {
	// unread cuts every why it is handed, so no branch below carries a bound of its own: each
	// fixed sentence is far inside failureLineUnits already and is left as it reads, and the one
	// reason that is unbounded in itself is held here rather than where it is written.
	unread := func(why string) string {
		return ": the daemon could not read its failing tests, " + cutLine(why)
	}
	tail, err := logs.JobLog(ctx, id)
	switch {
	case errors.Is(err, githubrest.ErrJobLogForbidden):
		return unread("since GitHub refused the log: this deployment's App was never granted GitHub's optional Actions read")
	case errors.Is(err, githubrest.ErrJobLogTooLong):
		return unread(fmt.Sprintf("since its log passed the %d MiB this read takes", githubrest.JobLogBound>>20))
	case errors.Is(err, githubrest.ErrJobLogSlow):
		return unread(fmt.Sprintf("since the read passed its %s bound", logs.JobLogTimeBound()))
	case err != nil && ctx.Err() != nil:
		return unread(readsEnded(ctx))
	case err != nil:
		return unread(fmt.Sprintf("since the fetch of its log failed: %s", fetchFailure(err)))
	}
	names, first, more := failingTests(tail)
	if len(names) == 0 {
		return unread("since the end of its log states no test name")
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = quoteName(name)
	}
	named := strings.Join(quoted, ", ")
	if more > 0 {
		named += fmt.Sprintf(" and %d more", more)
	}
	// This one is deliberately not cut: it is bounded in itself, by at most failingTestsNamed
	// names each already cut and a first failure line already cut (failingTests), and cutting
	// the joined text again would throw away nine of the ten names it just named.
	return fmt.Sprintf(": its log names %s as failing, its first failure line %q", named, first)
}

// readsEnded is what ended a read its caller's own context ended: the daemon stopping, which is
// a cancel and nothing wrong with the log, or the one deadline every read of this removal shares
// running out (the daemon's removalLogReads). Neither is a fetch that failed, and a line saying
// so would send an implementer after a log GitHub answered perfectly well.
func readsEnded(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "since the daemon stopped before its log was read"
	}
	return "since the removal's reads ran out of time"
}

// fetchFailure is why a fetch of a job's log failed, with no URL in what it says: GitHub answers
// that read with a redirect to blob storage, and the URL it redirects to is pre-signed, so the
// *url.Error Go's client returns for a GET that failed on the way carries that whole URL, its
// sig= token included. Every line this package writes is rendered into Dispatch messages, the
// architect's notices and an implementer's prompt, which no signature belongs in: what is left
// is the request's own operation and what failed under it, which say what went wrong without
// saying where the token is. An error of any other shape says no URL of its own and is kept.
func fetchFailure(err error) string {
	var route *url.Error
	if errors.As(err, &route) {
		return fmt.Sprintf("%s: %v", route.Op, route.Err)
	}
	return err.Error()
}

// goFailure is what Go's test binary prints before the name of every test that failed, at the
// start of the line — indented, for a subtest — with the name after it; browserSeparator is what
// a browser runner puts between the parts of a failing row, and browserProject what such a row
// opens the project with. The marker is matched where the line begins rather than anywhere in
// it, so a line that merely says the marker — a step that greps for it, a runner quoting what it
// looked for — states no failing test.
var goFailure = regexp.MustCompile(`^[ \t]*--- FAIL: +(\S+)`)

const (
	browserSeparator = " › "
	browserProject   = "["
)

// failingTestsNamed is how many names one job's line carries, the rest counted after them ("and
// 12 more"), and failureLineUnits how long any one string out of GitHub stays in a line this
// package writes — each name, the first failure line it quotes, the reason a read it could not
// make gives, and the name, result and link of the run itself (cutLine, FailingRun.String): a
// name is one word of a log line, and a table-driven subtest named for the sentence its case is
// runs as long as the line it is printed on.
//
// Both are read off the bound the whole line has to fit under. Every line this package writes
// is rendered into one block of a Dispatch post — the implementer's task and the architect's
// checks-red notice a failed merge sends, the READY withdrawal's post, the re-submission's, and
// merge-removed — and Dispatch refuses a block over DISPATCH_BLOCK_UNITS, 32,000 UTF-16 units
// (packages/contracts), naming it; the daemon then tells the architect its message was refused,
// which is the failure this read exists to spare it. A mass failure states hundreds of names,
// so a read that kept them all would write hundreds of kilobytes into that one block.
//
// What holds one job's line is not the shapes it happens to carry but the rule it is written
// to: every string of GitHub's in it passes through cutLine exactly once (jobFailures). A line
// that names tests is then at most failingTestsNamed names, one first failure line, the ", "
// between the names and the fixed prose holding them — some 2,400 units as the read writes
// them — and a line that could not read the log is one cut reason and one fixed sentence,
// whatever GitHub answered the read with.
//
// That 2,400 is counted before the quoting, and what a message carries is what the quoting
// makes of it, so the bound is stated as a rule per string rather than as a total that a later
// cut or a new string would leave wrong: a name is quoted where it is not plain text
// (quoteName) and the first failure line always (the %q in jobFailures), both of them after
// the cut, and cutLine returns failureLineUnits runes and the mark of the cut, so a cut string
// is failureLineUnits + 1 runes. Go's quoting writes one unprintable rune as an escape of up
// to ten characters ("\U000e0001") and the mark at one unit, so one quoted string is at most
// 10 × failureLineUnits + 3 units, and one job's line at most failingTestsNamed + 1 of those
// with the ", " between the names and the fixed prose (52 units: 16 before the names and 36
// before the first failure line). At today's numbers each of those strings is
// 10 × 200 + 1 + 2 = 2,003 units and the line is 10 × 2,003 + 18 + 2,003 + 52 = 22,103, some
// 22,121 where the names it did not state are counted after them ("and 12 more" at eight
// digits). That is the count Dispatch measures a block in, and it leaves about a third of the
// 32,000 one block takes.
//
// It also stands above failingRunsUnits, the bound a removal's runs are held to, which is the
// one case that bound gives way in: a line of that shape is named whole where it is the
// removal's first run and counted, with every run after it, where it is not (FailingRuns).
//
// No log measured here comes near that, since a name of ordinary text is quoted at two units
// or not at all: the longest shape measured, the browser row of 141 characters browserLog
// carries, holds one job's line under 1,800 units, ten of it and a first failure line cut to
// failureLineUnits. How many such lines one removal names is bounded in its turn
// (failingRunsUnits).
const (
	failingTestsNamed = 10
	failureLineUnits  = 200
)

// markdownPunctuation is the punctuation a name is quoted for carrying (quoteName): what
// markdown reads as a code span, a link, an emphasis, a heading, a quote, a table cell or an
// escape, and the quote mark itself. The underscore is deliberately not among them: it stands
// for every space in a Go subtest's name, and inside a word it opens no emphasis.
const markdownPunctuation = "`*[](){}<>|#~\\\""

// browserCount is the line that opens a browser runner's summary block at the end of its log, the
// count of the tests that failed ("  1 failed").
var browserCount = regexp.MustCompile(`^\s*\d+ failed\s*$`)

// logStamp is the prefix GitHub's job-log route writes before every line it carries: the UTC
// instant the line was printed and one space, as in "2026-11-07T18:04:24.8502586Z   1 failed",
// which stand before whatever the step printed, that line's own indentation included.
var logStamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z `)

// quoteName is a name as a run's line carries it: as it stands where it is plain text, and
// Go-quoted where it is not. A name is text out of a job's log, and the line it goes into is
// rendered into an implementer's prompt and an issue's markdown, so a name carrying markdown's
// own punctuation (markdownPunctuation) or a character that is not printable at all is quoted,
// which bounds it, shows where it ends, and escapes what it holds.
func quoteName(name string) string {
	unprintable := func(r rune) bool { return !unicode.IsPrint(r) }
	if strings.ContainsAny(name, markdownPunctuation) || strings.ContainsFunc(name, unprintable) {
		return strconv.Quote(name)
	}
	return name
}

// cutLine is line held to failureLineUnits runes, with "…" where it was cut. One line of a log
// is as long as whatever printed it made it — a runner that states a whole diff or a stack on
// the line it fails a test with would otherwise carry that into every message the removal is
// told in — and so is one name of it, which a generated test name or a browser row of a long
// title fills on its own.
func cutLine(line string) string {
	runes := []rune(line)
	if len(runes) <= failureLineUnits {
		return line
	}
	return string(runes[:failureLineUnits]) + "…"
}

// failingTests is what tail, the end of a failing job's log, states failed: each test it names as
// failed up to failingTestsNamed, how many it named past that, and the first line naming one,
// each name and the line cut to failureLineUnits (cutLine). Every line is read with GitHub's
// timestamp prefix taken off it first (logStamp), since the job-log route the tail is read from
// writes one before every line; a line that carries none is read as it stands, so a log off that
// route and one off a runner's own output read the same. Two shapes are read, the two the
// measured cases carry, both printed at the end of a log, which is the part this read holds:
//
//   - Go's test binary, which prints "--- FAIL: <name> (<time>)" for each test that failed, its
//     subtests among them, the line indented one step per level. The name is the word after the
//     marker, which the line has to open with (goFailure), and the first failure line that whole
//     line.
//   - a browser runner's summary block, a count of the failures ("  1 failed") followed by one
//     indented row for each, "[project] › file:line:col › title", until a row of another count
//     ends it ("  37 skipped"). The name is the row without the project it opens with, which the
//     first failure line, the whole row, keeps. The last such block in the tail is the one read,
//     since a runner prints its summary once, at the end.
//
// The "✘ <n> [project] › …" rows a browser runner prints as it goes are deliberately left out:
// they stand in the middle of the stream, which a read of the end never reaches. A tail stating
// neither shape names no test, which the caller reports as a log it could not read (jobFailures).
//
// tail is the bytes of the log as the read answers them, parsed where they lie: githubrest.JobLog
// hands over the ring it streamed the body through, so a 54 MB log is never copied into a string
// of its own first, and what this read copies out of it is the names and the one line it keeps.
func failingTests(tail []byte) ([]string, string, int) {
	lines := bytes.Split(tail, []byte("\n"))
	for i, line := range lines {
		if stamp := logStamp.FindIndex(line); stamp != nil {
			lines[i] = line[stamp[1]:]
		}
	}
	var names []string
	first := ""
	more := 0
	// Past the cap a name is counted and dropped rather than kept: a log stating thousands of
	// failures costs this read the cap, not the log.
	name := func(named string) {
		if len(names) < failingTestsNamed {
			names = append(names, cutLine(named))
			return
		}
		more++
	}
	for _, line := range lines {
		failure := goFailure.FindSubmatch(line)
		if failure == nil {
			continue
		}
		if first == "" {
			first = cutLine(string(bytes.TrimSpace(line)))
		}
		name(string(failure[1]))
	}
	if len(names) > 0 {
		return names, first, more
	}
	block := -1
	for i, line := range lines {
		if browserCount.Match(line) {
			block = i
		}
	}
	if block < 0 {
		return nil, "", 0
	}
	for _, line := range lines[block+1:] {
		row := string(bytes.TrimSpace(line))
		if !bytes.HasPrefix(line, []byte(" ")) || !strings.Contains(row, browserSeparator) {
			break
		}
		if first == "" {
			first = cutLine(row)
		}
		named := row
		if test, found := strings.CutPrefix(row, browserProject); found {
			if _, after, cut := strings.Cut(test, browserSeparator); cut {
				named = after
			}
		}
		name(named)
	}
	return names, first, more
}
