package workflow

import (
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// ChildrenFirstNotice is what the architect that owns a parent is told when the parent's own first
// phase begins to wait for its children: the parent, and each open child with the Dispatch status
// that holds it, whether the daemon released it, and the issue above it on its own parent chain
// that a blocked_by link of the child names (record.ChildWait's Cycle), empty where there is no
// such cycle. A cycle holds the child for an issue that is waiting for the child: the daemon names
// it and acts no further, since taking the link off decides what the work depends on, which is a
// person's decision, and dropping a Dispatch link of its own accord would lose a fact nobody asked
// it to lose.
//
// It is told once per issue and run of its tree — the mark's key is the issue, its tree and that
// tree's root generation (admit's tellChildrenFirst) — so a child filed later during one wait
// tells nobody again, while a re-admitted tree, whose architect writes its spec anew, and a
// sub-parent handed over to a tree of its own are told anew.
func ChildrenFirstNotice(wait record.ChildWait) texts.ChildrenFirstRecord {
	children := make([]texts.OpenChildFacts, 0, len(wait.Open))
	for _, child := range wait.Open {
		children = append(children, texts.OpenChildFacts{Key: child.Key, Status: child.Status, Released: child.Released, Cycle: child.Cycle})
	}
	return texts.ChildrenFirstRecord{NoticeHead: texts.NoticeHead{Kind: texts.ChildrenFirst, Issue: wait.Parent}, Children: children}
}
