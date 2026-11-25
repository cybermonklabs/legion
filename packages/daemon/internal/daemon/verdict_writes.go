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
// read goes on after waiting its bound on its issue's write, and each write that fails, naming
// what repairs it.
const (
	verdictAppIDNotANumber = "verdict: github_apps.implement.app_id is no number, so the writer of legion-verdict tells no check run as the App's, its own among them, and writes nothing; set it to the App's id"
	otherProjectsHead      = "verdict: the head is another project's issue branch, of whose steps this daemon holds none, so no verdict stands there"
	verdictTokenMode       = "verdict: legion-verdict is not written: the App is in token mode, which mints no verdict token"
	verdictReadWentOn      = "verdict: a merge-state read went on before its issue's write of legion-verdict ended"
	verdictWriteFailed     = "verdict: a write of legion-verdict failed; the writers' sweep at boot and every 10 minutes writes it again"
)

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
		wake: make(chan struct{}, 1), foreign: map[string]string{}}
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
// (spec R19, writeDecided). A success this daemon wrote stands only while the head's verdict does:
// a write a fact asks for on a head with no verdict now turns that success to failure, and writes
// nothing, and reads nothing of GitHub, on a head it wrote no success on.
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
	decided, err := w.decideVerdict(ctx, entry, target.repository, target.number, target.head)
	if err != nil {
		return err
	}
	written := verdictTarget{repository: target.repository, number: target.number, head: target.head}
	standing := false
	if decided.conclusion == verdict.None && !target.overForeign {
		if standing, err = w.wroteSuccess(ctx, written); err != nil || !standing {
			return err
		}
	}
	if decided.conclusion != verdict.Failure && !target.alone {
		if decided, err = w.atHead(ctx, entry, target.repository, target.number, decided); err != nil {
			return err
		}
	}
	if decided.conclusion == verdict.None && standing {
		decided.conclusion = verdict.Failure
	}
	changed, err := w.writeDecidedChanged(ctx, written, decided, target.overForeign)
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
