package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The anchors are the live records the rig's project boots over, one on each repository the rig's
// tests work: what the refresher's first job prepares (prepareAtStart), so both repositories are
// recorded on before a test's first admission where GitHub answers for them, and what GitHub
// intake hears from the boot (heardAtBoot). Each is a tree past its review, in merging, with its
// lifecycle open under the workflow and no slot: a live tree (record.LiveRoots) whose root waits in
// no line (record.Waiting) and holds none of the cap's slots, so admission neither takes it out of
// the workflow as a root nothing will start (admit's unslotted) nor offers it a slot, and it moves
// of its own nowhere in a test.
const anchorWidgets, anchorTools = "LEGION-9001", "LEGION-9002"

// anchorOn is the live record key, a tree on repo in merging that an earlier process recorded.
func anchorOn(key, repo string) record.Issue {
	return record.Issue{Key: key, Tree: key, Project: "LEGION", Title: "the tree the project boots over on " + repo, Phase: phase.Merging,
		Generation: 1, Status: "in_progress", Rank: "A", Repo: repo, DefaultBranch: "main", GitHubRepo: repo}
}

// repositoryRig is a workflow runtime over a fresh database and NATS whose project boots over a
// live record on acme/widgets and one on zeta/tools (the anchors), each App installed on each but
// those missing names ("<role> <owner/name>"), with GitHub intake hearing what the records name and
// the refresher running (refreshRepositories).
func repositoryRig(t *testing.T, missing map[string]bool) (*workflowRuntime, *repositoryDispatch, *installations, jetstream.Stream) {
	t.Helper()
	return repositoryRigWith(t, &installations{missing: missing})
}

// repositoryRigWith is repositoryRig answering each installation read from installs, with each of
// opts applied to the runtime after the anchors are recorded and before its boot reads the records.
func repositoryRigWith(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *repositoryDispatch, *installations, jetstream.Stream) {
	t.Helper()
	w, client, installs, stream, _ := repositoryRigJS(t, installs, opts...)
	return w, client, installs, stream
}

// repositoryRigJS is repositoryRigWith with the JetStream it opened, for a test that publishes on
// it. It returns once the refresher's first job has kept GitHub's answer for both anchors'
// repositories: recorded on, a role App answered not installed, or a read that did not answer.
func repositoryRigJS(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *repositoryDispatch, *installations, jetstream.Stream, jetstream.JetStream) {
	t.Helper()
	ctx := context.Background()
	conn := testnats.Connect(t, workflowNATS(t))
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatal(err)
	}
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main", "orbit/gadgets": "orbit/gadgets@main"})
	client := &repositoryDispatch{}
	if installs.missing == nil {
		installs.missing = map[string]bool{}
	}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), dispatch: client, dispatchProject: "LEGION", installs: installs,
		repositories: newRepositories("LEGION", ghrepo.Renames{}), defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now), log: quietLogger(),
		rereadWake: make(chan struct{}, 1), recoverWake: make(chan struct{}, 1), heardWake: make(chan struct{}, 1), heldWake: make(chan struct{}, 1), liveWake: make(chan struct{}, 1)}
	// The records ask the refresher for a read of the live records at the commit of each write that
	// can change them, as the daemon's do (projectRecords, refreshLive), so a repository a new
	// record names is among the ones whose bad answers the refresher reads again. The anchors are
	// written through the bare store, which asks for none: heardAtBoot reads them below, so such a
	// read would find nothing the boot's did not.
	records := projectRecords{Store: record.NewStore(), project: "LEGION", live: w.wakeLive}
	w.records = records
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, NoteUnread: w.noteUnread, HearRepository: w.hearRecorded,
		Steps: config.DesignGateOff.Steps()}, quietLogger())
	w.engine, w.admission = engine, admit.New(w.records, engine, 3, "LEGION", quietLogger())
	w.handlers = []intake.Handler{w.engine, w.admission}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		for _, anchor := range []record.Issue{anchorOn(anchorWidgets, "acme/widgets"), anchorOn(anchorTools, "zeta/tools")} {
			if err := records.Store.PutIssue(ctx, tx, anchor); err != nil {
				return err
			}
			if err := openTreeTx(ctx, tx, "LEGION", anchor.Key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, opt := range opts {
		opt(w)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w.consumers, err = intake.OpenConsumers(ctx, js, w.pool, intake.ConsumerSpec{Project: "LEGION", Repositories: heard, Logger: quietLogger(),
		AckWait: time.Second, NakDelay: 10 * time.Millisecond, PrepareRepository: w.prepareRepository}); err != nil {
		t.Fatal(err)
	}
	running, stop := context.WithCancel(ctx)
	refreshed := make(chan struct{})
	go func() { w.refreshRepositories(running); close(refreshed) }()
	t.Cleanup(func() { stop(); <-refreshed })
	testwait.Eventually(t, "the refresher's first job keeping GitHub's answer for acme/widgets and zeta/tools", func() bool {
		return prepared(w, "acme/widgets") && prepared(w, "zeta/tools")
	})
	return w, client, installs, stream, js
}

// prepared is whether a preparation of repo has kept GitHub's answer for it: recorded on, a role
// App answered not installed, or why a read did not answer.
func prepared(w *workflowRuntime, repo string) bool {
	entry := w.repositories.entryOf(repo)
	return recordsOn(w, repo) || entry.readError != "" || len(entry.missing()) != 0
}

// rigHandlers is the rig's engine and admission (of three slots), wired to the rig's runtime as the
// boot wires the daemon's before the refresher starts, so w's own applies and recoveries run
// through both.
func rigHandlers(w *workflowRuntime) (*workflow.Engine, *admit.Admission) {
	return w.engine, w.admission
}

// prepareAsDelivered prepares fact as the Dispatch consumer does, delivering it again after a
// held-back preparation (intake.RetryLater) until it is prepared or fails.
func prepareAsDelivered(t *testing.T, w *workflowRuntime, fact intake.DispatchIssue) (intake.DispatchIssue, error) {
	t.Helper()
	var prepared intake.DispatchIssue
	var err error
	testwait.Holds(10*time.Second, testwait.Poll, func() bool {
		prepared, err = w.prepareRepository(context.Background(), fact)
		var later *intake.RetryLater
		return !errors.As(err, &later)
	})
	return prepared, err
}

func githubFilterSubjects(t *testing.T, stream jetstream.Stream) []string {
	t.Helper()
	consumer, err := stream.Consumer(context.Background(), "legion-go-LEGION-github")
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info.Config.FilterSubjects
}

func recordedIssue(t *testing.T, w *workflowRuntime, key string) *record.Issue {
	t.Helper()
	var got *record.Issue
	if err := pgx.BeginFunc(context.Background(), w.pool, func(tx pgx.Tx) error {
		var err error
		got, err = w.records.Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// GitHub intake hears every repository a record of the project names, whatever its tree's state:
// a root that closed on a repository no live record names is heard across the publication of the
// whole heard set again (republishHeard) and across a restart, so a person moving it back to todo
// re-admits it on a repository whose pull request facts still arrive.
func TestAClosedTreesRepositoryStaysHeardAcrossARepublicationAndARestart(t *testing.T) {
	ctx := context.Background()
	w, _, _, stream := repositoryRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-300", Tree: "LEGION-300", Project: "LEGION", Title: "root", Phase: phase.Done,
			Generation: 1, Status: "done", Rank: "A", Repo: "orbit/gadgets", DefaultBranch: "main", GitHubRepo: "orbit/gadgets"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.republishHeard(ctx); err != nil {
		t.Fatal(err)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.orbit.gadgets.>") {
		t.Fatalf("GitHub filters = %v after the heard set was published again, want orbit/gadgets heard: a closed root is recorded on it", filters)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repositoryNames(heard), []string{"acme/widgets", "orbit/gadgets", "zeta/tools"}; !slices.Equal(got, want) {
		t.Fatalf("a restart hears %v, want %v", got, want)
	}
	reopened := intake.DispatchIssue{Key: "LEGION-300", Seq: 2, Status: "todo", Title: "root", Rank: "A", HandedOver: true, ResolvedRepo: "orbit/gadgets"}
	if reopened, err = w.prepareRepository(ctx, reopened); err != nil {
		t.Fatal(err)
	}
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "reopen", reopened, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-300"); got == nil || got.Phase != phase.Admitted || got.Repo != "orbit/gadgets" {
		t.Fatalf("LEGION-300 = %+v, want it re-admitted on orbit/gadgets", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.orbit.gadgets.>") {
		t.Fatalf("GitHub filters = %v, want orbit/gadgets heard for the re-admitted root", filters)
	}
}

// GitHub intake hears a repository as GitHub spells it, which Envoy's subjects carry, whatever
// spelling an issue's record keeps for its workspace: a record the backfill filled with a clone's
// spelling is heard once, under its GitHub spelling, beside the anchor on the same repository, and
// a record on a repository no preparation read is heard under the GitHub spelling its record keeps,
// with no read of GitHub.
func TestGitHubIntakeHearsEachRepositoryAsGitHubSpellsIt(t *testing.T) {
	ctx := context.Background()
	w, _, _, _ := repositoryRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		for index, issue := range []record.Issue{
			{Key: "LEGION-300", Repo: "ACME/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
			{Key: "LEGION-301", Repo: "orbit/gadgets", DefaultBranch: "main", GitHubRepo: "Orbit/Gadgets"},
		} {
			issue.Tree, issue.Project, issue.Title, issue.Phase, issue.Generation, issue.Status, issue.Rank = issue.Key, "LEGION", issue.Key, phase.Planning, 1, "in_progress", "A"
			if err := w.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if err := w.records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, Index: index, AdmittedAt: time.Now()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repositoryNames(heard), []string{"acme/widgets", "Orbit/Gadgets", "zeta/tools"}; !slices.Equal(got, want) {
		t.Fatalf("heard %v, want %v", got, want)
	}
}

// A project whose every repository a record names lacks an App boots, records no issue on them and
// names the App each lacks in its entry, which legion state shows; GitHub intake hears them all
// the same, since the records name them. An issue handed to Legion on one is not admitted, the
// daemon saying why on it. Once the App is installed, the issue's next hand-over reads the
// installation again and records the issue on the repository, which GitHub intake hears.
func TestAProjectServingNoRepositoryStartsServingOnceItsAppIsInstalled(t *testing.T) {
	ctx := context.Background()
	w, client, installs, stream := repositoryRig(t, map[string]bool{"review acme/widgets": true, "review zeta/tools": true})
	client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 2}
	for _, repo := range []string{"acme/widgets", "zeta/tools"} {
		if entry := w.repositories.entryOf(repo); recordsOn(w, repo) || !slices.Equal(entry.missing(), []appauth.AppRole{appauth.Review}) {
			t.Fatalf("%s recorded on %v, entry %+v; want nothing recorded on it and its review App named missing", repo, recordsOn(w, repo), entry)
		}
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.acme.widgets.>") || !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v, want both repositories the records name heard", filters)
	}
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	handOver := func(seq int64) {
		t.Helper()
		fact := intake.DispatchIssue{Key: "LEGION-301", Seq: seq, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
		prepared, err := prepareAsDelivered(t, w, fact)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-"+string(rune('0'+seq)), prepared, engine, admission); err != nil {
			t.Fatal(err)
		}
	}
	handOver(1)
	if got := recordedIssue(t, w, "LEGION-301"); got != nil {
		t.Fatalf("LEGION-301 recorded as %+v while the review App is not installed on acme/widgets", got)
	}
	if notes := repositoryNotes(t, w, "LEGION-301"); len(notes) != 1 || !strings.Contains(notes[0], "the review App is not installed on acme/widgets") {
		t.Fatalf("notes on LEGION-301 = %q, want one naming the review App acme/widgets lacks", notes)
	}

	installs.setMissing(false, "review acme/widgets")
	handOver(2)
	if got := recordedIssue(t, w, "LEGION-301"); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("LEGION-301 = %+v once the App is installed, want it recorded on acme/widgets", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.acme.widgets.>") {
		t.Fatalf("GitHub filters = %v, want acme/widgets heard", filters)
	}
}

// An issue handed to Legion on a repository no record names is not refused for that: its
// preparation reads GitHub for it and records it on what GitHub answers (R4). Its event is
// prepared as Dispatch now shows the issue, as every admission event of an unrecorded issue is:
// still handed over, it is recorded on its repository; withdrawn meanwhile (moved to backlog, the
// label taken off), it records nothing; and on another repository with no event of its own (an
// ancestor's or the project's default changed), it is recorded on the one it resolves to now. The
// repository is heard from the record that names it (R5).
func TestAnIssueOnARepositoryNoRecordNamesIsAdmittedAsDispatchNowShowsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    dispatch.Issue
		wantOn string
	}{
		{"still handed over in todo", dispatch.Issue{Key: "LEGION-302", Status: "todo", HandedOver: true, ResolvedRepo: "orbit/gadgets", LastSeq: 2}, "orbit/gadgets"},
		{"withdrawn before its event was prepared", dispatch.Issue{Key: "LEGION-302", Status: "backlog", ResolvedRepo: "orbit/gadgets", LastSeq: 2}, ""},
		{"on another repository with no event of its own", dispatch.Issue{Key: "LEGION-302", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 1}, "acme/widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, stream := repositoryRig(t, map[string]bool{})
			client.issue = tc.now
			fact := intake.DispatchIssue{Key: "LEGION-302", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "orbit/gadgets"}
			prepared, err := prepareAsDelivered(t, w, fact)
			if err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, HearRepository: w.hearRecorded, Steps: config.DesignGateOff.Steps()}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			if _, err := intake.ApplyFact(ctx, w.pool, "test", "handover", prepared, engine, admission); err != nil {
				t.Fatal(err)
			}
			got := recordedIssue(t, w, "LEGION-302")
			if tc.wantOn == "" {
				if got != nil {
					t.Fatalf("LEGION-302 = %+v, want nothing recorded: the person withdrew it before its event was prepared", got)
				}
				return
			}
			if got == nil || got.Repo != tc.wantOn {
				t.Fatalf("LEGION-302 = %+v, want it recorded on %s", got, tc.wantOn)
			}
			if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github."+strings.ReplaceAll(tc.wantOn, "/", ".")+".>") {
				t.Fatalf("GitHub filters = %v, want %s heard", filters, tc.wantOn)
			}
		})
	}
}

// An admission event whose preparation failed for any reason (here, the read of the issue's
// detail from Dispatch) is delivered again after the issue's newer events, so it is prepared as
// Dispatch now shows the issue: a person who withdrew it meanwhile is not overruled, and one still
// handed over is recorded, on a repository no record names as on one a record names.
func TestAnAdmissionWhosePreparationFailedIsPreparedAsDispatchNowShowsIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		now      dispatch.Issue
		admitted bool
	}{
		{"withdrawn after a failed preparation", dispatch.Issue{Key: "LEGION-305", Status: "backlog", ResolvedRepo: "acme/widgets", LastSeq: 2}, false},
		{"still handed over after a failed preparation", dispatch.Issue{Key: "LEGION-305", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 2}, true},
		{"still handed over on a repository no record names", dispatch.Issue{Key: "LEGION-305", Status: "todo", HandedOver: true, ResolvedRepo: "orbit/gadgets", LastSeq: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, _ := repositoryRig(t, map[string]bool{})
			fact := intake.DispatchIssue{Key: "LEGION-305", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: tc.now.ResolvedRepo}
			client.mu.Lock()
			client.failing = map[string]error{"LEGION-305": &dispatch.TransientError{Err: errors.New("Dispatch answered 502 to the issue's detail")}}
			client.mu.Unlock()
			if _, err := w.prepareRepository(ctx, fact); err == nil {
				t.Fatal("the delivery whose read of Dispatch failed was prepared, want it retried")
			}
			client.mu.Lock()
			client.failing = nil
			client.issues = map[string]dispatch.Issue{"LEGION-305": tc.now}
			client.mu.Unlock()
			prepared, err := prepareAsDelivered(t, w, fact)
			if err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			if _, err := intake.ApplyFact(ctx, w.pool, "test", "handover", prepared, engine, admission); err != nil {
				t.Fatal(err)
			}
			if got := recordedIssue(t, w, "LEGION-305"); (got != nil) != tc.admitted {
				t.Fatalf("LEGION-305 = %+v, want recorded %t", got, tc.admitted)
			}
		})
	}
}

