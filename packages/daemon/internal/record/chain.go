package record

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// MaxChainLinks is the most links Dispatch lets an issue stand below the top of its chain
// (IssueDepthCap in packages/envoy/internal/dispatch/refs/issues.go): a create or a reparent that
// would put an issue deeper is refused there, so a longer chain in the copy is one a stale row
// made, and the chain is unknown (R16 of dispatch://INFRA-3320).
const MaxChainLinks = 32

// ChainRow is one row of the copy a chain read reached (Store.ChainRows): Key's parent, status and
// labels as the copy holds them, "" for no parent, and Depth, the fewest links above a key the read
// started from that Key stands at, 0 for such a key itself. Present is false where the copy holds
// no row of Key, a link no chain can be read past; Parent, Status and Labels are then empty.
type ChainRow struct {
	Key     string
	Depth   int
	Parent  string
	Status  string
	Labels  []string
	Present bool
}

// ChainCause is why a chain is unknown, ChainKnown for a known one.
type ChainCause string

const (
	// ChainKnown is a chain read to its top: its last link, or the issue itself, has no parent.
	ChainKnown ChainCause = ""
	// ChainMissingRow is a chain the copy holds no row of one of its issues, the issue itself
	// included; Chain.At is that issue.
	ChainMissingRow ChainCause = "missing_row"
	// ChainRepeats is a chain that loops back on itself, as two late moves can leave the copy;
	// Chain.At is the key that repeats, the issue itself or one of its links.
	ChainRepeats ChainCause = "repeats"
	// ChainTooLong is a chain longer than MaxChainLinks, as a stale row can leave the copy;
	// Chain.At is its first link past the bound.
	ChainTooLong ChainCause = "too_long"
	// ChainUnread is a chain the read holds no end of: the issue is no key the read started from,
	// and every walk that reached it stopped at its bound below Chain.At. A caller that asks about
	// such an issue names it among ReadChains' extra keys, and the read walks its whole chain.
	ChainUnread ChainCause = "unread"
)

// Chain is an issue's links on its Dispatch chain as the copy holds them, its parent first, and
// whether the chain is known (Cause). An unknown chain holds the links read below the link it
// stopped at (At): the links up to the one whose row is missing, up to and including the one
// whose parent repeats a key, or the first MaxChainLinks of a chain too long. A caller never
// writes into Links: a chain's links are its links' chains too.
type Chain struct {
	Links []string
	Cause ChainCause
	At    string
}

// Known says whether the chain is read to its top (R16): every row of it in the copy, no key
// twice and no more than MaxChainLinks links.
func (c Chain) Known() bool { return c.Cause == ChainKnown }

// sameFromEachLink says whether the chain read from each of its links up is this chain's own
// remainder, ending as this one ends: a known chain and one that stops at a row the copy lacks
// or the read never reached. A loop is not, since a link inside it meets a key of its own
// first, and neither is a chain too long, since each link stands nearer the bound.
func (c Chain) sameFromEachLink() bool {
	return c.Cause != ChainRepeats && c.Cause != ChainTooLong
}

// Chains is one decision pass's reading of the Dispatch chains (R15, R16 of
// dispatch://INFRA-3320), read by ReadChains in a number of queries that does not grow with the
// number of issues or links, and the one reader every question of above, under, root and gate
// root asks. Its chains are the copy's alone (dispatch_issues): nothing of the daemon's records
// decides a link, and the records decide only which issues on a chain are in Legion's work
// (InLegionsWork) and which hold their own gate (Issue.HoldsGate).
//
// Each key's chain is walked once and kept: a walk that reads a chain to its top, or to a row the
// copy lacks, keeps that chain's remainder for each link it passed, and a later walk that reaches
// a link already kept joins its chain rather than reading it again, so a pass that asks about
// every issue of a tree reads each row of its chain once. Its methods take keys and answer keys;
// a caller that needs a record reads it from Record, so no caller holds a second copy of what the
// pass read. It is no value for two transactions, and no value for two goroutines: its answers
// are kept as they are asked.
type Chains struct {
	rows     map[string]ChainRow
	records  map[string]*Issue
	working  []string
	children map[string][]DispatchIssueRow
	chains   map[string]Chain
	above    map[string][]string
	under    map[string][]string
	// steps is how many rows the walks have read, which is what tells a pass that walks each
	// chain once from one that walks a shared chain again for each issue on it.
	steps int
}

