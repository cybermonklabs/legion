package api

// Case (f) of INFRA-4799's R8: a status change that a token naming one project makes does not cross
// an issue it leaves out. A step whose issue is in a private project that the token does not name
// is left out of the cascade, and so is each issue that the cascade reaches only through such an
// issue: a descendant below it for include_children, an ancestor above it for include_parent. The
// call moves or unlinks none of them, and neither its answer nor its dry run names them. A token
// that names the private project moves and names all of them. An entry of the caller's own issue
// whose parent is private keeps its parent field, as GET shows it.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// cascadeFamily is the three issues of one case: own is of the token's project, secret of the
// private project, and far of a third, public project. far stands below secret (include_children)
// or above it (include_parent).
type cascadeFamily struct{ own, secret, far string }

// cascadeTitles are the titles of the issues that the cascade leaves out, which no answer of a
// scoped token holds.
type cascadeTitles struct{ secret, far string }

// newCascadeRig is the private-project rig with the public project PUB beside the token's project
// and the private one.
func newCascadeRig(t *testing.T) *privateProjectRig {
	t.Helper()
	rig := newPrivateProjectRig(t, testServerOptions{})
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "PUB", "name": "PUB"}); created.Code != http.StatusCreated {
		t.Fatalf("create project PUB: status=%d body=%s", created.Code, created.Body.String())
	}
	return rig
}

// cascadeBelow files own (the token's project), its child secret (the private project) and
// secret's child far (PUB).
func (rig *privateProjectRig) cascadeBelow(t *testing.T, name string) (cascadeFamily, cascadeTitles) {
	t.Helper()
	titles := cascadeTitles{secret: name + ", the private child", far: name + ", the public grandchild"}
	own := rig.issue(t, brokerTestProject, name+", the issue", nil)
	secret := rig.issue(t, privateTestProject, titles.secret, map[string]any{"parent": own})
	far := rig.issue(t, "PUB", titles.far, map[string]any{"parent": secret})
	return cascadeFamily{own, secret, far}, titles
}

// cascadeAbove files far (PUB), its child secret (the private project) and secret's child own
// (the token's project).
func (rig *privateProjectRig) cascadeAbove(t *testing.T, name string) (cascadeFamily, cascadeTitles) {
	t.Helper()
	titles := cascadeTitles{secret: name + ", the private parent", far: name + ", the public grandparent"}
	far := rig.issue(t, "PUB", titles.far, nil)
	secret := rig.issue(t, privateTestProject, titles.secret, map[string]any{"parent": far})
	own := rig.issue(t, brokerTestProject, name+", the issue", map[string]any{"parent": secret})
	return cascadeFamily{own, secret, far}, titles
}

// cascadeStatus is the status of the issue key as a person reads it.
func (rig *privateProjectRig) cascadeStatus(t *testing.T, key string) string {
	t.Helper()
	read := rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+key, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, read.Code, read.Body.String())
	}
	return decodeBody[model.Issue](t, read).Status
}

// cascadeChanged is the keys of the entries that a status change's answer lists as changed.
func cascadeChanged(t *testing.T, response string) []string {
	t.Helper()
	var answer struct {
		Changed []struct {
			Key string `json:"key"`
		} `json:"changed"`
	}
	if err := json.Unmarshal([]byte(response), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", response, err)
	}
	keys := []string{}
	for _, entry := range answer.Changed {
		keys = append(keys, entry.Key)
	}
	return keys
}

