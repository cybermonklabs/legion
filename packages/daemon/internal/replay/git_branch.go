package replay

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

const gitReadLine = "replay: a head's merge state read from git"

const (
	gitClean      = "clean"
	gitConflict   = "conflict"
	gitOrder      = "order"
	gitAfterMerge = "after-merge"
	gitMissing    = "missing"
	gitNeverRan   = "never-ran"
)

var gitReadKinds = []string{gitClean, gitConflict, gitOrder, gitAfterMerge, gitMissing, gitNeverRan}

// GitReadCounts partitions the reads of one repository by answer or reason for no answer.
type GitReadCounts map[string]int

func (c *GitReadCounts) add(kind string) {
	if *c == nil {
		*c = make(GitReadCounts, len(gitReadKinds))
		for _, k := range gitReadKinds {
			(*c)[k] = 0
		}
	}
	(*c)[kind]++
}

// GitReadTable separates the end reads by the recorded outcome, and boot reads by verdict alone.
type GitReadTable struct {
	RecordClean    GitReadCounts `json:"recordClean,omitempty"`
	RecordConflict GitReadCounts `json:"recordConflict,omitempty"`
	Boot           GitReadCounts `json:"boot,omitempty"`
}

// GitLoopHours counts conflict work started by an answered read or by the generator.
type GitLoopHours struct {
	Answered float64 `json:"answered"`
	Drawn    float64 `json:"drawn"`
}

type landedCommit struct {
	sha string
	at  time.Time
}

type gitAnswer struct {
	kind, head string
	n          int
}

// gitRepository is the immutable part of the base and verdict index. Positions on the recorded
// first-parent chain do not change when a replay moves a commit's landing.
type gitRepository struct {
	branch   string
	input    GitInput
	chain    map[string]int
	paths    map[string][]string
	recorded []landedCommit
}

func newGitRepository(branch string, in GitInput, merges []Merge) *gitRepository {
	r := &gitRepository{branch: branch, input: in, chain: make(map[string]int, len(merges)), paths: make(map[string][]string, len(in.Commits))}
	for _, m := range merges {
		if m.Branch == "" || m.Branch == branch {
			r.recorded = append(r.recorded, landedCommit{sha: m.SHA, at: m.At})
			r.chain[m.SHA] = len(r.recorded)
		}
	}
	for _, c := range in.Commits {
		r.paths[c.SHA] = c.Paths
	}
	return r
}

func newGitRepositories(w Window, t Trace) map[string]*gitRepository {
	if len(w.Git) == 0 {
		return nil
	}
	out := make(map[string]*gitRepository, len(w.Git))
	for repo, input := range w.Git {
		out[strings.ToLower(repo)] = newGitRepository(cmp.Or(t.Branches[repo], defaultBranch), input, w.Merges[repo])
	}
	return out
}

func (p GitPull) standingHead(at time.Time) GitHead {
	priority := func(from string) int {
		switch from {
		case GitHeadForcePush:
			return 2
		case GitHeadRead:
			return 1
		default:
			return 0
		}
	}
	var head GitHead
	for _, h := range p.Heads {
		if h.At.After(at) {
			break
		}
		if h.At.After(head.At) || h.At.Equal(head.At) && priority(h.From) >= priority(head.From) {
			head = h
		}
	}
	return head
}

// touching keeps commits in landing order. A missing path list is not an empty path list.
// With a time cutoff, the head is synthetic and the cutoff replaces its recorded merge base.
func (r *gitRepository) touching(commits []landedCommit, paths []string, base, own string, after time.Time) ([]landedCommit, bool) {
	var out []landedCommit
	missing := false
	for _, c := range commits {
		if c.sha == own || (!after.IsZero() && !c.at.After(after)) || (after.IsZero() && r.chain[c.sha] <= r.chain[base]) {
			continue
		}
		changed, known := r.paths[c.sha]
		switch {
		case len(paths) == 0:
		case !known:
			missing = true
		case pathsIntersect(paths, changed):
			out = append(out, c)
		}
	}
	return out, missing
}

func pathsIntersect(a, b []string) bool {
	for len(a) > 0 && len(b) > 0 {
		switch strings.Compare(a[0], b[0]) {
		case -1:
			a = a[1:]
		case 1:
			b = b[1:]
		default:
			return true
		}
	}
	return false
}

