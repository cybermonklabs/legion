// spec_writer_test.go holds the behavior tests of R1 of the spec-writer spec: on an issue Legion
// runs, a version of the spec or the Verification document from a writer who is no person and no
// architect of the issue, or of an issue above it, is refused with 403
// DOCUMENT_WRITER_NOT_ARCHITECT on every route that writes one (spec_writer.go). Its rig
// (specWriterServer, specWriterRig) builds a project of its own for each run, and uses only routes
// the stack's base serves, so the inventory runs there as the negative control of the checks it
// holds. The tests of R2 at its end turn a project's proven-sessions setting on, a route the base
// does not serve: an architect's write and a role-holder publication then pass only on a
// credential that proves the session (newSpecWriterBrokerServer, provenSender).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// specWriterRefusalCode is the code of the check's refusal.
const specWriterRefusalCode = "DOCUMENT_WRITER_NOT_ARCHITECT"

// TestSpecWriteRefusesAWorkerOnEveryRoute is the behavioral inventory of the routes that write a
// version of an issue's spec or Verification document (plan, "The inventory of routes"): each row
// of specWriteInventory runs once for each writer and each document its columns name, each run on
// a rig of its own (specWriterServer.rig), and each refused write is held to the check's 403
// naming the issue, the document, the writer's session and the architect sessions recorded on the
// issue's chain, and to a project that holds the same issues, document versions and issue events
// as before it (specWriterRig.requireRefused), with each row's own checks after it.
func TestSpecWriteRefusesAWorkerOnEveryRoute(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	for _, row := range specWriteInventory() {
		for _, writer := range row.writers {
			for _, document := range row.documents {
				t.Run(row.name+"/"+writer.name+"/"+document, func(t *testing.T) {
					rig := server.rig(t, row.shape)
					row.run(t, specWriteAt{rig: rig, writer: writer.session(rig), issue: writer.issue(rig), document: document})
				})
			}
		}
	}
}

// specWriteRow is one row of the inventory: the route it writes on, run for each of its writers on
// each of its documents, on a rig of shape.
type specWriteRow struct {
	name      string
	documents []string
	writers   []specWriteWriter
	shape     specWriterRigShape
	run       func(t *testing.T, at specWriteAt)
}

// specWriteWriter is who writes in a run, and on which issue of the rig.
type specWriteWriter struct {
	name    string
	session func(rig *specWriterRig) string
	issue   func(rig *specWriterRig) specWriterIssue
}

// The writers the inventory names. "Own" is C's tester writing C's document, the issue's own
// worker; "child's" is C's tester writing R's, a worker of a child of R, which holds no role on R;
// "parent's" is R's implementer writing C's, as check 5b1d7e42 runs it.
var (
	specWriterOwn = specWriteWriter{"own", func(rig *specWriterRig) string { return rig.cTester },
		func(rig *specWriterRig) specWriterIssue { return rig.c }}
	specWriterChilds = specWriteWriter{"child's", func(rig *specWriterRig) string { return rig.cTester },
		func(rig *specWriterRig) specWriterIssue { return rig.r }}
	specWriterParents = specWriteWriter{"parent's", func(rig *specWriterRig) string { return rig.rImplementer },
		func(rig *specWriterRig) specWriterIssue { return rig.c }}
	specWriterTesterOnSibling = specWriteWriter{"C's tester on D", func(rig *specWriterRig) string { return rig.cTester },
		func(rig *specWriterRig) specWriterIssue { return rig.d }}
	specWriterChildArchitect = specWriteWriter{"C's architect on R", func(rig *specWriterRig) string { return rig.cArchitect },
		func(rig *specWriterRig) specWriterIssue { return rig.r }}
)

// specWriterBoth is the two documents the check holds.
var specWriterBoth = []string{"spec.md", "verification.md"}

// specWriteInventory is the rows of the plan's inventory table, in its order.
func specWriteInventory() []specWriteRow {
	spec := []string{"spec.md"}
	sibling := specWriterRigShape{sibling: true}
	return []specWriteRow{
		{name: "upload a new version", documents: specWriterBoth, writers: []specWriteWriter{specWriterOwn, specWriterChilds, specWriterParents},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's upload of a new version of %s's %s", at.writer, at.issue.key, at.document),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.upload(t, at.writer, at.issue, at.document, documentMarkdown(t, at.rig.handler, at.artifact())+"\nA worker's paragraph.\n")
					},
					refusal: at.refusal(),
				})
			}},
		{name: "first upload", documents: []string{"verification.md"}, writers: []specWriteWriter{specWriterOwn, specWriterChilds},
			shape: specWriterRigShape{noVerification: true},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's first upload of %s's %s", at.writer, at.issue.key, at.document),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.upload(t, at.writer, at.issue, at.document, "A worker's verification.\n")
					},
					refusal: at.refusal(),
				})
			}},
		{name: "edit by id", documents: specWriterBoth, writers: []specWriteWriter{specWriterOwn, specWriterChilds, specWriterParents},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, at.rig.editAttempt(at.writer, at.issue, at.document))
			}},
		{name: "edit by slug", documents: specWriterBoth, writers: []specWriteWriter{specWriterOwn, specWriterChilds},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's edit of %s's %s by its slug", at.writer, at.issue.key, at.document),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/issues/"+at.issue.key+"/artifacts/"+at.issue.documents[at.document].slug+"/edits",
							map[string]any{"ops": specWriterAppend})
					},
					refusal: at.refusal(),
				})
			}},
		{name: "edit that changes nothing", documents: spec, writers: []specWriteWriter{specWriterOwn},
			run: func(t *testing.T, at specWriteAt) {
				first := specWriterFirstLine(at.issue.name, at.document)
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's edit of %s's %s that replaces a line with itself", at.writer, at.issue.key, at.document),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/artifacts/"+at.artifact()+"/edits",
							map[string]any{"ops": []map[string]string{{"op": "replace", "find": first, "with": first}}})
					},
					refusal: at.refusal(),
				})
			}},
		{name: "upload that changes nothing", documents: spec, writers: []specWriteWriter{specWriterOwn},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's upload of %s's %s with its own text", at.writer, at.issue.key, at.document),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.upload(t, at.writer, at.issue, at.document, documentMarkdown(t, at.rig.handler, at.artifact()))
					},
					refusal: at.refusal(),
				})
			}},
		{name: "edit with a summary", documents: spec, writers: []specWriteWriter{specWriterOwn, specWriterChilds},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's edit of %s's %s with a summary", at.writer, at.issue.key, at.document),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/artifacts/"+at.artifact()+"/edits",
							map[string]any{"ops": specWriterAppend, "summary": "A worker's named change"})
					},
					refusal: at.refusal(),
				})
			}},
		{name: "named version by id", documents: specWriterBoth, writers: []specWriteWriter{specWriterOwn, specWriterChilds},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, at.rig.namedVersionAttempt(at.writer, at.issue, at.document, "/api/v1/artifacts/"+at.artifact()+"/versions"))
			}},
		{name: "named version by slug", documents: specWriterBoth, writers: []specWriteWriter{specWriterOwn, specWriterChilds},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, at.rig.namedVersionAttempt(at.writer, at.issue, at.document,
					"/api/v1/issues/"+at.issue.key+"/artifacts/"+at.issue.documents[at.document].slug+"/versions"))
			}},
		{name: "named version that changes nothing", documents: spec, writers: []specWriteWriter{specWriterOwn},
			run: func(t *testing.T, at specWriteAt) {
				// A person names a version first, so the worker's is sent right after the last
				// version with no edit between.
				named := dispatchRequest(t, at.rig.handler, http.MethodPost, "/api/v1/artifacts/"+at.artifact()+"/versions",
					map[string]any{"summary": "A person's named version"}, "alice")
				if named.Code != http.StatusCreated {
					t.Fatalf("rig: a person names a version of %s's %s: status=%d body=%s", at.issue.key, at.document, named.Code, named.Body.String())
				}
				at.rig.quiet(t)
				at.rig.requireRefused(t, at.rig.namedVersionAttempt(at.writer, at.issue, at.document, "/api/v1/artifacts/"+at.artifact()+"/versions"))
			}},
		{name: "resolution", documents: specWriterBoth, writers: []specWriteWriter{specWriterOwn, specWriterChilds},
			run: func(t *testing.T, at specWriteAt) { at.requireAskCloseRefused(t, "resolved") }},
		{name: "retraction", documents: specWriterBoth, writers: []specWriteWriter{specWriterOwn, specWriterChilds},
			run: func(t *testing.T, at specWriteAt) { at.requireAskCloseRefused(t, "retracted") }},
		{name: "ask edit", documents: specWriterBoth, writers: []specWriteWriter{specWriterOwn, specWriterChilds},
			run: func(t *testing.T, at specWriteAt) {
				ask := at.rig.askIn(t, at.issue, at.document, "tester")
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's edit of its own block ask %s in %s's %s", at.writer, ask, at.issue.key, at.document),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/asks/"+ask, map[string]any{"question": "Which way does the worker go now?"})
					},
					refusal: at.refusal(),
				})
			}},
		{name: "create under a Legion-run parent", documents: spec, writers: []specWriteWriter{
			{"own: C's tester under C", specWriterOwn.session, specWriterOwn.issue},
			{"child's: C's tester under R", specWriterChilds.session, specWriterChilds.issue},
		}, run: func(t *testing.T, at specWriteAt) {
			title := "A worker's issue under " + at.issue.key
			at.rig.requireRefused(t, specWriteAttempt{
				what: fmt.Sprintf("%s's create of an issue under %s", at.writer, at.issue.key),
				send: func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/issues", map[string]any{
						"project": at.rig.project, "title": title, "parent": at.issue.key, "force": true,
					})
				},
				refusal: at.refusal(),
			})
			if keys := at.rig.issuesTitled(t, title); len(keys) > 0 {
				t.Errorf("the refused create left the issues %v titled %q, want none", keys, title)
			}
		}},
		{name: "move under a Legion-run parent", documents: spec, writers: []specWriteWriter{specWriterOwn},
			run: func(t *testing.T, at specWriteAt) {
				created := at.rig.send(t, at.writer, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": at.rig.project, "title": "A worker's root", "labels": []string{"legion"}, "force": true,
				})
				if created.Code != http.StatusCreated {
					t.Fatalf("rig: %s creates a root with the label legion: status=%d body=%s", at.writer, created.Code, created.Body.String())
				}
				moved := decodeBody[struct {
					Key string `json:"key"`
				}](t, created).Key
				at.rig.quiet(t)
				refusal := at.refusal()
				refusal.issues = []string{moved, at.issue.key}
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's move of its root %s under %s", at.writer, moved, at.issue.key),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/issues/"+moved, map[string]any{"parent": at.issue.key})
					},
					refusal: refusal,
				})
				if parent := at.rig.issueRow(t, moved).parent; parent != nil {
					t.Errorf("the refused move left %s under %s, want it still a root", moved, *parent)
				}
			}},
		{name: "close", documents: spec, writers: []specWriteWriter{
			{"own: C's tester closes C", specWriterOwn.session, specWriterOwn.issue},
			{"child's: C's tester closes R", specWriterChilds.session, specWriterChilds.issue},
		}, run: func(t *testing.T, at specWriteAt) {
			at.requireCloseRefused(t, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/issues/"+at.issue.key, map[string]any{"status": "done"})
			})
		}},
		{name: "reopen", documents: spec, writers: []specWriteWriter{
			{"own: C's tester reopens C", specWriterOwn.session, specWriterOwn.issue},
			{"child's: C's tester reopens R", specWriterChilds.session, specWriterChilds.issue},
		}, run: func(t *testing.T, at specWriteAt) {
			at.requireReopenRefused(t, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/issues/"+at.issue.key, map[string]any{"status": "in_progress"})
			})
		}},
		{name: "label removal", documents: spec, writers: []specWriteWriter{{"child's: C's tester on R", specWriterChilds.session, specWriterChilds.issue}},
			shape: sibling,
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's removal of the label legion from %s", at.writer, at.issue.key),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/issues/"+at.issue.key, map[string]any{"labels": []string{}})
					},
					refusal: at.refusal(),
				}, at.rig.editAttempt(at.writer, at.rig.d, "spec.md"))
				if labels := at.rig.issueRow(t, at.issue.key).labels; !slices.Contains(labels, "legion") {
					t.Errorf("the refused label removal left %s with the labels %v, want legion among them", at.issue.key, labels)
				}
			}},
		{name: "parent clear", documents: spec, writers: []specWriteWriter{specWriterTesterOnSibling}, shape: sibling,
			run: func(t *testing.T, at specWriteAt) { at.requireParentWriteRefused(t, nil) }},
		{name: "parent clear to an unlabelled issue", documents: spec, writers: []specWriteWriter{specWriterTesterOnSibling},
			shape: specWriterRigShape{sibling: true, unlabelled: true},
			run:   func(t *testing.T, at specWriteAt) { at.requireParentWriteRefused(t, &at.rig.u2.key) }},
		{name: "status-change close", documents: spec, writers: []specWriteWriter{specWriterOwn},
			run: func(t *testing.T, at specWriteAt) {
				at.requireCloseRefused(t, func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/issues/"+at.issue.key+"/status-change", map[string]any{"status": "done"})
				})
			}},
		{name: "status-change reopen", documents: spec, writers: []specWriteWriter{specWriterOwn},
			run: func(t *testing.T, at specWriteAt) {
				at.requireReopenRefused(t, func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/issues/"+at.issue.key+"/status-change", map[string]any{"status": "in_progress"})
				})
			}},
		{name: "status-change reopen with include_children", documents: spec, writers: []specWriteWriter{specWriterOwn},
			run: func(t *testing.T, at specWriteAt) {
				at.requireReopenRefused(t, func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/issues/"+at.issue.key+"/status-change",
						map[string]any{"status": "in_progress", "include_children": true})
				})
			}},
		{name: "status-change unlink", documents: spec, writers: []specWriteWriter{{"C's tester on R", specWriterChilds.session, specWriterChilds.issue}},
			shape: sibling,
			run: func(t *testing.T, at specWriteAt) {
				// The unlink takes C and D out of R's tree, each a step of the call: the refusal
				// names the step it refuses.
				refusal := at.refusal()
				refusal.issues = []string{at.rig.c.key, at.rig.d.key}
				at.rig.requireRefused(t, specWriteAttempt{
					what: fmt.Sprintf("%s's status change of %s with unlink_children", at.writer, at.issue.key),
					send: func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/issues/"+at.issue.key+"/status-change",
							map[string]any{"status": "in_progress", "unlink_children": true})
					},
					refusal: refusal,
				}, at.rig.editAttempt(at.writer, at.rig.d, "spec.md"))
				for _, child := range []string{at.rig.c.key, at.rig.d.key} {
					if parent := at.rig.issueRow(t, child).parent; parent == nil || *parent != at.issue.key {
						t.Errorf("the refused unlink left %s under %s, want it still under %s", child, specWriterParentName(parent), at.issue.key)
					}
				}
			}},
		{name: "child's architect on the parent", documents: spec, writers: []specWriteWriter{specWriterChildArchitect},
			run: func(t *testing.T, at specWriteAt) {
				at.rig.requireRefused(t, at.rig.editAttempt(at.writer, at.issue, at.document))
			}},
	}
}

// specWriteAt is one run of a row: its rig, the writer's session, the issue whose document it
// writes, and the document's name.
type specWriteAt struct {
	rig      *specWriterRig
	writer   string
	issue    specWriterIssue
	document string
}

// artifact is the id of the run's document.
func (at specWriteAt) artifact() string {
	return at.issue.documents[at.document].id
}

// refusal is what the check's refusal of the run's write names.
func (at specWriteAt) refusal() specWriteRefusal {
	return at.rig.refusalOn(at.issue, at.document, at.writer)
}

// requireAskCloseRefused holds the resolution route (kind resolved or retracted) of the
// architect's block ask in the run's document to the check, and the ask to still being open.
func (at specWriteAt) requireAskCloseRefused(t *testing.T, kind string) {
	t.Helper()
	ask := at.rig.askIn(t, at.issue, at.document, "architect")
	at.rig.requireRefused(t, specWriteAttempt{
		what: fmt.Sprintf("%s's resolution (%s) of the architect's block ask %s in %s's %s", at.writer, kind, ask, at.issue.key, at.document),
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/asks/"+ask+"/resolve", map[string]any{"kind": kind, "reason": "A worker settles it."})
		},
		refusal: at.refusal(),
	})
	if state := at.rig.askState(t, ask); state != "open" {
		t.Errorf("the refused resolution left the ask %s %s, want it open", ask, state)
	}
}