// Dispatch's outbox puts off an event whose publish failed and publishes the issue's later events
// first, so an admission event can reach the daemon on its first delivery after a person withdrew
// the issue. It is prepared as Dispatch now shows the issue, as a redelivered one is: the
// withdrawal stands.
func TestALateFirstDeliveryOfAnAdmissionEventIsPreparedAsDispatchNowShowsIt(t *testing.T) {
	ctx := context.Background()
	w, client, _, _ := repositoryRig(t, map[string]bool{})
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	client.issue = dispatch.Issue{Key: "LEGION-307", Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets", LastSeq: 2}
	withdrawal := intake.DispatchIssue{Key: "LEGION-307", Seq: 2, Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets"}
	late := intake.DispatchIssue{Key: "LEGION-307", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
	for _, delivery := range []struct {
		id    string
		event intake.DispatchIssue
	}{{"withdrawal", withdrawal}, {"late-handover", late}} {
		prepared, err := w.prepareRepository(ctx, delivery.event)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "test", delivery.id, prepared, engine, admission); err != nil {
			t.Fatal(err)
		}
	}
	if got := recordedIssue(t, w, "LEGION-307"); got != nil {
		t.Fatalf("LEGION-307 = %+v, want nothing recorded: the person withdrew it before its hand-over reached the daemon", got)
	}
}

// An unrecorded issue's admission event is prepared from Dispatch's view outside the fact lock,
// and release_children may record the issue before that view applies; release_children's own view
// may likewise follow an event that recorded it. A view never reaches a recorded issue: the event
// it was prepared in place of applies as it was delivered, with its actor (here fenced below the
// record), and release_children's, prepared in place of no event, applies nothing. Either way the
// record stays as it was, and the issue's later events, each with its own actor, carry it on.
func TestAViewPreparedBeforeTheIssueWasRecordedNeverAppliesToItsRecord(t *testing.T) {
	for _, tc := range []struct {
		name          string
		releasedFirst bool
	}{
		{"an event's view, release_children recording the issue first", true},
		{"release_children's view, an event recording the issue first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, _ := repositoryRig(t, map[string]bool{})
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			apply := func(id string, fact intake.DispatchIssue) {
				t.Helper()
				if _, err := intake.ApplyFact(ctx, w.pool, "test", id, fact, engine, admission); err != nil {
					t.Fatal(err)
				}
			}
			atSix := dispatch.Issue{Key: "LEGION-308", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 6}
			var stale intake.DispatchIssue
			if tc.releasedFirst {
				// A session's backlog at sequence 7, whose own event the daemon has not yet read.
				client.issue = dispatch.Issue{Key: "LEGION-308", Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets", LastSeq: 7}
				handOver := intake.DispatchIssue{Key: "LEGION-308", Seq: 5, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
				var err error
				if stale, err = w.prepareRepository(ctx, handOver); err != nil {
					t.Fatal(err)
				}
				released, err := w.prepareObserved(ctx, intake.ObservedIssue(atSix))
				if err != nil {
					t.Fatal(err)
				}
				apply("release", released)
			} else {
				var err error
				if stale, err = w.prepareObserved(ctx, intake.ObservedIssue(dispatch.Issue{Key: "LEGION-308", Status: "todo", Title: "renamed", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 7})); err != nil {
					t.Fatal(err)
				}
				client.issue = atSix
				handOver := intake.DispatchIssue{Key: "LEGION-308", Seq: 6, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
				event, err := w.prepareRepository(ctx, handOver)
				if err != nil {
					t.Fatal(err)
				}
				apply("handover", event)
			}
			before := recordedIssue(t, w, "LEGION-308")
			if before == nil || before.LastDispatchSeq != 6 {
				t.Fatalf("LEGION-308 = %+v, want it recorded at sequence 6", before)
			}
			apply("stale-view", stale)
			after := recordedIssue(t, w, "LEGION-308")
			if after.LastDispatchSeq != 6 || after.Status != before.Status || after.DispatchStatus != before.DispatchStatus || after.Title != before.Title {
				t.Fatalf("LEGION-308 = %+v after the view prepared before it was recorded, want it as recorded: %+v", after, before)
			}
		})
	}
}

// quickInstallReads shortens installReadRetry for a test, restoring it once the rig the test
// opens after it has stopped reading it.
func quickInstallReads(t *testing.T) {
	t.Helper()
	retry := installReadRetry
	installReadRetry.Initial, installReadRetry.Max = 20*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { installReadRetry = retry })
}

// recordsOn is whether w records an issue that resolves to repo on it now: a preparation found
// both role Apps installed on it and GitHub naming it (repositories.lookup).
func recordsOn(w *workflowRuntime, repo string) bool {
	_, recorded := w.repositories.lookup(repo)
	return recorded
}

// repositoryNotes is every repository note the daemon queued on key.
func repositoryNotes(t *testing.T, w *workflowRuntime, key string) []string {
	t.Helper()
	rows, err := w.pool.Query(context.Background(), "select payload->>'body' from outbox where issue = $1 and kind = 'dispatch_message'", key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
	}
	return notes
}

// A repository a record names whose installation read failed at the refresher's first job is
// read again of the daemon's own (rereadBadAnswers) until GitHub answers, and is then recorded on,
// with no restart. An issue handed to Legion on it meanwhile is not refused and nothing is said on
// it; once GitHub answers, the daemon recovers it from Dispatch's listing and admits it on it. A
// repository recorded on whose read fails later stays recorded on its last answer and holds
// nothing else back; once GitHub answers for it too, the daemon stops reading again.
func TestARepositoryWhoseInstallationReadFailedIsServedOnceARetryReadsIt(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, stream := repositoryRigWith(t, installs)
	if !recordsOn(w, "acme/widgets") {
		t.Fatal("acme/widgets is not recorded on, want it recorded on beside the repository whose read failed")
	}
	if entry := w.repositories.entryOf("zeta/tools"); entry.answered() || !strings.Contains(entry.readError, "the implement App's installation on zeta/tools could not be read") {
		t.Fatalf("zeta/tools' entry = %+v, want it unanswered for its failed read", entry)
	}
	testwait.Eventually(t, "zeta/tools' installation read again", func() bool { return installs.reads("implement zeta/tools") >= 3 })
	if recordsOn(w, "zeta/tools") {
		t.Fatal("zeta/tools recorded on while its read still fails")
	}

	engine, admission := rigHandlers(w)
	client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{{Key: "LEGION-301", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}}
	client.mu.Unlock()
	fact := intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
	prepared, err := w.prepareRepository(ctx, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-301 on zeta/tools = %v, want it prepared to be applied and acknowledged", err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-1", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-301"); got != nil {
		t.Fatalf("LEGION-301 recorded as %+v while zeta/tools' read fails", got)
	}
	if notes := repositoryNotes(t, w, "LEGION-301"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-301 = %q while it is held, want none", notes)
	}

	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, "zeta/tools recorded on once GitHub answers", func() bool { return recordsOn(w, "zeta/tools") })
	// A later preparation of acme/widgets, as a launch's (prepareLive), whose review App read fails.
	installs.setFailing(errors.New("GitHub answered 502"), "review acme/widgets")
	if _, err := w.prepareLive(ctx, ghrepo.MustParse("acme/widgets"), w.onceRecordedBranches(ctx)); err == nil {
		t.Fatal("the preparation of acme/widgets whose review App read failed answered, want its failure")
	}
	if !recordsOn(w, "acme/widgets") {
		t.Fatalf("acme/widgets no longer recorded on once its read failed (entry %+v), want it kept on its last answer", w.repositories.entryOf("acme/widgets"))
	}
	if bad := w.badAnswers(); len(bad) != 1 || !strings.HasPrefix(bad[0], "acme/widgets: ") {
		t.Fatalf("bad answers = %q, want acme/widgets alone", bad)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") || !slices.Contains(filters, "notifications.github.acme.widgets.>") {
		t.Fatalf("GitHub filters = %v, want acme/widgets and zeta/tools heard", filters)
	}
	testwait.Eventually(t, "LEGION-301 admitted on zeta/tools with no one moving it", func() bool {
		got := recordedIssue(t, w, "LEGION-301")
		return got != nil && got.Repo == "zeta/tools"
	})
	if notes := repositoryNotes(t, w, "LEGION-301"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-301 = %q, want none: it was left, then recovered", notes)
	}

	installs.setFailing(nil, "review acme/widgets")
	testwait.Eventually(t, "acme/widgets read again", func() bool { return len(w.badAnswers()) == 0 })
	reads := installs.reads("review acme/widgets")
	time.Sleep(10 * installReadRetry.Max)
	if got := installs.reads("review acme/widgets"); got != reads {
		t.Fatalf("acme/widgets read %d times more once GitHub answered, want the reads again to stop", got-reads)
	}
}

// A repository a record names that an App is not installed on (GitHub's 404) has no issue
// recorded on it, its entry naming the App, and is a bad answer the refresher reads again of its
// own, as it reads a repository whose read did not answer (rereadBadAnswers, badAnswers). The
// refusal is nothing an issue is held to, though: the next preparation of an admission on it
// reads the installation again, so an App installed since admits the issue with no restart (R4).
func TestANotInstalledRepositoryIsReadAgainAtTheNextPreparation(t *testing.T) {
	ctx := context.Background()
	installs := &installations{missing: map[string]bool{"review acme/widgets": true}}
	installs.setFailing(&appauth.TransientError{Err: errors.New("GitHub answered 502")}, "review zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	engine, admission := rigHandlers(w)
	if entry := w.repositories.entryOf("zeta/tools"); entry.readError == "" || len(entry.missing()) != 0 {
		t.Fatalf("zeta/tools' entry = %+v, want its failed read and no App named missing", entry)
	}
	if entry := w.repositories.entryOf("acme/widgets"); entry.readError != "" || !slices.Equal(entry.missing(), []appauth.AppRole{appauth.Review}) {
		t.Fatalf("acme/widgets' entry = %+v, want its review App named missing and no failed read", entry)
	}
	if bad := w.badAnswers(); len(bad) != 2 || bad[0] != "acme/widgets: the review App is not installed on acme/widgets" || !strings.HasPrefix(bad[1], "zeta/tools: ") {
		t.Fatalf("bad answers = %q, want acme/widgets' missing App and zeta/tools' failed read, each read again", bad)
	}
	reads := installs.reads("review acme/widgets")

	issue, fact := handedOver("LEGION-311", "acme/widgets", 1)
	client.issue = issue
	installs.setMissing(false, "review acme/widgets")
	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-311 once the App is installed = %v, want its preparation to read the installation again", err)
	}
	if got := installs.reads("review acme/widgets"); got <= reads {
		t.Fatalf("the preparation made %d reads of the review App's installation, want it read again", got-reads)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-311", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-311"); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("LEGION-311 = %+v, want it admitted on acme/widgets with no restart", got)
	}
}

// A labelled todo root the boot's listing shows on a repository whose installation read failed is
// not refused: no note is said on it, and once GitHub answers for the repository the daemon
// recovers it from Dispatch's listing and admits it on it with no person's move. A root on a
// repository GitHub says lacks an App is refused with its note by that same recovery, whose
// preparation reads GitHub: the boot's listing reads GitHub for no repository no entry records
// issues on, so it leaves both repositories to the recovery (leaveForRecovery).
func TestABootListingsRootOnARepositoryWhoseReadFailedIsAdmittedOnceItIsServed(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{missing: map[string]bool{"review acme/widgets": true}}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	rigHandlers(w)
	w.bootID = "listing-boot"
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{
		{Key: "LEGION-401", ResolvedRepo: "zeta/tools", Title: "on the unread repository", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		{Key: "LEGION-402", ResolvedRepo: "acme/widgets", Title: "on the repository missing an App", Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1},
	}
	client.mu.Unlock()
	client.issue = dispatch.Issue{Key: "LEGION-401", ResolvedRepo: "zeta/tools", Title: "on the unread repository", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := recordedIssue(t, w, "LEGION-401"); got != nil {
		t.Fatalf("LEGION-401 recorded as %+v while zeta/tools' read fails", got)
	}
	if notes := repositoryNotes(t, w, "LEGION-401"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-401 = %q while it is held, want none", notes)
	}
	testwait.Eventually(t, "LEGION-402 refused by the recovery, naming the App acme/widgets lacks", func() bool {
		notes := repositoryNotes(t, w, "LEGION-402")
		return len(notes) == 1 && strings.Contains(notes[0], "the review App is not installed on acme/widgets")
	})

	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, "LEGION-401 admitted on zeta/tools", func() bool {
		got := recordedIssue(t, w, "LEGION-401")
		return got != nil && got.Repo == "zeta/tools"
	})
	if notes := repositoryNotes(t, w, "LEGION-401"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-401 = %q, want none: it was held, then admitted", notes)
	}
	testwait.Eventually(t, "nothing left unread or to recover", func() bool { return len(w.recoveringReasons()) == 0 && len(w.unreadReasons()) == 0 })
	if got := recordedIssue(t, w, "LEGION-402"); got != nil {
		t.Fatalf("LEGION-402 recorded as %+v, want it left refused", got)
	}
	if notes := repositoryNotes(t, w, "LEGION-402"); len(notes) != 1 {
		t.Fatalf("notes on LEGION-402 = %q, want its one refusal", notes)
	}
}

// within is whether condition holds within bound, polled every 10 ms: a bounded wait for a test
// whose old behaviour never meets the condition.
func within(bound time.Duration, condition func() bool) bool {
	return testwait.Holds(bound, testwait.Poll, condition)
}

// A live admission event of an issue on a repository whose installation read failed is
// acknowledged, not left on the stream: the Dispatch consumer reaches the position the boot's
// admission waits for, so a root on a served repository, which the boot's listing held for that
// position, is admitted while the failing repository still fails, and the issue is recovered once
// GitHub answers for its repository, with no note on it. Left on the stream, the event kept the
// consumer's acknowledgement floor below the boot's target, and no root of the boot's listing was
// admitted on any repository until the failing one healed.
func TestAnUnreadEventIsAcknowledgedSoTheBootAdmitsRootsOnServedRepositories(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _, js := repositoryRigJS(t, installs)
	engine, admission := rigHandlers(w)
	w.bootID, w.holdPollInterval = "held-event-boot", 10*time.Millisecond
	failing, served := dispatch.Issue{Key: "LEGION-501", ResolvedRepo: "zeta/tools", Title: "LEGION-501", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		dispatch.Issue{Key: "LEGION-502", ResolvedRepo: "acme/widgets", Title: "LEGION-502", Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1}
	client.mu.Lock()
	client.issues = map[string]dispatch.Issue{failing.Key: failing, served.Key: served}
	client.listed = []dispatch.IssueSummary{
		{Key: failing.Key, ResolvedRepo: failing.ResolvedRepo, Title: failing.Title, Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		{Key: served.Key, ResolvedRepo: served.ResolvedRepo, Title: served.Title, Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1},
	}
	client.mu.Unlock()
	repo := "zeta/tools"
	if _, err := js.Publish(ctx, "notifications.dispatch.issue.LEGION-501.issue.updated",
		retainedIssueEnvelope(t, failing.Key, 501, 1, "todo", map[string]string{"kind": "user", "id": "a-person"}, &repo)); err != nil {
		t.Fatal(err)
	}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !admission.Held() {
		t.Fatal("the boot's admission holds nothing for the consumer, want the listing held for the event still on the stream")
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.consumers.Run(run, engine, admission) }()
	polled := make(chan struct{})
	go func() { w.pollHoldRelease(run); close(polled) }()
	t.Cleanup(func() { cancel(); <-done; <-polled })

	if !within(10*time.Second, func() bool { return recordedIssue(t, w, served.Key) != nil }) {
		position, _ := w.consumers.DispatchPosition(ctx)
		t.Fatalf("%s on served acme/widgets not admitted while zeta/tools' read fails: admission held %t, consumer at %+v", served.Key, admission.Held(), position)
	}
	if got := recordedIssue(t, w, failing.Key); got != nil {
		t.Fatalf("%s recorded as %+v while zeta/tools' read fails", failing.Key, got)
	}
	if notes := repositoryNotes(t, w, failing.Key); len(notes) != 0 {
		t.Fatalf("notes on %s = %q while it is held, want none", failing.Key, notes)
	}

	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, failing.Key+" admitted on zeta/tools", func() bool {
		got := recordedIssue(t, w, failing.Key)
		return got != nil && got.Repo == "zeta/tools"
	})
}

// A repository's recovery records its children parents first, as the boot listing does: a child
// under a child of a live tree, both on a repository whose read failed, is recorded under the tree
// once GitHub answers, whatever order their keys sort in (LEGION-100 sorts before LEGION-99) or
// the listing's: the workflow records a child only under a recorded parent.
func TestARecoveryRecordsChildrenParentsFirst(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	engine, admission := rigHandlers(w)
	w.bootID = "parents-first-boot"
	root := dispatch.Issue{Key: "LEGION-98", ResolvedRepo: "acme/widgets", Title: "root", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}
	parent := dispatch.Issue{Key: "LEGION-99", ResolvedRepo: "zeta/tools", Title: "parent", Status: "todo", Rank: "B", Parent: new(root.Key), LastSeq: 1}
	child := dispatch.Issue{Key: "LEGION-100", ResolvedRepo: "zeta/tools", Title: "child", Status: "todo", Rank: "C", Parent: new(parent.Key), LastSeq: 1}
	client.mu.Lock()
	client.issues = map[string]dispatch.Issue{root.Key: root, parent.Key: parent, child.Key: child}
	client.mu.Unlock()
	prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: root.Key, Seq: 1, Status: "todo", Title: "root", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "root-todo", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, root.Key); got == nil {
		t.Fatalf("%s not admitted on acme/widgets", root.Key)
	}
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{
		{Key: child.Key, ResolvedRepo: "zeta/tools", Title: "child", Status: "todo", Rank: "C", Parent: child.Parent, LastSeq: 1},
		{Key: parent.Key, ResolvedRepo: "zeta/tools", Title: "parent", Status: "todo", Rank: "B", Parent: parent.Parent, LastSeq: 1},
	}
	client.mu.Unlock()
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, key := range []string{parent.Key, child.Key} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}

	installs.setFailing(nil, "implement zeta/tools")
	if !within(10*time.Second, func() bool {
		got := recordedIssue(t, w, child.Key)
		return got != nil && got.Tree == root.Key
	}) {
		t.Fatalf("%s = %+v and %s = %+v once zeta/tools is served, want both recorded in %s's tree", parent.Key, recordedIssue(t, w, parent.Key), child.Key, recordedIssue(t, w, child.Key), root.Key)
	}
	if got := recordedIssue(t, w, parent.Key); got == nil || got.Tree != root.Key {
		t.Fatalf("%s = %+v, want it recorded in %s's tree", parent.Key, got, root.Key)
	}
}

// A GitHub that answers each installation read slowly, but within installReadAttempt, is answered
// for when the refresher reads it again, however long the whole preparation takes: only a read
// longer than installReadAttempt is a failed read. A read bounded as a whole by appMintAttempt cut
// such a preparation off, so a repository whose read failed once was never served.
func TestASlowButAnsweringGitHubIsServedOnARefresh(t *testing.T) {
	quickInstallReads(t)
	attempt, read := appMintAttempt, installReadAttempt
	appMintAttempt, installReadAttempt = 500*time.Millisecond, 2*time.Second
	t.Cleanup(func() { appMintAttempt, installReadAttempt = attempt, read })
	installs := &installations{delay: 600 * time.Millisecond}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, _, installs, _ := repositoryRigWith(t, installs)
	if recordsOn(w, "zeta/tools") {
		t.Fatal("zeta/tools served at boot while its read fails")
	}
	installs.setFailing(nil, "implement zeta/tools")
	if !within(10*time.Second, func() bool { return recordsOn(w, "zeta/tools") }) {
		t.Fatalf("zeta/tools not served after %d slow reads, want it served once GitHub answers", installs.reads("implement zeta/tools"))
	}
}

// applyTodo prepares and applies key's labelled todo event at seq on repo, as the Dispatch
// consumer does.
func applyTodo(t *testing.T, w *workflowRuntime, engine *workflow.Engine, admission *admit.Admission, key, repo string, seq int64) {
	t.Helper()
	fact := intake.DispatchIssue{Key: key, Seq: seq, Status: "todo", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: repo}
	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", fmt.Sprintf("%s-todo-%d", key, seq), prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
}

// show makes Dispatch show each of issues as it is, in its issue detail and in the project's
// listing, in place of what it showed of the same key.
func (d *repositoryDispatch) show(issues ...dispatch.Issue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.issues == nil {
		d.issues = map[string]dispatch.Issue{}
	}
	for _, issue := range issues {
		d.issues[issue.Key] = issue
		summary := dispatch.IssueSummary{Key: issue.Key, Title: issue.Title, Status: issue.Status, Parent: issue.Parent, Rank: issue.Rank,
			HandedOver: issue.HandedOver, LastSeq: issue.LastSeq, ResolvedRepo: issue.ResolvedRepo}
		d.listed = slices.DeleteFunc(d.listed, func(listed dispatch.IssueSummary) bool { return listed.Key == issue.Key })
		d.listed = append(d.listed, summary)
	}
}

// deliver shows issue in Dispatch and delivers its event, at its sequence, as the Dispatch consumer
// prepares and applies one through w's handlers.
func deliver(t *testing.T, w *workflowRuntime, client *repositoryDispatch, issue dispatch.Issue) {
	t.Helper()
	client.show(issue)
	fact := intake.DispatchIssue{Key: issue.Key, Seq: issue.LastSeq, Type: "issue.updated", Status: issue.Status, Title: issue.Title, Rank: issue.Rank,
		Parent: parentKey(issue.Parent), HandedOver: issue.HandedOver, ResolvedRepo: issue.ResolvedRepo}
	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", fmt.Sprintf("%s-%s-%d", issue.Key, issue.Status, issue.LastSeq), prepared, w.handlers...); err != nil {
		t.Fatal(err)
	}
}

