package daemon

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/verdict"
)

// The lines the writer logs, besides each write's own legion_verdict_write: once where it is built
// on an App id that is no number, once per pull request whose head is another project's issue
// branch, where no verdict stands, once while the App is in token mode, each time a merge-state
// read goes on after waiting its bound on its issue's write, each write that fails, naming what
// repairs it, and once a head's change has had no answer from GitHub on noAnswerSweeps of the
// writers' sweeps in a row, when the head is decided failure.
const (
	verdictAppIDNotANumber = "verdict: github_apps.implement.app_id is no number, so the writer of legion-verdict tells no check run as the App's, its own among them, and writes nothing; set it to the App's id"
	otherProjectsHead      = "verdict: the head is another project's issue branch, of whose steps this daemon holds none, so no verdict stands there"
	verdictTokenMode       = "verdict: legion-verdict is not written: the App is in token mode, which mints no verdict token"
	verdictReadWentOn      = "verdict: a merge-state read went on before its issue's write of legion-verdict ended"
	verdictWriteFailed     = "verdict: a write of legion-verdict failed; the writers' sweep at boot and every 10 minutes writes it again"
)

// verdictNoAnswer is the WARN of a head GitHub gave no answer for on noAnswerSweeps of the writers'
// sweeps in a row, which names the bound from noAnswerSweeps itself, so the two cannot part.
var verdictNoAnswer = "verdict: GitHub has given no answer to the read of a head's change on " + strconv.Itoa(noAnswerSweeps) +
	" of the writers' sweeps in a row, so the change counts as unread and legion-verdict is now failure there, until GitHub answers"

// noAnswerSweeps is how many of the writers' sweeps in a row may find GitHub giving no answer to
// the read of one head's change before the head is decided failure, as for a change GitHub answers
// cannot be read (boundUnanswered), and the writer warns (verdictNoAnswer, whose text is built from
// this number; troubleshooting.md, concepts.md and the changelog name it too): 3 sweeps, 10 minutes
// apart (verdictSweepInterval), are 30 minutes, longer than the short outages of GitHub that end by
// themselves, and well inside the merge queue's 180-minute timeout, so the queue removes a group
// whose head GitHub never answers for, and the owner hears of it, before the group times out.
const noAnswerSweeps = 3

// verdictWriter is the state of the daemon's writer of the required check legion-verdict (spec
// R4, R19): the repositories its configuration names under verdict_repositories, the id of the one
// App, by which it tells that App's check runs from every other App's, the heads a fact asked it to
// write on that it has not written yet, and what the merge-state reads of an issue wait on. A fact
// that can change a head's verdict asks for the write on its commit (workflow.Config.WriteVerdict,
// queueVerdictWrite), a merge group's asks for one on the group's commit (queueMergeGroupWrite),
// and one goroutine writes them one at a time (writeVerdicts), so a fact's transaction waits on no
// GitHub call and two writes of one head never both make a run. A write a fact asked for is held in
// memory, and a restart loses one not made: the writers' sweep, at boot and every 10 minutes,
// writes each head whose verdict the record holds and GitHub shows no run of this daemon's on.
type verdictWriter struct {
	entries []config.VerdictRepository
	// appID is the one App's id (github_apps.implement.app_id), 0 where the configuration names
	// none a number reads from, which tells no run as the App's, so the writer writes nothing
	// (writeDecided). A rename of the App changes its slug and its bot's login, never its id (spec
	// R16, R19).
	appID int64
	// bound is how long a merge-state read waits on its issue's write (await).
	bound time.Duration

	mu sync.Mutex
	// queue is each head a fact asked for and not yet taken, oldest first, and swept each the
	// writers' sweep asked for (overForeign), which the writer takes only while queue is empty, so
	// a fact's write, which its issue's merge-state reads wait on, waits behind no sweep's; running
	// is how many writes are being made; pending is, by issue, how many of all three are that
	// issue's, which its merge-state reads wait on (await), and ended is closed and made again each
	// time a write ends.
	queue   []verdictTarget
	swept   []verdictTarget
	running int
	pending map[string]int
	ended   chan struct{}
	wake    chan struct{}
	// foreign is, by repository (lowercased) and number, each pull request whose head is another
	// project's issue branch, with that issue, which the daemon has logged once and decides as a
	// head no verdict stands at.
	foreign map[string]string
	// unanswered is, by repository (lowercased) and head, each head the writers' sweep listed in
	// its last pass of the head's repository, with how many passes in a row found GitHub giving no
	// answer to the read of the head's change (countUnanswered); sweeps counts the passes, each of
	// which sweepBegun numbers.
	unanswered map[string]unansweredHead
	sweeps     uint64
	// tokenMode logs once that the App is in token mode (appauth.ErrNoVerdictToken).
	tokenMode sync.Once
}