// requireCloseRefused holds a close of the run's issue, while its spec holds the architect's open
// block ask, to the check: the issue is still open and no ask of it was withdrawn.
func (at specWriteAt) requireCloseRefused(t *testing.T, send func(t *testing.T) *httptest.ResponseRecorder) {
	t.Helper()
	if state := at.rig.askState(t, at.rig.askIn(t, at.issue, "spec.md", "architect")); state != "open" {
		t.Fatalf("rig: the architect's block ask in %s's spec is %s, want it open before the close", at.issue.key, state)
	}
	status := at.rig.issueRow(t, at.issue.key).status
	withdrawn := at.rig.eventCount(t, at.issue.key, "ask.withdrawn")
	at.rig.requireRefused(t, specWriteAttempt{
		what:    fmt.Sprintf("%s's close of %s", at.writer, at.issue.key),
		send:    send,
		refusal: at.refusal(),
	})
	if after := at.rig.issueRow(t, at.issue.key).status; after != status {
		t.Errorf("the refused close left %s %s, want it still %s", at.issue.key, after, status)
	}
	if after := at.rig.eventCount(t, at.issue.key, "ask.withdrawn"); after != withdrawn {
		t.Errorf("the refused close appended %d ask.withdrawn events on %s, want none", after-withdrawn, at.issue.key)
	}
}

// requireReopenRefused holds a reopen of the run's issue, after the issue's architect closed it, to
// the check: the issue is still closed and no ask event was appended.
func (at specWriteAt) requireReopenRefused(t *testing.T, send func(t *testing.T) *httptest.ResponseRecorder) {
	t.Helper()
	architect := at.rig.architectsOf(at.issue)[0]
	closed := at.rig.send(t, architect, http.MethodPatch, "/api/v1/issues/"+at.issue.key, map[string]any{"status": "done"})
	if closed.Code != http.StatusOK {
		t.Fatalf("rig: the architect %s closes %s: status=%d body=%s", architect, at.issue.key, closed.Code, closed.Body.String())
	}
	at.rig.quiet(t)
	asks := at.rig.eventCount(t, at.issue.key, "ask.%")
	at.rig.requireRefused(t, specWriteAttempt{
		what:    fmt.Sprintf("%s's reopen of %s", at.writer, at.issue.key),
		send:    send,
		refusal: at.refusal(),
	})
	if status := at.rig.issueRow(t, at.issue.key).status; status != "done" {
		t.Errorf("the refused reopen left %s %s, want it still done", at.issue.key, status)
	}
	if after := at.rig.eventCount(t, at.issue.key, "ask.%"); after != asks {
		t.Errorf("the refused reopen appended %d ask events on %s, want none", after-asks, at.issue.key)
	}
}

// requireParentWriteRefused holds a write of the run's issue's parent to parent (nil for none, or
// an issue no labelled issue is above), which takes the issue out of Legion, to the check, then the
// writer's edit of the issue's spec after it too, and the issue to still being under the parent it
// had.
func (at specWriteAt) requireParentWriteRefused(t *testing.T, parent *string) {
	t.Helper()
	before := at.rig.issueRow(t, at.issue.key).parent
	if before == nil {
		t.Fatalf("rig: %s has no parent before the write", at.issue.key)
	}
	at.rig.requireRefused(t, specWriteAttempt{
		what: fmt.Sprintf("%s's write of %s's parent to %s", at.writer, at.issue.key, specWriterParentName(parent)),
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/issues/"+at.issue.key, map[string]any{"parent": parent})
		},
		refusal: at.refusal(),
	}, at.rig.editAttempt(at.writer, at.issue, "spec.md"))
	if after := at.rig.issueRow(t, at.issue.key).parent; after == nil || *after != *before {
		t.Errorf("the refused parent write left %s under %s, want it still under %s", at.issue.key, specWriterParentName(after), *before)
	}
}

// specWriterParentName is a stored parent as a failure line names it.
func specWriterParentName(parent *string) string {
	if parent == nil {
		return "no parent"
	}
	return *parent
}

// specWriterServer is the server a spec-writer test shares between its runs. Each run builds its
// rig in a project of its own (rig), so no run's write changes another's.
type specWriterServer struct {
	handler  http.Handler
	database *store.Store
	served   *server
	// projects is how many rigs the server has built; it numbers the next rig's project.
	projects int
}

func newSpecWriterServer(t *testing.T, options testServerOptions) *specWriterServer {
	t.Helper()
	handler, database, _, served := newRegisteredTestServer(t, options)
	return &specWriterServer{handler: handler, database: database, served: served}
}

// specWriterRigShape is what a rig holds beside R and C.
type specWriterRigShape struct {
	// noVerification leaves R and C without a Verification document, for a first upload of one.
	noVerification bool
	// sibling adds D, a child of R with no label and no role holder of its own: Legion runs it
	// only through R.
	sibling bool
	// unlabelled adds U2, a root of the project with no label.
	unlabelled bool
	// grandchild adds G, a child of C with no label and no role holder of its own.
	grandchild bool
	// unclaimed adds V, a root of P with the label legion that no publication names.
	unclaimed bool
	// unmarked adds Q, a second project that no publication marked, holding W, a root with the
	// label legion, and X, a root with no label.
	unmarked bool
	// offLegion files R with no label, so Legion runs no issue of R's tree.
	offLegion bool
}

// specWriterIssue is one issue of a rig: the name its texts and block ids are written after
// (root, child, sibling, unlabelled, grandchild, unclaimed, unmarked, other), its key, and its
// documents by name (spec.md, verification.md).
type specWriterIssue struct {
	name      string
	key       string
	documents map[string]specWriterDocument
}

// specWriterDocument is one document of a rig's issue.
type specWriterDocument struct {
	id, slug string
}

// specWriterRig is one project P of its own on a specWriterServer: a root R with the label legion
// (none in the shape offLegion) and a child C under it, each with a spec.md and, unless the shape
// says otherwise, a verification.md that a person wrote (dispatchRequest as alice), each document
// holding a block ask the person wrote - the architect's - and a block ask of C's tester's own,
// written by an edit under its session before the first role-holder publication; and then the
// role holders the daemon of P published on the shared token: R's architect and implementer, C's
// architect and tester. Each role holder is a session whose id names its role and the project,
// which a write on the shared token names as its actor.
type specWriterRig struct {
	*specWriterServer
	project                                       string
	r, c, d, u2, g, v                             specWriterIssue
	rArchitect, rImplementer, cArchitect, cTester string
	// q is the key of the unmarked project Q, and w and x are its issues (specWriterRigShape).
	q    string
	w, x specWriterIssue
	// asks are the ids of the block asks the rig wrote, by block id (specWriterBlockID).
	asks map[string]string
}

// rig builds a rig of shape in a project of its own, and waits until nothing it wrote is still
// settling, so the run's first snapshot reads only what the run writes.
func (s *specWriterServer) rig(t *testing.T, shape specWriterRigShape) *specWriterRig {
	t.Helper()
	rig := s.tree(t, shape)
	asked := map[*specWriterIssue][]string{}
	for _, issue := range []*specWriterIssue{&rig.r, &rig.c} {
		asked[issue] = append(asked[issue], specWriterBlockID(issue.name, "spec.md", "architect"))
		if !shape.noVerification {
			uploaded := uploadDocument(t, s.handler, "/api/v1/issues/"+issue.key, "verification.md", specWriterText(issue.name, "verification.md"))
			issue.documents["verification.md"] = specWriterDocument{id: uploaded.Artifact.ID, slug: uploaded.Artifact.Slug}
			asked[issue] = append(asked[issue], specWriterBlockID(issue.name, "verification.md", "architect"))
		}
	}
	// C's tester's own block asks, written while the project is not marked yet, so the ask-edit
	// rows have an ask of the writer's own to edit.
	for _, issue := range []*specWriterIssue{&rig.r, &rig.c} {
		for _, document := range slices.Sorted(maps.Keys(issue.documents)) {
			block := specWriterBlockID(issue.name, document, "tester")
			written := rig.send(t, rig.cTester, http.MethodPost, "/api/v1/artifacts/"+issue.documents[document].id+"/edits", map[string]any{
				"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": specWriterAsk(block)}},
			})
			if written.Code != http.StatusOK {
				t.Fatalf("rig: C's tester writes its block ask into %s's %s: status=%d body=%s", issue.key, document, written.Code, written.Body.String())
			}
			asked[issue] = append(asked[issue], block)
		}
	}
	for _, issue := range []*specWriterIssue{&rig.r, &rig.c} {
		rig.awaitAsks(t, *issue, asked[issue])
	}
	rig.publishRoles(t)
	rig.quiet(t)
	return rig
}

// tree files the issues of a rig of shape in a project of its own, each by a person with its spec,
// and writes nothing else: no Verification document, no block ask of a worker's and no
// publication.
func (s *specWriterServer) tree(t *testing.T, shape specWriterRigShape) *specWriterRig {
	t.Helper()
	s.projects++
	project := fmt.Sprintf("SW%d", s.projects)
	createAdviceProject(t, s.handler, project)
	session := func(role string) string { return "ses-" + role + "-" + strings.ToLower(project) }
	rig := &specWriterRig{
		specWriterServer: s, project: project, asks: map[string]string{},
		rArchitect: session("r-architect"), rImplementer: session("r-implementer"),
		cArchitect: session("c-architect"), cTester: session("c-tester"),
	}
	var rootLabels []string
	if !shape.offLegion {
		rootLabels = []string{"legion"}
	}
	rig.r = rig.file(t, "root", "", rootLabels)
	rig.c = rig.file(t, "child", rig.r.key, nil)
	if shape.sibling {
		rig.d = rig.file(t, "sibling", rig.r.key, nil)
	}
	if shape.unlabelled {
		rig.u2 = rig.file(t, "unlabelled", "", nil)
	}
	if shape.grandchild {
		rig.g = rig.file(t, "grandchild", rig.c.key, nil)
	}
	if shape.unclaimed {
		rig.v = rig.file(t, "unclaimed", "", []string{"legion"})
	}
	if shape.unmarked {
		rig.q = project + "Q"
		createAdviceProject(t, s.handler, rig.q)
		rig.w = rig.fileIn(t, rig.q, "unmarked", "", []string{"legion"})
		rig.x = rig.fileIn(t, rig.q, "other", "", nil)
	}
	return rig
}

// publishRoles publishes the rig's role holders as the daemon of P does: R's architect and
// implementer on R, and C's architect and tester on C.
func (rig *specWriterRig) publishRoles(t *testing.T) {
	t.Helper()
	rig.publish(t, rig.r.key, []issueRoleHolder{{Role: "architect", Session: rig.rArchitect}, {Role: "implementer", Session: rig.rImplementer}})
	rig.publish(t, rig.c.key, []issueRoleHolder{{Role: "architect", Session: rig.cArchitect}, {Role: "tester", Session: rig.cTester}})
}

// file creates an issue of the rig's project P as a person (fileIn).
func (rig *specWriterRig) file(t *testing.T, name, parent string, labels []string) specWriterIssue {
	t.Helper()
	return rig.fileIn(t, rig.project, name, parent, labels)
}

// fileIn creates an issue of project as a person, under parent when it is not empty, with labels,
// and with a spec of specWriterText's: R's and C's hold the architect's block ask.
func (rig *specWriterRig) fileIn(t *testing.T, project, name, parent string, labels []string) specWriterIssue {
	t.Helper()
	body := map[string]any{"project": project, "title": "The " + name + " issue", "spec": specWriterText(name, "spec.md"), "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	if labels != nil {
		body["labels"] = labels
	}
	created := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("rig: a person creates the %s issue: status=%d body=%s", name, created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	var slug string
	if err := rig.database.Pool.QueryRow(context.Background(), `select slug from artifacts where id = $1`, issue.PrimaryArtifactID).Scan(&slug); err != nil {
		t.Fatalf("rig: read the slug of %s's spec: %v", issue.Key, err)
	}
	return specWriterIssue{name: name, key: issue.Key, documents: map[string]specWriterDocument{
		"spec.md": {id: issue.PrimaryArtifactID, slug: slug},
	}}
}

// specWriterText is the text a person writes into an issue's document: its first line
// (specWriterFirstLine), and, in R's and C's, the architect's block ask.
func specWriterText(issue, document string) string {
	text := specWriterFirstLine(issue, document) + "\n"
	if issue == "root" || issue == "child" {
		text += "\n" + specWriterAsk(specWriterBlockID(issue, document, "architect"))
	}
	return text
}

// specWriterFirstLine is the first line of an issue's document, which no other line repeats.
func specWriterFirstLine(issue, document string) string {
	return "The " + strings.TrimSuffix(document, ".md") + " of the " + issue + " issue."
}

// specWriterBlockID is the id of the block ask asker wrote (architect, tester) in an issue's
// document, unique on the issue so no ask is read as another's copy.
func specWriterBlockID(issue, document, asker string) string {
	return issue + "-" + strings.TrimSuffix(document, ".md") + "-" + asker
}

// specWriterAsk is a block ask with its own question.
func specWriterAsk(block string) string {
	return ":::ask{#" + block + " urgency=\"med\" multiple=\"false\"}\nWhich way does " + block + " go?\n\n" +
		"- One: The first way.\n- Two: The second way.\n:::\n"
}

// specWriterAppend is the edit a worker sends: a paragraph of its own at the end.
var specWriterAppend = []map[string]string{{"op": "insert", "after": "end", "markdown": "A worker's paragraph.\n"}}

// awaitAsks waits until settlement has indexed each of blocks on issue as an open ask, keeps each
// ask's id, and fails the rig when a tester's block ask is not credited to the tester, which the
// ask edit's NOT_AUTHOR would then refuse first.
func (rig *specWriterRig) awaitAsks(t *testing.T, issue specWriterIssue, blocks []string) {
	t.Helper()
	var asks []model.Ask
	indexed := func() bool {
		listed := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+issue.key+"/asks?state=all", nil, "alice")
		if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &asks) != nil {
			return false
		}
		for _, block := range blocks {
			if !slices.ContainsFunc(asks, func(ask model.Ask) bool { return ask.BlockID != nil && *ask.BlockID == block && ask.State == "open" }) {
				return false
			}
		}
		return true
	}
	testwait.Eventually(t, "the block asks "+strings.Join(blocks, ", ")+" of "+issue.key+" to be indexed open", indexed)
	for _, ask := range asks {
		if ask.BlockID == nil || !slices.Contains(blocks, *ask.BlockID) {
			continue
		}
		if strings.HasSuffix(*ask.BlockID, "-tester") && ask.Author.ID != rig.cTester {
			t.Fatalf("rig: the block ask %s of %s is credited to %#v, want C's tester %s", *ask.BlockID, issue.key, ask.Author, rig.cTester)
		}
		rig.asks[*ask.BlockID] = ask.ID
	}
}

// askIn is the id of the block ask asker wrote in issue's document.
func (rig *specWriterRig) askIn(t *testing.T, issue specWriterIssue, document, asker string) string {
	t.Helper()
	block := specWriterBlockID(issue.name, document, asker)
	id, written := rig.asks[block]
	if !written {
		t.Fatalf("rig: no block ask %s was written", block)
	}
	return id
}

// publish publishes issue's whole set of role holders as the daemon of the issue's project does.
func (rig *specWriterRig) publish(t *testing.T, issue string, holders []issueRoleHolder) {
	t.Helper()
	project, _, _ := strings.Cut(issue, "-")
	response := putRoleHoldersAs(t, rig.handler, issue, map[string]any{"holders": holders, "actor": daemonSessionOf(project)})
	if response.Code != http.StatusOK {
		t.Fatalf("rig: the daemon of %s publishes %v on %s: status=%d body=%s", project, holders, issue, response.Code, response.Body.String())
	}
}

// quiet waits until no document owes a settlement and no write the server left running past its
// answer is still running.
func (rig *specWriterRig) quiet(t *testing.T) {
	t.Helper()
	awaitNoSettlementOwed(t, rig.database)
	awaitBackgroundWrites(t, rig.served)
}