func (r *gitRepository) read(pr int, drawn bool, recorded, after time.Time, commits []landedCommit) gitAnswer {
	if drawn || pr == 0 {
		return gitAnswer{kind: gitNeverRan}
	}
	answer := gitAnswer{kind: gitMissing}
	if r == nil {
		return answer
	}
	pull, ok := r.input.Pull(pr)
	if !ok {
		return answer
	}
	head := pull.standingHead(recorded)
	answer.head = head.SHA
	if !head.Read() || pull.Base != r.branch || len(pull.BaseChanges) > 0 && recorded.Before(pull.BaseChanges[len(pull.BaseChanges)-1]) {
		return answer
	}
	for _, missing := range r.input.Missing {
		if missing.PR == pr && (missing.Head == head.SHA || missing.Head == "" && githubReason(missing.Reason)) {
			return answer
		}
	}
	touching, missing := r.touching(commits, head.Paths, head.MergeBase, pull.Merge, after)
	if missing {
		return answer
	}
	answer.kind = gitClean
	if len(touching) == 0 {
		return answer
	}
	if own := r.chain[pull.Merge]; own > 0 {
		for _, c := range touching {
			if r.chain[c.sha] > own {
				answer.kind = gitAfterMerge
				return answer
			}
		}
	}
	answer.kind = gitOrder
	if !after.IsZero() || len(touching) > len(head.Verdicts) {
		return answer
	}
	for i, c := range touching {
		if c.sha != head.Verdicts[i].Base {
			return answer
		}
	}
	answer.kind, answer.n = gitClean, len(touching)
	if head.Verdicts[answer.n-1].Conflict {
		answer.kind = gitConflict
	}
	return answer
}

func (m ConflictModel) profileChance(in phase.Phase, merges int, at time.Time, boot bool) float64 {
	if merges == 0 {
		return 0
	}
	return m.chanceAt(in, merges, at, boot)
}

func (d *driver) gitIdentity(key string) (string, int) {
	if iss := d.trace.Issues[key]; iss != nil {
		return strings.ToLower(iss.Repo), recordedPR(iss)
	}
	return "", 0
}

func recordedPR(iss *IssueTrace) int {
	return cmp.Or(ownPullRequest(iss), iss.PullRequestNumber)
}

// profile returns the standing head's paths, or the last usable paths without a known head.
func (r *gitRepository) profile(pr int, at time.Time) GitHead {
	pull, ok := r.input.Pull(pr)
	if !ok {
		return GitHead{}
	}
	if head := pull.standingHead(at); head.Read() {
		return head
	}
	for _, head := range slices.Backward(pull.Heads) {
		if head.Read() {
			return GitHead{Tip: head.Tip, Paths: head.Paths}
		}
	}
	return GitHead{}
}

func (d *driver) drawProfile(p *piece, r *gitRepository, at time.Time) GitHead {
	repo, pr := d.gitIdentity(p.issue)
	profileRepo := r
	if pr == 0 && p.source != "" && p.source != p.issue {
		sourceRepo, sourcePR := d.gitIdentity(p.source)
		pr = sourcePR
		if sourceRepo != repo {
			profileRepo = d.merges.git[sourceRepo]
		}
	}
	if profileRepo == nil {
		return GitHead{}
	}
	head := profileRepo.profile(pr, at)
	if p.source != "" && p.source != p.issue {
		head.MergeBase = ""
	}
	return head
}

// replayGit keeps only mutable, per-run state. A held commit has no landing until its issue merges.
type replayGit struct {
	held       map[string]map[string]bool
	landed     map[string][]landedCommit
	nextCommit map[string]int
	loopEnds   map[string]time.Time
	firstImpl  map[string]time.Time
	forwardAt  map[string]time.Time
	loops      []*gitLoop
	openMoves  map[string][]*gitLoop
	reads      map[string]GitReadCounts
	tables     map[string]GitReadTable
	hours      map[string]GitLoopHours
}