// issue is a Dispatch issue key on repo in status, labelled for Legion when handed, under parent
// ("" for none), at sequence seq.
func issue(key, repo, status string, handed bool, parent string, seq int64) dispatch.Issue {
	shown := dispatch.Issue{Key: key, ResolvedRepo: repo, Title: key, Status: status, Rank: "A" + key, HandedOver: handed, LastSeq: seq}
	if parent != "" {
		shown.Parent = new(parent)
	}
	return shown
}

// liveRoot admits key as a labelled todo root on acme/widgets, recorded on, so its tree is live.
func liveRoot(t *testing.T, w *workflowRuntime, client *repositoryDispatch, key string) {
	t.Helper()
	deliver(t, w, client, issue(key, "acme/widgets", "todo", true, "", 1))
	if recordedIssue(t, w, key) == nil {
		t.Fatalf("%s not admitted on acme/widgets", key)
	}
}

// rowCount is the number of rows query counts in w's database.
func rowCount(t *testing.T, w *workflowRuntime, query string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// serveAndRecover lets GitHub answer zeta/tools' implement App read again and waits until the
// daemon records on zeta/tools and has nothing left unread or to recover.
func serveAndRecover(t *testing.T, w *workflowRuntime, installs *installations) {
	t.Helper()
	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, "zeta/tools recorded on and recovered", func() bool {
		return recordsOn(w, "zeta/tools") && len(w.unreadReasons()) == 0 && len(w.recoveringReasons()) == 0
	})
}