// send sends body on the shared token as session, which the body names as its actor.
func (rig *specWriterRig) send(t *testing.T, session, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	named := maps.Clone(body)
	if named == nil {
		named = map[string]any{}
	}
	named["actor"] = map[string]any{"kind": "session", "id": session}
	return sessionRequest(t, rig.handler, method, path, named)
}

// upload uploads content as issue's document name as session, naming the token of the version it
// replaces where the issue holds the document already.
func (rig *specWriterRig) upload(t *testing.T, session string, issue specWriterIssue, name, content string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"name": name, "content": content}
	if document, held := issue.documents[name]; held {
		body["precondition"] = uploadBase(t, rig.handler, document.id)
	}
	return rig.send(t, session, http.MethodPost, "/api/v1/issues/"+issue.key+"/artifacts", body)
}

// editAttempt is writer's edit of issue's document by id (specWriterAppend), sent as sendAs sends
// it, which the check refuses a session naming the issue, the document, the session and the
// issue's architects.
func (rig *specWriterRig) editAttempt(writer string, issue specWriterIssue, document string) specWriteAttempt {
	return specWriteAttempt{
		what: fmt.Sprintf("%s's edit of %s's %s", writer, issue.key, document),
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, writer, http.MethodPost, "/api/v1/artifacts/"+issue.documents[document].id+"/edits", map[string]any{"ops": specWriterAppend})
		},
		refusal: rig.refusalOn(issue, document, writer),
	}
}

// namedVersionAttempt is session's named version of issue's document on path.
func (rig *specWriterRig) namedVersionAttempt(session string, issue specWriterIssue, document, path string) specWriteAttempt {
	return specWriteAttempt{
		what: fmt.Sprintf("%s's named version of %s's %s (%s)", session, issue.key, document, path),
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, session, http.MethodPost, path, map[string]any{"summary": "A worker's named version"})
		},
		refusal: rig.refusalOn(issue, document, session),
	}
}

// architectsOf is the architect sessions the rig records on issue's chain, nearest first: C's
// architect and R's on C and on G, R's alone on R and on D, which has none of its own, and none on
// an issue of no rig's tree (V, W).
func (rig *specWriterRig) architectsOf(issue specWriterIssue) []string {
	switch issue.key {
	case rig.c.key, rig.g.key:
		return []string{rig.cArchitect, rig.rArchitect}
	case rig.r.key, rig.d.key:
		return []string{rig.rArchitect}
	}
	return nil
}

// refusalOn is what the check's refusal of session's write of issue's document names.
func (rig *specWriterRig) refusalOn(issue specWriterIssue, document, session string) specWriteRefusal {
	return specWriteRefusal{issues: []string{issue.key}, document: document, writer: session, architects: rig.architectsOf(issue)}
}

// specWriteRefusal is what the check's refusal of one write names: one of issues (the issue the
// write is on), the document, the writer's session, and each architect session recorded on the
// issue's chain; says are phrases its message holds, and saysNot phrases it does not hold.
type specWriteRefusal struct {
	issues        []string
	document      string
	writer        string
	architects    []string
	says, saysNot []string
}

// specWriterAlsoAbove is the clause of the refusal of a move, on a chain the move leaves, that says
// the writer must also be an architect at or above the new parent to. The refusal holds it only
// when the move is checked on to's chain too, because Legion runs the moved issue as the move
// leaves it.
func specWriterAlsoAbove(to string) string {
	return "that is also an architect of " + to + " or of an issue above it"
}

// specWriteAttempt is one write of a run, and what its refusal names.
type specWriteAttempt struct {
	what    string
	send    func(t *testing.T) *httptest.ResponseRecorder
	refusal specWriteRefusal
}

// requireRefused sends each attempt in order, then fails t unless each was refused with 403
// DOCUMENT_WRITER_NOT_ARCHITECT naming what its refusal names, and the project holds the issues,
// the document versions and the issue events it held before the first.
func (rig *specWriterRig) requireRefused(t *testing.T, attempts ...specWriteAttempt) {
	t.Helper()
	before := rig.snapshot(t)
	responses := make([]*httptest.ResponseRecorder, len(attempts))
	for index, attempt := range attempts {
		responses[index] = attempt.send(t)
	}
	rig.quiet(t)
	for index, attempt := range attempts {
		requireSpecWriteRefused(t, attempt.what, responses[index], attempt.refusal)
	}
	rig.requireUnchanged(t, attempts[0].what, before)
}

// requireSpecWriteRefused fails t unless response is the check's refusal naming want.
func requireSpecWriteRefused(t *testing.T, what string, response *httptest.ResponseRecorder, want specWriteRefusal) {
	t.Helper()
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusForbidden || refusal.Code != specWriterRefusalCode {
		t.Errorf("%s: status=%d body=%s, want 403 %s", what, response.Code, clippedBody(response), specWriterRefusalCode)
		return
	}
	if !slices.ContainsFunc(want.issues, func(key string) bool { return specWriterNames(refusal.Error, key) }) {
		t.Errorf("%s: the refusal %q names none of %v, the issue it writes", what, refusal.Error, want.issues)
	}
	for _, name := range append([]string{want.document, want.writer}, want.architects...) {
		if !specWriterNames(refusal.Error, name) {
			t.Errorf("%s: the refusal %q does not name %s", what, refusal.Error, name)
		}
	}
	for _, phrase := range want.says {
		if !strings.Contains(refusal.Error, phrase) {
			t.Errorf("%s: the refusal %q does not say %q", what, refusal.Error, phrase)
		}
	}
	for _, phrase := range want.saysNot {
		if strings.Contains(refusal.Error, phrase) {
			t.Errorf("%s: the refusal %q says %q, want it not to", what, refusal.Error, phrase)
		}
	}
}

// specWriterNames reports whether message names word as a word of its own, so SW1-1 is not read
// in SW1-12 nor a session in a longer one.
func specWriterNames(message, word string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(word) + `($|[^A-Za-z0-9_-])`).MatchString(message)
}

// clippedBody is a response's body cut to a length a failure line can carry.
func clippedBody(response *httptest.ResponseRecorder) string {
	body := response.Body.String()
	if len(body) > 300 {
		return body[:300] + "..."
	}
	return body
}

// specWriterSnapshot is what a refused write leaves as it found it in the rig's project: its
// issues, each issue's events, and each document's versions, by "<issue>/<name>".
type specWriterSnapshot struct {
	issues   []string
	events   map[string]int
	versions map[string]int
}

func (rig *specWriterRig) snapshot(t *testing.T) specWriterSnapshot {
	t.Helper()
	ctx := context.Background()
	snapshot := specWriterSnapshot{events: map[string]int{}, versions: map[string]int{}}
	read := func(query string, each func(key string, count int)) {
		rows, err := rig.database.Pool.Query(ctx, query, rig.project)
		if err != nil {
			t.Fatalf("rig: read the project %s: %v", rig.project, err)
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var count int
			if err := rows.Scan(&key, &count); err != nil {
				t.Fatalf("rig: read the project %s: %v", rig.project, err)
			}
			each(key, count)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rig: read the project %s: %v", rig.project, err)
		}
	}
	read(`
		select i.key, (select count(*) from events e where e.issue_key = i.key)
		from issues i where i.project_key = $1 order by i.key
	`, func(key string, count int) {
		snapshot.issues = append(snapshot.issues, key)
		snapshot.events[key] = count
	})
	read(`
		select a.issue_key || '/' || a.name, (select count(*) from artifact_versions v where v.artifact_id = a.id)
		from artifacts a join issues i on i.key = a.issue_key where i.project_key = $1
	`, func(key string, count int) { snapshot.versions[key] = count })
	return snapshot
}

// requireUnchanged fails t for each issue, document, version count and event count of the rig's
// project that differs from before.
func (rig *specWriterRig) requireUnchanged(t *testing.T, what string, before specWriterSnapshot) {
	t.Helper()
	after := rig.snapshot(t)
	if !slices.Equal(after.issues, before.issues) {
		t.Errorf("%s: the project holds the issues %v, want the %v it held before", what, after.issues, before.issues)
	}
	for _, counts := range []struct {
		of            string
		before, after map[string]int
	}{{"events", before.events, after.events}, {"versions", before.versions, after.versions}} {
		keys := maps.Clone(counts.before)
		maps.Copy(keys, counts.after)
		for _, key := range slices.Sorted(maps.Keys(keys)) {
			was, wasHeld := counts.before[key]
			is, isHeld := counts.after[key]
			switch {
			case !wasHeld:
				t.Errorf("%s: %s is new, holding %d %s", what, key, is, counts.of)
			case !isHeld:
				t.Errorf("%s: %s is gone, which held %d %s", what, key, was, counts.of)
			case was != is:
				t.Errorf("%s: %s holds %d %s, want the %d it held before", what, key, is, counts.of, was)
			}
		}
	}
}

// specWriterIssueRow is an issue's row as it is stored.
type specWriterIssueRow struct {
	status string
	parent *string
	labels []string
}

func (rig *specWriterRig) issueRow(t *testing.T, key string) specWriterIssueRow {
	t.Helper()
	var row specWriterIssueRow
	if err := rig.database.Pool.QueryRow(context.Background(), `select status, parent_key, labels from issues where key = $1`, key).
		Scan(&row.status, &row.parent, &row.labels); err != nil {
		t.Fatalf("rig: read the issue %s: %v", key, err)
	}
	return row
}

// issuesTitled is the keys of the rig's project's issues titled title.
func (rig *specWriterRig) issuesTitled(t *testing.T, title string) []string {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `select key from issues where project_key = $1 and title = $2`, rig.project, title)
	if err != nil {
		t.Fatalf("rig: read the issues titled %q: %v", title, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("rig: read the issues titled %q: %v", title, err)
	}
	return keys
}

// askState is an ask's state as it is stored.
func (rig *specWriterRig) askState(t *testing.T, id string) string {
	t.Helper()
	var state string
	if err := rig.database.Pool.QueryRow(context.Background(), `select state from asks where id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("rig: read the ask %s: %v", id, err)
	}
	return state
}

// eventCount is how many events of issue have a type like pattern (a SQL like pattern).
func (rig *specWriterRig) eventCount(t *testing.T, issue, pattern string) int {
	t.Helper()
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from events where issue_key = $1 and type like $2`, issue, pattern).
		Scan(&count); err != nil {
		t.Fatalf("rig: count the %s events of %s: %v", pattern, issue, err)
	}
	return count
}

// TestSpecWriteKeepsThePersonScopeRefusals: the five routes that write a version of a document and
// already need the person scope at the route gate - the accept and the reject of a suggestion, the
// answer and the decline of an ask, the rebuild - answer C's tester 403 OAUTH_SCOPE_REQUIRED, as
// they did before the check, with no new version, the suggestion still open and the ask still
// open; a person's accept and answer on the same spec are taken. C's tester's close of R with
// include_children on the status route keeps #4631's 403 BROWSER_SIGN_IN_REQUIRED, and R and its
// child stay open with no new version and no event.
func TestSpecWriteKeepsThePersonScopeRefusals(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{})
	spec := rig.c.documents["spec.md"].id
	suggestion := decodeBody[model.Comment](t, rig.requireTaken(t, "C's tester's suggestion on C's spec", http.StatusCreated,
		func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.cTester, http.MethodPost, "/api/v1/issues/"+rig.c.key+"/comments", map[string]any{
				"body": "Name the child.", "anchor": specWriterAnchor(spec, "child issue"),
				"suggestion": map[string]string{"replace_with": "child issue, C"},
			})
		})).ID
	ask := rig.askIn(t, rig.c, "spec.md", "architect")
	for _, route := range []struct {
		what, path string
		body       map[string]any
	}{
		{"accept of its suggestion", "/api/v1/comments/" + suggestion + "/accept", nil},
		{"reject of its suggestion", "/api/v1/comments/" + suggestion + "/reject", nil},
		{"answer of the architect's block ask", "/api/v1/asks/" + ask + "/answer", map[string]any{"selected": []string{"One"}, "expected_edited_at": nil}},
		{"decline of the architect's block ask", "/api/v1/asks/" + ask + "/decline", map[string]any{"note": "Not the tester's to say."}},
		{"rebuild of C's spec", "/api/v1/artifacts/" + spec + "/rebuild", nil},
	} {
		rig.requireRefusedWith(t, http.StatusForbidden, codeScopeRequired, specWriteAttempt{
			what: "C's tester's " + route.what,
			send: func(t *testing.T) *httptest.ResponseRecorder {
				return rig.send(t, rig.cTester, http.MethodPost, route.path, route.body)
			},
		})
	}
	if state := rig.askState(t, ask); state != "open" {
		t.Errorf("after the refused answer and decline the ask %s is %s, want it open", ask, state)
	}
	if resolved, _ := rig.commentState(t, suggestion); resolved {
		t.Errorf("after the refused accept and reject the suggestion %s is resolved, want it open", suggestion)
	}
	rig.requireRefusedWith(t, http.StatusForbidden, "BROWSER_SIGN_IN_REQUIRED", specWriteAttempt{
		what: "C's tester's close of " + rig.r.key + " with include_children",
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.cTester, http.MethodPost, "/api/v1/issues/"+rig.r.key+"/status-change",
				map[string]any{"status": "done", "include_children": true})
		},
	})
	for _, issue := range []specWriterIssue{rig.r, rig.c} {
		if status := rig.issueRow(t, issue.key).status; status == "done" {
			t.Errorf("the refused close left %s done, want it open", issue.key)
		}
	}

	rig.requireVersionWritten(t, "a person's accept of C's tester's suggestion", http.StatusOK, spec, true,
		func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, specWriterPerson, http.MethodPost, "/api/v1/comments/"+suggestion+"/accept", map[string]any{})
		})
	if resolved, accepted := rig.commentState(t, suggestion); !resolved || accepted == nil || !*accepted {
		t.Errorf("after a person's accept the suggestion %s is resolved=%t accepted=%v, want it resolved and accepted", suggestion, resolved, accepted)
	}
	rig.requireVersionWritten(t, "a person's answer of the architect's block ask", http.StatusOK, spec, true,
		func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, specWriterPerson, http.MethodPost, "/api/v1/asks/"+ask+"/answer", map[string]any{"selected": []string{"One"}})
		})
	if state := rig.askState(t, ask); state != "answered" {
		t.Errorf("after a person's answer the ask %s is %s, want it answered", ask, state)
	}
}

// TestSpecWriteNamesNoArchitectWhenNoneIsRecorded: in the marked project P, a worker's edit of V,
// a root with the label legion that no publication names, is refused, and the refusal says that
// Dispatch records no architect on V or above it.
func TestSpecWriteNamesNoArchitectWhenNoneIsRecorded(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{unclaimed: true})
	edit := rig.editAttempt(rig.cTester, rig.v, "spec.md")
	var refused *httptest.ResponseRecorder
	send := edit.send
	edit.send = func(t *testing.T) *httptest.ResponseRecorder {
		refused = send(t)
		return refused
	}
	rig.requireRefused(t, edit)
	if message := decodeBody[refusalAnswer](t, refused).Error; !strings.Contains(message, "records no architect") {
		t.Errorf("%s: the refusal %q does not say that Dispatch records no architect", edit.what, message)
	}
}