// verdictTarget is one head a write of legion-verdict is asked for: the pull request's repository
// and number, the head, and the issue whose merge-state reads wait on the write ("" for a pull
// request no Legion issue owns). A merge group's write (queueMergeGroupWrite) names the group's
// commit as group, which it writes on, and no head: it decides the pull request at the head that
// stands when the write is made (writeMergeGroup). overForeign is writeDecided's: the writers'
// sweep's ask to put failure over another writer's run or status where no verdict stands. alone is
// the sweep's word that its list of open pull requests shows no other at head, so the write reads
// GitHub for none (atHead).
type verdictTarget struct {
	issue       string
	repository  ghrepo.Repository
	number      int
	head        string
	group       string
	overForeign bool
	alone       bool
}

// newVerdictWriter is the writer of entries, appID being the one App's id as the configuration
// spells it. An id that is set and is no number is logged on log, once, here
// (verdictAppIDNotANumber); an id that is not set holds 0 and says nothing, since a daemon with no
// App id writes no legion-verdict as the App.
func newVerdictWriter(entries []config.VerdictRepository, appID string, log *slog.Logger) *verdictWriter {
	var id int64
	if appID != "" {
		parsed, err := strconv.ParseInt(appID, 10, 64)
		if err != nil {
			log.Error(verdictAppIDNotANumber, "setting", "github_apps.implement.app_id", "app_id", appID, "error", err)
		} else {
			id = parsed
		}
	}
	return &verdictWriter{entries: entries, appID: id, bound: issueRead, pending: map[string]int{}, ended: make(chan struct{}),
		wake: make(chan struct{}, 1), foreign: map[string]string{}, unanswered: map[string]unansweredHead{}}
}

// entry is the verdict_repositories entry that names repository, compared without case as GitHub
// compares names, and whether one does. A writer of no configuration names none.
func (v *verdictWriter) entry(repository string) (config.VerdictRepository, bool) {
	if v == nil {
		return config.VerdictRepository{}, false
	}
	for _, entry := range v.entries {
		if strings.EqualFold(entry.Repository.String(), repository) {
			return entry, true
		}
	}
	return config.VerdictRepository{}, false
}

// repositories is every repository the writer writes on, which GitHub intake hears whatever the
// mapping names (heardRepositories) and decodes the outside facts of (intake.ConsumerSpec).
func (v *verdictWriter) repositories() []ghrepo.Repository {
	if v == nil {
		return nil
	}
	repositories := make([]ghrepo.Repository, 0, len(v.entries))
	for _, entry := range v.entries {
		repositories = append(repositories, entry.Repository)
	}
	return repositories
}

// add queues target, a fact's or, where it asks for overForeign, the writers' sweep's, unless the
// same target is queued already and not yet taken, which the one write answers; and wakes the
// writer.
func (v *verdictWriter) add(target verdictTarget) {
	v.mu.Lock()
	defer v.mu.Unlock()
	queue := &v.queue
	if target.overForeign {
		queue = &v.swept
	}
	if slices.Contains(*queue, target) {
		return
	}
	*queue = append(*queue, target)
	v.pending[target.issue]++
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

// next takes the oldest queued target a fact asked for, or, where none waits, the oldest the
// writers' sweep asked for, and whether there was one.
func (v *verdictWriter) next() (verdictTarget, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	queue := &v.queue
	if len(*queue) == 0 {
		queue = &v.swept
	}
	if len(*queue) == 0 {
		return verdictTarget{}, false
	}
	target := (*queue)[0]
	*queue = (*queue)[1:]
	v.running++
	return target, true
}

// done ends the write of target that next took, and tells every read waiting on its issue.
func (v *verdictWriter) done(target verdictTarget) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.running--
	if v.pending[target.issue]--; v.pending[target.issue] <= 0 {
		delete(v.pending, target.issue)
	}
	close(v.ended)
	v.ended = make(chan struct{})
}

