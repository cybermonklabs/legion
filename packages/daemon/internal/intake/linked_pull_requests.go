package intake

import (
	"strings"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// LinkedPullRequest is one pull request an issue links in Dispatch, as the decoder's index of the
// links holds it (Consumers.LoadLinks): the issue, the pull request's repository and number
// (PullRequestLink), and its head branch as the record holds it (record.PullRequest.Branch), ""
// while the record holds no read of it.
type LinkedPullRequest struct {
	Issue  string
	Repo   string
	Number int
	Branch string
}

// linkIndex is the decoder's index of the pull requests the issues link (decision 17 of the plan
// of dispatch://INFRA-5910), held in memory beside the repositories GitHub intake hears: each
// linked pull request by its repository and number, with the issue that links it and its head
// branch. decodePullRequest takes a pull request event of a branch no issue's name gives
// (claim.BranchIssue) only for a pull request it holds, and decodePush takes a push of such a
// branch as a Push only for a head branch it holds, so an event of any other pull request of a
// heard repository still costs no transaction, as it did before any issue linked one. It is read
// on every GitHub event the consumer decodes, under a lock of its own, so a change of the links
// never waits on the consumer's filters (Consumers.mu, which SetRepositories holds across its
// JetStream call).
//
// A repository is held by its canonical name, the repository lowercased and, for either name of a
// configured rename, the name it moved to (canonical), as Dispatch's linkedPullRequestTerms holds a
// link's (refs.CanonicalRepoSQL): a link, an event and a record keep the name they were written
// under, so a link written before a transfer finds the events GitHub sends after it, and one name
// of each pull request is held. The index holds what its caller loads (Consumers.LoadLinks) and
// changes (Consumers.ReplaceLinks, Consumers.SetLinkHead), and nothing else: no decode adds to it.
type linkIndex struct {
	renames ghrepo.Renames
	mu      sync.RWMutex
	// pulls is each linked pull request, keyed by its canonical repository and its number.
	pulls map[linkKey]linkEntry
	// heads counts the linked pull requests on each head branch, keyed by the canonical repository
	// and the branch: a push names a branch and no number, and two pull requests may stand on one
	// branch (D1 of dispatch://INFRA-5910).
	heads map[headKey]int
}

type linkKey struct {
	repo   string
	number int
}

type linkEntry struct {
	issue, branch string
}

type headKey struct {
	repo, branch string
}

func newLinkIndex(renames ghrepo.Renames) *linkIndex {
	return &linkIndex{renames: renames, pulls: map[linkKey]linkEntry{}, heads: map[headKey]int{}}
}

// canonical is the name the index holds repo under: repo lowercased, and, where repo is either
// name of a configured rename, the name the pair moved it to, lowercased. A deployment that
// configures no rename makes no lookup of the pairs, and a name with no capital allocates nothing.
func (l *linkIndex) canonical(repo string) string {
	partner, paired := l.renames.Partner(repo)
	if !paired {
		return strings.ToLower(repo)
	}
	if _, partnerMoved := l.renames.MovedTo(partner); partnerMoved {
		// The partner is the pair's from, so repo is the name the pair moved to.
		return strings.ToLower(repo)
	}
	return strings.ToLower(partner.String())
}

// load replaces everything the index holds with linked. Of two entries of one pull request, the
// later is held.
func (l *linkIndex) load(linked []LinkedPullRequest) {
	loaded := &linkIndex{renames: l.renames, pulls: make(map[linkKey]linkEntry, len(linked)), heads: make(map[headKey]int, len(linked))}
	for _, pull := range linked {
		loaded.put(linkKey{repo: l.canonical(pull.Repo), number: pull.Number}, linkEntry{issue: pull.Issue, branch: pull.Branch})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pulls, l.heads = loaded.pulls, loaded.heads
}

// replace makes links the links of issue: each pull request issue linked that links does not name
// goes, and each that links names and the index does not hold for issue is held for it, with the
// head branch the index held for it before, under another issue's link (Dispatch lets one URL link
// one issue alone, so the link moved), or none. A pull request that stays keeps its head branch.
func (l *linkIndex) replace(issue string, links []PullRequestLink) {
	named := make(map[linkKey]bool, len(links))
	for _, link := range links {
		named[linkKey{repo: l.canonical(link.Repo), number: link.Number}] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, entry := range l.pulls {
		if entry.issue == issue && !named[key] {
			l.drop(key)
		}
	}
	for key := range named {
		if entry, held := l.pulls[key]; !held || entry.issue != issue {
			l.put(key, linkEntry{issue: issue, branch: entry.branch})
		}
	}
}

// setHead holds branch as the head branch of repo#number where the index holds that pull request,
// and changes nothing where it does not: a read of a pull request no issue links adds none.
func (l *linkIndex) setHead(repo string, number int, branch string) {
	key := linkKey{repo: l.canonical(repo), number: number}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, held := l.pulls[key]
	if !held || entry.branch == branch {
		return
	}
	entry.branch = branch
	l.put(key, entry)
}

// put holds entry for key, in place of what the index held for it. l.mu is held.
func (l *linkIndex) put(key linkKey, entry linkEntry) {
	l.drop(key)
	l.pulls[key] = entry
	if entry.branch != "" {
		l.heads[headKey{repo: key.repo, branch: entry.branch}]++
	}
}

// drop takes key out of the index, and its head branch with it once no other linked pull request
// stands on that branch. l.mu is held.
func (l *linkIndex) drop(key linkKey) {
	entry, held := l.pulls[key]
	if !held {
		return
	}
	delete(l.pulls, key)
	if entry.branch == "" {
		return
	}
	head := headKey{repo: key.repo, branch: entry.branch}
	if l.heads[head] <= 1 {
		delete(l.heads, head)
		return
	}
	l.heads[head]--
}

// linked says whether an issue links pull request number of repo, a heard repository's name as
// GitHub spells it. A nil index, a replay's built by hand (Records), links none.
func (l *linkIndex) linked(repo string, number int) bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.pulls) == 0 {
		return false
	}
	_, held := l.pulls[linkKey{repo: l.canonical(repo), number: number}]
	return held
}

// headLinked says whether branch of repo is the head branch of a pull request an issue links. A
// nil index links none.
func (l *linkIndex) headLinked(repo, branch string) bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.heads) == 0 {
		return false
	}
	return l.heads[headKey{repo: l.canonical(repo), branch: branch}] > 0
}