// A repository whose installation read fails at boot is recovered once GitHub answers for it: the
// boot's listing admits a root on a repository recorded on while it fails, and leaves the
// repository to the recovery, which once GitHub answers takes the issues on it from Dispatch's
// listing once, as the boot would have had GitHub answered: a labelled root admitted, a child of a
// live tree and that child's own child recorded under the tree, and a person's sub-issue under a
// person's epic left as it is. After that the daemon reads nothing more and applies nothing more,
// and tells the controller nothing. The per-issue hold this replaced kept the person's sub-issue
// held for good, reading and applying it at every read again.
func TestARepositoryUnreadAtBootIsRecoveredOnceServed(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	rigHandlers(w)
	w.bootID = "recovery-boot"
	const tree, root, child, grandchild, epic, sub = "LEGION-1000", "LEGION-1001", "LEGION-1002", "LEGION-1003", "LEGION-1004", "LEGION-1005"
	client.show(
		issue(tree, "acme/widgets", "todo", true, "", 1),
		issue(root, "zeta/tools", "todo", true, "", 1),
		issue(child, "zeta/tools", "todo", false, tree, 1),
		issue(grandchild, "zeta/tools", "todo", false, child, 1),
		issue(epic, "zeta/tools", "in_progress", false, "", 1),
		issue(sub, "zeta/tools", "todo", false, epic, 1),
	)
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if recordedIssue(t, w, tree) == nil {
		t.Fatalf("%s on served acme/widgets not admitted while zeta/tools' read fails", tree)
	}
	for _, key := range []string{root, child, grandchild, epic, sub} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}
	notices := rowCount(t, w, "select count(*) from outbox where kind = 'controller_notice'")

	serveAndRecover(t, w, installs)
	if recordedIssue(t, w, root) == nil {
		t.Fatalf("%s, a labelled root on zeta/tools, not admitted once zeta/tools is served", root)
	}
	for _, key := range []string{child, grandchild} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != tree {
			t.Fatalf("%s = %+v once zeta/tools is served, want it recorded in %s's tree", key, got, tree)
		}
	}
	for _, key := range []string{epic, sub} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s, a person's issue, recorded as %+v, want it left as it is", key, got)
		}
	}
	listings, reads := client.listingCount(), installs.reads("implement zeta/tools")
	facts := rowCount(t, w, "select count(*) from processed_events")
	time.Sleep(10 * installReadRetry.Max)
	if got := client.listingCount(); got != listings {
		t.Fatalf("Dispatch listed %d times more once recovered, want none", got-listings)
	}
	if got := installs.reads("implement zeta/tools"); got != reads {
		t.Fatalf("zeta/tools read %d times more once recovered, want none", got-reads)
	}
	if got := rowCount(t, w, "select count(*) from processed_events"); got != facts {
		t.Fatalf("%d more facts applied once recovered, want none", got-facts)
	}
	if got := rowCount(t, w, "select count(*) from outbox where kind = 'controller_notice'"); got != notices {
		t.Fatalf("%d controller notices queued by the recovery, want none", got-notices)
	}
}