// TestArchitectWritePassesForTheIssuesArchitectAnArchitectAboveAndAPerson: the writers the check
// passes. C's architect writes C's spec and Verification document on every route of the inventory
// that writes them for each, each run on a rig of its own. R's architect writes the spec of C, of
// G under C, and of D, a child of R with no architect of its own, whose first Verification
// document it uploads too; C's tester and C's architect, the architect of D's sibling, are refused
// D's spec. A person passes on each credential that holds the person scope: a sign-in, a personal
// token, and a broker token scoped to a person.
func TestArchitectWritePassesForTheIssuesArchitectAnArchitectAboveAndAPerson(t *testing.T) {
	server, brokered := newSpecWriterBrokerServer(t)
	routes := []string{"upload a new version", "edit by id", "edit by slug", "named version by id", "named version by slug", "resolution", "retraction", "ask edit"}
	for _, row := range specWriteTakenRows() {
		if !slices.Contains(routes, row.name) {
			continue
		}
		for _, document := range row.documents {
			t.Run("C's architect/"+row.name+"/"+document, func(t *testing.T) {
				rig := server.rig(t, row.shape)
				row.run(t, specWriteAt{rig: rig, writer: rig.cArchitect, issue: rig.c, document: document})
			})
		}
	}

	t.Run("an architect above", func(t *testing.T) {
		rig := server.rig(t, specWriterRigShape{sibling: true, grandchild: true})
		rig.requireRefused(t, rig.editAttempt(rig.cTester, rig.d, "spec.md"), rig.editAttempt(rig.cArchitect, rig.d, "spec.md"))
		rig.requireEditTaken(t, rig.rArchitect, rig.c, "spec.md")
		rig.requireEditTaken(t, rig.rArchitect, rig.g, "spec.md")
		uploaded := decodeBody[uploadedArtifact](t, rig.requireTaken(t, "R's architect's first upload of "+rig.d.key+"'s verification.md",
			http.StatusCreated, func(t *testing.T) *httptest.ResponseRecorder {
				return rig.upload(t, rig.rArchitect, rig.d, "verification.md", "The verification of the sibling issue.\n")
			}))
		if versions := rig.versionsOf(t, uploaded.Artifact.ID); versions != 1 {
			t.Errorf("R's architect's first upload of %s's verification.md left it %d versions, want 1", rig.d.key, versions)
		}
		rig.requireEditTaken(t, rig.rArchitect, rig.d, "spec.md")
	})

	t.Run("a person", func(t *testing.T) {
		rig := server.rig(t, specWriterRigShape{})
		spec := rig.c.documents["spec.md"].id
		path := "/api/v1/artifacts/" + spec + "/edits"
		body := map[string]any{"ops": specWriterAppend}
		personal := mintPersonalToken(t, rig.handler, specWriterPerson, "a person-scoped token", []string{model.ScopePerson}).Token
		caller := newBrokerCaller(t)
		const token = "a-broker-token-scoped-to-a-person"
		says := liveNamedToken(caller.jkt)
		says.ProjectScope, says.Projects = ciphertoken.ProjectScopePerson, nil
		brokered.broker.Says(token, says)
		for _, credential := range []struct {
			what string
			send func(t *testing.T) *httptest.ResponseRecorder
		}{
			{"a sign-in", func(t *testing.T) *httptest.ResponseRecorder {
				return rig.sendAs(t, specWriterPerson, http.MethodPost, path, body)
			}},
			{"a personal token with the person scope", func(t *testing.T) *httptest.ResponseRecorder {
				// A personal token names the session that acts for its person.
				acting := maps.Clone(body)
				acting["actor"] = map[string]any{"kind": "session", "id": "ses-acting-for-" + specWriterPerson}
				return agentRequest(t, rig.handler, http.MethodPost, path, acting, personal)
			}},
			{"a broker token scoped to a person", func(t *testing.T) *httptest.ResponseRecorder {
				return brokered.serve(caller.presenting(t, http.MethodPost, path, token, body))
			}},
		} {
			rig.requireVersionWritten(t, "an edit of "+rig.c.key+"'s spec on "+credential.what, http.StatusOK, spec, true, credential.send)
		}
	})
}

// TestSpecWriteChecksACreateOnlyUnderAParent: in the marked project P, a session's create of a
// root with the label legion is taken: a create under no parent is not checked. A create under a
// parent is checked on the parent's chain, with the labels of the new row: C's tester's create
// with the label legion under U2, a root with no label, is refused with no new issue, and a
// person's is taken; C's tester's move under U2 of its root with the label legion is refused, and
// the root stays one. C's architect's, R's architect's and a person's creates under C are taken.
// (C's tester's create under C is the inventory's row.)
func TestSpecWriteChecksACreateOnlyUnderAParent(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{unlabelled: true})
	legion := []string{"legion"}
	root := rig.requireCreated(t, "C's tester's create of a root with the label legion", "", rig.creating(rig.cTester, "A worker's root", "", legion))

	title := "A worker's issue under " + rig.u2.key
	rig.requireRefused(t, specWriteAttempt{
		what:    "C's tester's create of an issue with the label legion under " + rig.u2.key,
		send:    rig.creating(rig.cTester, title, rig.u2.key, legion),
		refusal: rig.refusalOn(rig.u2, "spec.md", rig.cTester),
	})
	if keys := rig.issuesTitled(t, title); len(keys) > 0 {
		t.Errorf("the refused create left the issues %v titled %q, want none", keys, title)
	}
	moved := rig.refusalOn(rig.u2, "spec.md", rig.cTester)
	moved.issues = []string{root, rig.u2.key}
	rig.requireRefused(t, specWriteAttempt{
		what: "C's tester's move of its root " + root + " under " + rig.u2.key,
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.cTester, http.MethodPatch, "/api/v1/issues/"+root, map[string]any{"parent": rig.u2.key})
		},
		refusal: moved,
	})
	if parent := rig.issueRow(t, root).parent; parent != nil {
		t.Errorf("the refused move left %s under %s, want it still a root", root, *parent)
	}

	rig.requireCreated(t, "a person's create of an issue with the label legion under "+rig.u2.key, rig.u2.key,
		rig.creating(specWriterPerson, "A person's issue under "+rig.u2.key, rig.u2.key, legion))
	for _, writer := range []struct{ name, session string }{
		{"C's architect", rig.cArchitect}, {"R's architect", rig.rArchitect}, {"a person", specWriterPerson},
	} {
		rig.requireCreated(t, writer.name+"'s create of an issue under "+rig.c.key, rig.c.key,
			rig.creating(writer.session, writer.name+"'s issue under "+rig.c.key, rig.c.key, nil))
	}
}

// TestSpecWriteAppliesOnceTheProjectIsMarked: the check holds the issues of a project once
// Dispatch has taken a publication of role holders for any issue of it. In Q, which no
// publication marked, a worker's edit of W, a root with the label legion, is taken; after a
// publication of an empty set on X, another issue of Q, the same edit is refused, and it still is
// after a later publication that empties every set of Q again. In the marked project P, a
// worker's edit of V, a root with the label legion that no publication names, is refused, and a
// person's is taken.
func TestSpecWriteAppliesOnceTheProjectIsMarked(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{unclaimed: true, unmarked: true})
	edit := rig.editAttempt(rig.cTester, rig.w, "spec.md")
	rig.requireVersionWritten(t, edit.what+", before any publication on "+rig.q, http.StatusOK, rig.w.documents["spec.md"].id, true, edit.send)

	rig.publish(t, rig.x.key, []issueRoleHolder{})
	rig.quiet(t)
	marked := edit
	marked.what += ", after a publication of an empty set on " + rig.x.key
	rig.requireRefused(t, marked)

	rig.publish(t, rig.x.key, []issueRoleHolder{{Role: "architect", Session: "ses-architect-" + strings.ToLower(rig.q)}})
	rig.publish(t, rig.x.key, []issueRoleHolder{})
	rig.quiet(t)
	emptied := edit
	emptied.what += ", after a publication that emptied every set of " + rig.q
	rig.requireRefused(t, emptied)

	rig.requireRefused(t, rig.editAttempt(rig.cTester, rig.v, "spec.md"))
	rig.requireEditTaken(t, specWriterPerson, rig.v, "spec.md")
}

// TestSpecWriteLeavesAnchoredWritesAlone: an anchored comment, an anchored ask and a suggestion add
// only their writer's mark around text others wrote, so C's tester's are taken on C's spec, also
// while a person's edit of the spec has not settled yet and the anchor's write is what makes a
// version of it; C's tester's resolve of its own anchored comment is taken.
func TestSpecWriteLeavesAnchoredWritesAlone(t *testing.T) {
	anchored := func(rig *specWriterRig) []specWriteAttempt {
		spec := rig.c.documents["spec.md"].id
		write := func(what, path string, body map[string]any) specWriteAttempt {
			return specWriteAttempt{what: "C's tester's " + what + " on " + rig.c.key + "'s spec", send: func(t *testing.T) *httptest.ResponseRecorder {
				return rig.send(t, rig.cTester, http.MethodPost, "/api/v1/issues/"+rig.c.key+path, body)
			}}
		}
		return []specWriteAttempt{
			write("anchored comment", "/comments", map[string]any{"body": "The tester reads this line.", "anchor": specWriterAnchor(spec, "The spec")}),
			write("anchored ask", "/asks", map[string]any{"question": "Which way does this line go?", "anchor": specWriterAnchor(spec, "of the child")}),
			write("suggestion", "/comments", map[string]any{
				"body": "Name the issue.", "anchor": specWriterAnchor(spec, "issue."), "suggestion": map[string]string{"replace_with": "issue, C."},
			}),
		}
	}

	t.Run("on a settled spec", func(t *testing.T) {
		server := newSpecWriterServer(t, testServerOptions{})
		rig := server.rig(t, specWriterRigShape{})
		spec := rig.c.documents["spec.md"].id
		writes := anchored(rig)
		comment := decodeBody[model.Comment](t, rig.requireTaken(t, writes[0].what, http.StatusCreated, writes[0].send))
		if comment.Anchor == nil || comment.Anchor.ArtifactID != spec {
			t.Errorf("%s: the comment's anchor is %+v, want one on %s", writes[0].what, comment.Anchor, spec)
		}
		ask := decodeBody[model.Ask](t, rig.requireTaken(t, writes[1].what, http.StatusCreated, writes[1].send))
		if ask.Anchor == nil || ask.Anchor.ArtifactID != spec {
			t.Errorf("%s: the ask's anchor is %+v, want one on %s", writes[1].what, ask.Anchor, spec)
		}
		suggestion := decodeBody[model.Comment](t, rig.requireTaken(t, writes[2].what, http.StatusCreated, writes[2].send))
		if suggestion.Anchor == nil || suggestion.Anchor.ArtifactID != spec || suggestion.Suggestion == nil {
			t.Errorf("%s: the suggestion's anchor is %+v and its change %+v, want both on %s", writes[2].what, suggestion.Anchor, suggestion.Suggestion, spec)
		}
		rig.requireTaken(t, "C's tester's resolve of its own comment "+comment.ID, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.cTester, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil)
		})
		if resolved, _ := rig.commentState(t, comment.ID); !resolved {
			t.Errorf("C's tester's resolve left its comment %s open, want it resolved", comment.ID)
		}
	})

	t.Run("while a person's edit is unsettled", func(t *testing.T) {
		// Settlement is held for the test's length, so the person's edit is still unsettled when
		// each anchored write takes its version: the rig is the tree and its publication alone,
		// with nothing that waits for a settlement.
		held := newSpecWriterServer(t, testServerOptions{settle: time.Hour})
		rig := held.tree(t, specWriterRigShape{})
		rig.publishRoles(t)
		spec := rig.c.documents["spec.md"].id
		for index, write := range anchored(rig) {
			typed := specWriterText(rig.c.name, "spec.md") + fmt.Sprintf("\nA person's paragraph %d.\n", index+1)
			if _, err := replaceDocumentText(rig.database, rig.served.deps.Docs, spec, typed, model.Actor{Kind: "user", ID: specWriterPerson}); err != nil {
				t.Fatalf("rig: a person types into %s's spec: %v", rig.c.key, err)
			}
			before := rig.versionsOf(t, spec)
			if response := write.send(t); response.Code != http.StatusCreated {
				t.Fatalf("%s: status=%d body=%s, want %d", write.what, response.Code, clippedBody(response), http.StatusCreated)
			}
			if after := rig.versionsOf(t, spec); after != before+1 {
				t.Errorf("%s over a person's unsettled edit: the spec holds %d versions, want the one its anchor wrote over the %d before", write.what, after, before)
			}
		}
	})
}

// TestSpecWriteChecksTheCloseAndTheReopen: while C's spec holds the architect's open block ask,
// a close and a reopen of C by C's architect, by R's architect, by a person and by the Legion
// daemon of P on the shared token are taken on both routes that write a status (PATCH and the
// status route), each followed by the version of C's spec that its settlement writes. C's tester's
// move of C from in_progress to testing, which neither closes nor reopens, is taken on both. C's
// architect's call of the status route that moves C and then R, closed, to backlog is refused at
// its later step, the reopen of R, and writes neither step. (C's tester's close and reopen are the
// inventory's rows.)
func TestSpecWriteChecksTheCloseAndTheReopen(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	routes := []struct {
		name string
		send func(t *testing.T, rig *specWriterRig, writer string, issue specWriterIssue, body map[string]any) *httptest.ResponseRecorder
	}{
		{"PATCH", func(t *testing.T, rig *specWriterRig, writer string, issue specWriterIssue, body map[string]any) *httptest.ResponseRecorder {
			return rig.sendAs(t, writer, http.MethodPatch, "/api/v1/issues/"+issue.key, body)
		}},
		{"status-change", func(t *testing.T, rig *specWriterRig, writer string, issue specWriterIssue, body map[string]any) *httptest.ResponseRecorder {
			return rig.sendAs(t, writer, http.MethodPost, "/api/v1/issues/"+issue.key+"/status-change", body)
		}},
	}
	writers := []struct {
		name   string
		writer func(rig *specWriterRig) string
	}{
		{"C's architect", func(rig *specWriterRig) string { return rig.cArchitect }},
		{"R's architect", func(rig *specWriterRig) string { return rig.rArchitect }},
		{"a person", func(*specWriterRig) string { return specWriterPerson }},
		{"the Legion daemon of P", func(rig *specWriterRig) string { return legionDaemonSessionOf(rig.c.key) }},
	}
	for _, route := range routes {
		for _, writer := range writers {
			t.Run(route.name+"/"+writer.name, func(t *testing.T) {
				rig := server.rig(t, specWriterRigShape{})
				who := writer.writer(rig)
				for _, step := range []struct{ act, status string }{{"close", "done"}, {"reopen", "in_progress"}} {
					rig.requireVersionWritten(t, fmt.Sprintf("%s's %s of %s (%s)", who, step.act, rig.c.key, route.name), http.StatusOK,
						rig.c.documents["spec.md"].id, true, func(t *testing.T) *httptest.ResponseRecorder {
							return route.send(t, rig, who, rig.c, map[string]any{"status": step.status})
						})
					if status := rig.issueRow(t, rig.c.key).status; status != step.status {
						t.Errorf("after %s's %s %s is %s, want %s", who, step.act, rig.c.key, status, step.status)
					}
				}
			})
		}
	}

	t.Run("a status that neither closes nor reopens", func(t *testing.T) {
		rig := server.rig(t, specWriterRigShape{})
		for _, route := range routes {
			rig.requireTaken(t, "a person's move of "+rig.c.key+" to in_progress", http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
				return rig.sendAs(t, specWriterPerson, http.MethodPatch, "/api/v1/issues/"+rig.c.key, map[string]any{"status": "in_progress"})
			})
			rig.requireTaken(t, "C's tester's move of "+rig.c.key+" from in_progress to testing ("+route.name+")", http.StatusOK,
				func(t *testing.T) *httptest.ResponseRecorder {
					return route.send(t, rig, rig.cTester, rig.c, map[string]any{"status": "testing"})
				})
			if status := rig.issueRow(t, rig.c.key).status; status != "testing" {
				t.Errorf("after C's tester's move (%s) %s is %s, want testing", route.name, rig.c.key, status)
			}
		}
	})

	t.Run("a refused later step", func(t *testing.T) {
		rig := server.rig(t, specWriterRigShape{})
		rig.requireTaken(t, "a person's move of "+rig.c.key+" to in_progress", http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, specWriterPerson, http.MethodPatch, "/api/v1/issues/"+rig.c.key, map[string]any{"status": "in_progress"})
		})
		rig.requireTaken(t, "R's architect's close of "+rig.r.key, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, rig.rArchitect, http.MethodPatch, "/api/v1/issues/"+rig.r.key, map[string]any{"status": "done"})
		})
		// The status route moves C first, deepest first for a status not started, which the check
		// does not hold, and then R, whose reopen it refuses C's architect.
		rig.requireRefused(t, specWriteAttempt{
			what: "C's architect's move of " + rig.r.key + " and the issues under it to backlog",
			send: func(t *testing.T) *httptest.ResponseRecorder {
				return rig.send(t, rig.cArchitect, http.MethodPost, "/api/v1/issues/"+rig.r.key+"/status-change",
					map[string]any{"status": "backlog", "include_children": true})
			},
			refusal: rig.refusalOn(rig.r, "spec.md", rig.cArchitect),
		})
		for key, want := range map[string]string{rig.c.key: "in_progress", rig.r.key: "done"} {
			if status := rig.issueRow(t, key).status; status != want {
				t.Errorf("the refused status change left %s %s, want it still %s", key, status, want)
			}
		}
	})
}

