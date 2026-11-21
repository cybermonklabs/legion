package shimwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The workspace read crosses the shim's lane with no chunk sequence, so its largest answer — the
// most commits it lists and a long refusal — has to fit one line, and both frames have to decode
// back to what was sent: the daemon reads the work it names on the issue off the result as it
// arrives.
func TestAWorkspaceWorkReadingFitsOneLineAndDecodesWhole(t *testing.T) {
	commits := make([]string, MaxWorkspaceWorkCommits)
	for index := range commits {
		commits[index] = fmt.Sprintf("%040x", index+1)
	}
	for name, sent := range map[string]Frame{
		"the request": WorkspaceWork{ID: "read-1", Issue: "INFRA-945", TimeoutMs: 30_000},
		"a reading with work": WorkspaceWorkResult{
			ID: "read-1", OK: true, Head: strings.Repeat("a", 40), Changed: true, Commits: commits, Count: 270,
		},
		"a reading with no work": WorkspaceWorkResult{ID: "read-2", OK: true, Head: strings.Repeat("b", 40)},
		"a refusal": WorkspaceWorkResult{
			ID: "read-3", Error: "Command timed out after 30 s (ran 30.0 s): jj log\n" + strings.Repeat("Error: the working copy is stale\n", 64),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if validator, ok := sent.(interface{ Validate() error }); !ok {
				t.Fatalf("a %s frame has no Validate", sent.FrameType())
			} else if err := validator.Validate(); err != nil {
				t.Fatal(err)
			}
			line, err := json.Marshal(sent)
			if err != nil {
				t.Fatal(err)
			}
			if len(line) > MaxFrameBytes {
				t.Fatalf("the frame is %d bytes: it must fit one line", len(line))
			}
			got, err := Decode(line)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, sent) {
				t.Fatalf("the frame decodes as %#v, want the %#v sent", got, sent)
			}
		})
	}
}

// The daemon is the request's only sender and the shim the answer's, so a frame either end could
// not act on is refused by name: a request with no id, no issue key or no budget, and an answer
// that answers no request, refuses with no reason, names no working copy, lists more commits
// than the wire carries or counts fewer than it lists.
func TestAWorkspaceWorkFrameNeitherEndCanActOnIsRefused(t *testing.T) {
	head := strings.Repeat("c", 40)
	tooMany := make([]string, MaxWorkspaceWorkCommits+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("%040x", index+1)
	}
	for name, frame := range map[string]interface{ Validate() error }{
		"a request with no id":                   WorkspaceWork{Issue: "INFRA-945", TimeoutMs: 1},
		"a request with no issue key":            WorkspaceWork{ID: "x", Issue: "legion/INFRA-945\") | all()", TimeoutMs: 1},
		"a request with a lowered key":           WorkspaceWork{ID: "x", Issue: "infra-945", TimeoutMs: 1},
		"a request with no budget":               WorkspaceWork{ID: "x", Issue: "INFRA-945"},
		"a request with a negative budget":       WorkspaceWork{ID: "x", Issue: "INFRA-945", TimeoutMs: -1},
		"an answer with no id":                   WorkspaceWorkResult{OK: true, Head: head},
		"a refusal with no reason":               WorkspaceWorkResult{ID: "x"},
		"a reading with no head":                 WorkspaceWorkResult{ID: "x", OK: true},
		"a reading listing too many commits":     WorkspaceWorkResult{ID: "x", OK: true, Head: head, Commits: tooMany, Count: len(tooMany)},
		"a reading counting fewer than it lists": WorkspaceWorkResult{ID: "x", OK: true, Head: head, Commits: tooMany[:2], Count: 1},
	} {
		if err := frame.Validate(); !errors.Is(err, ErrMalformedFrame) {
			t.Errorf("%s: Validate = %v, want ErrMalformedFrame", name, err)
		}
	}
}