// Events applied while a repository's read fails say nothing and keep nothing, and the recovery
// once GitHub answers ends each issue where its events would have, had GitHub answered: a root
// handed over during the outage is admitted; one moved back to backlog during it is not, and its
// later move to todo admits it once; one whose event was prepared while GitHub said an App was
// missing, and applied during the outage, is told the refusal its own preparation read and is
// admitted too, since a refusal decides no later preparation.
func TestEventsDuringAnOutageEndWhereTheyWouldHaveServed(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{missing: map[string]bool{"review zeta/tools": true}}
	w, client, installs, _ := repositoryRigWith(t, installs)
	rigHandlers(w)
	const handed, withdrawn, prepared404 = "LEGION-1100", "LEGION-1101", "LEGION-1102"
	z := issue(prepared404, "zeta/tools", "todo", true, "", 1)
	client.show(z)
	prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: z.Key, Seq: 1, Status: "todo", Title: z.Title, Rank: z.Rank, HandedOver: true, ResolvedRepo: "zeta/tools"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prepared.RepositoryRefusal, "the review App is not installed on zeta/tools") {
		t.Fatalf("%s's prepared refusal = %q, want the review App zeta/tools lacks", z.Key, prepared.RepositoryRefusal)
	}
	installs.setMissing(false, "review zeta/tools")
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	deliver(t, w, client, issue(handed, "zeta/tools", "todo", true, "", 1))
	if !w.repositoryUnread("zeta/tools") {
		t.Fatal("zeta/tools is not unread after the preparation whose installation read failed")
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", z.Key+"-todo-1", prepared, w.handlers...); err != nil {
		t.Fatal(err)
	}
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "todo", true, "", 1))
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "backlog", true, "", 2))
	for _, key := range []string{handed, withdrawn, prepared404} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}

	serveAndRecover(t, w, installs)
	for _, key := range []string{handed, prepared404} {
		if recordedIssue(t, w, key) == nil {
			t.Fatalf("%s not admitted once zeta/tools is served", key)
		}
	}
	if got := recordedIssue(t, w, withdrawn); got != nil {
		t.Fatalf("%s, moved to backlog during the outage, recorded as %+v", withdrawn, got)
	}
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "todo", true, "", 3))
	if got := recordedIssue(t, w, withdrawn); got == nil || got.LastDispatchSeq != 3 {
		t.Fatalf("%s = %+v after its move to todo, want it admitted at that move", withdrawn, got)
	}
	for _, key := range []string{handed, withdrawn} {
		if notes := repositoryNotes(t, w, key); len(notes) != 0 {
			t.Fatalf("notes on %s = %q, want none", key, notes)
		}
	}
	if notes := repositoryNotes(t, w, prepared404); len(notes) != 1 || !strings.Contains(notes[0], "the review App is not installed on zeta/tools") {
		t.Fatalf("notes on %s = %q, want the one refusal its preparation read", prepared404, notes)
	}
}

// A child on a served repository under a parent on a repository whose read fails, the parent a todo
// child of a live tree, is recovered with its parent: the child's own event recorded nothing, its
// parent then unrecorded, and the recovery records both, parent first. The per-issue hold held
// only the parent, and the child, already todo in Dispatch, was never recorded.
func TestAChildOnAServedRepositoryUnderAnUnreadParentIsRecovered(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	rigHandlers(w)
	const root, parent, child = "LEGION-1200", "LEGION-1201", "LEGION-1202"
	liveRoot(t, w, client, root)
	deliver(t, w, client, issue(parent, "zeta/tools", "todo", false, root, 1))
	deliver(t, w, client, issue(child, "acme/widgets", "todo", false, parent, 1))
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}
	serveAndRecover(t, w, installs)
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != root {
			t.Fatalf("%s = %+v once zeta/tools is served, want it recorded in %s's tree", key, got, root)
		}
	}
}

// A recovery whose read of Dispatch's listing fails stays to recover, and is tried again on the
// refresher's backoff: a later listing records the issue, and none is read after it.
func TestARecoveryWhoseListingFailsIsTriedAgain(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	rigHandlers(w)
	const key = "LEGION-1300"
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	client.mu.Lock()
	client.listFailures = 1
	client.mu.Unlock()
	serveAndRecover(t, w, installs)
	if recordedIssue(t, w, key) == nil {
		t.Fatalf("%s not admitted by the listing after the failed one", key)
	}
	client.mu.Lock()
	left := client.listFailures
	client.mu.Unlock()
	if left != 0 {
		t.Fatal("the listing that fails was never read, want it read and then read again")
	}
	listings := client.listingCount()
	time.Sleep(10 * installReadRetry.Max)
	if got := client.listingCount(); got != listings {
		t.Fatalf("Dispatch listed %d times more once recovered, want none", got-listings)
	}
}

// A recovery while the Dispatch consumer is behind the stream holds its listing as the boot's is,
// and admits the root once the consumer catches up, once; a second recovery in the same boot, on a
// stream the consumer has caught up with, admits at once.
func TestARecoveryWhileTheConsumerIsBehindAdmitsOnceItCatchesUp(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _, js := repositoryRigJS(t, installs)
	engine, admission := rigHandlers(w)
	w.bootID, w.holdPollInterval = "behind-boot", 10*time.Millisecond
	const first, second, unrelated = "LEGION-1400", "LEGION-1401", "LEGION-1499"
	client.show(issue(unrelated, "acme/widgets", "backlog", false, "", 1))
	repo := "acme/widgets"
	if _, err := js.Publish(ctx, "notifications.dispatch.issue."+unrelated+".issue.updated",
		retainedIssueEnvelope(t, unrelated, 1499, 1, "backlog", map[string]string{"kind": "user", "id": "a-person"}, &repo)); err != nil {
		t.Fatal(err)
	}
	// The root's own event, applied while GitHub fails zeta/tools, leaves zeta/tools unread.
	deliver(t, w, client, issue(first, "zeta/tools", "todo", true, "", 1))
	serveAndRecover(t, w, installs)
	if !admission.Held() || recordedIssue(t, w, first) != nil {
		t.Fatalf("admission held %t, %s recorded %t; want the recovery held for the consumer behind the stream", admission.Held(), first, recordedIssue(t, w, first) != nil)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.consumers.Run(run, engine, admission) }()
	polled := make(chan struct{})
	go func() { w.pollHoldRelease(run); close(polled) }()
	t.Cleanup(func() { cancel(); <-done; <-polled })
	testwait.Eventually(t, first+" admitted once the consumer caught up", func() bool { return recordedIssue(t, w, first) != nil })
	if n := rowCount(t, w, "select count(*) from slots where issue = '"+first+"'"); n != 1 {
		t.Fatalf("%s holds %d slots, want one", first, n)
	}

	// A second outage in the same boot, on a repository no record names yet, now that the consumer
	// has caught up with the stream: its recovery admits at once.
	installs.setFailing(errors.New("GitHub answered 502"), "implement orbit/gadgets")
	deliver(t, w, client, issue(second, "orbit/gadgets", "todo", true, "", 1))
	if got := recordedIssue(t, w, second); got != nil {
		t.Fatalf("%s recorded as %+v while orbit/gadgets' read fails", second, got)
	}
	installs.setFailing(nil, "implement orbit/gadgets")
	testwait.Eventually(t, second+" admitted by the second recovery", func() bool { return recordedIssue(t, w, second) != nil })
}

// The refresher reads GitHub again of its own for an issue's unread repository (its third job,
// recoverRepositories) on installReadRetry's backoff, the initial wait doubling to its cap, and
// no more; once nothing is unread it reads nothing of its own, and the next repository left
// unread starts again at the initial wait. The backoff runs here at 10 ms doubling to 80 ms, which
// the refresher's clock (time.After) waits out for real, so each wait it asks for is the job's own
// backoff: a job's wait is armed when its cause stands and moved on when the job has run.
func TestTheUnreadRepositoryBackoffDoublesToItsCapAndResets(t *testing.T) {
	retry := installReadRetry
	installReadRetry.Initial, installReadRetry.Max = 10*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { installReadRetry = retry })
	var mu sync.Mutex
	var waits []time.Duration
	w, client, installs, _ := repositoryRigWith(t, &installations{}, func(w *workflowRuntime) {
		w.recheckAfter = func(d time.Duration) <-chan time.Time {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
			return time.After(d)
		}
	})
	rigHandlers(w)
	recorded := func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(waits)
	}
	start := len(recorded())
	installs.setFailing(errors.New("GitHub answered 502"), "implement orbit/gadgets")
	deliver(t, w, client, issue("LEGION-1450", "orbit/gadgets", "todo", true, "", 1))
	testwait.Eventually(t, "six waits", func() bool { return len(recorded()) >= start+6 })
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 80 * time.Millisecond, 80 * time.Millisecond}
	if got := recorded()[start : start+6]; !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}

	installs.setFailing(nil, "implement orbit/gadgets")
	testwait.Eventually(t, "orbit/gadgets recorded on and recovered", func() bool {
		return recordsOn(w, "orbit/gadgets") && len(w.unreadReasons()) == 0 && len(w.recoveringReasons()) == 0
	})
	time.Sleep(100 * time.Millisecond)
	idle := len(recorded())
	time.Sleep(100 * time.Millisecond)
	if got := len(recorded()); got != idle {
		t.Fatalf("the refresher asked for %d more waits with nothing unread, want none", got-idle)
	}
	installs.setFailing(errors.New("GitHub answered 502"), "implement orbit/tools")
	deliver(t, w, client, issue("LEGION-1451", "orbit/tools", "todo", true, "", 1))
	testwait.Eventually(t, "a wait after orbit/tools is left unread", func() bool { return len(recorded()) > idle })
	if got := recorded()[idle]; got != installReadRetry.Initial {
		t.Fatalf("the first wait after orbit/tools was left unread = %s, want %s", got, installReadRetry.Initial)
	}
}

