package docs

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/testwait"
)

// requireDocument fails unless the write before it left artifactID's document reading want, as a
// caller reads it and as the latest version stored holds it, and its live tree holds no two text
// runs beside each other under the same marks, which the markdown writes as one. what names the
// write. A write that left the document reading anything else fails on the read at once. Then it
// waits for the latest version to read want, bounded by the test binary's deadline alone, as
// waitForDocumentVersion waits: the seed stores no version of its own, and the settlement after
// the write stores what both wrote, unless a read of the room between the seed and the write armed
// the seed's settlement, which then stores the seed's text first and the write's after it.
func requireDocument(t *testing.T, service *Service, artifactID, want, what string) {
	t.Helper()
	read, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	if read != want {
		t.Fatalf("%s:\n read markdown %q\nwant %q", what, read, want)
	}
	testwait.Until(t, fmt.Sprintf("%s: the latest stored version to read %q", what, want), func() (string, bool) {
		stored, _ := latestVersionOf(t, service.store, artifactID)
		return fmt.Sprintf("stored markdown %q", stored), stored == want
	})
	pmdoc.Walk(liveTree(t, service, artifactID), func(node *pmdoc.Node) bool {
		for index := 1; index < len(node.Children); index++ {
			if left, right := node.Children[index-1], node.Children[index]; left.Type == "text" && right.Type == "text" && reflect.DeepEqual(left.Marks, right.Marks) {
				t.Fatalf("%s leaves the runs %q and %q beside each other under the marks %v", what, left.Text, right.Text, left.Marks)
			}
		}
		return true
	})
}

// requireUnchanged fails unless a refused write, what, left artifactID's document as markdown, the
// text it was seeded with over the version before, before. A refused write stores no version: the
// latest is the one before the seed, or the seed's own if a settlement has stored it since.
func requireUnchanged(t *testing.T, service *Service, artifactID, before, markdown, what string) {
	t.Helper()
	read, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	if stored, _ := latestVersionOf(t, service.store, artifactID); (stored != before && stored != markdown) || read != markdown {
		t.Fatalf("after %s in %q:\n stored markdown %q, was %q\n read markdown   %q\nwant both unchanged", what, markdown, stored, before, read)
	}
}