// TestSpecWriteChecksAMoveUnderAParent: a move under a parent is checked as a create under it. C's
// tester's create of a root with the label legion is taken; its move of that root under C is
// refused, and the root stays one. Legion runs the root where it stands, through its own label, and
// Dispatch records no architect on it, so C's architect's move of it under C is refused too, on the
// root's own chain (TestSpecWriteChecksAMoveOnBothChains), and the root stays one; a person's move
// of it under C is taken.
func TestSpecWriteChecksAMoveUnderAParent(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{})
	root := rig.requireCreated(t, "C's tester's create of a root with the label legion", "",
		rig.creating(rig.cTester, "A worker's root", "", []string{"legion"}))
	move := func(writer string) func(t *testing.T) *httptest.ResponseRecorder {
		return func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, writer, http.MethodPatch, "/api/v1/issues/"+root, map[string]any{"parent": rig.c.key})
		}
	}
	refusal := rig.refusalOn(rig.c, "spec.md", rig.cTester)
	refusal.issues = []string{root, rig.c.key}
	rig.requireRefused(t, specWriteAttempt{what: "C's tester's move of its root " + root + " under " + rig.c.key, send: move(rig.cTester), refusal: refusal})
	if parent := rig.issueRow(t, root).parent; parent != nil {
		t.Errorf("the refused move left %s under %s, want it still a root", root, *parent)
	}
	rig.requireRefused(t, specWriteAttempt{
		what:    "C's architect's move of " + root + " under " + rig.c.key,
		send:    move(rig.cArchitect),
		refusal: specWriteRefusal{issues: []string{root}, document: "spec.md", writer: rig.cArchitect},
	})
	if parent := rig.issueRow(t, root).parent; parent != nil {
		t.Errorf("C's architect's refused move left %s under %s, want it still a root", root, *parent)
	}
	rig.requireTaken(t, "a person's move of "+root+" under "+rig.c.key, http.StatusOK, move(specWriterPerson))
	if parent := rig.issueRow(t, root).parent; parent == nil || *parent != rig.c.key {
		t.Errorf("the person's move left %s under %s, want it under %s", root, specWriterParentName(parent), rig.c.key)
	}
}

// TestSpecWriteChecksAMoveOnBothChains: a move of a Legion-run issue under a parent in a tree Legion
// runs is checked on the issue's own chain as it stands as well as on the new parent's, so it
// passes only a person or an architect at or above both places. On a rig with R, C and D, a sibling
// of C under R, and V, a labelled root whose own architect the daemon of P publishes: C's
// architect's move of D under C, V's architect's move of D under V, and the Legion daemon's move of
// D under C are each refused, D stays under R, and each architect's edit of D's spec after its
// move is refused too. A move of D under U2, a root with no label, takes D out of every tree Legion
// runs: it is checked on D's own chain as every move is, and the take-out check
// (TestSpecWriteChecksAWriteThatTakesAnIssueOutOfLegion) does not judge it. The daemon, which
// passes the take-out check but no check of a move, is refused it, and D stays under R. R's
// architect's move of D under C, a person's move of D back under R, and R's architect's move of D
// under U2 are taken.
func TestSpecWriteChecksAMoveOnBothChains(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{sibling: true, unlabelled: true, unclaimed: true})
	vArchitect := "ses-v-architect-" + strings.ToLower(rig.project)
	rig.publish(t, rig.v.key, []issueRoleHolder{{Role: "architect", Session: vArchitect}})
	rig.quiet(t)
	daemon := legionDaemonSessionOf(rig.r.key)
	move := func(writer string, parent specWriterIssue) func(t *testing.T) *httptest.ResponseRecorder {
		return func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, writer, http.MethodPatch, "/api/v1/issues/"+rig.d.key, map[string]any{"parent": parent.key})
		}
	}
	requireUnder := func(t *testing.T, what string, parent specWriterIssue) {
		t.Helper()
		if after := rig.issueRow(t, rig.d.key).parent; after == nil || *after != parent.key {
			t.Errorf("%s left %s under %s, want it under %s", what, rig.d.key, specWriterParentName(after), parent.key)
		}
	}

	for _, architect := range []struct {
		name, session string
		parent        specWriterIssue
	}{
		{"C's architect", rig.cArchitect, rig.c},
		{"V's architect", vArchitect, rig.v},
	} {
		what := fmt.Sprintf("%s's move of %s under %s", architect.name, rig.d.key, architect.parent.key)
		rig.requireRefused(t, specWriteAttempt{
			what:    what,
			send:    move(architect.session, architect.parent),
			refusal: rig.refusalOn(rig.d, "spec.md", architect.session),
		}, rig.editAttempt(architect.session, rig.d, "spec.md"))
		requireUnder(t, "the refused "+what, rig.r)
	}
	daemonsMove := rig.refusalOn(rig.c, "spec.md", daemon)
	daemonsMove.issues = []string{rig.d.key, rig.c.key}
	rig.requireRefused(t, specWriteAttempt{
		what:    "the Legion daemon's move of " + rig.d.key + " under " + rig.c.key,
		send:    move(daemon, rig.c),
		refusal: daemonsMove,
	})
	requireUnder(t, "the daemon's refused move", rig.r)
	rig.requireRefused(t, specWriteAttempt{
		what:    "the Legion daemon's move of " + rig.d.key + " under " + rig.u2.key,
		send:    move(daemon, rig.u2),
		refusal: rig.refusalOn(rig.d, "spec.md", daemon),
	})
	requireUnder(t, "the daemon's refused move out of every tree Legion runs", rig.r)

	for _, taken := range []struct {
		name, writer string
		parent       specWriterIssue
	}{
		{"R's architect", rig.rArchitect, rig.c},
		{"a person", specWriterPerson, rig.r},
		{"R's architect", rig.rArchitect, rig.u2},
	} {
		what := fmt.Sprintf("%s's move of %s under %s", taken.name, rig.d.key, taken.parent.key)
		rig.requireTaken(t, what, http.StatusOK, move(taken.writer, taken.parent))
		requireUnder(t, what, taken.parent)
	}
}

// TestSpecWriteChecksTheLabelledIssuesOfAMovedSubtree: a move under a parent in a tree Legion runs
// of an issue that Legion does not run is checked, as it stands, on the chain of each labelled
// issue under it, so no architect takes a labelled issue by moving its unlabelled parent under
// itself. On a rig with R and its child C, V, a second labelled root whose own architect the daemon
// of P publishes, and U2, a root with no label that holds Y, a labelled child whose own architect
// the daemon publishes: R's architect's move of U2 under C and V's architect's move of U2 under V
// are each refused naming Y, U2 stays a root, and each architect's edit of Y's spec after its move
// is refused too. Legion runs U2 under C and under V, so each refusal says that the writer must
// also be an architect of the new parent or of an issue above it. Y's architect's move of U2 under
// C is refused on C's chain: it is at or above Y but not at or above C. A person's move of U2
// under C is taken.
func TestSpecWriteChecksTheLabelledIssuesOfAMovedSubtree(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{unlabelled: true, unclaimed: true})
	y := rig.file(t, "labelled", rig.u2.key, []string{"legion"})
	vArchitect := "ses-v-architect-" + strings.ToLower(rig.project)
	yArchitect := "ses-y-architect-" + strings.ToLower(rig.project)
	rig.publish(t, rig.v.key, []issueRoleHolder{{Role: "architect", Session: vArchitect}})
	rig.publish(t, y.key, []issueRoleHolder{{Role: "architect", Session: yArchitect}})
	rig.quiet(t)
	move := func(writer string, parent specWriterIssue) func(t *testing.T) *httptest.ResponseRecorder {
		return func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, writer, http.MethodPatch, "/api/v1/issues/"+rig.u2.key, map[string]any{"parent": parent.key})
		}
	}
	// onY is what a refusal on Y's chain names: Y, its spec, the writer and Y's architect.
	onY := func(writer string) specWriteRefusal {
		return specWriteRefusal{issues: []string{y.key}, document: "spec.md", writer: writer, architects: []string{yArchitect}}
	}
	requireRoot := func(t *testing.T, what string) {
		t.Helper()
		if parent := rig.issueRow(t, rig.u2.key).parent; parent != nil {
			t.Errorf("%s left %s under %s, want it still a root", what, rig.u2.key, *parent)
		}
	}

	for _, architect := range []struct {
		name, session string
		parent        specWriterIssue
	}{
		{"R's architect", rig.rArchitect, rig.c},
		{"V's architect", vArchitect, rig.v},
	} {
		what := fmt.Sprintf("%s's move of %s under %s", architect.name, rig.u2.key, architect.parent.key)
		edit := rig.editAttempt(architect.session, y, "spec.md")
		edit.refusal = onY(architect.session)
		moved := edit.refusal
		moved.says = []string{specWriterAlsoAbove(architect.parent.key)}
		rig.requireRefused(t, specWriteAttempt{what: what, send: move(architect.session, architect.parent), refusal: moved}, edit)
		requireRoot(t, "the refused "+what)
	}
	ysMove := rig.refusalOn(rig.c, "spec.md", yArchitect)
	ysMove.issues = []string{rig.u2.key, rig.c.key}
	rig.requireRefused(t, specWriteAttempt{
		what:    "Y's architect's move of " + rig.u2.key + " under " + rig.c.key,
		send:    move(yArchitect, rig.c),
		refusal: ysMove,
	})
	requireRoot(t, "Y's architect's refused move")

	what := "a person's move of " + rig.u2.key + " under " + rig.c.key
	rig.requireTaken(t, what, http.StatusOK, move(specWriterPerson, rig.c))
	if parent := rig.issueRow(t, rig.u2.key).parent; parent == nil || *parent != rig.c.key {
		t.Errorf("%s left %s under %s, want it under %s", what, rig.u2.key, specWriterParentName(parent), rig.c.key)
	}
}

// TestSpecWriteChecksAMovedSubtreeWhereverItsNewParentIs: a move under a parent is checked on the
// labelled issues of the moved subtree wherever the new parent is, so an architect that first takes
// its own issue out of Legion still cannot move another tree's issue under it. Each run has a rig
// of its own with R and its child C, U2, a root with no label, and Y, a labelled child of U2 whose
// own architect the daemon of P publishes. In one run R's architect removes the label legion from
// R, and in the other C's architect moves C out to the root: each is a take-out that its own
// architect passes, and is taken. The architect's role-holder row is still stored after it, and it
// grants nothing: the architect's move of U2 under that issue, which Legion no longer runs, is
// refused naming Y, its spec, the architect and Y's architect, U2 stays a root, and the architect's
// edit of Y's spec after it is refused too. The refusal does not say that the writer must also be
// an architect of the new parent or of an issue above it: Legion does not run U2 there.
func TestSpecWriteChecksAMovedSubtreeWhereverItsNewParentIs(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	for _, takeOut := range []struct {
		name string
		// architect is the session that takes its own issue, own, out of Legion with body.
		architect func(rig *specWriterRig) string
		own       func(rig *specWriterRig) specWriterIssue
		body      map[string]any
	}{
		{"R's architect's removal of the label legion from R",
			func(rig *specWriterRig) string { return rig.rArchitect },
			func(rig *specWriterRig) specWriterIssue { return rig.r },
			map[string]any{"labels": []string{}}},
		{"C's architect's move of C to the root",
			func(rig *specWriterRig) string { return rig.cArchitect },
			func(rig *specWriterRig) specWriterIssue { return rig.c },
			map[string]any{"parent": nil}},
	} {
		t.Run(takeOut.name, func(t *testing.T) {
			rig := server.rig(t, specWriterRigShape{unlabelled: true})
			y := rig.file(t, "labelled", rig.u2.key, []string{"legion"})
			yArchitect := "ses-y-architect-" + strings.ToLower(rig.project)
			rig.publish(t, y.key, []issueRoleHolder{{Role: "architect", Session: yArchitect}})
			rig.quiet(t)
			architect, own := takeOut.architect(rig), takeOut.own(rig)
			ctx := context.Background()

			rig.requireTaken(t, takeOut.name, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
				return rig.sendAs(t, architect, http.MethodPatch, "/api/v1/issues/"+own.key, takeOut.body)
			})
			var runs bool
			if err := rig.database.Pool.QueryRow(ctx, legionManagedStatusSQL, own.key).Scan(&runs); err != nil {
				t.Fatalf("read whether Legion runs %s: %v", own.key, err)
			}
			if runs {
				t.Fatalf("after %s Legion still runs %s, want it out of Legion", takeOut.name, own.key)
			}
			requireHeldRole(t, rig.database, own.key, architect, "architect")

			onY := specWriteRefusal{issues: []string{y.key}, document: "spec.md", writer: architect, architects: []string{yArchitect}}
			edit := rig.editAttempt(architect, y, "spec.md")
			edit.refusal = onY
			moved := onY
			moved.saysNot = []string{specWriterAlsoAbove(own.key)}
			what := fmt.Sprintf("%s's move of %s under %s", architect, rig.u2.key, own.key)
			rig.requireRefused(t, specWriteAttempt{
				what: what,
				send: func(t *testing.T) *httptest.ResponseRecorder {
					return rig.sendAs(t, architect, http.MethodPatch, "/api/v1/issues/"+rig.u2.key, map[string]any{"parent": own.key})
				},
				refusal: moved,
			}, edit)
			if parent := rig.issueRow(t, rig.u2.key).parent; parent != nil {
				t.Errorf("the refused %s left %s under %s, want it still a root", what, rig.u2.key, *parent)
			}
		})
	}
}

// TestSpecWriteDecidesAMovedSubtreesLabelInSQL: the check of a moved subtree reads in SQL which
// issues under the moved issue carry the label legion, by the one match every answer to "Legion
// runs" reads, so it judges each of them as that answer does. Each run has a rig of its own with R
// and its child C, U2, a root with no label, and Y, a labelled child of U2 whose own architect the
// daemon of P publishes. A person sets Y's labels to one label: LEGİON, with U+0130, which a libc
// collation lowers to legion and an ICU one does not, or LEGION, which every collation lowers to
// legion. Then R's architect moves U2 under R. When SQL answers that Legion runs Y
// (legionManagedStatusSQL), the move is refused naming Y, its spec, R's architect and Y's
// architect, and U2 stays a root; when SQL answers that it does not, the move is taken. The test
// logs SQL's answer.
func TestSpecWriteDecidesAMovedSubtreesLabelInSQL(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	for _, label := range []string{"LEG\u0130ON", "LEGION"} {
		t.Run(label, func(t *testing.T) {
			rig := server.rig(t, specWriterRigShape{unlabelled: true})
			y := rig.file(t, "labelled", rig.u2.key, []string{"legion"})
			yArchitect := "ses-y-architect-" + strings.ToLower(rig.project)
			rig.publish(t, y.key, []issueRoleHolder{{Role: "architect", Session: yArchitect}})
			rig.quiet(t)
			rig.requireTaken(t, "a person's write of "+y.key+"'s labels to ["+label+"]", http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
				return rig.sendAs(t, specWriterPerson, http.MethodPatch, "/api/v1/issues/"+y.key, map[string]any{"labels": []string{label}})
			})
			if labels := rig.issueRow(t, y.key).labels; !slices.Equal(labels, []string{label}) {
				t.Fatalf("after the person's label write %s has the labels %v, want [%s]", y.key, labels, label)
			}
			var runs bool
			if err := rig.database.Pool.QueryRow(context.Background(), legionManagedStatusSQL, y.key).Scan(&runs); err != nil {
				t.Fatalf("read whether Legion runs %s: %v", y.key, err)
			}
			t.Logf("with the label %q on %s, SQL answers that Legion runs it: %t", label, y.key, runs)

			what := fmt.Sprintf("R's architect's move of %s under %s", rig.u2.key, rig.r.key)
			move := func(t *testing.T) *httptest.ResponseRecorder {
				return rig.sendAs(t, rig.rArchitect, http.MethodPatch, "/api/v1/issues/"+rig.u2.key, map[string]any{"parent": rig.r.key})
			}
			if !runs {
				rig.requireTaken(t, what, http.StatusOK, move)
				if parent := rig.issueRow(t, rig.u2.key).parent; parent == nil || *parent != rig.r.key {
					t.Errorf("%s left %s under %s, want it under %s", what, rig.u2.key, specWriterParentName(parent), rig.r.key)
				}
				return
			}
			rig.requireRefused(t, specWriteAttempt{
				what:    what,
				send:    move,
				refusal: specWriteRefusal{issues: []string{y.key}, document: "spec.md", writer: rig.rArchitect, architects: []string{yArchitect}},
			})
			if parent := rig.issueRow(t, rig.u2.key).parent; parent != nil {
				t.Errorf("the refused %s left %s under %s, want it still a root", what, rig.u2.key, *parent)
			}
		})
	}
}