// await returns true once issue has no write of legion-verdict queued or being made, false once
// the writer's bound passes with one still pending, or ctx's error once ctx ends. The merge-state
// reads of the issue call it before each read (readMergeState): GitHub answers `blocked` until
// the required check exists, so a read made before the write that a verdict asked for on the same
// commit would hold the round on a check about to be written (spec R4). The one writer makes every
// head's writes in turn, so a slow GitHub or a long queue before the issue's write would hold its
// reads with no end: after the bound the read goes on, as it did before there was a writer. A
// runtime with no writer waits on nothing.
func (v *verdictWriter) await(ctx context.Context, issue string) (bool, error) {
	if v == nil {
		return true, nil
	}
	bound := time.NewTimer(v.bound)
	defer bound.Stop()
	for {
		v.mu.Lock()
		pending, ended := v.pending[issue], v.ended
		v.mu.Unlock()
		if pending == 0 {
			return true, nil
		}
		select {
		case <-ended:
		case <-bound.C:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// foreignKey names a pull request in the writer's foreign set.
func foreignKey(repository string, number int) string {
	return strings.ToLower(repository) + "#" + strconv.Itoa(number)
}

// otherIssue is the other project's issue whose branch the pull request's head is, "" where the
// writer knows of none.
func (v *verdictWriter) otherIssue(repository string, number int) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.foreign[foreignKey(repository, number)]
}

// markForeign records that the pull request's head is issue's branch, another project's, and says
// whether this is the first time.
func (v *verdictWriter) markForeign(repository string, number int, issue string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := foreignKey(repository, number)
	if v.foreign[key] != "" {
		return false
	}
	v.foreign[key] = issue
	return true
}

// unansweredHead is what the writer keeps of a head the writers' sweep listed: how many of its passes
// in a row found GitHub giving no answer to the read of the head's change, the pass that last listed
// the head, and the pass that last counted it, so the pull requests of one head count once a pass.
type unansweredHead struct {
	count           int
	listed, counted uint64
}

// sweepBegun numbers a new pass of the writers' sweep over repository, whose list of open pull
// requests holds the heads listed, and keeps each listed head for that pass. A head the list does
// not hold, as one whose pull request closed or moved on, is forgotten: no sweep writes it again, so
// its count would stay in memory until a restart, and listed again it counts from none.
func (v *verdictWriter) sweepBegun(repository string, listed map[string]int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sweeps++
	prefix := unansweredKey(repository, "")
	for key := range v.unanswered {
		if head, ok := strings.CutPrefix(key, prefix); ok && listed[head] == 0 {
			delete(v.unanswered, key)
		}
	}
	for head := range listed {
		key := unansweredKey(repository, head)
		kept := v.unanswered[key]
		kept.listed = v.sweeps
		v.unanswered[key] = kept
	}
}

// countUnanswered counts decided, a write's decision of head of repository, in the head's run of
// passes with no answer, sweep being whether the writers' sweep asked for the write. A sweep's write
// of an undecided head counts one, once in the pass that listed the head, however many pull requests
// stand there. A sweep's write of a head decided otherwise starts the count again, and so does any
// write that read an answer of GitHub's about the head's change, its own pull request's or another's
// at the head (atHead). A fact's or a merge group's write of an undecided head, or one that read no
// change, leaves the count as it stands: GitHub has not answered. It answers the count, and whether
// this write brought it to noAnswerSweeps, the one time a run of such passes is warned of.
func (v *verdictWriter) countUnanswered(repository, head string, decided verdictDecision, sweep bool) (int, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := unansweredKey(repository, head)
	kept, listed := v.unanswered[key]
	switch {
	case decided.conclusion == verdict.Undecided && sweep && listed && kept.counted != kept.listed:
		kept.count++
		kept.counted = kept.listed
		v.unanswered[key] = kept
		return kept.count, kept.count == noAnswerSweeps
	case decided.conclusion == verdict.Undecided:
		return kept.count, false
	case listed && (sweep || decided.answered):
		kept.count = 0
		v.unanswered[key] = kept
	}
	return 0, false
}

// unansweredKey names head of repository in the writer's unanswered counts.
func unansweredKey(repository, head string) string {
	return strings.ToLower(repository) + "@" + head
}

// queueVerdictWrite is the engine's Config.WriteVerdict, and the outside facts' own ask: it queues a
// write of legion-verdict on head of pull request number of repo for the writer (writeVerdicts),
// with issue's merge-state reads held until it ends. It is called on a fact's commit, and before the
// same commit asks for a read (workflow's askVerdictWrite), so the read waits for the write. A
// repository no verdict entry names (verdictEntry) asks nothing, since its write would write
// nothing and its issue's reads would wait on it; so does a repository name that does not parse,
// which is logged, and a runtime with no writer.
func (w *workflowRuntime) queueVerdictWrite(issue, repo string, number int, head string) {
	if w.verdicts == nil {
		return
	}
	repository, err := ghrepo.Parse("the repository of a legion-verdict write", repo)
	if err != nil {
		w.log.Warn("verdict: a write of legion-verdict names no repository, so nothing is written", "repository", repo, "pull_request", number, "head", head, "error", err)
		return
	}
	if _, ok := w.verdictEntry(repository); !ok {
		return
	}
	w.verdicts.add(verdictTarget{issue: issue, repository: repository, number: number, head: head})
}

// verdictEntry is the verdict_repositories entry that names repository, under the name a record
// keeps, the one GitHub serves it under now (repositories.serving), or the other name of a
// configured rename (Renames.Partner), and whether one does: an entry written under the name a
// rename moved the repository away from names it under its new name too.
func (w *workflowRuntime) verdictEntry(repository ghrepo.Repository) (config.VerdictRepository, bool) {
	if entry, ok := w.verdicts.entry(repository.String()); ok {
		return entry, true
	}
	if entry, ok := w.verdicts.entry(w.repositories.serving(repository).String()); ok {
		return entry, true
	}
	if partner, renamed := w.renames().Partner(repository.String()); renamed {
		return w.verdicts.entry(partner.String())
	}
	return config.VerdictRepository{}, false
}

// namesVerdictRepository is whether repo, the name a webhook fact carries, is a verdict
// repository under any of its names (verdictEntry). A name that does not parse is none, and so is
// every name for a runtime with no writer.
func (w *workflowRuntime) namesVerdictRepository(repo string) bool {
	repository, err := ghrepo.Parse("the repository of a webhook fact", repo)
	if err != nil || w.verdicts == nil {
		return false
	}
	_, ok := w.verdictEntry(repository)
	return ok
}

// writeVerdicts makes each write queueVerdictWrite queued, one at a time and oldest first, until
// ctx ends. A write that fails - GitHub's error, its rate limit, a token that could not be minted -
// is logged and not made again here: the writers' sweep, at boot and every 10 minutes, writes each
// head whose verdict the record holds and that GitHub shows no run of this daemon's on.
func (w *workflowRuntime) writeVerdicts(ctx context.Context) {
	for {
		target, ok := w.verdicts.next()
		if !ok {
			select {
			case <-w.verdicts.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		var err error
		if target.group != "" {
			err = w.writeMergeGroup(ctx, target)
		} else {
			err = w.writeVerdictOver(ctx, target)
		}
		if err != nil && ctx.Err() == nil {
			w.log.Warn(verdictWriteFailed, "repository", target.repository.String(), "pull_request", target.number, "head", target.head,
				"group", target.group, "issue", target.issue, "error", err)
		}
		w.verdicts.done(target)
	}
}

// writeVerdictOver writes legion-verdict on target's head, of target's pull request, from the
// verdict the daemon records at that head (verdict.Decide) of that pull request and of every other
// open pull request at the head (atHead): success where it stands, failure where a refusal stands,
// and nothing where neither does, so the head cannot merge (spec R4); with target's overForeign,
// the writers' sweep's ask, failure over another writer's run or status there (writeDecided). It
// writes only on a repository its verdict_repositories names, under the name a record keeps or the
// one GitHub serves it under now (verdictEntry), and on every legion/<KEY> head there, of whatever
// project: another project's issue head holds no verdict here (decideVerdict). It creates its own
// check run or turns each run of the one App to the head's conclusion, the runs its record holds
// among them, and writes its conclusion over each status of the one App its record does not hold
// (spec R19, writeDecided). A run this daemon wrote stands only while the head's verdict does: a
// write a fact asks for on a head with no verdict now, of its pull request or over every open pull
// request there, turns that run to failure, and writes its output for the head as it is now. Where
// the pull request's own verdict is none and the daemon wrote nothing there, it writes nothing and
// reads nothing of GitHub. Where GitHub gave no answer to a read the decision needs, the head is
// undecided (verdict.Undecided): no run of the daemon's own is written, and one written before
// stands until a read is answered, while the writers' sweep still writes over each run and status
// of the App its record does not hold (writeDecided); and once noAnswerSweeps of its sweeps in a row
// found the head so, it is failure, as a change the daemon cannot read (boundUnanswered).
//
// Once a pass writes a Legion head's success, or turns its run or status to success, the issue's
// merge state is read once more, a ladder's wait later (mergeStateWait): GitHub can answer the read
// the write held (verdictWriter.await) with `blocked` while it has not yet counted the new run, an
// answer that ends the climb, and no other fact asks again, since Envoy leaves the one App's runs
// out of the CI record. A pass that finds the success already written asks for nothing more: the
// pass that wrote it asked already.
func (w *workflowRuntime) writeVerdictOver(ctx context.Context, target verdictTarget) error {
	entry, ok := w.verdictEntry(target.repository)
	if !ok {
		return nil
	}
	written := verdictTarget{repository: target.repository, number: target.number, head: target.head}
	writes := &headWrites{}
	decided, err := w.decideHead(ctx, entry, target, written, writes)
	if err != nil {
		return err
	}
	decided = w.boundUnanswered(target, decided)
	changed, err := w.writeDecidedChanged(ctx, written, decided, target.overForeign, writes)
	if err != nil {
		return err
	}
	if changed && decided.conclusion == verdict.Success && decided.issue != "" {
		issue := decided.issue
		time.AfterFunc(w.mergeStateWait(), func() {
			if ctx.Err() == nil {
				w.queueMergeState(issue)
			}
		})
	}
	return nil
}

// decideHead is what writeVerdictOver writes on target's head, written being that head of target's
// pull request: the pull request's decision (decideVerdict), made over every open pull request at
// the head unless the sweep found the pull request alone there (atHead), and failure where that is
// none and the daemon wrote a run or status there (wroteAt): a success stands only while the
// head's verdict does, and a failure's output is written again for the head as it is now, so a
// failure for a commit with no reader no longer says each check reads yes once one does not. A
// failure is made over the others too, for the head's output alone (headOutput): its conclusion
// needs no other read, so where GitHub does not let the others be read (gitHubReadError) the pull
// request's own failure stands, as its no decision yet does, which writes no run of the daemon's own
// and counts toward the bound. A store failure deciding the others is no answer of GitHub's: it
// fails the write, which the writers' sweep makes again. The write reads the open pull requests at
// the head once (headPulls), and what the store records there once (writes).
func (w *workflowRuntime) decideHead(ctx context.Context, entry config.VerdictRepository, target, written verdictTarget, writes *headWrites) (verdictDecision, error) {
	pulls := &headPulls{}
	decided, err := w.decideVerdict(ctx, entry, target.repository, target.number, target.head, pulls)
	if err != nil {
		return verdictDecision{}, err
	}
	standing, read := false, false
	if decided.conclusion == verdict.None && !target.overForeign {
		if standing, err = w.wroteAt(ctx, written, writes); err != nil || !standing {
			return decided, err
		}
		read = true
	}
	if !target.alone {
		combined, err := w.atHead(ctx, entry, target.repository, target.number, decided, pulls)
		switch {
		case err == nil:
			decided = combined
		case !gitHubReadFailed(err) || decided.conclusion != verdict.Failure && decided.conclusion != verdict.Undecided:
			return verdictDecision{}, err
		}
	}
	if decided.conclusion == verdict.None && !target.overForeign && !read {
		// Another pull request at the head turned the pull request's own verdict to none there.
		if standing, err = w.wroteAt(ctx, written, writes); err != nil {
			return verdictDecision{}, err
		}
	}
	if decided.conclusion == verdict.None && standing {
		decided.conclusion = verdict.Failure
	}
	return decided, nil
}

// wroteAt is whether the store records a run or a status the daemon wrote on target's head
// (recordedVerdictWrites), read once in the write writes is of.
func (w *workflowRuntime) wroteAt(ctx context.Context, target verdictTarget, writes *headWrites) (bool, error) {
	recorded, err := w.recordedVerdictWrites(ctx, target, writes)
	return recorded.newest != verdict.None, err
}

// boundUnanswered is decided, the decision of target's head, with no answer bounded (readCommits):
// it counts the decision (countUnanswered), and an undecided head whose count has reached
// noAnswerSweeps is one whose change the daemon cannot read, so failure (unreadAfterNoAnswer); the
// WARN that says so, naming the repository, the head, the count and the last error, is logged once,
// when the count reaches the bound. Every other decision is left as it is.
func (w *workflowRuntime) boundUnanswered(target verdictTarget, decided verdictDecision) verdictDecision {
	sweeps, reached := w.verdicts.countUnanswered(target.repository.String(), target.head, decided, target.overForeign)
	if decided.conclusion != verdict.Undecided || sweeps < noAnswerSweeps {
		return decided
	}
	if reached {
		w.log.Warn(verdictNoAnswer, "repository", target.repository.String(), "head", target.head, "sweeps", sweeps, "error", decided.unanswered)
	}
	return unreadAfterNoAnswer(decided)
}

// unreadAfterNoAnswer is decided, a head the writers' sweeps found undecided noAnswerSweeps times
// in a row, as one whose change the daemon cannot read: failure for that reason
// (verdict.ChangeUnread), which the run's title and summary name.
func unreadAfterNoAnswer(decided verdictDecision) verdictDecision {
	decided.conclusion, decided.reason, decided.noReader = verdict.Failure, verdict.ChangeUnread, nil
	return decided
}