// TestAScopedIncludeChildrenStopsAtAPrivateChild: an issue of the token's own project has a child in
// the private project, which has a public child of its own. A scoped status change of the own issue
// to todo with include_children, and its dry run, move neither the private child nor the public
// grandchild, and their answers name neither: the cascade does not walk through the private child.
// The token that names the private project moves both and lists both.
func TestAScopedIncludeChildrenStopsAtAPrivateChild(t *testing.T) {
	t.Parallel()
	rig := newCascadeRig(t)
	scoped, scopedTitles := rig.cascadeBelow(t, "Picked up over a private child")
	named, _ := rig.cascadeBelow(t, "Picked up by the naming token")
	rig.makePrivate(t)
	body := map[string]any{"status": "todo", "include_children": true}

	for _, call := range []struct {
		name    string
		members map[string]any
	}{{"the dry run", map[string]any{"dry_run": true}}, {"the call", nil}} {
		answer := rig.send(t, scopedToken, http.MethodPost, "/api/v1/issues/"+scoped.own+"/status-change", withMembers(body, call.members))
		text := answer.Body.String()
		if answer.Code != http.StatusOK && answer.Code != http.StatusConflict {
			t.Fatalf("%s: status=%d body=%s", call.name, answer.Code, text)
		}
		for _, hidden := range []string{scoped.secret, scoped.far, scopedTitles.secret, scopedTitles.far} {
			if strings.Contains(text, hidden) {
				t.Errorf("%s names %q, an issue the cascade leaves out: %s", call.name, hidden, text)
			}
		}
	}
	for key, want := range map[string]string{scoped.own: "todo", scoped.secret: model.IssueStatusTriage, scoped.far: model.IssueStatusTriage} {
		if got := rig.cascadeStatus(t, key); got != want {
			t.Errorf("after the scoped call %s is %s, want %s", key, got, want)
		}
	}

	moved := rig.send(t, namingToken, http.MethodPost, "/api/v1/issues/"+named.own+"/status-change", body)
	requireTaken(t, "the status change of the token that names the private project", moved, http.StatusOK)
	if keys := cascadeChanged(t, moved.Body.String()); !slices.Contains(keys, named.secret) || !slices.Contains(keys, named.far) {
		t.Errorf("the naming token's answer lists %v as changed, want %s and %s among them", keys, named.secret, named.far)
	}
	for _, key := range []string{named.own, named.secret, named.far} {
		if got := rig.cascadeStatus(t, key); got != "todo" {
			t.Errorf("after the naming token's call %s is %s, want todo", key, got)
		}
	}
}

// TestAScopedIncludeParentStopsAtAPrivateParent: an issue of the token's own project has a private
// parent, which has a public parent of its own. A scoped status change of the own issue to
// in_progress with include_parent, and its dry run, move neither parent, and their answers name
// neither parent's title and list neither as changed: the cascade does not walk through the private
// parent. The own issue's entry keeps its parent field, as GET shows it. The token that names the
// private project moves both parents and lists both.
func TestAScopedIncludeParentStopsAtAPrivateParent(t *testing.T) {
	t.Parallel()
	rig := newCascadeRig(t)
	scoped, scopedTitles := rig.cascadeAbove(t, "Started under a private parent")
	named, _ := rig.cascadeAbove(t, "Started by the naming token")
	rig.makePrivate(t)
	body := map[string]any{"status": "in_progress", "include_parent": true}

	for _, call := range []struct {
		name    string
		members map[string]any
	}{{"the dry run", map[string]any{"dry_run": true}}, {"the call", nil}} {
		answer := rig.send(t, scopedToken, http.MethodPost, "/api/v1/issues/"+scoped.own+"/status-change", withMembers(body, call.members))
		text := answer.Body.String()
		if answer.Code != http.StatusOK && answer.Code != http.StatusConflict {
			t.Fatalf("%s: status=%d body=%s", call.name, answer.Code, text)
		}
		for _, hidden := range []string{scoped.far, scopedTitles.secret, scopedTitles.far} {
			if strings.Contains(text, hidden) {
				t.Errorf("%s names %q, an issue the cascade leaves out: %s", call.name, hidden, text)
			}
		}
		for _, key := range cascadeChanged(t, text) {
			if key == scoped.secret || key == scoped.far {
				t.Errorf("%s lists %s as changed, an issue the cascade leaves out: %s", call.name, key, text)
			}
		}
	}
	for key, want := range map[string]string{scoped.secret: model.IssueStatusTriage, scoped.far: model.IssueStatusTriage} {
		if got := rig.cascadeStatus(t, key); got != want {
			t.Errorf("after the scoped call %s is %s, want %s", key, got, want)
		}
	}

	moved := rig.send(t, namingToken, http.MethodPost, "/api/v1/issues/"+named.own+"/status-change", body)
	requireTaken(t, "the status change of the token that names the private project", moved, http.StatusOK)
	if keys := cascadeChanged(t, moved.Body.String()); !slices.Contains(keys, named.secret) || !slices.Contains(keys, named.far) {
		t.Errorf("the naming token's answer lists %v as changed, want %s and %s among them", keys, named.secret, named.far)
	}
	for _, key := range []string{named.own, named.secret, named.far} {
		if got := rig.cascadeStatus(t, key); got != "in_progress" {
			t.Errorf("after the naming token's call %s is %s, want in_progress", key, got)
		}
	}
}
