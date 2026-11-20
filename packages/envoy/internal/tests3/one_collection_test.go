package tests3

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// storeCase is one case of TestOneCollectionFailsWhenTheRuleDoesNotHold: one-collection.sh with
// parts cut, and the failure it must end in.
type storeCase struct {
	name string
	// Each cut's first text is replaced by its second, and must occur in the script exactly once.
	cuts [][2]string
	// write says whether an object is written between prepare and check.
	write bool
	// shared says the case runs on the one server the shared cases start between them, and that it
	// makes that store write nothing: it writes no object, grows no collection and stores nothing in
	// the filer, not a rule, not a directory. An entry stored in the filer makes the filer write a
	// file of its own log at its next minute: after the rule of "the rule never in force" that file
	// went into tests3, which had no volume there, so the master grew tests3 for it, and every later
	// check of that server failed by that growth. A shared case reads nothing of the server that
	// another case sets either. A case that does any of these runs in a server of its own.
	shared bool
	// deadline is runScript's for the case's steps; scriptDeadline when it is zero. It counts the
	// docker exec's own start too, which took up to minutes on a loaded host, so a case leaves it
	// at scriptDeadline unless what it wants is that deadline's message.
	deadline time.Duration
	// step is the step that must fail, and want the whole line its error prints; the failing
	// step's output must hold alsoWant too, when it is set.
	step, want, alsoWant string
}

// growElsewhere aims the grow at another collection, so the collection has no volumes of its own
// before the first write. The grow still runs, with the steps before it.
var growElsewhere = [2]string{"collection=$collection&count=7", "collection=elsewhere&count=7"}

// noGrow cuts the grow, for a case that fails after it and needs no volumes: every volume the
// grow makes preallocates its 1 GiB in the container (the image's -master.volumePreallocate).
var noGrow = [2]string{"\n  grow\n  configure\n", "\n  configure\n"}

// noPrepare makes prepare do nothing, for a case that fails in check and needs no state.
var noPrepare = [2]string{"  prepare) prepare ;;", "  prepare) : ;;"}