func (d *driver) setupGit() {
	g := &d.git
	g.held, g.landed = make(map[string]map[string]bool), make(map[string][]landedCommit)
	g.nextCommit = make(map[string]int)
	g.openMoves = make(map[string][]*gitLoop)
	g.loopEnds, g.firstImpl, g.forwardAt = make(map[string]time.Time), make(map[string]time.Time), make(map[string]time.Time)
	g.reads, g.tables, g.hours = make(map[string]GitReadCounts), make(map[string]GitReadTable), make(map[string]GitLoopHours)
	for key, pieces := range d.script.pieces {
		if len(pieces) == 0 {
			continue
		}
		repo := strings.ToLower(d.trace.Issues[key].Repo)
		pr := ownPullRequest(d.trace.Issues[key])
		r := d.merges.git[repo]
		if r == nil || pr == 0 || neverStartedIn(d.trace, key) {
			continue
		}
		pull, ok := r.input.Pull(pr)
		if !ok || pull.Merge == "" {
			continue
		}
		if !slices.ContainsFunc(r.recorded, func(c landedCommit) bool { return c.sha == pull.Merge && !c.at.Before(d.window.From) }) {
			continue
		}
		if g.held[repo] == nil {
			g.held[repo] = make(map[string]bool)
		}
		g.held[repo][pull.Merge] = true
	}
}

func (d *driver) landGitMerge(key string) {
	repo, pr := d.gitIdentity(key)
	if r := d.merges.git[repo]; r != nil {
		if pull, ok := r.input.Pull(pr); ok && d.git.held[repo][pull.Merge] &&
			!slices.ContainsFunc(d.git.landed[repo], func(c landedCommit) bool { return c.sha == pull.Merge }) {
			now := d.clock.Instant()
			base := d.git.base(repo, r, now)
			d.git.landed[repo] = append(base, landedCommit{sha: pull.Merge, at: now})
		}
	}
}

func (g *replayGit) base(repo string, r *gitRepository, at time.Time) []landedCommit {
	if r == nil {
		return nil
	}
	// The driver visits instants in order. Advance external commits before processing
	// a simulated merge at that instant, so later reads get exactly the landing order.
	base := g.landed[repo]
	next := g.nextCommit[repo]
	for next < len(r.recorded) && !r.recorded[next].at.After(at) {
		c := r.recorded[next]
		next++
		if !g.held[repo][c.sha] {
			base = append(base, c)
		}
	}
	g.nextCommit[repo], g.landed[repo] = next, base
	return base
}

func (d *driver) readGit(p *piece, recorded time.Time, boot bool) gitAnswer {
	repo, own := d.gitIdentity(p.issue)
	r := d.merges.git[repo]
	now := d.clock.Instant()
	answer := r.read(own, p.source != "" && p.source != p.issue, recorded, d.git.loopEnds[p.issue], d.git.base(repo, r, now))
	by := "end"
	if boot {
		by = "boot"
	}
	attrs := []any{"issue", p.issue, "repo", repo, "head", answer.head, "recorded", recorded, "by", by}
	if answer.kind == gitClean || answer.kind == gitConflict {
		attrs = append(attrs, "answer", answer.kind, "n", answer.n)
	} else {
		attrs = append(attrs, "answer", "unanswered", "kind", answer.kind)
	}
	d.log.Info(gitReadLine, attrs...)
	counts := d.git.reads[repo]
	counts.add(answer.kind)
	d.git.reads[repo] = counts
	table := d.git.tables[repo]
	switch {
	case boot:
		table.Boot.add(answer.kind)
	case p.move().conflict():
		table.RecordConflict.add(answer.kind)
	default:
		table.RecordClean.add(answer.kind)
	}
	d.git.tables[repo] = table
	return answer
}

// gitLoop follows a real conflict round; a waiting conflict starts its round when the merge
// forward is asked for, not while the architect still works.
type gitLoop struct {
	issue      string
	phase      phase.Phase
	start, end time.Time
	kept       bool
	source     string
}

const (
	loopAnswered = "answered"
	loopDrawn    = "drawn"
)

func (d *driver) beginGitLoop(key string, ph phase.Phase, forward, kept bool, source string) *gitLoop {
	if d.git.loopEnds == nil || source == "" && len(d.merges.git) == 0 {
		return nil
	}
	l := &gitLoop{issue: key, phase: ph, start: d.clock.Instant(), kept: kept, source: source}
	d.git.loops = append(d.git.loops, l)
	if !forward {
		d.git.openMoves[key] = append(d.git.openMoves[key], l)
	}
	if kept {
		delete(d.git.loopEnds, key)
	}
	return l
}