// ReadChains reads everything one decision pass asks of the Dispatch chains of project, in three
// queries whatever the number of issues and links: every row of the copy the chains reach
// (Store.ChainRows), walked from every record of project at a phase other than done, every row of
// the project's copy at todo, and each key of extra, which a caller names to ask about an issue
// that is neither; the copy's children, at any status, of each key the walk started from
// (Store.DispatchChildren); and the daemon's record of every key those two reads name
// (Store.IssuesByKey), whatever its project. A read that starts from nothing makes the first
// query alone.
func ReadChains(ctx context.Context, tx pgx.Tx, store Store, project string, extra []string) (*Chains, error) {
	rows, err := store.ChainRows(ctx, tx, project, extra)
	if err != nil {
		return nil, err
	}
	return readChainsFrom(ctx, tx, store, project, rows)
}

// ReadChainsOf is ReadChains for a decision about keys alone: the walk starts from keys and from
// no other issue (Store.ChainRowsOf), so the read costs the length of their chains whatever the
// number of issues the project has at work. For each of keys and every link above it its Chains
// answer as ReadChains's do (Chain, Above, IsRoot, HoldsOwnGate, GateRoot, TopIssue, and Children
// of keys); Under and the issues at work cover only the issues this read holds, so a caller that
// asks what stands under an issue reads ReadChains.
func ReadChainsOf(ctx context.Context, tx pgx.Tx, store Store, project string, keys []string) (*Chains, error) {
	rows, err := store.ChainRowsOf(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	return readChainsFrom(ctx, tx, store, project, rows)
}

// readChainsFrom is the rest of a chain read once its walk has read rows: the copy's children of
// each key the walk started from, and the daemon's record of every key those two name.
func readChainsFrom(ctx context.Context, tx pgx.Tx, store Store, project string, rows []ChainRow) (*Chains, error) {
	keys := make([]string, 0, len(rows))
	var starts []string
	for _, row := range rows {
		keys = append(keys, row.Key)
		if row.Depth == 0 {
			starts = append(starts, row.Key)
		}
	}
	children, err := store.DispatchChildren(ctx, tx, project, starts)
	if err != nil {
		return nil, err
	}
	for _, under := range children {
		for _, child := range under {
			keys = append(keys, child.Key)
		}
	}
	slices.Sort(keys)
	records, err := store.IssuesByKey(ctx, tx, slices.Compact(keys))
	if err != nil {
		return nil, fmt.Errorf("read the records on the chains of %s: %w", project, err)
	}
	return newChains(rows, children, records), nil
}

// newChains is the chains of one read: the rows of the copy it reached, the copy's children of
// each key it started from, and the daemon's record of every key those name (ReadChains).
func newChains(rows []ChainRow, children map[string][]DispatchIssueRow, records []Issue) *Chains {
	chains := &Chains{
		rows:     make(map[string]ChainRow, len(rows)),
		records:  make(map[string]*Issue, len(records)),
		children: children,
		chains:   make(map[string]Chain, len(rows)),
		above:    make(map[string][]string, len(rows)),
	}
	for _, row := range rows {
		chains.rows[row.Key] = row
	}
	for i := range records {
		chains.records[records[i].Key] = &records[i]
		if InLegionsWork(records[i]) {
			chains.working = append(chains.working, records[i].Key)
		}
	}
	return chains
}

// Record is the daemon's record of key as the read holds it, and whether it records key. The
// read holds the record of every key on a chain it walked and of every child it read.
func (c *Chains) Record(key string) (Issue, bool) {
	issue, recorded := c.records[key]
	if !recorded {
		return Issue{}, false
	}
	return *issue, true
}

// inWork says whether the daemon records key and that record is in Legion's work.
func (c *Chains) inWork(key string) bool {
	issue, recorded := c.records[key]
	return recorded && InLegionsWork(*issue)
}

// Status is the Dispatch status of key as the copy holds it, and whether the read holds a present
// row of key: a key on a chain it walked or a key it started from.
func (c *Chains) Status(key string) (string, bool) {
	row, read := c.rows[key]
	if !read || !row.Present {
		return "", false
	}
	return row.Status, true
}

// Labels is the labels of key as the copy holds them, empty where the read holds no present row of
// key. A caller never writes into them.
func (c *Chains) Labels(key string) []string {
	return c.rows[key].Labels
}

// Chain is key's links from its parent up as the copy holds them, and whether the chain is known:
// it is unknown where the copy holds no row of one of its issues, where a key repeats, or where it
// is longer than MaxChainLinks, and an unknown chain names its cause and the link it stopped at.
// A key the read neither started from nor reached is ChainUnread at the key itself.
func (c *Chains) Chain(key string) Chain {
	if chain, walked := c.chains[key]; walked {
		return chain
	}
	chain := c.walk(key)
	c.chains[key] = chain
	if chain.sameFromEachLink() {
		for i, link := range chain.Links {
			if _, walked := c.chains[link]; !walked {
				c.chains[link] = Chain{Links: chain.Links[i+1:], Cause: chain.Cause, At: chain.At}
			}
		}
	}
	return chain
}

// walk reads key's chain from the rows, one link at a time, until the chain ends or reaches a
// link whose chain is already kept and is the same from each of its links (joined).
func (c *Chains) walk(key string) Chain {
	path := make([]string, 1, 8)
	path[0] = key
	for {
		n := len(path) - 1
		at := path[n]
		// before is the links read below at: none where at is key itself.
		var before []string
		if n > 0 {
			before = path[1:n:n]
		}
		row, read := c.rows[at]
		if !read {
			return Chain{Links: before, Cause: ChainUnread, At: at}
		}
		c.steps++
		if !row.Present {
			return Chain{Links: before, Cause: ChainMissingRow, At: at}
		}
		links := path[1 : n+1 : n+1]
		switch {
		case row.Parent == "":
			return Chain{Links: links}
		case slices.Contains(path, row.Parent):
			return Chain{Links: links, Cause: ChainRepeats, At: row.Parent}
		case len(links) == MaxChainLinks:
			return Chain{Links: links, Cause: ChainTooLong, At: row.Parent}
		}
		if above, walked := c.chains[row.Parent]; walked && above.sameFromEachLink() && above.At != row.Parent {
			return joined(links, row.Parent, above)
		}
		path = append(path, row.Parent)
	}
}

// joined is the chain of an issue whose walk read links and reached parent, whose own chain is
// above and is the same from each of its links: the links, parent and above's, ending as above
// ends, unless together they pass MaxChainLinks first, where a walk of the whole would have
// stopped. A chain of parent that stops at parent itself is not joined (walk reads that row).
func joined(links []string, parent string, above Chain) Chain {
	all := make([]string, 0, len(links)+1+len(above.Links))
	all = append(append(append(all, links...), parent), above.Links...)
	switch {
	case len(all) > MaxChainLinks:
		return Chain{Links: all[:MaxChainLinks:MaxChainLinks], Cause: ChainTooLong, At: all[MaxChainLinks]}
	case len(all) == MaxChainLinks && !above.Known():
		return Chain{Links: all, Cause: ChainTooLong, At: above.At}
	}
	return Chain{Links: all, Cause: above.Cause, At: above.At}
}

// Above is the issues above key on its chain that are in Legion's work, nearest first, whatever
// the status of the issues between them: an issue at work under a parent on the backlog under an
// issue in work has that issue above it. It reads the links the copy holds, known chain or not;
// IsRoot is what asks that the chain be known.
func (c *Chains) Above(key string) []string {
	if above, read := c.above[key]; read {
		return above
	}
	var above []string
	for _, link := range c.Chain(key).Links {
		if c.inWork(link) {
			above = append(above, link)
		}
	}
	c.above[key] = above
	return above
}

// IsRoot says whether key is a root (R15): its chain is known and no issue above it is in
// Legion's work. An issue whose chain is unknown is no root, so nothing makes one of it.
func (c *Chains) IsRoot(key string) bool {
	return c.Chain(key).Known() && len(c.Above(key)) == 0
}

// TopIssue is key's top issue (R15): the highest issue on its chain in Legion's work, the last of
// Above, else key itself.
func (c *Chains) TopIssue(key string) string {
	if above := c.Above(key); len(above) > 0 {
		return above[len(above)-1]
	}
	return key
}

// HoldsOwnGate says whether key holds its own design gate (R15): it is a root, or the daemon
// records it as having held its gate as a root (Issue.HoldsGate), which an issue that ran as a
// root keeps once it gets a parent in Legion's work.
func (c *Chains) HoldsOwnGate(key string) bool {
	if issue, recorded := c.records[key]; recorded && issue.HoldsGate {
		return true
	}
	return c.IsRoot(key)
}

// GateRoot is key's gate root (R15): the nearest issue at or above it in Legion's work that holds
// its own gate, "" where none does. On a known chain an issue in work always has one, its top
// issue being a root, so "" is an issue in work only on an unknown chain, or an issue not in work
// with no issue above it in work.
func (c *Chains) GateRoot(key string) string {
	if c.inWork(key) && c.HoldsOwnGate(key) {
		return key
	}
	for _, above := range c.Above(key) {
		if c.HoldsOwnGate(above) {
			return above
		}
	}
	return ""
}

// StateTree is the tree the state names for key (R22's `tree`): its gate root where key is in
// Legion's work, and otherwise the nearest issue above it that the daemon records and that holds
// its own gate; else key itself.
func (c *Chains) StateTree(key string) string {
	if c.inWork(key) {
		if root := c.GateRoot(key); root != "" {
			return root
		}
		return key
	}
	for _, link := range c.Chain(key).Links {
		if _, recorded := c.records[link]; recorded && c.HoldsOwnGate(link) {
			return link
		}
	}
	return key
}

// Under is the issues in Legion's work whose chains pass through key, by key, whatever the status
// of the issues between them: every recorded issue in work with key among its links, known chain
// or not. key itself is not among them.
func (c *Chains) Under(key string) []string {
	if c.under == nil {
		c.under = make(map[string][]string, len(c.working))
		for _, issue := range c.working {
			for _, link := range c.Chain(issue).Links {
				c.under[link] = append(c.under[link], issue)
			}
		}
	}
	return c.under[key]
}

// GateTree is key and the issues in Legion's work whose gate root is key (R15: "its tree" of a gate
// root), key first and the rest by key: what a question about the agents of a gate root's tree
// reads where the stored tree was read, a session's claim of one of them among them. An issue under
// key whose gate root is another gate holder below key is that holder's, not key's.
func (c *Chains) GateTree(key string) []string {
	tree := []string{key}
	for _, issue := range c.Under(key) {
		if c.GateRoot(issue) == key {
			tree = append(tree, issue)
		}
	}
	return tree
}

// Children is key's children in the copy at any status, by key, for a key the read started from
// (ReadChains): every record of the project not done, every todo row of its copy and each extra
// key. A key the read did not start from has none read.
func (c *Chains) Children(key string) []DispatchIssueRow {
	return c.children[key]
}

// unrecordedTodo is every key the read started from that the copy shows todo and the daemon
// records nothing of, in key order: the issues a person or an architect put at todo that no
// decision has recorded yet (HeldList lists the ones on an unknown chain).
func (c *Chains) unrecordedTodo() []string {
	var keys []string
	for key, row := range c.rows {
		if _, recorded := c.records[key]; !recorded && row.Depth == 0 && row.Present && row.Status == "todo" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}