// treeNotices is the payload, as JSON text, of every notice queued on tree, in queue order.
func treeNotices(t *testing.T, w *workflowRuntime, tree string) []string {
	t.Helper()
	rows, err := w.pool.Query(context.Background(), "select payload::text from outbox where issue = $1 and kind = 'notice' order by id", tree)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var notices []string
	for rows.Next() {
		var notice string
		if err := rows.Scan(&notice); err != nil {
			t.Fatal(err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// leftForRecovery has release_children's preparation of child, a todo child in Dispatch on a
// repository GitHub fails for, leave that repository unread (prepareObserved, leaveUnread), and the
// release leave child to the repository's recovery (awaitRecovery), as the route does for a child
// whose apply recorded nothing.
func leftForRecovery(t *testing.T, w *workflowRuntime, child dispatch.Issue, tree, parent string) {
	t.Helper()
	if _, err := w.prepareObserved(context.Background(), intake.ObservedIssue(child)); err != nil {
		t.Fatal(err)
	}
	if !w.awaitRecovery(child.ResolvedRepo, tree, child.Key, parent) {
		t.Fatalf("release_children's wait for %s on %s was refused, want it held while %s is unread", child.Key, child.ResolvedRepo, child.ResolvedRepo)
	}
}

// A labelled todo root whose event the daemon applied silently while its repository was unread is
// recovered even when Dispatch resolves it to another repository by the time GitHub answers (it
// took its repository from the project's default, or from a parent, and that changed, with no
// event of its own): the event's silence is noted with the repository it was silent for, and that
// repository's recovery takes the issue in whatever it resolves to. Scoped by its resolution at
// the recovery alone, it waited for the next boot.
func TestAnIssueSilentForAnUnreadRepositoryIsRecoveredWhereverItResolvesThen(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	const key = "LEGION-1500"
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	if recordedIssue(t, w, key) != nil {
		t.Fatalf("%s recorded while zeta/tools' read fails", key)
	}
	// The project's default moves to acme/widgets: Dispatch resolves the issue there now, and sends
	// no event of it.
	moved := issue(key, "acme/widgets", "todo", true, "", 1)
	client.show(moved)
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, key); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("%s = %+v once zeta/tools is recovered, want it admitted on acme/widgets, where it resolves now", key, got)
	}
}

// A repository's recovery tells the architect that owns each child's parent, else the tree root's
// architect, once, which children a release_children left for the repository the recovery still
// did not record, with what to release: here C, todo under a backlog parent P, which the recovery
// records no child under. A child the recovery recorded, E, is not named. Before, such a child
// stranded silently.
func TestARecoveryTellsTheArchitectOfTheChildrenItDidNotRecord(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	const tree, p, c, e = "LEGION-1600", "LEGION-1601", "LEGION-1602", "LEGION-1603"
	liveRoot(t, w, client, tree)
	client.show(issue(p, "zeta/tools", "backlog", false, tree, 1), issue(c, "zeta/tools", "todo", false, p, 1), issue(e, "zeta/tools", "todo", false, tree, 1))
	leftForRecovery(t, w, issue(c, "zeta/tools", "todo", false, p, 1), tree, p)
	leftForRecovery(t, w, issue(e, "zeta/tools", "todo", false, tree, 1), tree, tree)
	before := len(treeNotices(t, w, tree))
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, e); got == nil || got.Tree != tree {
		t.Fatalf("%s = %+v, want the recovery to record it in %s's tree", e, got, tree)
	}
	if got := recordedIssue(t, w, c); got != nil {
		t.Fatalf("%s recorded as %+v under backlog %s", c, got, p)
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], "child-release-stalled") ||
		!strings.Contains(notices[0], c+" (its parent "+p+" is not recorded: release "+p+" first, then "+c+")") || strings.Contains(notices[0], e) {
		t.Fatalf("notices to %s's architect after the recovery = %q, want one naming %s and %s, not %s", tree, notices, c, p, e)
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 1 {
		t.Fatalf("notices after the recovery = %d, want the one only", len(got))
	}
}

// A child release_children left for unread zeta/tools under a parent todo on served acme/widgets
// that the daemon had not recorded yet is recorded with its parent once both happen: the parent's
// own event records the parent, the recovery records the child, and the architect is told
// nothing, since nothing is left unrecorded.
func TestARecoveryTellsNothingOnceAChildUnderALaggingTodoParentIsRecorded(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs)
	const tree, parent, child = "LEGION-1700", "LEGION-1701", "LEGION-1702"
	liveRoot(t, w, client, tree)
	client.show(issue(child, "zeta/tools", "todo", false, parent, 1), issue(parent, "acme/widgets", "todo", false, tree, 1))
	leftForRecovery(t, w, issue(child, "zeta/tools", "todo", false, parent, 1), tree, parent)
	deliver(t, w, client, issue(parent, "acme/widgets", "todo", false, tree, 1))
	before := len(treeNotices(t, w, tree))
	serveAndRecover(t, w, installs)
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != tree {
			t.Fatalf("%s = %+v once zeta/tools is recovered, want it recorded in %s's tree", key, got, tree)
		}
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 0 {
		t.Fatalf("notices after the recovery = %q, want none: nothing is left unrecorded", got)
	}
}

// childEnvelope is the Dispatch todo event of key, a child of parent on repo, at seq, as the
// Dispatch consumer reads it off the stream.
func childEnvelope(t *testing.T, key, parent, repo string, id, seq int64) []byte {
	t.Helper()
	issue := map[string]any{"key": key, "status": "todo", "title": key, "rank": "A", "parent": parent, "labels": []string{}, "resolved_repo": repo}
	event, err := json.Marshal(map[string]any{"id": id, "issue_key": key, "seq": seq, "type": "issue.updated", "actor": map[string]string{"kind": "user", "id": "a-person"}, "notify": true, "payload": issue})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": fmt.Sprintf("dispatch-%s-%d", key, seq), "source": "dispatch", "source_event_id": fmt.Sprint(id), "topic": "issue",
		"dedupe_key": fmt.Sprintf("dispatch-%s-%d", key, seq), "issued_at": 1, "payload_summary": "issue updated", "payload": string(event), "trace_id": "child"})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// A recovery whose listing stays held, here because the todo parent's own event waits on a paused
// Dispatch consumer and keeps it below the stream's last position, tells the architect of the child
// its release left under that parent once the hold has lasted recoveryHoldBound, and only once;
// when the consumer runs, the parent's event records the parent, the hold releases, the child is
// recorded, and nothing more is told. Before, the notice waited for the hold to release, so while
// the hold lasted the child was neither recorded nor named.
func TestARecoveryHeldPastItsBoundTellsItsReleaseWaitsOnce(t *testing.T) {
	h := newHeldRecovery(t, 40*time.Millisecond, 300*time.Millisecond)
	before := h.notices(t)
	h.behind(t, heldParent)
	h.serve()
	testwait.Eventually(t, "the recovery's listing held", func() bool { return recordsOn(h.w, "zeta/tools") && h.admission.Held() })
	testwait.Eventually(t, "the release wait told past the bound", func() bool { return h.notices(t) > before })
	notices := treeNotices(t, h.w, heldTree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], heldChild+" (its parent "+heldParent+" is not recorded: release "+heldParent+" first, then "+heldChild+")") {
		t.Fatalf("notices once the hold outlasted its bound = %q, want one naming %s and %s", notices, heldChild, heldParent)
	}
	time.Sleep(4 * recoveryHoldBound)
	if n := h.notices(t) - before; n != 1 || !h.admission.Held() {
		t.Fatalf("notices = %d with the hold still %t, want the one only while the hold lasts", n, h.admission.Held())
	}

	h.consume()
	testwait.Eventually(t, "parent and child recorded once the consumer catches up", func() bool {
		p, c := recordedIssue(t, h.w, heldParent), recordedIssue(t, h.w, heldChild)
		return p != nil && p.Tree == heldTree && c != nil && c.Tree == heldTree
	})
	testwait.Eventually(t, "nothing left to recover", func() bool { return len(h.w.recoveringReasons()) == 0 })
	time.Sleep(4 * recoveryHoldBound)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices after the hold released = %d, want the one only", n)
	}
}

