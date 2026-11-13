package daemon

// The trigger of a mapping change: Dispatch's settings.repo_project.updated of the daemon's project
// names no issue, and a change of the project's default repository moves, in Dispatch, every
// issue that names no repository of its own and has no ancestor that does, with no event of any of
// them. On a running daemon the event has the refresher read the mapping again and then read
// Dispatch's listing, whose summaries carry each issue's resolved_repo, and correct each recorded
// issue through the single-issue path every other pass takes (followMapping,
// workflow.(*Engine).ApplyRepositoryChange). A read of the mapping that newly serves a repository
// asks for the same follow (refreshMapping's answer, refreshOnMappingChange), with no event: GitHub answering a
// failed installation read again, or an App installed and the mapping read again. These tests
// publish the event on a real NATS, run the daemon's own Dispatch consumer and refresher over a
// real PostgreSQL, and read the records.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mappingEventStart is where the engine's clock stands when a test begins; each read of it moves
// a minute on, so a record stamped with one correction's moment cannot be holding another's.
var mappingEventStart = time.Date(2026, 11, 12, 15, 30, 0, 0, time.UTC)

// countedGitHub is a GitHub stand-in that answers each repository's name and default branch, as
// newNamedGitHub does, and counts the reads of each, by lowercased owner/name.
type countedGitHub struct {
	url   string
	mu    sync.Mutex
	reads map[string]int
}

func newCountedGitHub(t *testing.T, repos map[string]string) *countedGitHub {
	t.Helper()
	g := &countedGitHub{reads: map[string]int{}}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		repo := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		g.mu.Lock()
		g.reads[repo]++
		g.mu.Unlock()
		name, branch, ok := strings.Cut(repos[repo], "@")
		if r.Method != http.MethodGet || !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + branch + `"}`))
	}})
	return g
}

// readsOf is how many times repo was read.
func (g *countedGitHub) readsOf(repo string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads[strings.ToLower(repo)]
}

// mappingEventRig is a running daemon's runtime whose project maps acme/widgets alone, as its
// default, at boot, with its Dispatch consumer and its refresher running, and an engine whose
// clock answers a new moment at every read.
type mappingEventRig struct {
	t      *testing.T
	w      *workflowRuntime
	client *mappingDispatch
	js     jetstream.JetStream
	github *countedGitHub
	events int

	clockMu sync.Mutex
	read    []time.Time
}

// newMappingEventRig opens the rig with the project mapping boot at its boot, acme/widgets alone,
// as its default, when boot names none.
func newMappingEventRig(t *testing.T, installs *installations, boot ...dispatch.RepoProject) *mappingEventRig {
	t.Helper()
	if len(boot) == 0 {
		boot = []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}}
	}
	r := &mappingEventRig{t: t, github: newCountedGitHub(t, map[string]string{
		"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@release/v7.1", "orbit/gadgets": "orbit/gadgets@main",
	})}
	at := mappingEventStart
	clock := func() time.Time {
		r.clockMu.Lock()
		defer r.clockMu.Unlock()
		at = at.Add(time.Minute)
		r.read = append(r.read, at)
		return at
	}
	r.w, r.client, _, _, r.js = mappingRigJS(t, installs, func(w *workflowRuntime) {
		w.dispatch.(*mappingDispatch).set(boot...)
		w.defaultBranches = newDefaultBranches(roleTokens{}, nil, r.github.url, defaultBranchTTL, time.Now)
		engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, NoteUnread: w.noteUnread,
			Steps: config.DesignGateOff.Steps(), Clock: clock}, quietLogger())
		w.engine, w.admission = engine, admit.New(w.records, engine, 3, "LEGION", quietLogger())
		w.handlers = []intake.Handler{w.engine, w.admission}
	})
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.w.consumers.Run(run, r.w.engine, r.w.admission) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