// TestSpecWriteChecksAWriteThatTakesAnIssueOutOfLegion: a labels write or a write of no parent
// after which a Legion-run issue is no longer one is checked as a close is. C's tester's removal of
// legion from R, its clear of D's parent, and its call of the status route on R with
// unlink_children are each refused with nothing written, and its edit of D's spec after each is
// refused too. Its move of D under U2, which takes D out of every tree Legion runs, is refused the
// same way, by the check of a move on D's own chain (TestSpecWriteChecksAMoveOnBothChains). The
// same unlink by R's architect, by a person and by the Legion daemon of P is taken, and so are R's
// architect's clear of D's parent and the removal of R's label by a person and by the daemon on the
// shared token. A labels write on C that keeps legion on R, a labels_add on R, and C's tester's
// removal of legion from W in Q, which no publication marked, are taken: none takes an issue out of
// a tree the check holds.
func TestSpecWriteChecksAWriteThatTakesAnIssueOutOfLegion(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	tree := specWriterRigShape{sibling: true, unlabelled: true}
	unlink := map[string]any{"status": "in_progress", "unlink_children": true}

	t.Run("C's tester", func(t *testing.T) {
		rig := server.rig(t, tree)
		editD := rig.editAttempt(rig.cTester, rig.d, "spec.md")
		rig.requireRefused(t, specWriteAttempt{
			what: "C's tester's removal of the label legion from " + rig.r.key,
			send: func(t *testing.T) *httptest.ResponseRecorder {
				return rig.send(t, rig.cTester, http.MethodPatch, "/api/v1/issues/"+rig.r.key, map[string]any{"labels": []string{}})
			},
			refusal: rig.refusalOn(rig.r, "spec.md", rig.cTester),
		}, editD)
		if labels := rig.issueRow(t, rig.r.key).labels; !slices.Contains(labels, "legion") {
			t.Errorf("the refused label removal left %s with the labels %v, want legion among them", rig.r.key, labels)
		}
		onD := specWriteAt{rig: rig, writer: rig.cTester, issue: rig.d, document: "spec.md"}
		onD.requireParentWriteRefused(t, nil)
		onD.requireParentWriteRefused(t, &rig.u2.key)
		refusal := rig.refusalOn(rig.r, "spec.md", rig.cTester)
		refusal.issues = []string{rig.c.key, rig.d.key}
		rig.requireRefused(t, specWriteAttempt{
			what: "C's tester's status change of " + rig.r.key + " with unlink_children",
			send: func(t *testing.T) *httptest.ResponseRecorder {
				return rig.send(t, rig.cTester, http.MethodPost, "/api/v1/issues/"+rig.r.key+"/status-change", unlink)
			},
			refusal: refusal,
		}, editD)
		for _, child := range []string{rig.c.key, rig.d.key} {
			if parent := rig.issueRow(t, child).parent; parent == nil || *parent != rig.r.key {
				t.Errorf("the refused unlink left %s under %s, want it still under %s", child, specWriterParentName(parent), rig.r.key)
			}
		}
	})

	daemon := func(rig *specWriterRig) string { return legionDaemonSessionOf(rig.r.key) }
	for _, writer := range []struct {
		name   string
		writer func(rig *specWriterRig) string
	}{
		{"R's architect", func(rig *specWriterRig) string { return rig.rArchitect }},
		{"a person", func(*specWriterRig) string { return specWriterPerson }},
		{"the Legion daemon of P", daemon},
	} {
		t.Run(writer.name+"'s unlink", func(t *testing.T) {
			rig := server.rig(t, tree)
			rig.requireTaken(t, writer.name+"'s status change of "+rig.r.key+" with unlink_children", http.StatusOK,
				func(t *testing.T) *httptest.ResponseRecorder {
					return rig.sendAs(t, writer.writer(rig), http.MethodPost, "/api/v1/issues/"+rig.r.key+"/status-change", unlink)
				})
			for _, child := range []string{rig.c.key, rig.d.key} {
				if parent := rig.issueRow(t, child).parent; parent != nil {
					t.Errorf("%s's unlink left %s under %s, want it a root", writer.name, child, *parent)
				}
			}
		})
	}

	t.Run("R's architect's parent clear and the writes that take nothing out", func(t *testing.T) {
		rig := server.rig(t, specWriterRigShape{sibling: true, unmarked: true})
		rig.requireTaken(t, "R's architect's clear of "+rig.d.key+"'s parent", http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.rArchitect, http.MethodPatch, "/api/v1/issues/"+rig.d.key, map[string]any{"parent": nil})
		})
		if parent := rig.issueRow(t, rig.d.key).parent; parent != nil {
			t.Errorf("R's architect's parent clear left %s under %s, want it a root", rig.d.key, *parent)
		}
		for _, write := range []struct {
			what  string
			issue specWriterIssue
			body  map[string]any
			want  []string
		}{
			{"labels write on " + rig.c.key + ", which keeps legion on " + rig.r.key, rig.c, map[string]any{"labels": []string{"bug"}}, []string{"bug"}},
			{"labels_add on " + rig.r.key, rig.r, map[string]any{"labels_add": []string{"bug"}}, []string{"bug", "legion"}},
			{"removal of legion from " + rig.w.key + " in " + rig.q + ", which no publication marked", rig.w, map[string]any{"labels": []string{}}, []string{}},
		} {
			rig.requireTaken(t, "C's tester's "+write.what, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
				return rig.send(t, rig.cTester, http.MethodPatch, "/api/v1/issues/"+write.issue.key, write.body)
			})
			if labels := slices.Sorted(slices.Values(rig.issueRow(t, write.issue.key).labels)); !slices.Equal(labels, write.want) {
				t.Errorf("after C's tester's %s, %s has the labels %v, want %v", write.what, write.issue.key, labels, write.want)
			}
		}
	})

	for _, writer := range []struct {
		name   string
		writer func(rig *specWriterRig) string
	}{
		{"a person", func(*specWriterRig) string { return specWriterPerson }},
		{"the Legion daemon of P", daemon},
	} {
		t.Run(writer.name+"'s label removal", func(t *testing.T) {
			rig := server.rig(t, specWriterRigShape{})
			rig.requireTaken(t, writer.name+"'s removal of the label legion from "+rig.r.key, http.StatusOK,
				func(t *testing.T) *httptest.ResponseRecorder {
					return rig.sendAs(t, writer.writer(rig), http.MethodPatch, "/api/v1/issues/"+rig.r.key, map[string]any{"labels": []string{}})
				})
			if labels := rig.issueRow(t, rig.r.key).labels; slices.Contains(labels, "legion") {
				t.Errorf("%s's label removal left %s with the labels %v, want legion gone", writer.name, rig.r.key, labels)
			}
		})
	}
}

// TestSpecWriteLetsTheBootFixWrite: Dispatch's own system writer passes the check, so the check
// never stops a boot. With #4631's rule turned on at boot, in the marked project P, where R is done
// and its Legion-run child C open, the boot's fix (RunParentStatusFixAtBoot) unlinks C as
// parentStatusFixWriter - a write that takes C out of Legion, which the check refuses a worker -
// records that it ran, and answers the rule on with no error.
func TestSpecWriteLetsTheBootFixWrite(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{})
	rig.requireTaken(t, "a person's close of "+rig.r.key+" over its open child "+rig.c.key, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
		return rig.sendAs(t, specWriterPerson, http.MethodPatch, "/api/v1/issues/"+rig.r.key, map[string]any{"status": "done"})
	})

	rule, err := RunParentStatusFixAtBoot(t.Context(), rig.served.deps, storetest.Leases(t, rig.database.Pool), true)
	if err != nil || !rule.On() {
		t.Fatalf("the boot's fix answered the rule on=%t and %v, want the rule on and no error", rule.On(), err)
	}
	if parent := rig.issueRow(t, rig.c.key).parent; parent != nil {
		t.Errorf("the boot's fix left %s under %s, want it unlinked from its done parent", rig.c.key, *parent)
	}
	var unlinked int
	if err := rig.database.Pool.QueryRow(context.Background(), `select unlinked from parent_status_fix`).Scan(&unlinked); err != nil || unlinked != 1 {
		t.Errorf("the fix's record holds %d unlinked (%v), want its one unlink of %s", unlinked, err, rig.c.key)
	}
}

// TestSpecWriteChangesNothingOffLegion: on a tree with no label legion on any chain, in a project
// a publication of role holders marked, each route of the inventory answers a worker as it does
// where nothing checks it, each run on a rig of its own: C's tester writes C's documents on each
// document route, and each issue route is sent by the writer the inventory names.
func TestSpecWriteChangesNothingOffLegion(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	for _, row := range append(specWriteTakenRows(), specWriteOffLegionIssueRows()...) {
		writers := row.writers
		if writers == nil {
			writers = []specWriteWriter{specWriterOwn}
		}
		for _, writer := range writers {
			for _, document := range row.documents {
				t.Run(row.name+"/"+writer.name+"/"+document, func(t *testing.T) {
					shape := row.shape
					shape.offLegion = true
					rig := server.rig(t, shape)
					row.run(t, specWriteAt{rig: rig, writer: writer.session(rig), issue: writer.issue(rig), document: document})
				})
			}
		}
	}
}

// TestSpecWriteChangesNothingAfterTheLabelGoes: once a person removes the label legion from R,
// Legion runs no issue of R's tree, and C's tester's edit of C's spec is taken.
func TestSpecWriteChangesNothingAfterTheLabelGoes(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{})
	rig.requireTaken(t, "a person's removal of the label legion from "+rig.r.key, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
		return rig.sendAs(t, specWriterPerson, http.MethodPatch, "/api/v1/issues/"+rig.r.key, map[string]any{"labels": []string{}})
	})
	rig.requireEditTaken(t, rig.cTester, rig.c, "spec.md")
}

// TestSpecWriteChecksEachCopyOfAResolvedAsk: a resolution and a retraction write a block ask's
// state into its document and into each copy, so the check reads each copy, and an ask edit,
// which settles no copy, reads its source alone. C's tester's draft.md, a plain document the
// check does not hold, holds two block asks of its own: a person copies the first into C's spec and
// the second into C's notes.md, another plain document. C's tester's resolution and retraction of
// the first are refused with no version of either document, and its edit of the first is taken.
// Its resolution of the second, whose copy is in notes.md alone, is taken. (Its edit of its own
// block ask in C's spec is the inventory's row.) The source is a plain document and not C's
// plan.md: an issue born converted holds its plan in the issue's room beside its spec, where a
// block written into the spec is the spec's own and no copy, so only a document in a room of its
// own can be copied into the spec.
func TestSpecWriteChecksEachCopyOfAResolvedAsk(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{})
	spec := rig.c.documents["spec.md"].id
	copied, noted := specWriterBlockID(rig.c.name, "draft.md", "copied-tester"), specWriterBlockID(rig.c.name, "draft.md", "noted-tester")
	draft := decodeBody[uploadedArtifact](t, rig.requireTaken(t, "C's tester's upload of "+rig.c.key+"'s draft.md", http.StatusCreated,
		func(t *testing.T) *httptest.ResponseRecorder {
			return rig.upload(t, rig.cTester, rig.c, "draft.md", "The draft of the child issue.\n\n"+specWriterAsk(copied)+"\n"+specWriterAsk(noted))
		})).Artifact.ID
	rig.awaitAsks(t, rig.c, []string{copied, noted})
	rig.requireVersionWritten(t, "a person's copy of "+copied+" into "+rig.c.key+"'s spec", http.StatusOK, spec, true,
		func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, specWriterPerson, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
				"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": specWriterAsk(copied)}},
			})
		})
	notes := uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.c.key, "notes.md", "The notes of the child issue.\n\n"+specWriterAsk(noted)).Artifact.ID
	// The text routes serve a copy's source in the asks member and not on its block
	// (docs.TextWithAsks); the latest version carries it on the block.
	for document, block := range map[string]string{spec: copied, notes: noted} {
		awaitTextShows(t, rig.handler, document, "the copy of "+block, contains(`copied_from="`+rig.asks[block]+`"`), nil)
		awaitVersionText(t, rig.handler, document, "the copy of "+block, contains(`copied_from="`+rig.asks[block]+`"`))
	}
	rig.quiet(t)

	resolve := func(ask, kind string) func(t *testing.T) *httptest.ResponseRecorder {
		return func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.cTester, http.MethodPost, "/api/v1/asks/"+ask+"/resolve", map[string]any{"kind": kind, "reason": "The tester settles it."})
		}
	}
	for _, kind := range []string{"resolved", "retracted"} {
		rig.requireRefused(t, specWriteAttempt{
			what:    fmt.Sprintf("C's tester's resolution (%s) of its block ask %s in draft.md, copied into %s's spec", kind, copied, rig.c.key),
			send:    resolve(rig.asks[copied], kind),
			refusal: rig.refusalOn(rig.c, "spec.md", rig.cTester),
		})
		if state := rig.askState(t, rig.asks[copied]); state != "open" {
			t.Errorf("the refused resolution (%s) left the ask %s %s, want it open", kind, rig.asks[copied], state)
		}
	}
	rig.requireVersionWritten(t, "C's tester's edit of its block ask "+copied+" in draft.md, copied into "+rig.c.key+"'s spec", http.StatusOK, draft, true,
		func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.cTester, http.MethodPatch, "/api/v1/asks/"+rig.asks[copied], map[string]any{"question": "Which way does the draft go now?"})
		})
	rig.requireVersionWritten(t, "C's tester's resolution of its block ask "+noted+" in draft.md, copied into notes.md alone", http.StatusOK, notes, true,
		resolve(rig.asks[noted], "resolved"))
	if state := rig.askState(t, rig.asks[noted]); state != "resolved" {
		t.Errorf("C's tester's resolution left the ask %s %s, want it resolved", rig.asks[noted], state)
	}
}

// TestSpecWriteKeepsNotAuthorFirst: an ask edit answers 403 NOT_AUTHOR to a writer who did not ask
// the ask before the check reads anything, so C's tester's edit of the architect's block ask in
// C's spec is refused as it was before the check, with nothing written.
func TestSpecWriteKeepsNotAuthorFirst(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{})
	ask := rig.askIn(t, rig.c, "spec.md", "architect")
	rig.requireRefusedWith(t, http.StatusForbidden, "NOT_AUTHOR", specWriteAttempt{
		what: "C's tester's edit of the architect's block ask " + ask + " in " + rig.c.key + "'s spec",
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.cTester, http.MethodPatch, "/api/v1/asks/"+ask, map[string]any{"question": "Which way does the tester go?"})
		},
	})
}

// TestSpecWriteRefusesAWorkersFileUploadBeforeItsObject: where a file store holds files, an
// upload's object is written before its transaction opens, and nothing deletes an object that no
// row names (putUploadedFile). So the check refuses a worker's file upload of a first
// verification.md before its object is written: C's tester's upload is refused with 403
// DOCUMENT_WRITER_NOT_ARCHITECT and the file store holds no object more after it. The same upload
// by C's architect is taken and writes its object.
func TestSpecWriteRefusesAWorkersFileUploadBeforeItsObject(t *testing.T) {
	memory := filestest.NewMemory()
	server := newSpecWriterServer(t, testServerOptions{files: memory})
	rig := server.rig(t, specWriterRigShape{noVerification: true})
	before := memory.Len()
	rig.requireRefused(t, specWriteAttempt{
		what: rig.cTester + "'s first upload of " + rig.c.key + "'s verification.md as a file",
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.uploadFile(t, rig.cTester, rig.c, "verification.md", "text/plain", "A worker's verification.\n")
		},
		refusal: rig.refusalOn(rig.c, "verification.md", rig.cTester),
	})
	if objects := memory.Len(); objects != before {
		t.Errorf("the file store holds %d objects after C's tester's refused upload, want the %d it held before", objects, before)
	}
	rig.requireTaken(t, "C's architect's first upload of "+rig.c.key+"'s verification.md as a file", http.StatusCreated, func(t *testing.T) *httptest.ResponseRecorder {
		return rig.uploadFile(t, rig.cArchitect, rig.c, "verification.md", "text/plain", "C's architect's verification.\n")
	})
	if objects := memory.Len(); objects != before+1 {
		t.Errorf("the file store holds %d objects after C's architect's upload, want %d", objects, before+1)
	}
}