// A repository a pass would let go while the recovery's listing is held is put back, its release
// waits untold, only until the hold has lasted recoveryHoldBound: then its architect is told, the
// hold still standing, on the one clock the recovery's own waits are told on. Put back for as long
// as the hold stood, a hold that never releases kept those waits untold, and the repository listed
// again on the backoff, for the life of the daemon.
func TestALetGoHeldPastItsBoundTellsItsReleaseWaits(t *testing.T) {
	h := newHeldRecovery(t, 40*time.Millisecond, 300*time.Millisecond)
	const let, stranger = "LEGION-1903", "stranger/tools"
	before := h.notices(t)
	// The paused consumer falls behind the stream, so a recovery's listing holds a key it shows
	// behind: LEGION-1903, below.
	h.behind(t, heldParent)
	h.client.show(issue(let, stranger, "backlog", false, heldTree, 1))
	// What a preparation GitHub failed for and a release that left LEGION-1903 to the recovery
	// leave, written in one step so that no pass lets the repository go between the two: no
	// record names stranger/tools and no todo issue is left on it, so a pass lets it go.
	h.w.recoveringMu.Lock()
	if h.w.unread == nil {
		h.w.unread = map[string]string{}
	}
	h.w.unread[stranger] = "the implement App's installation on stranger/tools could not be read: GitHub answered 502; Legion reads it again"
	if h.w.releaseWaits == nil {
		h.w.releaseWaits = map[string]map[string]releaseWait{}
	}
	h.w.releaseWaits[stranger] = map[string]releaseWait{let: {tree: heldTree, parent: heldTree}}
	h.w.recoveringMu.Unlock()
	started := time.Now()
	wake(h.w.recoverWake)

	letGo := func() []string {
		var told []string
		for _, notice := range treeNotices(t, h.w, heldTree)[before:] {
			if strings.Contains(notice, "the daemon no longer waits for GitHub to answer for "+stranger) {
				told = append(told, notice)
			}
		}
		return told
	}
	if !within(10*time.Second, func() bool { return len(letGo()) != 0 }) {
		t.Fatalf("%s's architect was not told of %s while the hold stood (held %t, unread %q), want it told once the hold outlasted %s",
			heldTree, let, h.admission.Held(), h.w.unreadReasons(), recoveryHoldBound)
	}
	waited := time.Since(started)
	if !h.admission.Held() {
		t.Fatal("the hold released before the let-go was told, want it told while the hold stands")
	}
	if waited < recoveryHoldBound {
		t.Fatalf("the let-go was told after %s, want it put back while the hold lasted less than %s", waited, recoveryHoldBound)
	}
	if notices := letGo(); len(notices) != 1 || !strings.Contains(notices[0], let+" (backlog in Dispatch now: release it again once it is todo)") {
		t.Fatalf("let-go notices = %q, want one naming %s", notices, let)
	}
	h.w.recoveringMu.Lock()
	left := len(h.w.releaseWaits[stranger])
	marker, unread := h.w.unread[stranger]
	h.w.recoveringMu.Unlock()
	if left != 0 || unread {
		t.Fatalf("%d release waits and a marker %q are left on %s, want both gone with the telling", left, marker, stranger)
	}
}

// heldRecovery is a rig whose unread zeta/tools left the child C, under P (todo on acme/widgets,
// never recorded, its event never sent), of the live tree T to the recovery. The boot's hold
// poller runs throughout; the Dispatch consumer runs only between consume and pause, so an event
// published while it is paused keeps it behind the stream and holds the recovery's listing, and
// its run catches it up and releases the hold. (An apply that fails naks its event, and between
// redeliveries the consumer reads as idle, which would release the hold at random; one that
// blocks would hold the fact lock the recovery's listing takes.)
type heldRecovery struct {
	w          *workflowRuntime
	client     *repositoryDispatch
	js         jetstream.JetStream
	installs   *installations
	admission  *admit.Admission
	cancel     context.CancelFunc
	done       chan error
	seq        int64
	failNotice atomic.Bool
}

// noticeFailingStore is the rig's record store, its Enqueue failing for a notice while fail is set:
// the write of a recovery's notice fails, as a database refusing it would.
type noticeFailingStore struct {
	record.Store
	fail *atomic.Bool
}

func (s noticeFailingStore) Enqueue(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if s.fail.Load() && row.Kind == "notice" {
		return errors.New("the notice's write fails")
	}
	return s.Store.Enqueue(ctx, tx, row)
}

const heldTree, heldParent, heldChild = "LEGION-1900", "LEGION-1901", "LEGION-1902"

// newHeldRecovery opens the rig with installReadRetry at backoff (initial and cap) and
// recoveryHoldBound at bound, the consumer paused. The hold poller reads the consumer's position,
// or reader's when one is given.
func newHeldRecovery(t *testing.T, backoff, bound time.Duration, reader ...positionReader) *heldRecovery {
	t.Helper()
	quickInstallReads(t)
	installReadRetry.Initial, installReadRetry.Max = backoff, backoff
	was := recoveryHoldBound
	recoveryHoldBound = bound
	t.Cleanup(func() { recoveryHoldBound = was })
	h := &heldRecovery{installs: &installations{}}
	h.installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	h.w, h.client, h.installs, _, h.js = repositoryRigJS(t, h.installs)
	_, h.admission = rigHandlers(h.w)
	h.w.records = noticeFailingStore{Store: h.w.records, fail: &h.failNotice}
	h.w.bootID, h.w.holdPollInterval = "held-clock-boot", 10*time.Millisecond
	liveRoot(t, h.w, h.client, heldTree)
	h.client.show(issue(heldParent, "acme/widgets", "todo", false, heldTree, 1), issue(heldChild, "zeta/tools", "todo", false, heldParent, 1))
	leftForRecovery(t, h.w, issue(heldChild, "zeta/tools", "todo", false, heldParent, 1), heldTree, heldParent)
	var poll positionReader = h.w.consumers
	if len(reader) != 0 {
		poll = reader[0]
	}
	run, cancel := context.WithCancel(context.Background())
	polled := make(chan struct{})
	go func() { h.w.pollHoldReleaseWith(run, poll); close(polled) }()
	t.Cleanup(func() { h.pause(); cancel(); <-polled })
	return h
}

// behind publishes a todo event of key, a child of T on acme/widgets, for the paused consumer.
func (h *heldRecovery) behind(t *testing.T, key string) {
	t.Helper()
	h.seq++
	h.client.show(issue(key, "acme/widgets", "todo", false, heldTree, 1))
	if _, err := h.js.Publish(context.Background(), "notifications.dispatch.issue."+key+".issue.updated", childEnvelope(t, key, heldTree, "acme/widgets", 2000+h.seq, 1)); err != nil {
		t.Fatal(err)
	}
}

// consume runs the Dispatch consumer until pause.
func (h *heldRecovery) consume() {
	run, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- h.w.consumers.Run(run, h.w.handlers...) }()
}

func (h *heldRecovery) pause() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

// serve lets GitHub answer zeta/tools and wakes the refresher's recovery at once.
func (h *heldRecovery) serve() {
	h.installs.setFailing(nil, "implement zeta/tools")
	wake(h.w.recoverWake)
}

func (h *heldRecovery) notices(t *testing.T) int {
	t.Helper()
	return len(treeNotices(t, h.w, heldTree))
}