// storeCases holds a case for each check and each wait of one-collection.sh, and for runScript's
// deadline: each wants the message that check or the end of that wait prints, and
// TestEveryWaitAndCheckOfTheScriptHasACase fails when a message of the script is the want of no
// case here. A case of a wait cuts that wait's bound, the text the script reads, to a few
// seconds, so it shows that bound ends the wait without waiting out the real one, and runs under
// a deadline the wait would meet were its bound removed.
var storeCases = []storeCase{{
	// Then the first write would be the one that grows the collection, waiting on it.
	name: "grow aimed elsewhere: prepare fails before any write",
	cuts: [][2]string{growElsewhere},
	step: "prepare", want: "::error::before the first write the store holds no volumes of tests3 alone",
}, {
	// weed shell exits 0 over the command it does not know, and the rule is not stored. Its own
	// server: it reads the filer's rule, which a shared case may not store (shared).
	name: "rule misspelt: prepare fails by the rule read back",
	cuts: [][2]string{{"fs.configure -locationPrefix=/", "fs.configurx -locationPrefix=/"}},
	step: "prepare", want: "::error::the filer holds no rule putting every path into tests3",
}, {
	// The check before the first write cut too, so only check's own reading of the log can
	// see that the write grew the collection; the log is read before the collections are.
	name:  "a write grew storage: check fails by the server's log",
	cuts:  [][2]string{growElsewhere, {"\n  alone_before_writes\n", "\n"}},
	write: true,
	step:  "check",
	want:  "::error::a write grew the store's storage: the master grew a collection for a write that found no writable volume",
}, {
	// check pointed at a log that does not exist: a log that cannot be read must not read as one
	// with no growth in it.
	name:   "no log: check fails rather than read nothing as no growth",
	cuts:   [][2]string{noPrepare, {"  check) check ;;", "  check) log=/no-such.log; check ;;"}},
	shared: true,
	step:   "check", want: "::error::the server's log /no-such.log is missing or empty, so no growth can be read from it",
}, {
	// A second collection made after prepare by /vol/grow, which the master logs as
	// volumeGrowHandler and not as the growth a write asks for, so check's log reading passes and
	// only its list of the collections can see it.
	name: "another collection after prepare: check fails by the collections listed",
	cuts: [][2]string{{"  check) check ;;", "  check) within 10 \"no grow of elsewhere\" \"$work/elsewhere.txt\" master \"/vol/grow?collection=elsewhere&count=1\" \"$work/elsewhere.txt\"; check ;;"}},
	step: "check", want: "::error::the store holds a collection other than tests3, so a write grew one",
}, {
	// check's read of the volume server, on a master that lists none: a store that stopped
	// during the run fails by name at once, not at a shell's bound. check's reading of the log
	// comes first and passes on the shared server's log, since no shared case makes that store
	// write (shared). The case times the refusal inside the container, where a docker exec's own
	// start, up to 40 s on a loaded host, is not counted: the script's exit prints the line
	// alsoWant wants only when check ended within 5 s of its start, so a read that waits fails
	// the case, as `registered 30` would wait 30 s.
	name:   "no volume server listed: check fails by name at once",
	cuts:   [][2]string{noPrepare, {"  check) check ;;", `  check) listed() { : > "$work/status.txt"; false; }; started=$(date +%s); trap '[ $(( $(date +%s) - started )) -le 5 ] && echo "check ended within 5 s of its start"; rm -rf "$work"' EXIT; check ;;`}},
	shared: true,
	step:   "check", want: "::error::the master lists no volume server, so the store cannot take a write",
	alsoWant: "check ended within 5 s of its start",
}, {
	// A master that takes each request and never answers, as a stopped one does, with the wait's
	// bounds cut to 3 s of waiting and 5 s a request, and each request numbered in the status file
	// the failure prints, which each try writes anew: the first request ends at its bound, and the
	// wait by name after it, so the file shows try 1. Counting tries, as the wait did before, it
	// would end after try 3; with the request's bound removed it would wait for ever and meet
	// runScript's deadline.
	name: "a master that never answers: prepare fails at the end of its wait for a volume server",
	cuts: [][2]string{
		{`timeout -s KILL 10 wget -q -O - "http://127.0.0.1:9333$1"`, `{ tries=$((${tries:-0} + 1)); echo "try $tries of a request no one answers"; timeout -s KILL 5 sleep 100000; }`},
		{"prepare() {\n  registered 60\n", "prepare() {\n  registered 3\n"},
	},
	shared: true,
	step:   "prepare", want: "::error::the master lists no volume server, so the store cannot take a write",
	alsoWant: "try 1 of a request no one answers",
}, {
	// A shell whose master does not answer it, with the shell's bound cut to 3 s: the bound ends
	// it by KILL, which reads as a failure. By TERM it read as a success, and the case would fail
	// later, by the rule read back; with no bound it would wait for ever and meet runScript's
	// deadline.
	name:   "a shell that hangs: prepare fails by its bound",
	cuts:   [][2]string{noGrow, {"timeout -s KILL 60 weed shell -master=127.0.0.1:9333 ", "timeout -s KILL 3 weed shell -master=127.0.0.1:1 "}},
	shared: true,
	step:   "prepare", want: "::error::weed shell could not run",
}, {
	// A master that refuses every grow, with the grow's 10 s cut to 3 s: its tries end by name.
	name:   "the grow refused: prepare fails at the end of its tries",
	cuts:   [][2]string{{`within 10 "the master grew no volumes for $collection" "$work/grow.txt" master "/vol/grow?`, `within 3 "the master grew no volumes for $collection" "$work/grow.txt" false "/vol/grow?`}},
	shared: true,
	step:   "prepare", want: "::error::the master grew no volumes for tests3",
}, {
	// A filer that never logs the marker, with the wait's minute cut to 3 s: it ends by name. Its
	// own server: it stores the rule, and the filer's next log file then grows tests3 (shared).
	name: "the rule never in force: prepare fails at the end of its wait",
	cuts: [][2]string{noGrow, {`within 60 "the filer did not put the rule for $collection in force within a minute" "$work/rule.txt" grep -q "procesing.*$marker"`, `within 3 "the filer did not put the rule for $collection in force within a minute" "$work/rule.txt" grep -q "a line no filer logs.*$marker"`}},
	step: "prepare", want: "::error::the filer did not put the rule for tests3 in force within a minute",
}, {
	// A step that never ends, which the script cannot bound: runScript's deadline ends it, here
	// after 5 s rather than scriptDeadline's minutes, by name.
	name:     "a step that never ends: runScript fails it by its deadline",
	cuts:     [][2]string{{"  prepare) prepare ;;", "  prepare) sleep 100000 ;;"}},
	shared:   true,
	deadline: 5 * time.Second,
	step:     "prepare", want: "one-collection.sh prepare did not end within 5s",
}}

