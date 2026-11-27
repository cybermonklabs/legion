package graphstoretest

import (
	"errors"
	"fmt"
)

// The visibilities a node is written under, copies of graphstore.VisibilityProject and
// graphstore.VisibilityAuthor, which graphstore's TestGraphstoretestVisibilitiesAreTheStoresOwn
// holds equal to them through what VisibilityFor answers.
const (
	visibilityProject = "project"
	visibilityAuthor  = "author"
)

// ErrAuthorNamesNobody refuses a node reserved for its author written on a credential that
// proves none (VisibilityFor). Its words are the reason itself: they say what is wrong with the
// credential and what would be wrong with the node.
var ErrAuthorNamesNobody = errors.New(
	"this credential names nobody, so a node reserved for its author would be nobody's to read: " +
		"every holder of the deployment's shared bearer presents the same value, and two such " +
		"credentials compare as one credential, so write it project-visible or write it under a " +
		"credential that names a session, a person or a workload")

// VisibilityFor is the visibility a test's write of a node gets for what it asked for. No route
// lets a write ask for one yet, so a test that reserves a node for its author writes it through
// the store's own writer with the visibility this answers, as such a route would. A write that
// asks for nothing is project-visible, which is what every node Dispatch writes today is; a write
// that asks to reserve a node for its author gets that only on a credential that proves one, and
// is refused with ErrAuthorNamesNobody otherwise. proves is whether the writing credential proves
// an author, the store's one rule (graphstore.ProvesAuthor), which the caller asks, since this
// package imports nothing of graphstore. A word that is neither is refused by name, so a
// misspelling is a failed write rather than a node nobody can read.
func VisibilityFor(requested string, proves bool) (string, error) {
	switch requested {
	case "", visibilityProject:
		return visibilityProject, nil
	case visibilityAuthor:
		if !proves {
			return "", ErrAuthorNamesNobody
		}
		return visibilityAuthor, nil
	}
	return "", fmt.Errorf("visibility %q is no visibility this store holds, which are %q and %q",
		requested, visibilityProject, visibilityAuthor)
}
