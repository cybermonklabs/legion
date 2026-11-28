//go:build twobuild

// c-previous-release-doc-read: the `dispatch doc-read` of the release before still reads which
// asks of a spec are answered or resolved, and by whom, from this build's text route. That command
// reads the markdown alone and sends no `Dispatch-Asks` header, so the route serves it the kept
// form (docs' KeptText). This build's `dispatch doc-read` sends `Dispatch-Asks: 1`, reads none of
// the eight server-owned ask attributes in the markdown, and reads each answer from the asks member.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/testwait"
)

// docReadToken is the line a `dispatch doc-read` of either build prints its document's token on.
var docReadToken = regexp.MustCompile(`(?m)^Document token: (\S+)$`)

// docReadAskOpeners is the attributes of each ask block's opening line in what a `dispatch
// doc-read` printed, by the block's id.
func docReadAskOpeners(printed string) map[string]pmdoc.Attrs {
	openers := map[string]pmdoc.Attrs{}
	for _, line := range previousCLIAskOpener.FindAllString(printed, -1) {
		_, attrs, opens := pmdoc.ParseTypedDirectiveOpen(strings.TrimLeft(line, "> \t-"))
		if id, _ := attrs[pmdoc.BlockIDAttr].(string); opens && id != "" {
			openers[id] = attrs
		}
	}
	return openers
}

// decidedAsk is one ask of the row's spec: its block's id, its ask's id, the state a person left
// it in ("" for the open ask), and the words of the answer to it ("" where none answered it).
type decidedAsk struct {
	block, ask, state, words string
}

// Only this build serves. Through it an agent writes a spec that holds four asks, and a person
// answers two of them, resolves the third and leaves the fourth open. What the previous build's
// `dispatch doc-read` of the spec prints depends on whether that build reads a person's answer from
// the server, which the row reads from the schema version the build registers
// (previousReadsAnswersFromTheServer), never from its commit. A build that reads answers from the
// blocks, as the release before this one does, prints the state, the answerer and the time of each
// answered ask on its block's opening line, equal to the ask's node, the state of the resolved ask,
// no state on the open ask, and no answer's words on any block. A build that reads answers from the
// server, as this tree built as the previous build in the merge group's dispatch-two-builds-same
// does, prints what this build's own read prints. This build's `dispatch doc-read` of the same spec
// prints none of the eight server-owned attributes on any block, and each answer in its trailer from
// the asks member. Both reads print the same token.
func TestThePreviousReleasesDocReadReadsWhichAsksAreDecided(t *testing.T) {
	ctx := context.Background()
	builds := newTwoBuilds(t)
	fromTheServer := previousReadsAnswersFromTheServer(t, builds.build)
	current := builds.startCurrent(t, nil)
	project := uniqueProjectKey(t, "PDR")
	current.createProject(t, project, "The previous doc-read")
	home := t.TempDir()
	created := currentCLI(t).runOK(t, current, builds.currentName(), home,
		"issue", "--project", project, "--title", "Decided asks read", "--spec", previousCLISpec, "--force")
	issue := key(t, project, created.stdout)
	var read servedIssue
	getJSON(t, current, "/api/v1/issues/"+issue, &read)
	spec := read.PrimaryArtifactID
	first := settledAskOf(t, current, issue, "first")
	second := settledAskOf(t, current, issue, "second")
	third := settledAskOf(t, current, issue, "third")
	settledAskOf(t, current, issue, "fourth")

	answerThrough(t, current, first, "Ship", "Ship the first now.")
	answerThrough(t, current, second, "Hold", "Hold the second.")
	current.call(t, http.MethodPost, "/api/v1/asks/"+third+"/resolve", `{"kind":"resolved","reason":"Settled in the thread."}`, http.StatusOK)
	waitForNoSettlementOwed(t, ctx, builds, spec)

	want := []decidedAsk{
		{"first", first, asks.StateAnswered, "Ship the first now."},
		{"second", second, asks.StateAnswered, "Hold the second."},
		{"third", third, asks.StateResolved, ""},
		{"fourth", "", "", ""},
	}
	answered := map[string][2]string{}
	for _, ask := range want {
		if ask.state != asks.StateAnswered {
			continue
		}
		state, answeredBy, answeredAt := askNodeAnswerOf(t, ctx, builds, ask.ask)
		if state != asks.StateAnswered || answeredBy == "" || answeredAt == "" {
			t.Fatalf("the node of ask %s (block %s) holds state %q, answered_by %q and answered_at %q, want the person's answer", ask.ask, ask.block, state, answeredBy, answeredAt)
		}
		answered[ask.block] = [2]string{answeredBy, answeredAt}
	}

	previousRead := builds.previousCLI().runOK(t, current, builds.currentName(), home, "doc-read", "--issue", issue)
	if fromTheServer {
		requireReadFromTheAsksMember(t, "the previous build's", previousRead.stdout, want, answered)
	} else {
		requireReadFromTheKeptForm(t, previousRead.stdout, want, answered)
	}
	currentRead := currentCLI(t).runOK(t, current, builds.currentName(), home, "doc-read", "--issue", issue)
	requireReadFromTheAsksMember(t, "this build's", currentRead.stdout, want, answered)

	previousToken, currentToken := docReadToken.FindStringSubmatch(previousRead.stdout), docReadToken.FindStringSubmatch(currentRead.stdout)
	if previousToken == nil || currentToken == nil || previousToken[1] != currentToken[1] {
		t.Errorf("the previous build's dispatch doc-read prints the token %q and this build's %q, want one token whichever form the reader read", previousToken, currentToken)
	}
	t.Logf("the previous build's dispatch doc-read (it reads answers from the server: %t):\n%s\nthis build's dispatch doc-read:\n%s",
		fromTheServer, previousRead.stdout, currentRead.stdout)
}