// record records key as a running root on acme/widgets at main, at Dispatch sequence 1, and has
// Dispatch's listing show it at that sequence resolved to resolved: nothing of the issue moves in
// Dispatch but the repository it resolves to.
func (r *mappingEventRig) record(key, resolved string) {
	r.t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, r.w.pool, func(tx pgx.Tx) error {
		return r.w.records.PutIssue(ctx, tx, record.Issue{Key: key, Tree: key, Project: "LEGION", Title: key, Phase: phase.Implementing,
			Generation: correctionGeneration, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", HandedOver: true,
			LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	}); err != nil {
		r.t.Fatalf("record %s: %v", key, err)
	}
	r.client.show(issue(key, resolved, "in_progress", true, "", 1))
}

// mappingChange makes Dispatch's mapping of the project mappings and publishes the
// settings.repo_project.updated Dispatch emits for it.
func (r *mappingEventRig) mappingChange(changed dispatch.RepoProject, mappings ...dispatch.RepoProject) {
	r.t.Helper()
	r.client.set(mappings...)
	r.events++
	id := 900 + r.events
	payload, err := json.Marshal(map[string]any{"id": id, "issue_key": nil, "project": "LEGION", "seq": 0, "type": "settings.repo_project.updated",
		"actor": map[string]string{"kind": "user", "id": "a-person"}, "notify": true, "created_at": "2026-11-12T15:30:00Z",
		"payload": map[string]any{"mapping": map[string]any{"repo": changed.Repo, "project": changed.Project, "default": changed.Default}, "deleted": false}})
	if err != nil {
		r.t.Fatal(err)
	}
	eventID := fmt.Sprintf("dispatch-%d", id)
	envelope, err := json.Marshal(map[string]any{"event_id": eventID, "source": "dispatch", "source_event_id": eventID, "topic": "test",
		"dedupe_key": eventID, "issued_at": 1, "payload_summary": "mapping updated", "payload": string(payload), "trace_id": eventID})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.js.Publish(context.Background(), "notifications.dispatch.project.LEGION.settings.repo_project.updated", envelope); err != nil {
		r.t.Fatalf("publish the mapping change: %v", err)
	}
}

// issue is key's record as it stands.
func (r *mappingEventRig) issue(key string) record.Issue {
	r.t.Helper()
	got := recordedIssue(r.t, r.w, key)
	if got == nil {
		r.t.Fatalf("%s is not recorded", key)
	}
	return *got
}

// awaitOn waits until every one of keys is recorded on repo.
func (r *mappingEventRig) awaitOn(repo string, keys ...string) {
	r.t.Helper()
	testwait.Eventually(r.t, strings.Join(keys, ", ")+" on "+repo+" after the mapping change", func() bool {
		for _, key := range keys {
			if got := recordedIssue(r.t, r.w, key); got == nil || got.Repo != repo {
				return false
			}
		}
		return true
	})
}

// clockRead reports whether at is a moment the engine's clock answered.
func (r *mappingEventRig) clockRead(at time.Time) bool {
	r.clockMu.Lock()
	defer r.clockMu.Unlock()
	for _, read := range r.read {
		if read.Equal(at) {
			return true
		}
	}
	return false
}

// zetaToolsDefault is the mapping after zeta/tools is mapped to the project as its default.
var zetaToolsDefault = []dispatch.RepoProject{
	{Repo: "acme/widgets", Project: "LEGION"},
	{Repo: "zeta/tools", Project: "LEGION", Default: true},
}

// One settings.repo_project.updated that makes zeta/tools the project's default moves every
// recorded issue that inherits the default at once, with no event of any of them: each is
// corrected by its own call, naming the repository it left and a moment of its own, which is a
// read of the engine's clock at that issue's correction, and keeps its generation and phase.
func TestRepositoryCorrectionFollowsOneMappingChangeForEveryIssueThatInheritsTheDefault(t *testing.T) {
	r := newMappingEventRig(t, &installations{})
	keys := []string{"LEGION-620", "LEGION-621", "LEGION-622"}
	for _, key := range keys {
		r.record(key, "acme/widgets")
	}

	for _, key := range keys {
		r.client.show(issue(key, "zeta/tools", "in_progress", true, "", 1))
	}
	r.mappingChange(zetaToolsDefault[1], zetaToolsDefault...)
	r.awaitOn("zeta/tools", keys...)

	moments := map[time.Time]string{}
	for _, key := range keys {
		got := r.issue(key)
		if got.DefaultBranch != "release/v7.1" || got.GitHubRepo != "zeta/tools" {
			t.Fatalf("%s is on %s at %s, spelled %s; want zeta/tools at release/v7.1", key, got.Repo, got.DefaultBranch, got.GitHubRepo)
		}
		if got.RepoBefore != "acme/widgets" {
			t.Fatalf("the repository %s left = %q, want acme/widgets", key, got.RepoBefore)
		}
		if got.RepoCorrectedAt == nil {
			t.Fatalf("%s carries no moment of its correction", key)
		}
		if got.Generation != correctionGeneration || got.Phase != phase.Implementing {
			t.Fatalf("%s is at generation %d in %s, want generation %d in %s: a correction takes no new generation and moves no phase",
				key, got.Generation, got.Phase, correctionGeneration, phase.Implementing)
		}
		at := *got.RepoCorrectedAt
		if other, shared := moments[at]; shared {
			t.Fatalf("%s and %s were both corrected at %v, want a moment each: each correction reads the clock", key, other, at)
		}
		if !r.clockRead(at) {
			t.Fatalf("%s was corrected at %v, which the engine's clock never answered", key, at)
		}
		moments[at] = key
	}
}

// An issue that names a repository of its own resolves to it whatever the project's default is,
// so the event that moves the issues inheriting the default leaves it where it is: no correction,
// no waiting change and nothing said on it.
func TestRepositoryCorrectionLeavesAnIssueNamingItsOwnRepositoryWhereItIsOnAMappingChange(t *testing.T) {
	r := newMappingEventRig(t, &installations{})
	const inherits, own = "LEGION-630", "LEGION-631"
	r.record(inherits, "acme/widgets")
	r.record(own, "acme/widgets")

	// The default moves to zeta/tools; own names acme/widgets itself, so Dispatch still resolves it there.
	r.client.show(issue(inherits, "zeta/tools", "in_progress", true, "", 1))
	r.mappingChange(zetaToolsDefault[1], zetaToolsDefault...)
	r.awaitOn("zeta/tools", inherits)

	got := r.issue(own)
	if got.Repo != "acme/widgets" || got.RepoBefore != "" || got.RepoCorrectedAt != nil {
		t.Fatalf("%s is on %s, having left %q at %v; want it on acme/widgets with no correction", own, got.Repo, got.RepoBefore, got.RepoCorrectedAt)
	}
	if got.RepoChangePending != "" || got.RepoChangeReason != "" {
		t.Fatalf("%s waits on %q because %q, want nothing waiting", own, got.RepoChangePending, got.RepoChangeReason)
	}
	if notes := repositoryNotes(t, r.w, own); len(notes) != 0 {
		t.Fatalf("%s was told %q, want nothing", own, notes)
	}
}

// A mapping change that makes the project's default a repository Legion serves no installation of
// (orbit/gadgets, mapped with no review App installed) leaves each issue that inherits it on the
// repository its record holds, with the change waiting on the record and the issue told once, as
// a single correction onto an unserved repository does.
func TestRepositoryCorrectionWaitsOnTheRecordWhenAMappingChangeMovesTheDefaultToAnUnservedRepository(t *testing.T) {
	r := newMappingEventRig(t, &installations{missing: map[string]bool{"review orbit/gadgets": true}})
	keys := []string{"LEGION-640", "LEGION-641"}
	for _, key := range keys {
		r.record(key, "acme/widgets")
		r.client.show(issue(key, "orbit/gadgets", "in_progress", true, "", 1))
	}
	mappings := []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION"}, {Repo: "orbit/gadgets", Project: "LEGION", Default: true}}
	r.mappingChange(mappings[1], mappings...)

	testwait.Eventually(t, "each issue waiting on orbit/gadgets", func() bool {
		for _, key := range keys {
			if got := recordedIssue(t, r.w, key); got == nil || got.RepoChangePending != "orbit/gadgets" {
				return false
			}
		}
		return true
	})
	for _, key := range keys {
		got := r.issue(key)
		if got.Repo != "acme/widgets" || got.DefaultBranch != "main" || got.RepoBefore != "" || got.RepoCorrectedAt != nil {
			t.Fatalf("%s is on %s at %s, having left %q at %v; want it kept on acme/widgets at main, uncorrected",
				key, got.Repo, got.DefaultBranch, got.RepoBefore, got.RepoCorrectedAt)
		}
		if !strings.Contains(got.RepoChangeReason, "review App") {
			t.Fatalf("%s waits because %q, want the missing review App named", key, got.RepoChangeReason)
		}
		notes := repositoryNotes(t, r.w, key)
		if len(notes) != 1 || !strings.HasPrefix(notes[0], "Legion did not move this issue off acme/widgets: ") || !strings.Contains(notes[0], "Install it on orbit/gadgets") {
			t.Fatalf("%s was told %q, want one note that the change waits on orbit/gadgets", key, notes)
		}
	}
	if reads := r.github.readsOf("orbit/gadgets"); reads != 0 {
		t.Fatalf("GitHub was read %d times for orbit/gadgets, want none: Legion serves it no installation", reads)
	}
}

// However many recorded issues one mapping change moves onto a repository, GitHub is read once for
// it: the mapping's read names the repository it newly serves (nameAccepted), and each issue's
// preparation takes that read from the cache (defaultBranches.Named).
func TestRepositoryCorrectionReadsGitHubOncePerRepositoryOnAMappingChange(t *testing.T) {
	r := newMappingEventRig(t, &installations{})
	keys := []string{"LEGION-650", "LEGION-651", "LEGION-652", "LEGION-653"}
	for _, key := range keys {
		r.record(key, "acme/widgets")
		r.client.show(issue(key, "zeta/tools", "in_progress", true, "", 1))
	}
	if reads := r.github.readsOf("zeta/tools"); reads != 0 {
		t.Fatalf("GitHub was read %d times for zeta/tools before it was mapped, want none", reads)
	}
	r.mappingChange(zetaToolsDefault[1], zetaToolsDefault...)
	r.awaitOn("zeta/tools", keys...)
	if reads := r.github.readsOf("zeta/tools"); reads != 1 {
		t.Fatalf("GitHub was read %d times for zeta/tools while %d issues moved onto it, want once", reads, len(keys))
	}
}

// uncorrected fails unless key's record is still on acme/widgets at main, with no correction.
func (r *mappingEventRig) uncorrected(key string) record.Issue {
	r.t.Helper()
	got := r.issue(key)
	if got.Repo != "acme/widgets" || got.DefaultBranch != "main" || got.RepoBefore != "" || got.RepoCorrectedAt != nil {
		r.t.Fatalf("%s is on %s at %s, having left %q at %v; want it on acme/widgets at main, uncorrected",
			key, got.Repo, got.DefaultBranch, got.RepoBefore, got.RepoCorrectedAt)
	}
	return got
}

// correctedOnto fails unless key's record is on repo at branch, having left acme/widgets, with
// nothing waiting.
func (r *mappingEventRig) correctedOnto(key, repo, branch string) {
	r.t.Helper()
	got := r.issue(key)
	if got.Repo != repo || got.DefaultBranch != branch || got.GitHubRepo != repo || got.RepoBefore != "acme/widgets" || got.RepoCorrectedAt == nil {
		r.t.Fatalf("%s is on %s at %s, spelled %s, having left %q at %v; want %s at %s, having left acme/widgets",
			key, got.Repo, got.DefaultBranch, got.GitHubRepo, got.RepoBefore, got.RepoCorrectedAt, repo, branch)
	}
	if got.RepoChangePending != "" || got.RepoChangeReason != "" {
		r.t.Fatalf("%s still waits on %q because %q, want nothing waiting", key, got.RepoChangePending, got.RepoChangeReason)
	}
	// The issue's own event, delivered before the correction, may move its phase (the rig opens no
	// tree for it); the correction takes no new generation.
	if got.Generation != correctionGeneration {
		r.t.Fatalf("%s is at generation %d, want %d: a correction takes no new generation", key, got.Generation, correctionGeneration)
	}
}

// A recorded issue Dispatch resolves to a mapped repository whose installation read fails keeps
// its record's repository, and its event writes nothing (workflow.Resolution.Unread); the recovery
// of the repository, once GitHub answers, takes no recorded issue (recoveredOn). The read that
// serves the repository again asks for the follow a mapping change asks for, so the issue is
// corrected onto it with no settings.repo_project.updated and no event of its own.
func TestRepositoryCorrectionFollowsARepositoryServedAgainOnceItsInstallationReadRecovers(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	r := newMappingEventRig(t, installs,
		dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true}, dispatch.RepoProject{Repo: "zeta/tools", Project: "LEGION"})
	if _, _, _, unread := r.w.repositories.lookup("zeta/tools"); !unread {
		t.Fatal("zeta/tools is not left out for its failed installation read at boot")
	}
	const key = "LEGION-660"
	r.record(key, "acme/widgets")

	// A person sets the issue's repository to zeta/tools while its installation read fails.
	deliver(t, r.w, r.client, issue(key, "zeta/tools", "in_progress", true, "", 2))
	if got := r.uncorrected(key); got.LastDispatchSeq != 2 {
		t.Fatalf("%s's record is at Dispatch sequence %d, want the event's 2 applied", key, got.LastDispatchSeq)
	}

	installs.setFailing(nil, "implement zeta/tools")
	r.awaitOn("zeta/tools", key)
	r.correctedOnto(key, "zeta/tools", "release/v7.1")
	if r.events != 0 {
		t.Fatalf("the test published %d mapping changes, want none", r.events)
	}
}