// specWriteTakenRows is the inventory's rows that write a version of an issue's document, run by a
// writer the check takes: each write answers as it does where nothing checks it, and the document
// holds the version it wrote, or no new one for an edit that changes nothing. Each runs on a rig of
// its own, as the inventory's rows do.
func specWriteTakenRows() []specWriteRow {
	spec := []string{"spec.md"}
	resolve := func(kind string) func(t *testing.T, at specWriteAt) {
		return func(t *testing.T, at specWriteAt) {
			ask := at.rig.askIn(t, at.issue, at.document, "architect")
			at.rig.requireVersionWritten(t, at.of("resolution ("+kind+") of the architect's block ask in"), http.StatusOK, at.artifact(), true,
				at.post("/api/v1/asks/"+ask+"/resolve", map[string]any{"kind": kind, "reason": "The writer settles it."}))
			if state := at.rig.askState(t, ask); state != "resolved" {
				t.Errorf("the resolution (%s) left the ask %s %s, want it resolved", kind, ask, state)
			}
		}
	}
	return []specWriteRow{
		{name: "upload a new version", documents: specWriterBoth, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireVersionWritten(t, at.of("upload of a new version of"), http.StatusCreated, at.artifact(), true,
				func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.upload(t, at.writer, at.issue, at.document, documentMarkdown(t, at.rig.handler, at.artifact())+"\nA paragraph of its own.\n")
				})
		}},
		{name: "first upload", documents: []string{"verification.md"}, shape: specWriterRigShape{noVerification: true},
			run: func(t *testing.T, at specWriteAt) {
				uploaded := decodeBody[uploadedArtifact](t, at.rig.requireTaken(t, at.of("first upload of"), http.StatusCreated,
					func(t *testing.T) *httptest.ResponseRecorder {
						return at.rig.upload(t, at.writer, at.issue, at.document, "A verification of its own.\n")
					}))
				if versions := at.rig.versionsOf(t, uploaded.Artifact.ID); versions != 1 {
					t.Errorf("%s: the new document holds %d versions, want 1", at.of("first upload of"), versions)
				}
			}},
		{name: "edit by id", documents: specWriterBoth, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireVersionWritten(t, at.of("edit of"), http.StatusOK, at.artifact(), true,
				at.post("/api/v1/artifacts/"+at.artifact()+"/edits", map[string]any{"ops": specWriterAppend}))
		}},
		{name: "edit by slug", documents: specWriterBoth, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireVersionWritten(t, at.of("edit by slug of"), http.StatusOK, at.artifact(), true,
				at.post("/api/v1/issues/"+at.issue.key+"/artifacts/"+at.issue.documents[at.document].slug+"/edits", map[string]any{"ops": specWriterAppend}))
		}},
		{name: "edit that changes nothing", documents: spec, run: func(t *testing.T, at specWriteAt) {
			first := specWriterFirstLine(at.issue.name, at.document)
			at.rig.requireVersionWritten(t, at.of("edit that replaces a line with itself of"), http.StatusOK, at.artifact(), false,
				at.post("/api/v1/artifacts/"+at.artifact()+"/edits", map[string]any{"ops": []map[string]string{{"op": "replace", "find": first, "with": first}}}))
		}},
		// An upload writes a version whatever text it holds (storeArtifact), so the upload of the
		// document's own text writes one too.
		{name: "upload that changes nothing", documents: spec, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireVersionWritten(t, at.of("upload with its own text of"), http.StatusCreated, at.artifact(), true,
				func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.upload(t, at.writer, at.issue, at.document, documentMarkdown(t, at.rig.handler, at.artifact()))
				})
		}},
		{name: "edit with a summary", documents: spec, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireVersionWritten(t, at.of("edit with a summary of"), http.StatusOK, at.artifact(), true,
				at.post("/api/v1/artifacts/"+at.artifact()+"/edits", map[string]any{"ops": specWriterAppend, "summary": "A named change"}))
		}},
		{name: "named version by id", documents: specWriterBoth, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireVersionWritten(t, at.of("named version of"), http.StatusCreated, at.artifact(), true,
				at.post("/api/v1/artifacts/"+at.artifact()+"/versions", map[string]any{"summary": "A named version"}))
		}},
		{name: "named version by slug", documents: specWriterBoth, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireVersionWritten(t, at.of("named version by slug of"), http.StatusCreated, at.artifact(), true,
				at.post("/api/v1/issues/"+at.issue.key+"/artifacts/"+at.issue.documents[at.document].slug+"/versions", map[string]any{"summary": "A named version"}))
		}},
		{name: "named version that changes nothing", documents: spec, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireVersionWritten(t, "a person's named version of "+at.issue.key+"'s "+at.document, http.StatusCreated, at.artifact(), true,
				func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.sendAs(t, specWriterPerson, http.MethodPost, "/api/v1/artifacts/"+at.artifact()+"/versions", map[string]any{"summary": "A person's named version"})
				})
			at.rig.requireVersionWritten(t, at.of("named version, right after the last one, of"), http.StatusCreated, at.artifact(), true,
				at.post("/api/v1/artifacts/"+at.artifact()+"/versions", map[string]any{"summary": "A named version"}))
		}},
		{name: "resolution", documents: specWriterBoth, run: resolve("resolved")},
		{name: "retraction", documents: specWriterBoth, run: resolve("retracted")},
		{name: "ask edit", documents: specWriterBoth, run: func(t *testing.T, at specWriteAt) {
			ask := at.rig.ownAsk(t, at.writer, at.issue, at.document)
			at.rig.requireVersionWritten(t, at.of("edit of its own block ask "+ask+" in"), http.StatusOK, at.artifact(), true,
				func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/asks/"+ask, map[string]any{"question": "Which way does the writer go now?"})
				})
		}},
	}
}

// specWriteOffLegionIssueRows is the inventory's rows that write an issue, and its edit of the
// parent's spec by the child's architect, run on a tree Legion does not run: each is taken and
// stores what it writes. The move moves a root with no label, which Legion does not run either.
func specWriteOffLegionIssueRows() []specWriteRow {
	spec := []string{"spec.md"}
	sibling := specWriterRigShape{sibling: true}
	testerOnR := []specWriteWriter{{"C's tester on R", specWriterChilds.session, specWriterChilds.issue}}
	// status sends the run's writer's write of the run's issue's status on route, PATCH or the
	// status route, with extra beside it, and fails t unless it is taken, the issue then has the
	// status, and its spec holds the version the settlement of the architect's block ask wrote.
	status := func(t *testing.T, at specWriteAt, route, status string, extra map[string]any) {
		t.Helper()
		body := map[string]any{"status": status}
		maps.Copy(body, extra)
		method, path := http.MethodPatch, "/api/v1/issues/"+at.issue.key
		if route == "status-change" {
			method, path = http.MethodPost, path+"/status-change"
		}
		at.rig.requireVersionWritten(t, fmt.Sprintf("%s's write of %s's status to %s (%s)", at.writer, at.issue.key, status, route), http.StatusOK,
			at.artifact(), true, func(t *testing.T) *httptest.ResponseRecorder { return at.rig.send(t, at.writer, method, path, body) })
		if stored := at.rig.issueRow(t, at.issue.key).status; stored != status {
			t.Errorf("after %s's write of its status %s is %s, want %s", at.writer, at.issue.key, stored, status)
		}
	}
	reopen := func(route string, extra map[string]any) func(t *testing.T, at specWriteAt) {
		return func(t *testing.T, at specWriteAt) {
			architect := at.rig.architectsOf(at.issue)[0]
			at.rig.requireTaken(t, architect+"'s close of "+at.issue.key, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, architect, http.MethodPatch, "/api/v1/issues/"+at.issue.key, map[string]any{"status": "done"})
			})
			status(t, at, route, "in_progress", extra)
		}
	}
	// reparent writes the parent of D to parent (nil for none), then edits D's spec, both as the
	// run's writer.
	reparent := func(parent func(rig *specWriterRig) *string) func(t *testing.T, at specWriteAt) {
		return func(t *testing.T, at specWriteAt) {
			to := parent(at.rig)
			at.rig.requireTaken(t, fmt.Sprintf("%s's write of %s's parent to %s", at.writer, at.issue.key, specWriterParentName(to)), http.StatusOK,
				func(t *testing.T) *httptest.ResponseRecorder {
					return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/issues/"+at.issue.key, map[string]any{"parent": to})
				})
			if stored := at.rig.issueRow(t, at.issue.key).parent; specWriterParentName(stored) != specWriterParentName(to) {
				t.Errorf("the parent write left %s under %s, want it under %s", at.issue.key, specWriterParentName(stored), specWriterParentName(to))
			}
			at.rig.requireEditTaken(t, at.writer, at.issue, "spec.md")
		}
	}
	return []specWriteRow{
		{name: "create under a parent", documents: spec, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireCreated(t, at.writer+"'s create of an issue under "+at.issue.key, at.issue.key,
				at.rig.creating(at.writer, "A worker's issue under "+at.issue.key, at.issue.key, nil))
		}},
		{name: "move under a parent", documents: spec, run: func(t *testing.T, at specWriteAt) {
			root := at.rig.requireCreated(t, at.writer+"'s create of a root", "", at.rig.creating(at.writer, "A worker's root", "", nil))
			at.rig.requireTaken(t, at.writer+"'s move of "+root+" under "+at.issue.key, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/issues/"+root, map[string]any{"parent": at.issue.key})
			})
			if parent := at.rig.issueRow(t, root).parent; parent == nil || *parent != at.issue.key {
				t.Errorf("the move left %s under %s, want it under %s", root, specWriterParentName(parent), at.issue.key)
			}
		}},
		{name: "close", documents: spec, run: func(t *testing.T, at specWriteAt) { status(t, at, "PATCH", "done", nil) }},
		{name: "reopen", documents: spec, run: reopen("PATCH", nil)},
		{name: "label removal", documents: spec, writers: testerOnR, shape: sibling, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireTaken(t, at.writer+"'s write of the labels of "+at.issue.key+" with no legion", http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPatch, "/api/v1/issues/"+at.issue.key, map[string]any{"labels": []string{}})
			})
			at.rig.requireEditTaken(t, at.writer, at.rig.d, "spec.md")
		}},
		{name: "parent clear", documents: spec, writers: []specWriteWriter{specWriterTesterOnSibling}, shape: sibling,
			run: reparent(func(*specWriterRig) *string { return nil })},
		{name: "parent clear to an unlabelled issue", documents: spec, writers: []specWriteWriter{specWriterTesterOnSibling},
			shape: specWriterRigShape{sibling: true, unlabelled: true},
			run:   reparent(func(rig *specWriterRig) *string { return &rig.u2.key })},
		{name: "status-change close", documents: spec, run: func(t *testing.T, at specWriteAt) { status(t, at, "status-change", "done", nil) }},
		{name: "status-change reopen", documents: spec, run: reopen("status-change", nil)},
		{name: "status-change reopen with include_children", documents: spec, run: reopen("status-change", map[string]any{"include_children": true})},
		{name: "status-change unlink", documents: spec, writers: testerOnR, shape: sibling, run: func(t *testing.T, at specWriteAt) {
			at.rig.requireTaken(t, at.writer+"'s status change of "+at.issue.key+" with unlink_children", http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPost, "/api/v1/issues/"+at.issue.key+"/status-change",
					map[string]any{"status": "in_progress", "unlink_children": true})
			})
			for _, child := range []string{at.rig.c.key, at.rig.d.key} {
				if parent := at.rig.issueRow(t, child).parent; parent != nil {
					t.Errorf("the unlink left %s under %s, want it a root", child, *parent)
				}
			}
			at.rig.requireEditTaken(t, at.writer, at.rig.d, "spec.md")
		}},
		{name: "child's architect on the parent", documents: spec, writers: []specWriteWriter{specWriterChildArchitect},
			run: func(t *testing.T, at specWriteAt) { at.rig.requireEditTaken(t, at.writer, at.issue, at.document) }},
	}
}

// of is what a failure line calls the run's write: the writer's write of the run's issue's
// document.
func (at specWriteAt) of(write string) string {
	return fmt.Sprintf("%s's %s %s's %s", at.writer, write, at.issue.key, at.document)
}

// post is the run's writer's POST of body on path.
func (at specWriteAt) post(path string, body map[string]any) func(t *testing.T) *httptest.ResponseRecorder {
	return func(t *testing.T) *httptest.ResponseRecorder {
		return at.rig.send(t, at.writer, http.MethodPost, path, body)
	}
}

// newSpecWriterBrokerServer is newSpecWriterServer on a server that also takes broker tokens, which
// the stub broker of the answered broker rig decides (newBrokerTokenRigOn).
func newSpecWriterBrokerServer(t *testing.T) (*specWriterServer, *brokerTokenRig) {
	t.Helper()
	brokered := newBrokerTokenRigOn(t, testServerOptions{})
	return &specWriterServer{handler: brokered.handler, database: brokered.database, served: brokered.served}, brokered
}

// specWriterPerson is the writer a rig's write names for a person: alice, signed in at a browser
// (dispatchRequest). Every other writer the rig names is a session on the shared token.
const specWriterPerson = "alice"

// sendAs sends body as writer: the person alice for specWriterPerson, and else the session writer
// on the shared token (send).
func (rig *specWriterRig) sendAs(t *testing.T, writer, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if writer == specWriterPerson {
		return dispatchRequest(t, rig.handler, method, path, body, specWriterPerson)
	}
	return rig.send(t, writer, method, path, body)
}

// creating is writer's create (sendAs) of an issue of P titled title with a spec of its own, under
// parent when it is not empty and with labels.
func (rig *specWriterRig) creating(writer, title, parent string, labels []string) func(t *testing.T) *httptest.ResponseRecorder {
	body := map[string]any{"project": rig.project, "title": title, "spec": "The spec of " + title + ".\n", "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	if labels != nil {
		body["labels"] = labels
	}
	return func(t *testing.T) *httptest.ResponseRecorder {
		return rig.sendAs(t, writer, http.MethodPost, "/api/v1/issues", body)
	}
}

// requireCreated is requireTaken of a create of an issue, which also fails t unless the issue it
// answers is stored under parent (a root for "") with a version of its spec. It answers the key.
func (rig *specWriterRig) requireCreated(t *testing.T, what, parent string, send func(t *testing.T) *httptest.ResponseRecorder) string {
	t.Helper()
	created := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, rig.requireTaken(t, what, http.StatusCreated, send))
	want := "no parent"
	if parent != "" {
		want = parent
	}
	if stored := specWriterParentName(rig.issueRow(t, created.Key).parent); stored != want {
		t.Errorf("%s: %s is stored under %s, want it under %s", what, created.Key, stored, want)
	}
	if versions := rig.versionsOf(t, created.PrimaryArtifactID); versions == 0 {
		t.Errorf("%s: the spec of %s holds no version", what, created.Key)
	}
	return created.Key
}

// specWriterAnchor is the anchor of a comment, an ask or a suggestion on quote in the document id.
func specWriterAnchor(id, quote string) map[string]any {
	return map[string]any{"artifact": id, "quote": quote}
}

// ownAsk writes a block ask of session's own at the end of issue's document, by an edit the check
// takes, and answers its id once settlement has indexed it.
func (rig *specWriterRig) ownAsk(t *testing.T, session string, issue specWriterIssue, document string) string {
	t.Helper()
	block := specWriterBlockID(issue.name, document, "own")
	written := rig.send(t, session, http.MethodPost, "/api/v1/artifacts/"+issue.documents[document].id+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": specWriterAsk(block)}},
	})
	if written.Code != http.StatusOK {
		t.Fatalf("rig: %s writes its block ask into %s's %s: status=%d body=%s", session, issue.key, document, written.Code, written.Body.String())
	}
	rig.awaitAsks(t, issue, []string{block})
	rig.quiet(t)
	return rig.asks[block]
}

