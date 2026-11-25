package daemon

// A reviewer starts only in an issue pod that runs the configured image. The reviewer works in a
// git clone of its own, with a git that runs no program a configuration names
// (internal/workspace's ProvisionReviewClone and ReviewGitEnv), and only an image of this release
// serves that: an older image's role-init makes no clone and its launcher works the wrong path,
// so a reviewer started in a pod on such an image would review in the issue's shared workspace.
// The boot's image probe holds the configured image to this daemon's contract, so the configured
// image is the one this rule can trust; a pod on any other image is replaced first.
//
// The replacement is the pod's own update round (podupdate.go): it asks every role in the pod to
// push its work and answer, and replaces the pod once they have, or at its bound. A reviewer's
// start opens that round at once, outside the project's limits on openings, because a reviewer
// waits on it; a start while the round is already open joins it. The reviewer starts in the pod
// the first launch after the replacement makes, on the configured image.
//
// Under a runtime that runs no issue pods (tmux) nothing here holds anything: a pane runs on the
// daemon's own host, whose reviewer clone the daemon makes itself (specs.reviewerGit).

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

const (
	// reviewerStartReason is the reason a round a reviewer's start opens names first, before the
	// drift the runtime reads of the pod.
	reviewerStartReason = "reviewer start"
	// reviewerPodReplacedLine is the line written once for each pod a reviewer's start holds.
	reviewerPodReplacedLine = "supervise: the issue pod is replaced before its reviewer starts, because it runs an image other than the configured one"
)

// issuePodImages is what a runtime that runs issue pods answers of the image an issue's live pod
// runs (sandbox.Runtime.IssuePodImage), and how it ends that pod for a round no claim records a
// process in it for (sandbox.Runtime.ReplaceIssuePod). A runtime that runs no pods implements
// none of it.
type issuePodImages interface {
	IssuePodImage(issue string) (sandbox.PodImage, bool)
	ReplaceIssuePod(ctx context.Context, issue, pod string, due func(context.Context) error) error
}

// The Sandbox runtime is the one that answers it: a signature that drifted would leave the rule
// holding nothing in production while every test, which hands the reading in, still passed.
var _ issuePodImages = (*sandbox.Runtime)(nil)

// reviewerPods holds a reviewer's start out of an issue pod on another image than the configured
// one, and keeps, by issue, the pod whose replacement a held start waits for, which the
// update-round sweep reads (waitsFor). It keeps nothing in the store: a restarted daemon holds the
// next start again, and that start records the pod again.
type reviewerPods struct {
	// images is the runtime's reading, nil under a runtime that runs no issue pods.
	images issuePodImages
	log    *slog.Logger

	mu      sync.Mutex
	waiting map[string]string
}

func newReviewerPods(rt runtime.Runtime, log *slog.Logger) *reviewerPods {
	images, _ := rt.(issuePodImages)
	return &reviewerPods{images: images, log: log, waiting: map[string]string{}}
}

// onAnotherImage is the live pod of c's issue and the image it runs, where c is a reviewer's claim
// and that pod runs another image than the configured one. A pod the runtime does not answer live
// (no Sandbox, a suspended one, a pod that ended) is none: the launch makes a fresh pod on the
// configured image.
func (p *reviewerPods) onAnotherImage(c supervise.Claim) (sandbox.PodImage, bool) {
	if c.Role != claim.RoleReviewer || c.Issue == "" || p == nil || p.images == nil {
		return sandbox.PodImage{}, false
	}
	image, live := p.images.IssuePodImage(c.Issue)
	return image, live && image.Image != image.Configured
}

// behind is the stale-plugin lane's term (staleForTheLane): c is a reviewer whose issue pod runs
// another image than the configured one, which the lane leaves to that pod's round. It records
// nothing, since the state's count reads it too.
func (p *reviewerPods) behind(c supervise.Claim) bool {
	_, other := p.onAnotherImage(c)
	return other
}

// check is the rule at a launch: a reviewer's launch into an issue pod on another image than the
// configured one is refused with a wait, the pod is recorded as the one the issue's reviewer
// waits for, and the line is written the first time a start is held for that pod. A reviewer's
// launch the rule lets through drops what was recorded for its issue. The wait is routine
// (routineWaiting): the daemon opens the round itself and the round's bound ends it, so a start
// row that retries it stays at debug however long the round runs.
func (p *reviewerPods) check(c supervise.Claim) error {
	if c.Role != claim.RoleReviewer || p == nil {
		return nil
	}
	image, other := p.onAnotherImage(c)
	p.mu.Lock()
	if !other {
		delete(p.waiting, c.Issue)
		p.mu.Unlock()
		return nil
	}
	first := p.waiting[c.Issue] != image.Pod
	p.waiting[c.Issue] = image.Pod
	p.mu.Unlock()
	if first {
		p.log.Info(reviewerPodReplacedLine, "issue", c.Issue, "pod", image.Pod, "image", image.Image, "configured_image", image.Configured)
	}
	return routineWaiting{err: wait.Errorf("the reviewer of %s starts only in a pod on the configured image %s, and its pod %s runs %s: the pod's update round replaces it first",
		c.Issue, image.Configured, image.Pod, image.Image)}
}

// holdsTask is the rule at a task's send (supervise.Deps.DeliveryWaits): a reviewer process
// already running in an issue pod on another image is handed no task there, whichever backend
// runs it and whatever contract its plugin records. The task stays pending, the pod is recorded
// as a held start records it, so the sweep opens its round at once, and the reviewer is handed
// the task once it runs in the pod that replaces this one.
func (p *reviewerPods) holdsTask(c supervise.Claim) bool {
	return p.check(c) != nil
}

