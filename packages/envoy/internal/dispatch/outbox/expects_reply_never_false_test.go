package outbox

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// No path of the Dispatch server writes expects_reply as the JSON literal false. A listener built
// before #2903 reads the field only as a string: a targeted send carrying false is answered 400,
// and a bus envelope carrying it fails that listener's decode and is acknowledged and dropped. So
// a server rolled ahead of the listeners of its deployment must say nothing about a reply rather
// than say false, and this test is what holds that for every path at once until every listener of
// a deployment is at or after #2903.
//
// It reads two things:
//
//   - every event type the outbox can build an envelope for, taken from the contract's own
//     DispatchEvent union (packages/contracts/src/dispatch-api.ts), so a type added there is
//     covered here without an edit. Each is built as an issue's event and as a document's, the
//     two owners envelope() has, marshalled as the bus marshals it, and refused if the field is
//     present as anything but "optional" or "required".
//
//   - the server's own source under internal/dispatch, refused if any of it names
//     contracts.ExpectsReplyNone, the one value that marshals to false. That covers the paths no
//     envelope of this package builds: the comment and message deliveries' HTTP body
//     (internal/dispatch/api, internal/dispatch/envoy), whatever their delivery mode.
//     The read is of each file's syntax tree, so the constant named in a comment - as
//     commentExpectsReply's and envelope()'s say what removes the workaround - is not a hit.
func TestNoDispatchServerPathWritesExpectsReplyFalse(t *testing.T) {
	t.Run("no envelope the outbox builds carries false", func(t *testing.T) {
		types := dispatchEventTypes(t)
		for _, eventType := range types {
			for _, owner := range []struct {
				name  string
				event model.Event
			}{
				{name: "on an issue", event: model.Event{ID: 1, IssueKey: new("T-1")}},
				{name: "on a document", event: model.Event{
					ID: 2, Project: "TEST", ArtifactID: new("00000000-0000-0000-0000-000000000040"),
				}},
			} {
				t.Run(eventType+" "+owner.name, func(t *testing.T) {
					event := owner.event
					event.Type = eventType
					event.CreatedAt = time.Now()
					event.Payload = map[string]any{
						"id":          "00000000-0000-0000-0000-000000000030",
						"artifact_id": "00000000-0000-0000-0000-000000000040",
						"answer":      "Approve",
						"urgency":     "high",
					}
					item, err := envelope(event, "spec")
					if err != nil {
						t.Fatalf("envelope: %v", err)
					}
					if err := item.Validate(); err != nil {
						t.Fatalf("validate envelope: %v", err)
					}
					raw, err := json.Marshal(item)
					if err != nil {
						t.Fatalf("marshal envelope: %v", err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil {
						t.Fatalf("decode envelope %s: %v", raw, err)
					}
					got, present := fields["expects_reply"]
					if !present {
						return
					}
					if word := string(got); word != `"optional"` && word != `"required"` {
						t.Fatalf("expects_reply of %s = %s, want it absent, \"optional\" or \"required\"", eventType, word)
					}
				})
			}
		}
	})

	t.Run("no source under internal/dispatch names the value that marshals to false", func(t *testing.T) {
		fileSet := token.NewFileSet()
		err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "ExpectsReplyNone" {
					return true
				}
				if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "contracts" {
					return true
				}
				t.Errorf(
					"%s names contracts.ExpectsReplyNone, which marshals to the JSON literal false "+
						"no listener built before #2903 can read; leave the field out instead",
					fileSet.Position(selector.Pos()),
				)
				return false
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk internal/dispatch: %v", err)
		}
	})
}

// dispatchEventTypes are the event types of the contract's DispatchEvent union, the one list of
// what an event's type may be. Reading the TypeScript is reading the source of truth: Go holds no
// enumeration of its own, and a type added to the union is one the outbox will be handed.
func dispatchEventTypes(t *testing.T) []string {
	t.Helper()
	const contract = "../../../../contracts/src/dispatch-api.ts"
	source, err := os.ReadFile(contract)
	if err != nil {
		t.Fatalf("read %s: %v", contract, err)
	}
	lines := strings.Split(string(source), "\n")
	start := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "export type DispatchEvent =") {
			start = index + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s declares no DispatchEvent union", contract)
	}
	pattern := regexp.MustCompile(`readonly type: "([a-z_.]+)"`)
	seen := map[string]bool{}
	var types []string
	for _, line := range lines[start:] {
		// The next top-level declaration ends the union.
		if strings.HasPrefix(line, "export ") {
			break
		}
		match := pattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		types = append(types, match[1])
	}
	// The two types this workaround is about, so a union this stopped reading fails here rather
	// than passing over an empty list.
	for _, required := range []string{"ask.answered", "artifact.approved"} {
		if !seen[required] {
			t.Fatalf("read %d event types from %s, none of them %s", len(types), contract, required)
		}
	}
	return types
}
