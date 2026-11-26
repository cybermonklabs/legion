package docs

// roomTarget is the target of artifactID read as a room of its own, its own fragment, resolving
// nothing (targetOf resolves a document's room): the tests read a document that is its own room's
// stored tree through it.
func roomTarget(artifactID string) liveTarget {
	return liveTarget{document: artifactID, room: artifactID, fragment: fragmentName}
}