func (d *driver) endGitLoop(l *gitLoop) {
	if l == nil || !l.end.IsZero() {
		return
	}
	l.end = d.clock.Instant()
	if !l.kept {
		d.git.loopEnds[l.issue] = l.end
	}
}

func (d *driver) endedGitLoops(key string, to phase.Phase) {
	if loops := d.git.openMoves[key]; len(loops) > 0 {
		d.git.openMoves[key] = slices.DeleteFunc(loops, func(l *gitLoop) bool {
			if l.phase != to {
				return false
			}
			d.endGitLoop(l)
			return true
		})
	}
}

func (d *driver) gitLoopHours() map[string]GitLoopHours {
	if !d.cfg.Branch.asksGit() && d.cfg.Branch.Conflicts != conflictsDrawn {
		return nil
	}
	for repo := range d.git.reads {
		d.git.hours[repo] = GitLoopHours{}
	}
	for _, l := range d.git.loops {
		if l.source == "" || l.start.Before(d.window.From) || !l.start.Before(d.window.To) {
			continue
		}
		end := d.window.To
		if !l.end.IsZero() && l.end.Before(end) {
			end = l.end
		}
		repo, _ := d.gitIdentity(l.issue)
		h := d.git.hours[repo]
		if l.source == loopAnswered {
			h.Answered += end.Sub(l.start).Hours()
		} else {
			h.Drawn += end.Sub(l.start).Hours()
		}
		d.git.hours[repo] = h
	}
	return d.git.hours
}

// recordedM counts touching commits at the recorded end, with no simulated merge order.
func (r *gitRepository) recordedM(pr int, end time.Time) (count int, profile, missing bool) {
	pull, ok := r.input.Pull(pr)
	if !ok {
		return 0, false, false
	}
	head := pull.standingHead(end)
	if !head.Read() {
		return 0, false, false
	}
	i := 0
	for i < len(r.recorded) && r.recorded[i].at.Before(end) {
		i++
	}
	touching, missing := r.touching(r.recorded[:i], head.Paths, head.MergeBase, pull.Merge, time.Time{})
	return len(touching), true, missing
}

func (d *driver) profileM(p *piece, r *gitRepository, repo string, own int, start, at, recorded time.Time) int {
	head := d.drawProfile(p, r, recorded)
	base := d.git.base(strings.ToLower(repo), r, at)
	pull, _ := r.input.Pull(own)
	if !head.Read() {
		d.conflicts.NoProfileDraws++
		n, missing := 0, false
		for _, c := range base {
			if c.at.After(start) && c.sha != pull.Merge {
				n++
				_, known := r.paths[c.sha]
				missing = missing || !known
			}
		}
		if missing {
			d.conflicts.DrawnPathsUnknown++
		}
		return n
	}
	after := d.git.loopEnds[p.issue]
	if after.IsZero() && head.MergeBase == "" {
		after = maxTime(d.git.firstImpl[p.issue], d.git.forwardAt[p.issue])
	}
	touching, missing := r.touching(base, head.Paths, head.MergeBase, pull.Merge, after)
	if missing {
		d.conflicts.DrawnPathsUnknown++
	}
	return len(touching)
}

func (d *driver) noteGitPieceStart(p *piece, at time.Time) {
	if len(d.merges.git) == 0 || p.phase != phase.Implementing || d.git.firstImpl == nil {
		return
	}
	if _, ok := d.git.firstImpl[p.issue]; !ok {
		d.git.firstImpl[p.issue] = at
	}
}

// recordedAt maps elapsed work at a boot back to the running piece's recorded clock.
func (p *piece) recordedAt(now time.Time, factor float64) time.Time {
	elapsed := max(0, now.Sub(p.at)-p.pausedFor)
	scale := slowed(time.Second, factor, p.n, p.m).Seconds()
	if scale > 0 {
		elapsed = time.Duration(float64(elapsed) / scale)
	}
	at := p.start.Add(elapsed)
	if !p.end.IsZero() && at.After(p.end) {
		return p.end
	}
	return at
}