// LoadLinks makes linked the whole of the decoder's index of the pull requests the issues link
// (linkIndex), in place of what it held: what the daemon reads of the record at its boot, each link
// with the head branch of the pull request the record holds for it. Until the first load the index
// holds no link, so the consumers decode every pull request event and push of a branch no issue's
// name gives as they did before any issue linked a pull request.
func (c *Consumers) LoadLinks(linked []LinkedPullRequest) {
	c.links.load(linked)
}

// ReplaceLinks makes links the links of issue in the decoder's index, as the record holds them once
// a fact that replaced them (record.Store.ReplacePullRequestLinks) has committed: a link removed
// stops its pull request's events off legion/<KEY> from decoding, and a link added decodes its
// pull request's events from the call on, and the pushes of its head branch once a read of it has
// told that branch (SetLinkHead). An event delivered between the commit and the call is decoded
// against the links as they stood, and the read the link's change owes reads the pull request as
// GitHub shows it then.
func (c *Consumers) ReplaceLinks(issue string, links []PullRequestLink) {
	c.links.replace(issue, links)
}

// SetLinkHead holds branch as the head branch of repo#number, once the read of that linked pull
// request (PullRequestLinkReadDone) has committed, so a push of branch is decoded as the Push of
// that pull request (decodePush). It adds no link: a read of a pull request the index does not hold
// changes nothing.
func (c *Consumers) SetLinkHead(repo string, number int, branch string) {
	c.links.setHead(repo, number, branch)
}
