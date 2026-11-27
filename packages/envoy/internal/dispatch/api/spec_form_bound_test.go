package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cputest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// boundDocumentBytes is the size of the broken document of the read route's bound, under the
// 1 MiB that a document's markdown may hold.
const boundDocumentBytes = 1_000_000

// boundBroken is a statement of stops that no space follows, which gives eight findings, and
// boundClean a statement of as many bytes that breaks no rule.
const (
	boundBroken = "Dispatch.Keeps it.Each.Record ends.It.Stops here.Then.Again it.Runs on.And.On it.Goes back.Now."
	boundClean  = "Dispatch keeps each record of each issue, and it shows each record to each person who reads it."
)

// boundStatements is a document of count statements, each body under its own id.
func boundStatements(body string, count int) string {
	var document strings.Builder
	document.WriteString("# Bound\n")
	for index := range count {
		fmt.Fprintf(&document, "\n:::requirement{#statement-%d}\n%s\n:::\n", index+1, body)
	}
	return document.String()
}

// heapAllocated is the bytes that the process has allocated on the heap since it started.
func heapAllocated() uint64 {
	sample := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// boundWorkTimes is how many times the work and the allocation of the judgment of the broken
// document may be those of the judgment of the clean one.
const boundWorkTimes = 3

// cancelAtPoll is a context that ends itself at the poll-th call of its Err. The judgment polls
// its context once before each text (specform.Check), so it ends partway through the judgment at
// a place that no clock decides.
type cancelAtPoll struct {
	context.Context
	cancel context.CancelFunc
	poll   int64
	polls  atomic.Int64
}

func (c *cancelAtPoll) Err() error {
	if c.polls.Add(1) == c.poll {
		c.cancel()
	}
	return c.Context.Err()
}

// The read route judges any document, at any setting of the roots, for any signed-in caller, so
// its judgment of the largest document holds a bound. Two documents stand on an issue, with no
// root named: one of about 1 MB (the markdown of a document holds at most 1 MiB) whose every
// statement is boundBroken, and one of as many statements, each boundClean. The work of the
// judgment of the broken one (specFormOf at actRead), the CPU time of the thread that judges it
// (cputest.OnThread), and the bytes that the process allocates during it, are each less than
// boundWorkTimes times those of the clean one, the least of two samples of each. The route answers
// at most specFormFindingsHeld findings, and counts them all in findings_total. A context that
// ends stops the judgment: it answers the context's error at the next text, and judges no text
// after it. Each subtest judges what it compares, so each runs alone (`-run`) as it runs among the
// others.
func TestTheReadRouteJudgesADocumentOfOneMebibyteInBoundedWork(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{})
	served := tree.deps.SpecForm
	statements := (boundDocumentBytes - len("# Bound\n")) / len(boundStatements(boundBroken, 1))
	documents := map[string]model.Artifact{
		"broken": uploadFormDocument(t, tree.handler, tree.root, "broken.md", boundStatements(boundBroken, statements)),
		"clean":  uploadFormDocument(t, tree.handler, tree.root, "clean.md", boundStatements(boundClean, statements)),
	}
	awaitSettled(t, tree.database, documents["broken"].ID, documents["clean"].ID)

	// judge judges the document name and gives the CPU time of the thread that judged it, the bytes
	// the process allocated meanwhile, and the count of its findings.
	judge := func(ctx context.Context, name string) (time.Duration, uint64, int, error) {
		runtime.GC()
		before := heapAllocated()
		named := []model.AskApprovalDocument{{ArtifactID: documents[name].ID, Name: documents[name].Name, Version: 1}}
		var (
			findings int
			err      error
		)
		work := cputest.OnThread(func() {
			var result specFormResult
			if result, err = served.specFormOf(ctx, tree.database.Pool, named, actRead); err == nil {
				findings = len(result.documents[0].findings)
			}
		})
		return work, heapAllocated() - before, findings, err
	}
	t.Run("the work", func(t *testing.T) {
		work, allocated := map[string]time.Duration{}, map[string]uint64{}
		findings := 0
		for sample := range 2 {
			for _, name := range []string{"clean", "broken"} {
				each, bytes, found, err := judge(context.Background(), name)
				if err != nil {
					t.Fatalf("judge the %s document: %v", name, err)
				}
				t.Logf("the %s document: %v of CPU, %.1f MB allocated", name, each, float64(bytes)/1e6)
				if name == "broken" {
					findings = found
				}
				if sample == 0 || each < work[name] {
					work[name] = each
				}
				if sample == 0 || bytes < allocated[name] {
					allocated[name] = bytes
				}
			}
		}
		if findings < statements {
			t.Fatalf("the judgment of %d broken statements gives %d findings, want one or more for each", statements, findings)
		}
		times := float64(work["broken"]) / float64(work["clean"])
		bytes := float64(allocated["broken"]) / float64(allocated["clean"])
		t.Logf("%d statements, %d findings: %.1f times the work and %.1f times the allocation of the clean document", statements, findings, times, bytes)
		if times >= boundWorkTimes || bytes >= boundWorkTimes {
			t.Errorf("the judgment of %d broken statements is %.1f times the work (%v) and %.1f times the allocation (%.1f MB) of as many clean ones, want less than %d times each",
				statements, times, work["broken"], bytes, float64(allocated["broken"])/1e6, boundWorkTimes)
		}
	})
	t.Run("the findings it answers", func(t *testing.T) {
		_, _, findings, err := judge(context.Background(), "broken")
		if err != nil {
			t.Fatalf("judge the broken document: %v", err)
		}
		response := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+documents["broken"].ID+"/spec-form", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read the broken document's form: status=%d body=%.300s", response.Code, response.Body.String())
		}
		var read specFormRead
		if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
			t.Fatal(err)
		}
		t.Logf("the route answers %d of %d findings in %d bytes", len(read.Findings), read.FindingsTotal, response.Body.Len())
		if len(read.Findings) != specFormFindingsHeld || read.FindingsTotal != findings || read.FindingsTotal <= specFormFindingsHeld {
			t.Errorf("the route answers %d findings and findings_total %d, want %d and %d", len(read.Findings), read.FindingsTotal, specFormFindingsHeld, findings)
		}
	})
	t.Run("a context that ends", func(t *testing.T) {
		inner, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The context ends at the poll of the text half way through the statements: the reads
		// before the judgment poll it a few times at most, and the judgment once for each
		// statement's text.
		ctx := &cancelAtPoll{Context: inner, cancel: cancel, poll: int64(statements / 2)}
		_, _, _, err := judge(ctx, "broken")
		polls := ctx.polls.Load()
		t.Logf("the judgment polled its context %d times, and ended at poll %d with the error %v", polls, ctx.poll, err)
		if !errors.Is(err, context.Canceled) || polls != ctx.poll {
			t.Errorf("the judgment polled its context %d times and ended with the error %v, want context.Canceled at poll %d, the poll that saw the context end",
				polls, err, ctx.poll)
		}
	})
}