// A recovery held with release waits is told at its bound, not at the refresher's next backoff:
// with the backoff's cap at 2 s and the bound at 500 ms, the notice comes about 500 ms after the
// hold begins. The refresher cut its wait to the time left to the bound; before, the bound was
// checked only when the backoff next fired, up to its cap later.
func TestARecoveryHeldWithReleaseWaitsIsToldAtItsBoundNotItsBackoff(t *testing.T) {
	h := newHeldRecovery(t, 2*time.Second, 500*time.Millisecond)
	before := h.notices(t)
	h.behind(t, "LEGION-1950")
	h.serve()
	testwait.Eventually(t, "the recovery's listing held", func() bool { return recordsOn(h.w, "zeta/tools") && h.admission.Held() })
	start := time.Now()
	if !within(1500*time.Millisecond, func() bool { return h.notices(t) > before }) {
		t.Fatalf("no notice %s after the hold began, want one at about the 500ms bound, before the 2s backoff", time.Since(start))
	}
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// A hold that flaps (held at each recovery run, released between them) is told at the bound all
// the same: the clock runs from the first held run, so the notice comes once, at about the bound.
// Here the consumer stays behind the stream, so each run's listing is held, and the hold poller
// reads a position that has reached it, so each hold is released before the next run. A clock
// that started anew at each hold released and made again never reached the bound.
func TestARecoveryWhoseHoldFlapsIsToldOnceAtTheBound(t *testing.T) {
	caughtUp := &failingPositionReader{position: intake.DispatchConsumerPosition{AckFloorStream: 1 << 40, Idle: true}}
	h := newHeldRecovery(t, 100*time.Millisecond, time.Second, caughtUp)
	before := h.notices(t)
	h.behind(t, "LEGION-1960")
	h.serve()
	testwait.Eventually(t, "zeta/tools served", func() bool { return recordsOn(h.w, "zeta/tools") })
	start := time.Now()
	// Each run's hold is released at the next poll, so the flap is seen as Held turning false
	// between runs that keep listing.
	listings := h.client.listingCount()
	testwait.Eventually(t, "the recovery's listing held and released at several runs", func() bool { return h.client.listingCount() >= listings+3 })
	if !within(3*time.Second, func() bool { return h.notices(t) > before }) {
		t.Fatalf("no notice %s after the flapping hold began, want one at about the 1s bound", time.Since(start))
	}
	time.Sleep(500 * time.Millisecond)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// failPastTheBound holds the recovery's listing, lets the bound pass with fault set, and then
// checks the refresher waits its backoff (1 s here) between runs, not a busy loop: in 2 s, fewer
// than five Dispatch listings and five installation reads of zeta/tools. It then clears the fault
// with clear, and the notice comes once.
func failPastTheBound(t *testing.T, fault, clear func(h *heldRecovery)) {
	t.Helper()
	h := newHeldRecovery(t, time.Second, 300*time.Millisecond)
	before := h.notices(t)
	h.behind(t, "LEGION-1970")
	h.serve()
	testwait.Eventually(t, "the recovery's listing held", func() bool { return recordsOn(h.w, "zeta/tools") && h.admission.Held() })
	fault(h)
	time.Sleep(2 * recoveryHoldBound)
	listings, reads := h.client.listingCount(), h.installs.reads("implement zeta/tools")
	time.Sleep(2 * time.Second)
	if got, read := h.client.listingCount()-listings, h.installs.reads("implement zeta/tools")-reads; got >= 5 || read >= 5 {
		t.Fatalf("in 2 s past the bound: %d Dispatch listings and %d installation reads of zeta/tools, want fewer than 5 each at a 1 s backoff", got, read)
	}
	if n := h.notices(t) - before; n != 0 {
		t.Fatalf("notices while the fault lasts = %d, want none", n)
	}
	clear(h)
	if !within(4*time.Second, func() bool { return h.notices(t) > before }) {
		t.Fatal("no notice once the fault cleared")
	}
	time.Sleep(500 * time.Millisecond)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// A held recovery whose Dispatch listing keeps failing once its bound has passed waits the
// refresher's backoff between runs, and tells its waits once the listing answers. Before, the wait
// cut to the bound's zero time left made every failed run re-run at once.
func TestARecoveryWhoseListingFailsPastItsBoundWaitsItsBackoff(t *testing.T) {
	failPastTheBound(t, func(h *heldRecovery) {
		h.client.mu.Lock()
		h.client.listFailures = 1 << 30
		h.client.mu.Unlock()
	}, func(h *heldRecovery) {
		h.client.mu.Lock()
		h.client.listFailures = 0
		h.client.mu.Unlock()
	})
}

// The same when the write of the recovery's notice keeps failing past the bound.
func TestARecoveryWhoseNoticeFailsPastItsBoundWaitsItsBackoff(t *testing.T) {
	failPastTheBound(t, func(h *heldRecovery) { h.failNotice.Store(true) }, func(h *heldRecovery) { h.failNotice.Store(false) })
}

// The boot hears every repository a record of the project names and each configured rename's
// partner of those (heardAtBoot, heardRepositories), and caches each repository a live record
// names, with no partner (refreshCacheRepositories): here a live record on acme/widgets, the
// `from` of a pair from acme/widgets to cybermonklabs/widgets, and a done record on
// orbit/gadgets, against a Dispatch whose repository settings name zeta/extra for the project as
// well. The partner cybermonklabs/widgets is heard and not cached, zeta/extra is in neither set,
// and the boot asks Dispatch nothing under /api/v1/settings/repo-projects. Controls
// (dispatch://INFRA-4401 check a-go, item 3): with the boot's sets read from those settings
// again, zeta/extra is heard and cached, and the count of requests under the route is not 0; with
// each pair's partner added to the cache set (refreshCacheRepositories storing the live
// repositories with the partner of each pair whose `from` they hold), cybermonklabs/widgets is
// cached.
func TestTheBootHearsTheRecordsRepositoriesAndCachesTheLiveOnes(t *testing.T) {
	ctx := context.Background()
	var settings atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/settings/repo-projects") {
			settings.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"repo":"acme/widgets","project":"LEGION","default":true},{"repo":"zeta/extra","project":"LEGION","default":false}]`)
			return
		}
		t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: projectRecords{Store: record.NewStore(), project: "LEGION"}, dispatch: dispatch.New(server.URL, "dispatch-test-token"),
		dispatchProject: "LEGION", repositories: newRepositories("LEGION", ghrepo.MustRenames(ghrepo.MustRename("acme/widgets", "cybermonklabs/widgets"))), log: quietLogger()}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		for _, issue := range []record.Issue{anchorOn("LEGION-1", "acme/widgets"), doneOn("LEGION-2", "LEGION", "orbit/gadgets", "main")} {
			if err := w.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repositoryNames(heard), []string{"acme/widgets", "cybermonklabs/widgets", "orbit/gadgets"}; !slices.Equal(got, want) {
		t.Fatalf("the boot hears %v, want %v: every record's repository and its partner, and nothing Dispatch's settings name", got, want)
	}
	if got, want := repositoryNames(w.cacheRepositories()), []string{"acme/widgets"}; !slices.Equal(got, want) {
		t.Fatalf("the boot caches %v, want %v: the live record's repository alone, with no partner", got, want)
	}
	if got := slices.Sorted(maps.Keys(w.repositories.liveNames())); !slices.Equal(got, []string{"acme/widgets"}) {
		t.Fatalf("the live records name %v, want acme/widgets alone", got)
	}
	if n := settings.Load(); n != 0 {
		t.Fatalf("Dispatch was asked %d times under /api/v1/settings/repo-projects, want 0", n)
	}
}

// Each of the refresher's six jobs (refreshRepositories) runs on its own cause, with nothing in
// the sub-test standing in for it: each sub-test fails with its job deleted from the refresher and
// nothing in its place (dispatch://INFRA-4401 check a-go, item 7). republishHeard, job 4, keeps a
// test of its own besides (heard_republish_test.go).
func TestEachRefresherJobRunsOnItsOwnCause(t *testing.T) {
	t.Run("job 1 prepares the repositories the records name as the refresher starts", func(t *testing.T) {
		// prepareAtStart: with no admission, launch or other read, each anchor's repository has
		// both role Apps read once, is recorded on, and has Dispatch's webhook App checked.
		installs := &installations{}
		w, client, _, _ := repositoryRigWith(t, installs)
		for _, repo := range []string{"acme/widgets", "zeta/tools"} {
			if !recordsOn(w, repo) {
				t.Fatalf("%s is not recorded on after the refresher's start (entry %+v), want the first job's preparation of it", repo, w.repositories.entryOf(repo))
			}
			for _, role := range []string{"implement ", "review "} {
				if reads := installs.reads(role + repo); reads != 1 {
					t.Fatalf("%d reads of %q, want the first job's one", reads, role+repo)
				}
			}
		}
		if checked := client.webhookChecks(); !slices.Equal(checked, []string{"acme/widgets", "zeta/tools"}) {
			t.Fatalf("Dispatch was asked of its webhook App on %v, want both repositories the records name", checked)
		}
	})
	t.Run("job 2 checks again the webhook App of a repository only a record names", func(t *testing.T) {
		// rereadBadAnswers: zeta/tools is a repository no issue's preparation reads, and the
		// webhook App gap the first job kept for it is a bad answer read again on installReadRetry.
		quickInstallReads(t)
		w, client, _, _ := repositoryRigWith(t, &installations{}, func(w *workflowRuntime) {
			w.dispatch.(*repositoryDispatch).webhookMissing = map[string]bool{"zeta/tools": true}
		})
		if gap := w.repositories.entryOf("zeta/tools").webhookApp; gap != notInstalledWebhookApp("zeta/tools") {
			t.Fatalf("zeta/tools' webhook App gap = %q, want it named without Dispatch's GitHub App", gap)
		}
		client.mu.Lock()
		client.webhookMissing = nil
		client.mu.Unlock()
		testwait.Eventually(t, "zeta/tools' webhook App checked again and found installed", func() bool {
			return w.repositories.entryOf("zeta/tools").webhookApp == ""
		})
	})
	t.Run("job 3 reads an unread repository again and recovers its issue", func(t *testing.T) {
		// recoverRepositories: orbit/gadgets is a repository no record names, so the second job
		// never reads it, and the issue its read left is admitted once GitHub answers.
		quickInstallReads(t)
		installs := &installations{}
		installs.setFailing(errors.New("GitHub answered 502"), "implement orbit/gadgets")
		w, client, installs, _ := repositoryRigWith(t, installs)
		deliver(t, w, client, issue("LEGION-1460", "orbit/gadgets", "todo", true, "", 1))
		if got := recordedIssue(t, w, "LEGION-1460"); got != nil {
			t.Fatalf("LEGION-1460 recorded as %+v while orbit/gadgets' read fails", got)
		}
		installs.setFailing(nil, "implement orbit/gadgets")
		testwait.Eventually(t, "LEGION-1460 admitted on orbit/gadgets with no one moving it", func() bool {
			got := recordedIssue(t, w, "LEGION-1460")
			return got != nil && got.Repo == "orbit/gadgets"
		})
	})
	t.Run("job 4 publishes the heard set again after JetStream refused an update", func(t *testing.T) {
		// republishHeard: the consumer update a recording on orbit/gadgets asks for is refused
		// once, and nothing but the fourth job asks for orbit/gadgets again.
		var refused atomic.Bool
		w, client, _, stream := repositoryRigWith(t, &installations{}, func(w *workflowRuntime) {
			w.setRepositories = func(ctx context.Context, repositories []ghrepo.Repository) error {
				if !refused.Swap(true) {
					return errors.New("JetStream refused the consumer update")
				}
				return w.consumers.SetRepositories(ctx, repositories)
			}
		})
		deliver(t, w, client, issue("LEGION-1470", "orbit/gadgets", "todo", true, "", 1))
		if got := recordedIssue(t, w, "LEGION-1470"); got == nil || got.Repo != "orbit/gadgets" {
			t.Fatalf("LEGION-1470 = %+v, want it recorded on orbit/gadgets", got)
		}
		testwait.Eventually(t, "orbit/gadgets heard once the heard set is published again", func() bool {
			return refused.Load() && !w.heardPending.Load() && slices.Contains(githubFilterSubjects(t, stream), "notifications.github.orbit.gadgets.>")
		})
	})
	t.Run("job 5 reads a default branch held through a rate limit again after the limit", func(t *testing.T) {
		// rereadHeldBranches: the first job's read of zeta/tools is refused for a spent limit and
		// served on the branch its anchor holds, so nothing else reads zeta/tools again.
		var reset atomic.Int64
		github := newLimitedGitHub(t, limitedBoth, spendsOnce("zeta/tools", 2*time.Second, &reset))
		w, _, _, _ := repositoryRigWith(t, &installations{}, limitedRig(t, github, &syncBuffer{}))
		resetAt := time.Unix(reset.Load(), 0)
		if !recordsOn(w, "zeta/tools") {
			t.Fatalf("zeta/tools is not recorded on through the limit (entry %+v), want it served on the branch its record holds", w.repositories.entryOf("zeta/tools"))
		}
		testwait.Eventually(t, "zeta/tools read from GitHub again after the limit's reset", func() bool {
			kept, ok := w.defaultBranches.Kept("zeta/tools")
			return ok && kept.DefaultBranch == "trunk"
		})
		if reads := github.readsOf("zeta/tools"); len(reads) != 2 || reads[1].Before(resetAt) {
			t.Fatalf("GitHub was read for zeta/tools at %v, want the refused read and one more after the reset at %s", reads, resetAt)
		}
	})
	t.Run("job 6 reads the live records again at the commit of a tree's close", func(t *testing.T) {
		// refreshLive: the close of zeta/tools' tree takes the repository out of the cache set at
		// its commit, so the sweep stamps its cache and retires it after retain_hours.
		ctx := context.Background()
		w, _, _, _ := repositoryRigWith(t, &installations{})
		if got := repositoryNames(w.cacheRepositories()); !slices.Equal(got, []string{"acme/widgets", "zeta/tools"}) {
			t.Fatalf("the cache set at boot = %v, want both anchors' repositories", got)
		}
		closed := anchorOn(anchorTools, "zeta/tools")
		closed.Phase, closed.Status, closed.DispatchStatus = phase.Done, "done", "done"
		scoped, committed := intake.WithCommitHooks(ctx)
		if err := pgx.BeginFunc(scoped, w.pool, func(tx pgx.Tx) error { return w.records.PutIssue(scoped, tx, closed) }); err != nil {
			t.Fatal(err)
		}
		committed()
		testwait.Eventually(t, "zeta/tools out of the cache set once its tree closed", func() bool {
			return slices.Equal(repositoryNames(w.cacheRepositories()), []string{"acme/widgets"})
		})
	})
}