// waitsFor is whether a held reviewer start waits for pod, the pod issue's claims record, to be
// replaced. It reads and changes nothing: the next check of the reviewer's launch, and keepOnly,
// keep the record.
func (p *reviewerPods) waitsFor(issue, pod string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	held, ok := p.waiting[issue]
	return ok && held == pod
}

// keepOnly drops what is recorded for every issue but issues, sorted, the issues with a live
// claim: an issue that has none starts no reviewer.
func (p *reviewerPods) keepOnly(issues []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	maps.DeleteFunc(p.waiting, func(issue, _ string) bool {
		_, found := slices.BinarySearch(issues, issue)
		return !found
	})
}

// gate wraps store so that every launch's recheck (supervise.Store.CheckLaunch, which a launch
// makes before it records a launch or calls the runtime) also holds a reviewer's start to this
// rule. The store's own check is asked first, so a tree's reserved cleanup is answered as it
// always is.
func (p *reviewerPods) gate(store supervise.Store) supervise.Store {
	return reviewerGatedStore{Store: store, pods: p}
}

type reviewerGatedStore struct {
	supervise.Store
	pods *reviewerPods
}

func (s reviewerGatedStore) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	if err := s.Store.CheckLaunch(ctx, c); err != nil {
		return err
	}
	return s.pods.check(c)
}

// heldPod is the pod a held reviewer start of issue waits on, recorded by check.
func (p *reviewerPods) heldPod(issue string) (string, bool) {
	if p == nil {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pod, ok := p.waiting[issue]
	return pod, ok
}

// reviewerPodlessRound carries the round a held reviewer start opens over a pod no claim of the
// issue records a process in (the sweep's group records no pod), and reports whether it did: a
// held start depends on no other role's process. The pod is the one the start was held for, as
// the runtime still reads it live on another image; any other reading leaves the issue to the
// sweep's own podless rule (podlessRound).
//
// The round is opened, or kept open, on the issue's blocking claims as any round is
// (openPodUpdate), under `reviewer start`. It is replaced through the issue's Sandbox
// (ReplaceIssuePod), there being no process to replace it through, once no claim it waits for is
// left unanswered (podlessWaiting) or its bound has come, and while no operator's suspension and
// no project's pause holds it. The last word, under the pod's launch turn, refuses the
// replacement where a claim of the issue records a process by then (a launch kept the pod) or an
// operator suspended one.
func (s *supervision) reviewerPodlessRound(ctx context.Context, issue string, group []supervise.Claim, open string, clock roundClock) bool {
	held, ok := s.reviewerPods.heldPod(issue)
	if !ok {
		return false
	}
	image, running := s.reviewerPods.images.IssuePodImage(issue)
	round := supervise.PodRound(held)
	if !running || image.Pod != held || image.Image == image.Configured || (open != "" && open != round) {
		return false
	}
	live := s.liveGroup(group)
	if open == "" {
		s.openPodUpdate(ctx, issue, live, held, reviewerStartReason+", image "+image.Image+", now "+image.Configured, clock)
	}
	if len(heldByOperator(live, round, clock)) > 0 || clock.held() || (!clock.pastBound(live, round) && len(podlessWaiting(live, round, clock)) > 0) {
		return true
	}
	due := func(ctx context.Context) error {
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			return err
		}
		now := s.viewedGroup(claims, issue)
		if pod, _ := groupPod(now); pod != "" {
			return fmt.Errorf("%w: a claim of %s records a process in pod %s now", errReplacementNotDue, issue, pod)
		}
		if len(heldByOperator(now, round, clock)) > 0 {
			return fmt.Errorf("%w: an operator suspended a claim of %s", errReplacementNotDue, issue)
		}
		return nil
	}
	if err := s.reviewerPods.images.ReplaceIssuePod(ctx, issue, held, due); err != nil {
		if ctx.Err() == nil {
			s.log.Info("supervise: the pod a reviewer's start waits on was not replaced; the next sweep decides again",
				"issue", issue, "round", round, "pod", held, "reason", err)
		}
		return true
	}
	s.log.Info("supervise: the pod a reviewer's start waits on is replaced; no claim ran a process in it", "issue", issue, "round", round, "pod", held)
	s.endPodUpdates(issue, live, round)
	return true
}

// podlessWaiting is whom a podless round waits for before its bound: the round's wait set
// (waitingClaims), and each claim with a session that the daemon suspended, that has not answered
// the round and has not left it. The pod a reviewer's start waits on runs no process, so its
// replacement deletes the disk of every role that ever ran in it, and a claim whose agent ran may
// have left unpushed work there; the round holds that disk for such a claim's answer until its
// bound. A pod round with a process in it passes such a claim over (roundAsks,
// roundClock.passesOver), since a pod that keeps running keeps its
// disk; this rule is the podless round's alone. A failed claim waits for nothing here either: its
// launches or prompts ran out, and only a retry brings it back.
func podlessWaiting(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	waiting := waitingClaims(group, round, clock)
	for _, c := range group {
		if c.State == supervise.StateSuspended && c.Session != "" && !answered(c, round) && !c.LeftPodRound(round) &&
			!slices.ContainsFunc(waiting, func(w supervise.Claim) bool { return w.Token == c.Token }) {
			waiting = append(waiting, c)
		}
	}
	return waiting
}