// requireReadFromTheKeptForm fails t unless printed, what the previous build's `dispatch doc-read`
// printed, reads each ask of want in the kept form, as a build that reads answers from the blocks
// is served it: the state of each decided ask on its block's opening line, and for an answered ask
// its answerer and time, equal to its node's (answered, by block); no state on the open ask; and no
// answer's words on any block.
func requireReadFromTheKeptForm(t *testing.T, printed string, want []decidedAsk, answered map[string][2]string) {
	t.Helper()
	openers := docReadAskOpeners(printed)
	for _, ask := range want {
		attrs, held := openers[ask.block]
		if !held {
			t.Errorf("the previous build's dispatch doc-read prints no ask block %s:\n%s", ask.block, printed)
			continue
		}
		switch state, stated := attrs["state"]; {
		case ask.state == "" && stated:
			t.Errorf("the previous build's dispatch doc-read prints the open ask block %s with state=%v, want no state, as the release before reads an open ask", ask.block, state)
		case ask.state != "" && !stated:
			t.Errorf("the previous build's dispatch doc-read prints the ask block %s with no state, want state=%q, as its node holds it", ask.block, ask.state)
		case ask.state != "" && state != ask.state:
			t.Errorf("the previous build's dispatch doc-read prints the ask block %s with state=%v, want state=%q, as its node holds it", ask.block, state, ask.state)
		}
		if node, wanted := answered[ask.block]; wanted && (attrs["answered_by"] != node[0] || attrs["answered_at"] != node[1]) {
			t.Errorf("the previous build's dispatch doc-read prints the ask block %s with answered_by=%v and answered_at=%v, want its node's %q and %q", ask.block, attrs["answered_by"], attrs["answered_at"], node[0], node[1])
		}
		for _, name := range []string{"selected", "answer", "invalid"} {
			if value, held := attrs[name]; held {
				t.Errorf("the previous build's dispatch doc-read prints the ask block %s with %s=%v, want no answer's words on any block", ask.block, name, value)
			}
		}
	}
}