// requireTaken sends a write the check takes, waits until the rig is quiet, and fails t unless the
// write answered status. It answers the response, for the caller's checks of what the write stored.
func (rig *specWriterRig) requireTaken(t *testing.T, what string, status int, send func(t *testing.T) *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	response := send(t)
	rig.quiet(t)
	if response.Code != status {
		t.Fatalf("%s: status=%d body=%s, want %d", what, response.Code, clippedBody(response), status)
	}
	return response
}

// requireVersionWritten is requireTaken of a write of the document id, which also fails t unless
// the document then holds more versions than before the write, or as many for a write that
// changes nothing (wrote false).
func (rig *specWriterRig) requireVersionWritten(t *testing.T, what string, status int, id string, wrote bool, send func(t *testing.T) *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	before := rig.versionsOf(t, id)
	response := rig.requireTaken(t, what, status, send)
	switch after := rig.versionsOf(t, id); {
	case wrote && after <= before:
		t.Errorf("%s: the document %s holds %d versions after it, want more than the %d before it", what, id, after, before)
	case !wrote && after != before:
		t.Errorf("%s: the document %s holds %d versions after it, want the %d before it", what, id, after, before)
	}
	return response
}

// requireEditTaken is writer's edit of issue's document (editAttempt), which the check takes: it
// answers 200 and the document holds a new version.
func (rig *specWriterRig) requireEditTaken(t *testing.T, writer string, issue specWriterIssue, document string) {
	t.Helper()
	edit := rig.editAttempt(writer, issue, document)
	rig.requireVersionWritten(t, edit.what, http.StatusOK, issue.documents[document].id, true, edit.send)
}

// requireRefusedWith sends attempt, then fails t unless it answered status with code, and the
// project holds the issues, the document versions and the issue events it held before it: a
// refusal other than the check's, which the check leaves as it was.
func (rig *specWriterRig) requireRefusedWith(t *testing.T, status int, code string, attempt specWriteAttempt) {
	t.Helper()
	before := rig.snapshot(t)
	response := attempt.send(t)
	rig.quiet(t)
	if refusal := decodeBody[refusalAnswer](t, response); response.Code != status || refusal.Code != code {
		t.Errorf("%s: status=%d body=%s, want %d %s", attempt.what, response.Code, clippedBody(response), status, code)
	}
	rig.requireUnchanged(t, attempt.what, before)
}

// versionsOf is how many versions the document id holds.
func (rig *specWriterRig) versionsOf(t *testing.T, id string) int {
	t.Helper()
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select count(*) from artifact_versions where artifact_id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count the versions of %s: %v", id, err)
	}
	return count
}

// commentState is a comment's row as it is stored: whether it is resolved, and, for a suggestion,
// whether it was accepted, null while it is not settled.
func (rig *specWriterRig) commentState(t *testing.T, id string) (resolved bool, accepted *bool) {
	t.Helper()
	if err := rig.database.Pool.QueryRow(context.Background(), `select resolved, (suggestion ->> 'accepted')::boolean from comments where id = $1`, id).
		Scan(&resolved, &accepted); err != nil {
		t.Fatalf("rig: read the comment %s: %v", id, err)
	}
	return resolved, accepted
}

// TestArchitectWriteOnAProvenProjectNeedsAProvenSession: while P's proven-sessions setting is on,
// an architect's session passes the check only on a credential that proves it. C's architect's
// edit of C's spec on the shared token is refused with the proof refusal, and so is C's tester's
// edit of C's Verification document on the shared token naming C's architect's session; on a
// broker token that proves C's tester, a body naming C's architect is 403 ACTOR_SESSION_MISMATCH.
// C's architect's edit on a broker token that proves its session is taken, and so is a person's.
// The close of C by the Legion daemon of P is refused on the shared token and taken on a broker
// token that proves the daemon's session. Once a person turns the setting off, C's architect's
// edit and the daemon's close on the shared token are taken.
func TestArchitectWriteOnAProvenProjectNeedsAProvenSession(t *testing.T) {
	server, brokered := newSpecWriterBrokerServer(t)
	rig := server.rig(t, specWriterRigShape{})
	spec := rig.c.documents["spec.md"].id
	edits := "/api/v1/artifacts/" + spec + "/edits"
	daemon := legionDaemonSessionOf(rig.c.key)
	shared := func(session string) specWriterSender {
		return func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			return rig.send(t, session, method, path, body)
		}
	}
	closing := func(send specWriterSender) func(t *testing.T) *httptest.ResponseRecorder {
		return func(t *testing.T) *httptest.ResponseRecorder {
			return send(t, http.MethodPatch, "/api/v1/issues/"+rig.c.key, map[string]any{"status": "done"})
		}
	}
	requireStatus := func(t *testing.T, what, want string) {
		t.Helper()
		if status := rig.issueRow(t, rig.c.key).status; status != want {
			t.Errorf("%s: %s is %s, want %s", what, rig.c.key, status, want)
		}
	}
	rig.setProvenSessions(t, rig.project, true)

	edit := rig.editAttempt(rig.cArchitect, rig.c, "spec.md")
	edit.what = "C's architect's edit of " + rig.c.key + "'s spec.md on the shared token"
	rig.requireUnproven(t, edit)
	named := rig.editAttempt(rig.cArchitect, rig.c, "verification.md")
	named.what = "C's tester's edit of " + rig.c.key + "'s verification.md on the shared token, naming C's architect's session"
	rig.requireUnproven(t, named)
	tester := rig.provenSender(t, brokered, rig.cTester)
	rig.requireRefusedWith(t, http.StatusForbidden, "ACTOR_SESSION_MISMATCH", specWriteAttempt{
		what: "C's tester's edit of " + rig.c.key + "'s spec.md on a broker token that proves its session, naming C's architect",
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return tester(t, http.MethodPost, edits, map[string]any{
				"ops": specWriterAppend, "actor": map[string]any{"kind": "session", "id": rig.cArchitect},
			})
		},
	})
	architect := rig.provenSender(t, brokered, rig.cArchitect)
	rig.requireVersionWritten(t, "C's architect's edit of "+rig.c.key+"'s spec.md on a broker token that proves its session",
		http.StatusOK, spec, true, func(t *testing.T) *httptest.ResponseRecorder {
			return architect(t, http.MethodPost, edits, map[string]any{"ops": specWriterAppend})
		})
	rig.requireEditTaken(t, specWriterPerson, rig.c, "spec.md")

	open := rig.issueRow(t, rig.c.key).status
	rig.requireUnproven(t, specWriteAttempt{
		what:    "the close of " + rig.c.key + " by " + daemon + " on the shared token",
		send:    closing(shared(daemon)),
		refusal: specWriteRefusal{issues: []string{rig.c.key}, writer: daemon},
	})
	requireStatus(t, "after the refused close on the shared token", open)
	closed := "the close of " + rig.c.key + " by " + daemon + " on a broker token that proves its session"
	rig.requireVersionWritten(t, closed, http.StatusOK, spec, true, closing(rig.provenSender(t, brokered, daemon)))
	requireStatus(t, "after "+closed, "done")

	rig.requireTaken(t, "a person's reopen of "+rig.c.key, http.StatusOK, func(t *testing.T) *httptest.ResponseRecorder {
		return rig.sendAs(t, specWriterPerson, http.MethodPatch, "/api/v1/issues/"+rig.c.key, map[string]any{"status": "in_progress"})
	})
	rig.setProvenSessions(t, rig.project, false)
	rig.requireEditTaken(t, rig.cArchitect, rig.c, "spec.md")
	closed = "the close of " + rig.c.key + " by " + daemon + " on the shared token, with the setting off"
	rig.requireVersionWritten(t, closed, http.StatusOK, spec, true, closing(shared(daemon)))
	requireStatus(t, "after "+closed, "done")
}

// TestRoleHolderPublicationOnAProvenProjectNeedsTheDaemonsProof: while a project's
// proven-sessions setting is on, a role-holder publication passes only on a credential that proves
// the session legion-daemon:<project>. In P, the shared token's publication as the Legion daemon
// of P is 403 ROLE_HOLDER_NOT_PROVEN naming the project, and C's stored set stays as it was; a
// broker token that proves the daemon's session publishes; a broker token that proves C's tester
// and names the daemon in its body is 403 ACTOR_SESSION_MISMATCH. Once a person turns the setting
// off, the shared token publishes again. In Q, which no publication marked, the setting on, the
// shared token's refused publication marks nothing, so a worker's edit of W's spec is still taken.
func TestRoleHolderPublicationOnAProvenProjectNeedsTheDaemonsProof(t *testing.T) {
	server, brokered := newSpecWriterBrokerServer(t)
	rig := server.rig(t, specWriterRigShape{unmarked: true})
	daemon := legionDaemonSessionOf(rig.c.key)
	path := "/api/v1/issues/" + rig.c.key + "/role-holders"
	published := []issueRoleHolder{{Role: "architect", Session: rig.cArchitect}, {Role: "tester", Session: rig.cTester}}
	next := []issueRoleHolder{{Role: "architect", Session: rig.cArchitect}, {Role: "reviewer", Session: "ses-c-reviewer-" + strings.ToLower(rig.project)}}
	rig.setProvenSessions(t, rig.project, true)

	rig.requireRoleHoldersNotProven(t, rig.c.key, next)
	rig.requireRoleHolders(t, "after the shared token's refused publication", rig.c.key, published)
	proven := "the publication on a broker token that proves " + daemon
	if response := rig.provenSender(t, brokered, daemon)(t, http.MethodPut, path, map[string]any{"holders": next}); response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s, want 200", proven, response.Code, clippedBody(response))
	}
	rig.requireRoleHolders(t, "after "+proven, rig.c.key, next)
	mismatch := rig.provenSender(t, brokered, rig.cTester)(t, http.MethodPut, path, map[string]any{
		"holders": published, "actor": map[string]any{"kind": "session", "id": daemon},
	})
	if code, _ := refusalOf(t, mismatch); mismatch.Code != http.StatusForbidden || code != "ACTOR_SESSION_MISMATCH" {
		t.Errorf("the publication on a broker token that proves C's tester, naming %s: status=%d body=%s, want 403 ACTOR_SESSION_MISMATCH",
			daemon, mismatch.Code, clippedBody(mismatch))
	}
	rig.requireRoleHolders(t, "after the publication on a broker token that proves C's tester", rig.c.key, next)

	rig.setProvenSessions(t, rig.project, false)
	rig.publish(t, rig.c.key, published)
	rig.requireRoleHolders(t, "after the shared token's publication with the setting off", rig.c.key, published)

	rig.setProvenSessions(t, rig.q, true)
	rig.requireRoleHoldersNotProven(t, rig.x.key, []issueRoleHolder{})
	if rig.marked(t, rig.q) {
		t.Errorf("the refused publication on %s marked %s, want it unmarked", rig.x.key, rig.q)
	}
	rig.requireEditTaken(t, rig.cTester, rig.w, "spec.md")
}

// specWriterSender sends body on path by method, on one credential.
type specWriterSender func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder

// provenSender is a sender on a broker token that proves session and is scoped to the rig's
// project P, the credential a pane of P holds once its daemon writes with proven sessions: the
// token names the session, so a body need not. brokered is the rig's server's broker
// (newSpecWriterBrokerServer).
func (rig *specWriterRig) provenSender(t *testing.T, brokered *brokerTokenRig, session string) specWriterSender {
	t.Helper()
	caller := newBrokerCaller(t)
	token := "a-broker-token-proving-" + strings.ReplaceAll(session, ":", "-")
	says := liveNamedToken(caller.jkt)
	says.Session, says.Projects = session, []string{rig.project}
	brokered.broker.Says(token, says)
	return func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return brokered.serve(caller.presenting(t, method, path, token, body))
	}
}

// setProvenSessions turns project's proven-sessions setting on, on the shared token as the Legion
// daemon of the project, or off, as a person, the one writer the DELETE takes. It fails t unless
// the route answers the project with the setting as asked.
func (rig *specWriterRig) setProvenSessions(t *testing.T, project string, on bool) {
	t.Helper()
	path := "/api/v1/projects/" + project + "/proven-sessions"
	var response *httptest.ResponseRecorder
	if on {
		response = rig.send(t, legionDaemonSessionPrefix+project, http.MethodPut, path, nil)
	} else {
		response = dispatchRequest(t, rig.handler, http.MethodDelete, path, nil, specWriterPerson)
	}
	if response.Code != http.StatusOK || decodeBody[model.Project](t, response).ProvenSessions != on {
		t.Fatalf("rig: turn %s's proven-sessions setting to %t: status=%d body=%s", project, on, response.Code, clippedBody(response))
	}
}

// requireUnproven sends attempt, then fails t unless the check refused it with its proof refusal -
// 403 DOCUMENT_WRITER_NOT_ARCHITECT naming the issue, the document when the refusal names one, the
// writer's session and the project, and saying that the request's credential does not prove that
// session - and the project holds what it held before it.
func (rig *specWriterRig) requireUnproven(t *testing.T, attempt specWriteAttempt) {
	t.Helper()
	var response *httptest.ResponseRecorder
	send := attempt.send
	attempt.send = func(t *testing.T) *httptest.ResponseRecorder {
		response = send(t)
		return response
	}
	rig.requireRefusedWith(t, http.StatusForbidden, specWriterRefusalCode, attempt)
	if response.Code != http.StatusForbidden {
		return
	}
	message := decodeBody[refusalAnswer](t, response).Error
	want := attempt.refusal
	if !slices.ContainsFunc(want.issues, func(key string) bool { return specWriterNames(message, key) }) {
		t.Errorf("%s: the refusal %q names none of %v, the issue it writes", attempt.what, message, want.issues)
	}
	for _, name := range []string{want.document, want.writer, rig.project} {
		if name != "" && !specWriterNames(message, name) {
			t.Errorf("%s: the refusal %q does not name %s", attempt.what, message, name)
		}
	}
	if !strings.Contains(message, "records proven sessions") || !strings.Contains(message, "does not prove session "+want.writer) {
		t.Errorf("%s: the refusal %q does not say that the project records proven sessions and the credential does not prove session %s",
			attempt.what, message, want.writer)
	}
}

// requireRoleHoldersNotProven sends holders on the shared token as the Legion daemon of issue's
// project, and fails t unless the route answers 403 ROLE_HOLDER_NOT_PROVEN naming the project and
// the daemon's session.
func (rig *specWriterRig) requireRoleHoldersNotProven(t *testing.T, issue string, holders []issueRoleHolder) {
	t.Helper()
	project, _, _ := strings.Cut(issue, "-")
	response := putRoleHoldersAs(t, rig.handler, issue, map[string]any{"holders": holders, "actor": daemonSessionOf(project)})
	code, message := refusalOf(t, response)
	if response.Code != http.StatusForbidden || code != "ROLE_HOLDER_NOT_PROVEN" {
		t.Errorf("the shared token's publication on %s: status=%d body=%s, want 403 ROLE_HOLDER_NOT_PROVEN", issue, response.Code, clippedBody(response))
		return
	}
	for _, name := range []string{project, legionDaemonSessionOf(issue)} {
		if !specWriterNames(message, name) {
			t.Errorf("the shared token's publication on %s: the refusal %q does not name %s", issue, message, name)
		}
	}
}

// requireRoleHolders fails t unless issue's stored set of role holders is want, in workflow order.
func (rig *specWriterRig) requireRoleHolders(t *testing.T, what, issue string, want []issueRoleHolder) {
	t.Helper()
	stored := readRoleHolders(t, rig.handler, issue).Holders
	held := make([]issueRoleHolder, len(stored))
	for index, holder := range stored {
		held[index] = issueRoleHolder{Role: holder.Role, Session: holder.Session}
	}
	if !slices.Equal(held, want) {
		t.Errorf("%s: %s's role holders are %v, want %v", what, issue, held, want)
	}
}

// marked reports whether project_role_holder_marks holds project.
func (rig *specWriterRig) marked(t *testing.T, project string) bool {
	t.Helper()
	var marked bool
	if err := rig.database.Pool.QueryRow(context.Background(), `select exists (select 1 from project_role_holder_marks where project_key = $1)`, project).
		Scan(&marked); err != nil {
		t.Fatalf("rig: read whether %s is marked: %v", project, err)
	}
	return marked
}