// Each check and each wait of one-collection.sh fails when what it checks is broken or what it
// waits for never comes, on the plain server and not only on the rare run where the growth race
// comes. Each case runs the script with parts cut; the cases that make their store write nothing
// share one server, each with its script at a path of its own, and the rest run in a server each,
// so the test starts six servers, not one a case. TestS3AgainstAnS3CompatibleServer
// (internal/dispatch/files) runs the whole script through Start, which is the case that must
// pass. The shard job of .github/workflows/dispatch-browser.yaml runs the same script, so these
// cases hold for its server too. Every weed shell of the script runs behind its wait for a
// registered volume server and under its own bound, so no case can leave a shell waiting for ever.
func TestOneCollectionFailsWhenTheRuleDoesNotHold(t *testing.T) {
	if os.Getenv("DISPATCH_TEST_NO_CONTAINERS") == "1" {
		t.Skip("DISPATCH_TEST_NO_CONTAINERS=1: this box runs no test containers")
	}
	t.Run("on one shared server", func(t *testing.T) {
		t.Parallel()
		server, endpoint := startServer(context.Background(), t, oneCollection)
		for i, c := range storeCases {
			if !c.shared {
				continue
			}
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				path := fmt.Sprintf("/one-collection-case-%d.sh", i)
				if err := server.CopyToContainer(context.Background(), []byte(cutScript(t, c)), path, 0o755); err != nil {
					t.Fatalf("copy the case's one-collection.sh into the shared server: %v", err)
				}
				runCase(t, c, server, endpoint, path)
			})
		}
	})
	for _, c := range storeCases {
		if c.shared {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			server, endpoint := startServer(context.Background(), t, cutScript(t, c))
			runCase(t, c, server, endpoint, scriptPath)
		})
	}
}

// cutScript answers one-collection.sh with c's cuts made, each of which must match once.
func cutScript(t *testing.T, c storeCase) string {
	t.Helper()
	script := oneCollection
	for _, cut := range c.cuts {
		script = replaceOnce(t, "one-collection.sh", script, cut[0], cut[1])
	}
	return script
}

// replaceOnce answers text, which is file's, with old replaced by replacement, and fails t unless
// old occurs in it exactly once: a case's cut or a planted fault whose text the file no longer
// holds, or holds twice, would test something other than what it names. It is the one place that
// rule is kept, for the cases here and the planted faults of waits_test.go.
func replaceOnce(t *testing.T, file, text, old, replacement string) string {
	t.Helper()
	if n := strings.Count(text, old); n != 1 {
		t.Fatalf("the text to replace occurs %d times in %s, not once: %q", n, file, old)
	}
	return strings.Replace(text, old, replacement, 1)
}

// runCase runs c's steps with the script at path in server, and fails t unless c's step fails
// with c's message.
func runCase(t *testing.T, c storeCase, server testcontainers.Container, endpoint, path string) {
	t.Helper()
	ctx := context.Background()
	deadline := c.deadline
	if deadline == 0 {
		deadline = scriptDeadline
	}
	// run answers a step's exit and output, and an error of runScriptWithin as an exit of -1
	// with the error for output, so a case can want it.
	run := func(step string) (int, string) {
		code, output, err := runScriptWithin(ctx, server, path, step, deadline)
		if err != nil {
			return -1, err.Error()
		}
		return code, output
	}
	// failed reads a failing step's output: it must hold want, and alsoWant when the case has one.
	failed := func(code int, output string) bool {
		return code != 0 && strings.Contains(output, c.want) && strings.Contains(output, c.alsoWant)
	}
	code, output := run("prepare")
	if c.step == "prepare" {
		if !failed(code, output) {
			t.Fatalf("prepare: exit %d, want a failure saying %q and %q:\n%s", code, c.want, c.alsoWant, output)
		}
		return
	}
	if code != 0 {
		t.Fatalf("prepare: exit %d, want 0:\n%s", code, output)
	}
	if c.write {
		const bucket = "cut-bucket"
		if status, body := call(ctx, t, http.MethodPut, endpoint+"/"+bucket, ""); status != http.StatusOK {
			t.Fatalf("create bucket %s: %d %s", bucket, status, body)
		}
		// This write grows the collection, so it meets the growth race the script exists to keep
		// from every other write, and its answer can be a 500. The master logs the growth when the
		// write's assign asks for it, before the race can act, and that line is all check reads,
		// so the answer is logged and not asserted.
		status, body := call(ctx, t, http.MethodPut, endpoint+"/"+bucket+"/object", "bytes")
		t.Logf("write an object: %d %s", status, body)
	}
	if code, output := run("check"); !failed(code, output) {
		t.Fatalf("check: exit %d, want a failure saying %q and %q:\n%s", code, c.want, c.alsoWant, output)
	}
}
