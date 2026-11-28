package intake

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// A pull request whose head branch lives in another repository than the one it is opened against
// - a fork's - is no issue's, whatever its branch is named: anyone may open one and name its head
// legion/<KEY> or a task's branch, legion/<KEY>.<task>. The listener names the repository of the
// head (head_repo, GitHub's head.repo.full_name, which it leaves out once GitHub no longer has the
// fork), and every event of such a pull request is no fact, so it is acknowledged without a
// transaction and never reaches the workflow. The same events of a pull request whose head is a
// branch of the repository itself, in whatever case GitHub spells its name, decode to their facts,
// and the opened carries the repository of its head.
//
// The fault: the decode reads head_ref alone, so a fork's pull request on a task's branch decodes
// to that task's facts. The control: the decode without its read of head_repo.
func TestAPullRequestFromAForkIsNoFact(t *testing.T) {
	repositories := []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion")}
	decode := func(t *testing.T, action, branch, headRepo string) Fact {
		t.Helper()
		payload := map[string]string{"kind": "pr", "action": action, "repo": "sjawhar/legion", "number": "9", "head_ref": branch,
			"head_sha": "head-9", "before": "head-8", "base_ref": "main", "merged": "false", "updated_at": "2026-11-23T09:00:00Z"}
		if action == "merged" {
			payload["action"], payload["merged"], payload["merge_commit_sha"] = "closed", "true", "merge-9"
		}
		if headRepo != "" {
			payload["head_repo"] = headRepo
		}
		text, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeMessage("notifications.github.sjawhar.legion.pr.9", "LEGION", repositories, nil, envelopeJSON(t, "fork-"+action, "github", string(text)))
		if err != nil {
			t.Fatalf("decode the %s of #9 on %s: %v", action, branch, err)
		}
		return decoded.Fact
	}
	for _, branch := range []string{"legion/LEGION-208.impl", "legion/LEGION-208"} {
		for _, action := range []string{"opened", "reopened", "synchronize", "closed", "merged", "dequeued"} {
			// A fork's head, and the head of a fork GitHub no longer has.
			for _, headRepo := range []string{"someone/legion", ""} {
				if fact := decode(t, action, branch, headRepo); fact != nil {
					t.Errorf("the %s of #9 on %s, its head in %q, decoded %#v; want no fact: its head is no branch of sjawhar/legion", action, branch, headRepo, fact)
				}
			}
			if fact := decode(t, action, branch, "Sjawhar/Legion"); fact == nil {
				t.Errorf("the %s of #9 on %s, its head in Sjawhar/Legion, decoded no fact; want its fact: its head is a branch of sjawhar/legion", action, branch)
			}
		}
		if opened, ok := decode(t, "opened", branch, "sjawhar/legion").(PullRequestOpened); !ok || opened.Branch != branch || opened.HeadRepo != "sjawhar/legion" {
			t.Errorf("the open of #9 on %s decoded %#v; want the opened on %s whose head is in sjawhar/legion", branch, opened, branch)
		}
	}
}

// noHeadRepoWarning is the message of the consumer's line for a pull request event of an issue's
// branch that names no head repository.
const noHeadRepoWarning = "intake: a pull request event of an issue's branch was dropped: it names no head repository, so no pull request is recorded from it: " +
	"either an Envoy listener of an earlier release than legion-envoy-v29.1.0, the first that sends head_repo, sent it, " +
	"and this daemon records no pull request until that listener is upgraded, or its head is a fork that GitHub no longer has"