// requireReadFromTheAsksMember fails t unless printed, what reader's `dispatch doc-read` printed,
// reads the asks of want from the asks member, as a build that reads answers from the server does:
// none of the eight server-owned attributes on any ask block, and a trailer line for each answered
// ask with its answerer and time, equal to its node's (answered, by block), and its words.
func requireReadFromTheAsksMember(t *testing.T, reader, printed string, want []decidedAsk, answered map[string][2]string) {
	t.Helper()
	openers := docReadAskOpeners(printed)
	for _, ask := range want {
		attrs, held := openers[ask.block]
		if !held {
			t.Errorf("%s dispatch doc-read prints no ask block %s:\n%s", reader, ask.block, printed)
			continue
		}
		for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"} {
			if value, held := attrs[name]; held {
				t.Errorf("%s dispatch doc-read prints the ask block %s with %s=%v, want none of the eight server-owned ask attributes on any block", reader, ask.block, name, value)
			}
		}
		node, wanted := answered[ask.block]
		if !wanted {
			continue
		}
		id, _ := json.Marshal(ask.block)
		quoted, _ := json.Marshal(ask.words)
		if line := fmt.Sprintf("Answered %s by %s at %s: %s", id, node[0], node[1], quoted); !strings.Contains(printed, line) {
			t.Errorf("%s dispatch doc-read prints no line opening %q, the answer the asks member holds:\n%s", reader, line, printed)
		}
	}
}

// previousReadsAnswersFromTheServer reports whether the previous build reads a person's answer to
// an ask from the server rather than from the attributes of the ask's block. The rule is the one
// docs' anotherBuildServes reads a live process by (R5): a build reads answers from the server when
// the schema version it registers (previousBuildSchemaVersion) is at or past the version at which
// answers left the markdown, which is the since of the decision type in this build's schema. A
// build below it, as the release before this one is, reads them from the block, and its `dispatch
// doc-read` reads the markdown alone and sends no `Dispatch-Asks` header. A build at or past it,
// such as this tree built as the previous build, reads them from the asks member, as this build's
// `dispatch doc-read` does.
func previousReadsAnswersFromTheServer(t *testing.T, build previousBuild) bool {
	t.Helper()
	left, declared := pmdoc.Element{Kind: pmdoc.TypedBlockElement, Name: "decision"}.Versions()
	if !declared {
		t.Fatal("this build's schema declares no decision type, whose since is the version at which answers left the markdown")
	}
	return previousBuildSchemaVersion(t, build) >= left.Since
}

// probedSchemaVersions is the schema version each previous build registered when
// previousBuildSchemaVersion booted it, by the build's directory. It lasts as long as the test
// binary, so a later read boots no second probe.
var probedSchemaVersions = struct {
	sync.Mutex
	byDir map[string]int
}{byDir: map[string]int{}}

// previousBuildSchemaVersion answers the schema version build's process registers in the server
// process registry: the version of the schema it serves (pmdoc.SchemaVersion), or 0, the column's
// default, for a build before the fleet columns, which registers none. This row's database serves
// this build alone, so the probe boots build once alone on a database of its own
// (laneDatabaseAfter) and a NATS server of its own, as previousBuildOwnerLockProtocol does, reads
// the one row the registry holds, and kills the process.
func previousBuildSchemaVersion(t *testing.T, build previousBuild) int {
	t.Helper()
	probed := &probedSchemaVersions
	probed.Lock()
	defer probed.Unlock()
	if version, known := probed.byDir[build.dir]; known {
		return version
	}
	name := "probe of the previous build (" + shortCommit(build.commit) + ")"
	database := laneDatabaseAfter(t, build, cmdtest.Build(t, "envoy-dispatch"))
	_, nats := multiprocessNATS(t)
	probe := startDispatchProcessWith(t, databaseURLOf(t, database),
		dispatchOptions{NATSURL: nats, SigningKey: multiprocessSigningKey, Binary: build.path("envoy-dispatch")})
	registered := testwait.Until(t, "the "+name+" to register (the schema versions the registry holds)", func() ([]int, bool) {
		select {
		case <-probe.Exited:
			t.Fatalf("the %s exited before it registered:\n%s", name, probe.Output.String())
		default:
		}
		rows, err := database.Pool.Query(context.Background(), `select schema_version from server_processes`)
		if err != nil {
			t.Fatalf("read the registered server processes: %v", err)
		}
		versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil {
			t.Fatalf("read the registered server processes: %v", err)
		}
		return versions, len(versions) > 0
	})
	if len(registered) != 1 {
		t.Fatalf("the registry holds %d server processes, at schema versions %v, where the %s's should be its only one", len(registered), registered, name)
	}
	if err := probe.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the %s: %v", name, err)
	}
	testwait.Receive(t, "the "+name+" to exit once killed", probe.Exited)
	probed.byDir[build.dir] = registered[0]
	return registered[0]
}
