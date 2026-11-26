package docs

import (
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// roomTarget is the target of artifactID read as a room of its own, its own fragment, resolving
// nothing (targetOf resolves a document's room): the tests read a document that is its own room's
// stored tree through it.
func roomTarget(artifactID string) liveTarget {
	return liveTarget{document: artifactID, room: artifactID, fragment: fragmentName}
}

// liveTree is the tree of live, the document resident under room, as of one moment: its own
// fragment, as liveFragmentsTree reads it.
func (s *Service) liveTree(room string, live *crdt.Doc) (*pmdoc.Node, error) {
	tree, _, err := s.liveFragmentsTree(room, live, []string{fragmentName})
	return tree, err
}