// A refusal of more findings than its body holds names the command that lists the first
// specFormFindingsHeld of them, and that command's read route answers those and counts them all.
// Control: the refusal of fewer findings names the command that lists them all
// (TestTheRefusalListsTwentyAndCountsTheRest).
func TestARefusalOfMoreFindingsThanItHoldsNamesWhatTheReadRouteLists(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.root, "Seventy broken statements", boundStatements(boundBroken, 70))
	response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("request: status=%d body=%.300s, want 409", response.Code, response.Body.String())
	}
	refused := decodeBody[specFormError](t, response)
	lines := strings.Split(refused.Error, "\n")
	more := fmt.Sprintf("And %d more: dispatch spec-form --issue %s --artifact spec.md --version 1 lists the first %d of them.",
		refused.FindingsTotal-20, issue.Key, specFormFindingsHeld)
	if refused.FindingsTotal <= specFormFindingsHeld || len(refused.Findings) != specFormFindingsHeld || len(lines) < 22 || lines[21] != more {
		t.Errorf("the refusal holds %d of %d findings and its 22nd line is %q, want %d of more than %d and %q",
			len(refused.Findings), refused.FindingsTotal, lines[min(21, len(lines)-1)], specFormFindingsHeld, specFormFindingsHeld, more)
	}
	read := decodeBody[specFormRead](t, dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil, "alice"))
	if len(read.Findings) != specFormFindingsHeld || read.FindingsTotal != refused.FindingsTotal {
		t.Errorf("the read route answers %d findings and findings_total %d, want %d and %d", len(read.Findings), read.FindingsTotal, specFormFindingsHeld, refused.FindingsTotal)
	}
}
