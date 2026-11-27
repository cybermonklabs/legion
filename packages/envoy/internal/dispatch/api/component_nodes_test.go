package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// componentNodeFiles is the model the test imports first: legion holds daemon, envoy, store and
// relay, and daemon depends on envoy and on store.
func componentNodeFiles() map[string]string {
	return map[string]string{
		"legion.md": "---\ntitle: Legion\n---\nThe swarm.\n",
		"daemon.md": "---\ntitle: Daemon\nparent: legion\ndepends_on: [envoy, store]\n---\nThe daemon.\n",
		"envoy.md":  "---\ntitle: Envoy\nparent: legion\n---\nRouting.\n",
		"store.md":  "---\ntitle: Store\nparent: legion\n---\nThe store.\n",
		"relay.md":  "---\ntitle: Relay\nparent: legion\n---\nThe relay.\n",
	}
}

// componentStore is what the store and the three projections hold of project CORE's components at
// one moment, each as a sorted list of lines: the components rows with the node each names, the
// component_depends rows with theirs, the component nodes with their supertag, state and project,
// the current part_of and depends_on out of each component node and the current affects out of
// each issue node, issue's issue_component_members rows with theirs, and what graph_edges answers
// of the three relations.
type componentStore struct {
	rows, depends, nodes, relations, members, edges []string
}

// readComponentStore reads componentStore for project CORE and issue.
func readComponentStore(t *testing.T, database *store.Store, issue string) componentStore {
	t.Helper()
	read := func(what, statement string, args ...any) []string {
		t.Helper()
		lines, err := readStrings(context.Background(), database.Pool, statement, args...)
		if err != nil {
			t.Fatalf("read %s: %v", what, err)
		}
		slices.Sort(lines)
		return lines
	}
	return componentStore{
		rows: read("the components rows", `
			select id || ' ' || coalesce(node_kind, '-') || ' ' || coalesce(node_address, '-')
			from components where project_key = 'CORE'`),
		depends: read("the component_depends rows", `
			select from_id || ' depends_on ' || to_id || ' ' || coalesce(node_kind, '-') || ' ' || coalesce(node_address, '-')
			from component_depends where project_key = 'CORE'`),
		nodes: read("the component nodes", `
			select id || ' ' || supertag || ' ' || state || ' ' || coalesce(project_key, '-')
			from graph_nodes where kind = 'component' and id like 'CORE/%'`),
		relations: read("the current relations", `
			select held.from_id || ' ' || held.kind || ' ' || held.to_id from (
				select distinct on (t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id) t.from_id, t.kind, t.to_id, t.asserted
				from graph_tuples t
				where t.to_kind = 'component' and t.to_id like 'CORE/%'
				  and ((t.from_kind = 'component' and t.kind in ('part_of', 'depends_on')) or (t.from_kind = 'issue' and t.kind = 'affects'))
				order by t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.seq desc
			) held where held.asserted`),
		members: read("the issue's issue_component_members rows", `
			select component_id || ' ' || coalesce(node_kind, '-') || ' ' || coalesce(node_address, '-')
			from issue_component_members where issue_key = $1`, issue),
		edges: read("graph_edges", `
			select from_id || ' ' || kind || ' ' || to_id from graph_edges
			where to_kind = 'component' and to_id like 'CORE/%' and kind in ('part_of', 'depends_on', 'affects')`),
	}
}

