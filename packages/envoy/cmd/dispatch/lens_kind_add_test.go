package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// `envoy-dispatch lens-kind-add`, run through the command table the binary dispatches on, adds one
// kind to the catalogue - its lens_kind node with its words, the constraint holding its source and
// reference, and the constrained_by tuple between them - and exits 0. Run again with the id it
// added, or with an id the seed holds, it exits 1 naming the id and its address and writes
// nothing; an id that is no slug exits 1 naming that id, an empty source exits 1 naming the
// source, and a flag it does not take exits 2.
func TestLensKindAddWritesOneKindAndRefusesAHeldID(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, append([]string{"lens-kind-add"}, args...), env, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	count := func() (nodes, tuples int) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx,
			`select (select count(*) from graph_nodes), (select count(*) from graph_tuples)`).Scan(&nodes, &tuples); err != nil {
			t.Fatalf("count the store: %v", err)
		}
		return nodes, tuples
	}
	kind := []string{"--id", "command-test-kind", "--words", "a kind the command added",
		"--source", "A source the command names", "--reference", "a reference the command names"}

	code, stdout, stderr := run(kind...)
	if code != 0 || !strings.Contains(stdout, "dispatch://lens_kind/command-test-kind") {
		t.Fatalf("lens-kind-add exited %d with stdout %q and stderr %q, want 0 naming the kind it added", code, stdout, stderr)
	}
	node, found, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindLensKind, "command-test-kind")
	if err != nil || !found || node.Attributes[graphstore.AttributeWords] != "a kind the command added" ||
		node.Visibility != graphstore.VisibilityProject || node.State != graphstore.StateLive || node.Project != "" {
		t.Fatalf("the added kind reads %+v (found %t, %v), want it live, project-visible, in no project, with its words", node, found, err)
	}
	sources, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindLensKind, "command-test-kind", []string{graphstore.TupleConstrainedBy})
	if err != nil || len(sources) != 1 || sources[0].ToKind != graphstore.KindConstraint {
		t.Fatalf("the added kind holds the constrained_by tuples %+v (%v), want one to its constraint", sources, err)
	}
	source, found, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindConstraint, sources[0].ToID)
	if err != nil || !found || source.Attributes[graphstore.AttributeStandard] != "A source the command names" ||
		source.Attributes[graphstore.AttributeReference] != "a reference the command names" {
		t.Fatalf("the added kind's source reads %+v (found %t, %v), want its standard and reference", source, found, err)
	}

	nodes, tuples := count()
	// Each refusal names what it refuses: the id the catalogue holds, by itself and by its address,
	// the id that is no slug, the part that is empty.
	for _, refused := range []struct {
		name  string
		args  []string
		names []string
	}{
		{"the id it added", kind, []string{"command-test-kind", "dispatch://lens_kind/command-test-kind"}},
		{"an id the seed holds", []string{"--id", "pre-mortem", "--words", "w", "--source", "s", "--reference", "r"},
			[]string{"pre-mortem", "dispatch://lens_kind/pre-mortem"}},
		{"an id that is no slug", []string{"--id", "Not A Slug", "--words", "w", "--source", "s", "--reference", "r"},
			[]string{"id", "Not A Slug"}},
		{"an empty source", []string{"--id", "no-source", "--words", "w", "--source", " ", "--reference", "r"},
			[]string{"source"}},
	} {
		code, stdout, stderr := run(refused.args...)
		if code != 1 || !graphstoretest.NamesAsAWord(stderr, refused.names...) {
			t.Errorf("lens-kind-add with %s exited %d with stdout %q and stderr %q, want 1 naming %q", refused.name, code, stdout, stderr, refused.names)
		}
		if afterNodes, afterTuples := count(); afterNodes != nodes || afterTuples != tuples {
			t.Errorf("lens-kind-add with %s left %d nodes and %d tuples, want the %d and %d it found: a refusal writes nothing",
				refused.name, afterNodes, afterTuples, nodes, tuples)
		}
	}
	if code, _, stderr := run("--kind", "x"); code != 2 {
		t.Errorf("lens-kind-add with a flag it does not take exited %d with stderr %q, want 2", code, stderr)
	}
}
