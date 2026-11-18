package store

import (
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// subjectIndex indexes the cached interests' topic patterns by subject token, one tree per machine
// id, so a subject is matched against a machine's interests in time that follows the subject's
// tokens and the patterns that share them rather than every cached interest. Its wildcard rules are
// routing.Match's: a "*" token matches any one token, a ">" token matches one or more remaining
// tokens (and ends the pattern, whatever follows it), and tokens split on every ".", an empty token
// included. Registry holds it under its mu and keeps it in step with its cache.
//
// Each topic is indexed under every form a reader takes it in (contracts.TopicForms), so an
// interest stored in the old form of an issue's or an agent's name matches a publish in the new
// form, and one stored in the new form matches a publish in the old, while every reader takes
// both; a topic of a family the rename leaves alone is its one pattern.
type subjectIndex struct {
	roots map[string]*subjectNode
	// entries is what each cache key is indexed under, so a change replaces it and an eviction
	// removes it.
	entries map[string]indexedInterest
}

// indexedInterest is one cache key's machine id and its distinct topic patterns.
type indexedInterest struct {
	machineID string
	patterns  []string
}

// subjectNode is one token position of a machine's tree.
type subjectNode struct {
	literal map[string]*subjectNode
	star    *subjectNode
	// full holds the keys whose pattern has ">" at this position; ends those whose pattern ends here.
	full map[string]struct{}
	ends map[string]struct{}
}

func newSubjectIndex() *subjectIndex {
	return &subjectIndex{roots: map[string]*subjectNode{}, entries: map[string]indexedInterest{}}
}

// set indexes key under item's machine id and every form of its topics, replacing what key was
// indexed under.
func (x *subjectIndex) set(key string, item Interest) {
	patterns := make([]string, 0, len(item.Topics))
	for _, topic := range item.Topics {
		patterns = append(patterns, contracts.TopicForms(topic)...)
	}
	slices.Sort(patterns)
	patterns = slices.Compact(patterns)
	if current, ok := x.entries[key]; ok {
		if current.machineID == item.MachineID && slices.Equal(current.patterns, patterns) {
			return
		}
		x.remove(key)
	}
	root := x.roots[item.MachineID]
	if root == nil {
		root = &subjectNode{}
		x.roots[item.MachineID] = root
	}
	for _, pattern := range patterns {
		root.insert(key, pattern)
	}
	x.entries[key] = indexedInterest{machineID: item.MachineID, patterns: patterns}
}

// remove drops key from the index, and every node it alone kept.
func (x *subjectIndex) remove(key string) {
	current, ok := x.entries[key]
	if !ok {
		return
	}
	delete(x.entries, key)
	root := x.roots[current.machineID]
	if root == nil {
		return
	}
	for _, pattern := range current.patterns {
		root.delete(key, pattern)
	}
	if root.empty() {
		delete(x.roots, current.machineID)
	}
}

// match calls visit with each key of machineID whose patterns match subject; a key with several
// matching patterns is visited once per pattern.
func (x *subjectIndex) match(machineID, subject string, visit func(key string)) {
	if root := x.roots[machineID]; root != nil {
		root.match(subject, false, visit)
	}
}

// matchAllMachines calls visit with each key, on whatever machine, whose patterns match subject;
// a key with several matching patterns is visited once per pattern. A key belongs to one machine's
// tree, so no key is visited from two of them.
func (x *subjectIndex) matchAllMachines(subject string, visit func(key string)) {
	for _, root := range x.roots {
		root.match(subject, false, visit)
	}
}

// oncePerKey wraps visit so that each key reaches it once, however many times the index visits
// that key: match and matchAllMachines visit a key once per matching pattern. The set of keys
// seen is made on the first visit, so a walk that matches nothing allocates nothing.
func oncePerKey(visit func(key string)) func(key string) {
	var seen map[string]struct{}
	return func(key string) {
		if _, dup := seen[key]; dup {
			return
		}
		if seen == nil {
			seen = map[string]struct{}{}
		}
		seen[key] = struct{}{}
		visit(key)
	}
}

func (n *subjectNode) insert(key, pattern string) {
	node := n
	for token := range strings.SplitSeq(pattern, ".") {
		switch token {
		case ">":
			if node.full == nil {
				node.full = map[string]struct{}{}
			}
			node.full[key] = struct{}{}
			return
		case "*":
			if node.star == nil {
				node.star = &subjectNode{}
			}
			node = node.star
		default:
			child := node.literal[token]
			if child == nil {
				if node.literal == nil {
					node.literal = map[string]*subjectNode{}
				}
				child = &subjectNode{}
				node.literal[token] = child
			}
			node = child
		}
	}
	if node.ends == nil {
		node.ends = map[string]struct{}{}
	}
	node.ends[key] = struct{}{}
}

// delete drops key's pattern from the subtree at n, pruning the nodes left empty.
func (n *subjectNode) delete(key, pattern string) {
	token, rest, more := strings.Cut(pattern, ".")
	switch {
	case token == ">":
		delete(n.full, key)
		return
	case token == "*":
		if n.star == nil {
			return
		}
		n.star.deleteRest(key, rest, more)
		if n.star.empty() {
			n.star = nil
		}
	default:
		child := n.literal[token]
		if child == nil {
			return
		}
		child.deleteRest(key, rest, more)
		if child.empty() {
			delete(n.literal, token)
		}
	}
}

// deleteRest drops key's pattern from n, which its tokens before rest led to; more reports whether
// rest holds any.
func (n *subjectNode) deleteRest(key, rest string, more bool) {
	if !more {
		delete(n.ends, key)
		return
	}
	n.delete(key, rest)
}

func (n *subjectNode) empty() bool {
	return len(n.literal) == 0 && n.star == nil && len(n.full) == 0 && len(n.ends) == 0
}

// match visits the keys of n's subtree that the subject's remaining tokens match: rest holds them,
// and exhausted reports that none remain.
func (n *subjectNode) match(rest string, exhausted bool, visit func(string)) {
	if exhausted {
		for key := range n.ends {
			visit(key)
		}
		return
	}
	for key := range n.full {
		visit(key)
	}
	token, next, more := strings.Cut(rest, ".")
	if child := n.literal[token]; child != nil {
		child.match(next, !more, visit)
	}
	if n.star != nil {
		n.star.match(next, !more, visit)
	}
}