// TestAComponentIsANodeWithItsThreeTupleKinds holds the writers of a component and its relations
// (R9 of the issue-tree spec): a component is a node of kind and supertag component, its part_of
// to its parent component, its depends_on to another component and an issue's affects to a
// component are tuples, and each projection row names the node it is the projection of. An import
// writes each component's node, its part_of and its depends_on, and names the node on every
// components and component_depends row; a re-import that drops a dependency and a component
// retracts the dropped depends_on and states the dropped component gone, with its part_of
// retracted; an issue's components set, narrowed and cleared append and retract its affects, its
// issue_component_members rows naming its node; and the removal of the source states every
// component it held gone, with each part_of and depends_on retracted. At each step graph_edges
// answers each standing relation once, from the store, and no dropped one.
func TestAComponentIsANodeWithItsThreeTupleKinds(t *testing.T) {
	fake := &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read"},
		repos:         map[string]*fakeArchitectureRepo{"legion/arch": {commit: shaOne, files: componentNodeFiles()}},
	}
	handler, database := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")
	putSource(t, handler, "CORE", "legion/arch", "main")
	issue := createComponentIssue(t, handler, "Holds no component yet", nil, nil).Key
	require := func(stage string, want componentStore) {
		t.Helper()
		got := readComponentStore(t, database, issue)
		for _, side := range []struct {
			what      string
			got, want []string
		}{
			{"the components rows", got.rows, want.rows},
			{"the component_depends rows", got.depends, want.depends},
			{"the component nodes", got.nodes, want.nodes},
			{"the current relations", got.relations, want.relations},
			{"the issue's issue_component_members rows", got.members, want.members},
			{"graph_edges", got.edges, want.edges},
		} {
			if !slices.Equal(side.got, side.want) {
				t.Errorf("%s %s are %q, want %q", stage, side.what, side.got, side.want)
			}
		}
	}

	syncComponentTree(t, handler, "legion/arch")
	imported := componentStore{
		rows: []string{
			"daemon component CORE/daemon", "envoy component CORE/envoy", "legion component CORE/legion",
			"relay component CORE/relay", "store component CORE/store",
		},
		depends: []string{"daemon depends_on envoy component CORE/daemon", "daemon depends_on store component CORE/daemon"},
		nodes: []string{
			"CORE/daemon component live CORE", "CORE/envoy component live CORE", "CORE/legion component live CORE",
			"CORE/relay component live CORE", "CORE/store component live CORE",
		},
		relations: []string{
			"CORE/daemon depends_on CORE/envoy", "CORE/daemon depends_on CORE/store", "CORE/daemon part_of CORE/legion",
			"CORE/envoy part_of CORE/legion", "CORE/relay part_of CORE/legion", "CORE/store part_of CORE/legion",
		},
	}
	imported.edges = imported.relations
	require("after the import", imported)

	// A re-import at another commit drops daemon's dependency on store and drops relay.
	reimport := componentNodeFiles()
	reimport["daemon.md"] = "---\ntitle: Daemon\nparent: legion\ndepends_on: [envoy]\n---\nThe daemon.\n"
	delete(reimport, "relay.md")
	fake.repos["legion/arch"].commit, fake.repos["legion/arch"].files = shaTwo, reimport
	syncComponentTree(t, handler, "legion/arch")
	reimported := componentStore{
		rows: []string{
			"daemon component CORE/daemon", "envoy component CORE/envoy", "legion component CORE/legion", "store component CORE/store",
		},
		depends: []string{"daemon depends_on envoy component CORE/daemon"},
		nodes: []string{
			"CORE/daemon component live CORE", "CORE/envoy component live CORE", "CORE/legion component live CORE",
			"CORE/relay component gone CORE", "CORE/store component live CORE",
		},
		relations: []string{
			"CORE/daemon depends_on CORE/envoy", "CORE/daemon part_of CORE/legion",
			"CORE/envoy part_of CORE/legion", "CORE/store part_of CORE/legion",
		},
	}
	reimported.edges = reimported.relations
	require("after the re-import", reimported)

	// The issue's components set, narrowed to one, then cleared.
	setTo := func(components any, members, affects []string) {
		t.Helper()
		if response := patchComponents(t, handler, issue, components); response.Code != http.StatusOK {
			t.Fatalf("patch %s's components to %v: status=%d body=%s", issue, components, response.Code, response.Body.String())
		}
		want := reimported
		want.members = members
		want.relations = slices.Sorted(slices.Values(append(slices.Clone(reimported.relations), affects...)))
		want.edges = want.relations
		require("after "+issue+"'s components are set to "+stringOf(components), want)
	}
	setTo(map[string]any{"mode": "explicit", "ids": []string{"daemon", "envoy"}},
		[]string{"daemon issue " + issue, "envoy issue " + issue},
		[]string{issue + " affects CORE/daemon", issue + " affects CORE/envoy"})
	setTo(map[string]any{"mode": "explicit", "ids": []string{"envoy"}},
		[]string{"envoy issue " + issue},
		[]string{issue + " affects CORE/envoy"})
	setTo(nil, nil, nil)

	// The source's removal takes every component it held.
	if deleted := dispatchRequest(t, handler, http.MethodDelete, sourcePath("CORE", "legion/arch"), nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete legion/arch: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	require("after the source's removal", componentStore{
		nodes: []string{
			"CORE/daemon component gone CORE", "CORE/envoy component gone CORE", "CORE/legion component gone CORE",
			"CORE/relay component gone CORE", "CORE/store component gone CORE",
		},
	})
}

// stringOf names a components value a PATCH sends, for a failure.
func stringOf(components any) string {
	if components == nil {
		return "null"
	}
	return fmt.Sprint(components)
}
