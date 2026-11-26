// Package graphstoretest holds what the tests of more than one package read of the node and
// tuple store and no code of the store reads: the tuple kinds of the base attributes
// (BaseAttributes), the store writers the source scans hold every write of the store to
// (WriteFunctions), the visibility a test's write reserves a node for its author under
// (VisibilityFor), and the rules a test holds a refusal's words to: each name a word of its own
// (NamesAsAWord), and each after the one before it (NamesInOrder). It is an ordinary package
// rather than a _test.go file because Go cannot share test-only code across packages, and it
// imports nothing of graphstore, so graphstore's own tests can import it: they hold each copy kept
// here of a value of graphstore to that value (graphstore's
// TestGraphstoretestBaseAttributesAreTheStoresOwn and
// TestGraphstoretestVisibilitiesAreTheStoresOwn). What a test seeds through the store's own
// writers, which imports graphstore, is graphseedtest's.
package graphstoretest

import (
	"strings"
	"unicode"
)

// BaseAttributes is the tuple kind of every base attribute, a fresh slice for each caller: a copy
// of the set graphstore.IsBaseAttribute reads, which graphstore's
// TestGraphstoretestBaseAttributesAreTheStoresOwn holds equal to it, element for element. No other
// attribute's values may be stored under one of these kinds: a value tuple of one of them out of a
// node reads as that node's own visibility, state, project or parties, whoever wrote it, so an
// attribute a type declares under one of these names is stored under another name
// (pmdoc/schemanodes.StoreName).
func BaseAttributes() []string { return []string{"visibility", "state", "project", "parties"} }

// WriteFunctions is the name of every function of graphstore a write of graph_nodes and
// graph_tuples from outside that package goes through, a fresh slice for each caller: Write and
// Caused in a caller's transaction, and QueueWrite, QueueNodes and QueueCaused, the same writes
// queued on a write phase's batch; WriteState, Write of the state of nodes, with what stands
// beneath each node it takes gone; Retarget, the move of one pointer tuple of a node from one far
// end to another; QueueFrameTuple, Write's append of one frame value of a run queued on a caller's
// batch; WriteRun, the write of a run's node from its row; WriteArms and WriteArmsUncaused spliced
// into a caller's statement; QueueAuthored, QueueReached and QueueSessionRecorded, which queue
// those arms for the writers that record an author, a reached session and a session an ask names;
// and FillScopes, the backfill's fill of the project of a node stored with none. Each is a
// function of the package, never a method, so a call of one is named by the package wherever it
// is made. It is the one list the source scans that hold the store's writers read: graphstore's
// register of their call sites (TestEveryStoreWriteCallSiteIsListed) and api's scan of the writers
// of the projected tables (TestNoProjectionRowIsWrittenWithoutTheStore).
func WriteFunctions() []string {
	return []string{"Write", "WriteState", "WriteArms", "WriteArmsUncaused", "Caused",
		"QueueWrite", "QueueNodes", "QueueCaused",
		"QueueAuthored", "QueueReached", "QueueSessionRecorded",
		"Retarget", "QueueFrameTuple", "WriteRun", "FillScopes"}
}

// NamesAsAWord is whether text names each of names as words of its own: text split on spaces and
// commas, each word shorn of the quotes, brackets, colons, semicolons and full stops at its ends,
// holds each name as one of its words, and a name of several words as that run of words. An
// address a comma or a colon follows is a word, and one that only begins a longer word, as a
// lens's address begins its finding's `<lens address>#b-<id>`, is none.
func NamesAsAWord(text string, names ...string) bool {
	spaced := spacedWords(text)
	for _, name := range names {
		if !strings.Contains(spaced, " "+name+" ") {
			return false
		}
	}
	return true
}

// NamesInOrder is whether text names each of names as words of its own, as NamesAsAWord reads
// them, each after the one before it: a refusal that names the near end of a tuple before its
// far end names the two in that order, and one that names one node at both ends names it twice.
func NamesInOrder(text string, names ...string) bool {
	spaced := spacedWords(text)
	for _, name := range names {
		at := strings.Index(spaced, " "+name+" ")
		if at < 0 {
			return false
		}
		spaced = spaced[at+1+len(name):]
	}
	return true
}

// spacedWords is the words of text as NamesAsAWord reads them, joined by one space, with a space
// before the first and after the last, so a name is a word of text, or a run of its words, where
// the name with a space at each side stands in it.
func spacedWords(text string) string {
	var words []string
	for word := range strings.FieldsFuncSeq(text, func(r rune) bool { return unicode.IsSpace(r) || r == ',' }) {
		words = append(words, strings.Trim(word, "\"'`()[]:;."))
	}
	return " " + strings.Join(words, " ") + " "
}