// A pull request event of an issue's branch that names no head repository is no fact, as one of a
// fork's head is, and the consumer says why. The Envoy listener sends head_repo from
// legion-envoy-v29.1.0 on, so behind a listener of an earlier release every such event comes
// without it and this daemon records no pull request at all; a current listener leaves the field
// out too, for a fork GitHub no longer has, and nothing else in the payload tells the two apart.
// The first such event a consumer reads is logged at WARN at once, naming both causes, the
// listener's release and the running count; another inside the interval is counted and writes no
// line, and the first after the interval writes one more with the running count. An event of a
// fork GitHub still has (its head_repo another repository) is no such event: it is dropped with no
// line and not counted. None of them is recorded, and the same event whose head is a branch of the
// repository itself is, so the count of recorded events below can see a recorded pull request.
//
// The fault: the consumer acknowledges the event with no line, so a daemon behind an older listener
// records no pull request in silence. The control: the consumer without its log call.
func TestAPullRequestEventThatNamesNoHeadRepositoryIsLogged(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	logs := &lockedBuffer{}
	ctx, spec, names, dropped := dropConsumer(t, logs)
	noHeadRepo := &pacedCount{}
	var elapsed atomic.Int64
	read := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	spec.Now = func() time.Time { return read.Add(time.Duration(elapsed.Load())) }
	var acks atomic.Int64
	consume := func(id, headRepo string) {
		t.Helper()
		payload := map[string]string{"kind": "pr", "action": "opened", "repo": "sjawhar/legion", "number": "9", "head_ref": "legion/LEGION-208",
			"head_sha": "head-9", "base_ref": "main", "merged": "false", "updated_at": "2026-11-23T09:00:00Z"}
		if headRepo != "" {
			payload["head_repo"] = headRepo
		}
		text, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		message := deliveredMessage{unexpectedMessage: unexpectedMessage{t: t}, subject: "notifications.github.sjawhar.legion.pr.9",
			data: envelopeJSON(t, id, "github", string(text)), acks: &acks}
		before := acks.Load()
		metadata, _ := message.Metadata()
		run := consumerRun{spec: spec, repositories: capturedRepositories, pool: pool, names: names, remade: &remadeFailures{}, dropped: dropped,
			noHeadRepo: noHeadRepo}
		if !consumeMessage(ctx, message, metadata, run, []Handler{writeHandler("recorded", nil)}) {
			t.Fatalf("%s was not handled, want it acknowledged; the consumer's log:\n%s", id, logs)
		}
		if got := acks.Load() - before; got != 1 {
			t.Fatalf("%s was acknowledged %d times, want once", id, got)
		}
	}
	lines := func() int { return strings.Count(logs.String(), noHeadRepoWarning) }

	consume("no-head-1", "")
	requireLogLine(t, logs, "level=WARN", `msg="`+noHeadRepoWarning+`"`, "event_id=no-head-1", "no_head_repo_count=1",
		"subject=notifications.github.sjawhar.legion.pr.9")

	consume("no-head-2", "")
	if got := lines(); got != 1 {
		t.Fatalf("%d lines after a second event inside the interval, want 1:\n%s", got, logs)
	}
	if got := noHeadRepo.count.Load(); got != 2 {
		t.Fatalf("the consumer counts %d events that name no head repository, want 2", got)
	}

	consume("fork-head", "someone/legion")
	if got := lines(); got != 1 || strings.Contains(logs.String(), "event_id=fork-head") {
		t.Fatalf("%d lines after an event of a fork GitHub still has, want 1 and none naming it:\n%s", got, logs)
	}
	if got := noHeadRepo.count.Load(); got != 2 {
		t.Fatalf("the consumer counts %d events that name no head repository after a fork's, want still 2", got)
	}

	elapsed.Store(int64(time.Minute))
	consume("no-head-3", "")
	if got := lines(); got != 2 {
		t.Fatalf("%d lines after an event past the interval, want 2:\n%s", got, logs)
	}
	requireLogLine(t, logs, noHeadRepoWarning, "event_id=no-head-3", "no_head_repo_count=3")
	if got := writeCount(t, pool); got != 0 {
		t.Fatalf("%d pull request events recorded, want none: none names a head of sjawhar/legion", got)
	}

	consume("own-head", "sjawhar/legion")
	if got := writeCount(t, pool); got != 1 {
		t.Fatalf("%d pull request events recorded after one whose head is a branch of sjawhar/legion, want 1", got)
	}
	if got := lines(); got != 2 {
		t.Fatalf("%d lines after an event whose head is a branch of sjawhar/legion, want 2:\n%s", got, logs)
	}
}