// A recorded issue whose change waits on a mapped repository Legion serves no installation of
// (its review App missing) is corrected onto it by the first read of the mapping that finds the
// App installed and serves it, with no settings.repo_project.updated and no event of its own. The
// read here is the one an admission event of another issue on that repository asks for
// (awaitMappingRead), which is what reads the mapping again once an App is installed.
func TestRepositoryCorrectionFollowsARepositoryANewlyInstalledAppServes(t *testing.T) {
	installs := &installations{missing: map[string]bool{"review orbit/gadgets": true}}
	r := newMappingEventRig(t, installs,
		dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true}, dispatch.RepoProject{Repo: "orbit/gadgets", Project: "LEGION"})
	const waiting, admitted = "LEGION-670", "LEGION-671"
	r.record(waiting, "acme/widgets")
	deliver(t, r.w, r.client, issue(waiting, "orbit/gadgets", "in_progress", true, "", 2))
	if got := r.uncorrected(waiting); got.RepoChangePending != "orbit/gadgets" {
		t.Fatalf("%s waits on %q, want orbit/gadgets before its review App is installed", waiting, got.RepoChangePending)
	}

	installs.mu.Lock()
	installs.missing["review orbit/gadgets"] = false
	installs.mu.Unlock()
	deliver(t, r.w, r.client, issue(admitted, "orbit/gadgets", "todo", true, "", 1))
	if got := recordedIssue(t, r.w, admitted); got == nil || got.Repo != "orbit/gadgets" {
		t.Fatalf("%s recorded as %+v, want it admitted on orbit/gadgets once the mapping serves it", admitted, got)
	}
	r.awaitOn("orbit/gadgets", waiting)
	r.correctedOnto(waiting, "orbit/gadgets", "main")
	if r.events != 0 {
		t.Fatalf("the test published %d mapping changes, want none", r.events)
	}
}
