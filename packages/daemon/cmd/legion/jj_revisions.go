package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// This file holds what the step (jjStep, jj.go) keeps of each revision argument of the push: the
// commits it names before any rewrite (readRevisions, revisionSets), the words it reaches each dry
// run and the real push with (jjStep.given, jjStep.words), which name those commits as
// revisionGiven says, and whether the push's command would still be under jjArgMax bytes once the
// rewrite of setAuthors leaves each argument its words (checkArgMax).

// jjArgument is what the step keeps of one revision argument of the push (readRevisions): commits,
// the commits of it that the step can rewrite, as jj resolves it before any rewrite
// (readRevision), each followed from that read; set, where read is true, every commit jj resolves
// it to before any rewrite, by full id, with whether it is the working-copy commit (revisionSet);
// and refused, whether jj refused a read of it, which leaves it to go as the role wrote it.
type jjArgument struct {
	commits       []*jjFollowed
	set           []jjSent
	read, refused bool
}

// jjSent is one commit a revision argument sends after the step (jjStep.sends): its full id, and
// whether it is the working-copy commit.
type jjSent struct {
	id      string
	working bool
}

// word is the commit as a revision argument's words write it: `@` for the working-copy commit, and
// its full id for any other. Where the step took its snapshot, the real push runs as
// jjStep.snapshotted says. The working-copy commit is `@` rather than its id because another jj
// command may rewrite @ between the step's read and the real push: `@` then names the @ that
// command left, where the id would name a hidden commit.
func (c jjSent) word() string {
	if c.working {
		return "@"
	}
	return c.id
}

// readRevisions reads each revision argument of the push with jj itself, before any rewrite: the
// commits of it that the step can rewrite (readRevision), each followed from that read, on its own
// (follow), with whether it is the working-copy commit. A hidden commit the argument names is
// among them: jj's all() holds the hidden commits a revset names, and their ancestors. A hidden
// commit whose change has exactly one visible commit stands for that commit, and is the
// working-copy commit where that one is, as the commit id of @ is after a rewrite of @ hid it; one
// whose change has none, or more than one, stands for itself. An argument with a hidden commit
// that stands for another has its set read now too (revisionSet): each pass of the dry run sends
// that commit in its place (revisionGiven), a word the step changes where it may change no commit.
// A run of the step that reads such a commit ends with errJJSnapshot, while the step's snapshot
// is due (snapshotDue), once all its reads are made and it has nothing else to change (once,
// standsIn): without the step's snapshot, the push's own could rebase the visible commit, a
// descendant of a working-copy commit with a write that no command has snapshotted, and the word
// would then name the commit that snapshot hid. Every other argument's set is read before the
// step's first metaedit (revisionSets), so that a push of which the step changes nothing, no
// commit and no word, costs one read of each argument. An argument whose read jj refuses (a
// revset jj cannot parse or resolve, an ambiguous prefix, a commit that git made where git moved
// neither HEAD nor a branch, such as one only a tag of git's names, and that no jj command has
// imported yet) is followed by nothing, and goes to the dry run and the real push as the role
// wrote it (given): jj alone may still push such a commit, since the real push imports it, while
// no read of the step, each with --ignore-working-copy, imports it or can read it. The dry run
// then fails as the read did, with jj's error, which the step cannot read (dryRunFilled), so the
// step takes no snapshot, which would import it. The step reads a commit that git made on HEAD or
// a branch after its snapshot, as rewrite says.
func (s *jjStep) readRevisions() error {
	s.revisions = make([]jjArgument, len(s.push.revisions))
	var hidden []string
	for i := range s.push.revisions {
		_, argument := s.push.revision(i)
		commits, err := s.readRevision(argument)
		var failed jjStepFailure
		if errors.As(err, &failed) {
			s.revisions[i].refused = true
			continue
		}
		if err != nil {
			return err
		}
		for _, c := range commits {
			s.revisions[i].commits = append(s.revisions[i].commits, s.follow(c, true))
			if c.hidden && !slices.Contains(hidden, c.change) {
				hidden = append(hidden, c.change)
			}
		}
	}
	if len(hidden) == 0 {
		return nil
	}
	visible, err := s.visible(hidden)
	if err != nil {
		return err
	}
	for i := range s.revisions {
		stands := false
		for _, f := range s.revisions[i].commits {
			if now := visible[f.change]; f.hidden && len(now) == 1 {
				f.commit, f.hidden, f.working = now[0].commit, false, now[0].working
				stands = true
			}
		}
		if stands {
			s.revisionSet(i)
		}
	}
	return nil
}

// standsIn is whether a revision argument names a hidden commit that stands for its change's one
// visible commit (readRevisions), which each of its words then names in its place: before any
// metaedit of the step, the one way a followed commit of an argument stands for another.
func (s *jjStep) standsIn() bool {
	for _, a := range s.revisions {
		if slices.ContainsFunc(a.commits, func(f *jjFollowed) bool { return f.commit != f.origin }) {
			return true
		}
	}
	return false
}

// readRevision reads argument, a revision argument as the role wrote it, with jjCommitTemplate:
// the commits of `((<argument>) & mutable()) ~ ::remote_bookmarks()`; a read jj refuses is a
// jjStepFailure. Those are the commits of the argument that the step can rewrite: it rewrites no
// commit that a remote bookmark holds or that is under one (readSet, readFillable), and a metaedit
// moves only the commits it rewrites and their descendants, which no remote bookmark holds either.
// Leaving the rest out keeps small, for a broad argument such as `::<C>` on a pushed branch, both
// this read and what the step follows of it through each later read.
func (s *jjStep) readRevision(argument string) ([]jjCommitRead, error) {
	return s.read("((" + argument + ") & mutable()) ~ ::remote_bookmarks()")
}

// revisionSets reads the set of each revision argument that has none yet (revisionSet). The
// step's first metaedit runs after it, and checkArgMax counts each argument by it: once a metaedit
// of the step has run, every argument is held to its set (revisionGiven), whether or not its own
// commits moved, since a rewrite of another argument's commits can change what it names.
func (s *jjStep) revisionSets() {
	for i := range s.revisions {
		s.revisionSet(i)
	}
}

// revisionSet reads, before any rewrite, the set of revision argument i, where it has none and jj
// has not refused a read of it: every commit jj resolves it to, by full id, once, with whether it
// is the working-copy commit (ids). Each failure of that read is a jjStepFailure (ids), which
// leaves the argument refused.
func (s *jjStep) revisionSet(i int) {
	a := &s.revisions[i]
	if a.read || a.refused {
		return
	}
	_, argument := s.push.revision(i)
	ids, working, err := s.ids(argument)
	if err != nil {
		a.refused = true
		return
	}
	a.set, a.read = make([]jjSent, len(ids)), true
	for k, id := range ids {
		a.set[k] = jjSent{id: id, working: id == working}
	}
}

// jjGiven is a revision argument as the step gives it to a pass of the dry run or to the real push
// (revisionGiven): words, the words that hold it, each with what stands in front of the argument in
// its word (the name of --named, or the option's own spelling where the value is attached to it);
// ids, where those words give it as the ids of its commits, those ids, each a full commit id or
// `@`, and else nil; and line, the one line the step prints of it, or "" for none.
type jjGiven struct {
	words []string
	ids   []string
	line  string
}

// given is each revision argument as the step gives it to a pass (revisionGiven), by its place in
// push.revisions. Without moved, an argument counts every followed commit that no longer stands
// for itself, whether readRevisions' reading of a hidden commit as its change's one visible commit
// or a metaedit of the step moved it: the words of each pass of the dry run (dryRunFilled), and the
// real push's after a step that ends without error. With moved, it counts only a commit a metaedit
// of the step moved (jjFollowed): the real push's words after a step that changes nothing more
// (errJJAsWritten), so that a hidden commit that no metaedit moved goes as the role named it. The
// step's loop over a commit with no author can run a metaedit before a dry run it cannot read or
// before checkArgMax: each argument is then held to the commits it named before the step, each one
// a metaedit moved as the commit that replaced it. Only the real push prints the lines (runJJ); a
// dry run drops them.
func (s *jjStep) given(moved bool) []jjGiven {
	given := make([]jjGiven, len(s.push.revisions))
	for i := range given {
		given[i] = s.revisionGiven(i, moved)
	}
	return given
}

// words is the push's words with each revision argument in the words that given gives it, as its
// option takes them (jjPush.revisionWords), and every other word as the role wrote it, every byte,
// each option's own spelling included; dry leaves out the words that stay on the real push alone
// (jjPush.pushOnly).
func (s *jjStep) words(given []jjGiven, dry bool) []string {
	return s.appendWords(make([]string, 0, len(s.push.args)), given, dry, 0, len(s.push.args))
}

// appendWords appends to words what jjStep.words gives of the push's words at indexes from to
// to-1, and answers the slice: the dry run puts words of its own between such ranges
// (jjStep.dryRun).
func (s *jjStep) appendWords(words []string, given []jjGiven, dry bool, from, to int) []string {
	next := 0
	for next < len(s.push.revisions) && s.push.revisions[next].index < from {
		next++
	}
	for j := from; j < to; j++ {
		if dry && s.push.pushOnly[j] {
			continue
		}
		if next == len(s.push.revisions) || s.push.revisions[next].index != j {
			words = append(words, s.push.args[j])
			continue
		}
		for w := range s.push.revisionWords(next, given[next].words) {
			words = append(words, w)
		}
		next++
	}
	return words
}

// revisionTerms is each revision argument as given, a pass of its dry run, gives it, in terms of
// a revset: each id of one that given gives as the ids of its commits, and else its one word
// (revisionGiven's 1, 2 and 4), less what stands in front of the argument, in parentheses, so that
// a read of them goes to as many revsets as keep each under jjWordBound bytes (jjWords).
func (s *jjStep) revisionTerms(given []jjGiven) []string {
	var terms []string
	for i, g := range given {
		if g.ids != nil {
			terms = append(terms, g.ids...)
			continue
		}
		terms = append(terms, "("+g.words[0][s.push.revisions[i].offset:]+")")
	}
	return terms
}

// revisionGiven is revision argument i as the step gives it (jjGiven). Its words name, as jj
// resolves them then, exactly the commits the argument sends (sends): each commit it named before
// any rewrite, or the commit that stands for one now that a counted followed commit no longer
// stands for (given's moved). So each revision argument reaches each pass of the dry run and the
// real push naming exactly the commits it named before the step, each one a metaedit of the step
// replaced as the commit that replaced it, and nothing that a rewrite changes, a change offset
// `<change>/<n>`, `latest()`, `heads()` or an author revset, makes the push send a commit the role
// did not name, whether the rewrite moved the argument's own commits or another argument's. Its
// words are:
//
//  1. the argument as the role wrote it, every byte, where jj refused a read of it
//     (readRevisions), or where no commit of it counts and no metaedit of the step has run
//     (jjStep.edited): nothing has changed what it names, and the step reads nothing more;
//  2. the same where jj resolves the argument as written now to exactly the commits it sends, as
//     jj moves `@`, a bookmark and a change id to the commit a metaedit made;
//  3. else the commits it sends as their ids (jjIDsGiven), where they fit one word under
//     jjWordBound bytes, and where jj refuses the read of 2, in as many words of its option as
//     keep each under the bound (jjWords, jjPush.revisionWords);
//  4. else its primary form (checkedPrimary), where jj resolves it to exactly those commits and
//     its word is under jjWordBound bytes;
//  5. else the ids of 3, in as many words of its option as keep each under the bound. --named
//     names one commit, whose id is one word, so its argument never gets here.
//
// Each of 3 and 5 comes with one line that counts the commits and the words, which only the real
// push prints (runJJ), and the push goes on; 3 of an argument that named no commit before the
// step, the one word `none()`, comes with one line that says so instead. So no word the step writes
// in place of an argument is over jjWordBound bytes, and none names commits other than those the
// argument sends.
func (s *jjStep) revisionGiven(i int, moved bool) jjGiven {
	a := s.revisions[i]
	var counted []*jjFollowed
	for _, f := range a.commits {
		if f.commit != f.origin && (f.moved || !moved) {
			counted = append(counted, f)
		}
	}
	if !a.read || len(counted) == 0 && !s.edited {
		return s.written(i)
	}
	prefix, argument := s.push.revision(i)
	send, sendIDs := s.sends(i, counted)
	now, _, err := s.ids(argument)
	if err == nil && jjSameSet(now, sendIDs) {
		return s.written(i)
	}
	given := jjIDsGiven(prefix, send)
	if len(send) == 0 {
		given.line = fmt.Sprintf("legion jj: revision `%s` names other commits after the step; it named no commit before the step, so the push gets none()", argument)
		return given
	}
	if err == nil && len(given.words) > 1 {
		if primary := s.checkedPrimary(prefix, argument, now, counted, send, sendIDs); primary != "" {
			return jjGiven{words: []string{prefix + primary}}
		}
	}
	given.line = fmt.Sprintf("legion jj: revision `%s` names other commits after the step; the push gets the %d commit(s) it named before the step, as the step left them, by their ids, in %d word(s) of its option, each under %d bytes", argument, len(given.ids), len(given.words), jjWordBound)
	return given
}

// written is revision argument i as the role wrote it: its one word, every byte.
func (s *jjStep) written(i int) jjGiven {
	return jjGiven{words: []string{s.push.args[s.push.revisions[i].index]}}
}

// jjIDsGiven is a revision argument that gives the push sent, the commits it sends, as their ids
// (jjSent.word), in as many words of its option as keep each, prefix, what stands in front of the
// argument, then the ids joined by ` | `, under jjWordBound bytes (jjWords); and as the one word
// prefix then `none()` where it sends no commit, as an argument that named none before the step
// does.
func jjIDsGiven(prefix string, sent []jjSent) jjGiven {
	ids := make([]string, len(sent))
	for k, c := range sent {
		ids[k] = c.word()
	}
	if len(ids) == 0 {
		return jjGiven{words: []string{prefix + "none()"}, ids: ids}
	}
	return jjGiven{words: jjWords(prefix, ids, ""), ids: ids}
}

// checkedPrimary is the primary form of revision argument argument (revisionGiven's 4),
// `(<argument>) ~ (<old>) ~ (<extra>) | <new> | <missing>` (jjPrimary), or "" where the step gives
// none: old the ids of counted's commits, new the commits that stand for them, extra the commits
// jj resolves less, `(<argument>) ~ (<old>)`, to that the argument does not send, and missing
// those it sends (send, whose ids sendIDs holds) that are neither among them nor new, `@` for the
// working-copy commit in new and missing, each part left out where it is empty. An argument with
// no counted commit has neither old nor new, and its less is `(<argument>)`, which jj resolved to
// now, its read of the argument as written. Inside the form the argument means what it meant in
// that read, so the form resolves to the commits it sends. The step gives the form where jj
// resolves it to exactly send's commits and its word, prefix and then the form, is under
// jjWordBound bytes. It reads less only where the word of the form without that read, less then
// ` | <new>`, is under the bound: the form holds that one, so its word would not be under the
// bound either. It reads the form only where its word is under the bound. A read jj refuses gives
// no form.
func (s *jjStep) checkedPrimary(prefix, argument string, now []string, counted []*jjFollowed, send []jjSent, sendIDs map[string]bool) string {
	var old, added []string
	isNew := map[string]bool{}
	for _, f := range counted {
		old = append(old, f.origin)
		if !isNew[f.commit] {
			isNew[f.commit] = true
			added = append(added, jjSent{id: f.commit, working: f.working}.word())
		}
	}
	less, left := "("+argument+")", now
	if len(old) > 0 {
		less += " ~ (" + strings.Join(old, " | ") + ")"
		if len(prefix)+len(less)+len(" | ")+len(strings.Join(added, " | ")) >= jjWordBound {
			return ""
		}
		var err error
		if left, _, err = s.ids(less); err != nil {
			return ""
		}
	}
	primary := jjPrimary(less, left, added, send, sendIDs, isNew)
	if len(prefix)+len(primary) >= jjWordBound {
		return ""
	}
	if got, _, err := s.ids(primary); err != nil || !jjSameSet(got, sendIDs) {
		return ""
	}
	return primary
}

// jjPrimary is the primary form of a revision argument (checkedPrimary): less, the argument less
// its old commits, then ` ~ (<extra>)`, extra being each commit of left, the commits jj resolved
// less to, that send does not hold, then ` | <added>`, the commits that stand for the old ones, and
// ` | <missing>`, missing being each commit of send that neither left nor isNew holds, each list
// joined by ` | ` and each part left out where its list is empty. sendIDs is send's ids.
func jjPrimary(less string, left, added []string, send []jjSent, sendIDs, isNew map[string]bool) string {
	inLeft := make(map[string]bool, len(left))
	for _, id := range left {
		inLeft[id] = true
	}
	var missing []string
	for _, c := range send {
		if !inLeft[c.id] && !isNew[c.id] {
			missing = append(missing, c.word())
		}
	}
	primary := less
	if extra := jjNotIn(left, sendIDs); len(extra) > 0 {
		primary += " ~ (" + strings.Join(extra, " | ") + ")"
	}
	for _, part := range [][]string{added, missing} {
		if len(part) > 0 {
			primary += " | " + strings.Join(part, " | ")
		}
	}
	return primary
}

// jjNotIn is each of ids that set does not hold, in order.
func jjNotIn(ids []string, set map[string]bool) []string {
	var out []string
	for _, id := range ids {
		if !set[id] {
			out = append(out, id)
		}
	}
	return out
}

// checkArgMax decides, before setAuthors' first metaedit, and before the step's snapshot where that
// is due (setAuthors), whether the push can still run once each revision argument has the words the
// step would give it after that rewrite (revisionGiven). Once a metaedit has run, every argument is
// held to its set, and the step cannot know its words before the rewrite: it goes as the role wrote
// it where jj then resolves it to the commits it sends, else as their ids where those fit one word
// under jjWordBound bytes, else as its primary form, whose word is under that bound and so shorter
// than those ids in several words, and else as those ids. Each id is as long as the id that stands
// for it after the rewrite. So the step reads each argument's set (revisionSets) and counts each
// argument at the larger (givenSize, jjSize) of its word as the role wrote it and the ids of its
// set (jjIDsGiven), and one that jj refused a read of at its own word. Where the push's command
// counted so would not be under jjArgMax bytes (commandSize), the step prints one line that names
// the argument with the most ids, their number and the limit (asWritten), and answers
// errJJAsWritten: setAuthors rewrites nothing, the step takes no snapshot it has not taken, and the
// push runs with the role's own words, but where the fill of a commit with no author ran a
// metaedit (jjStep.given), and as jjStep.snapshotted says where the step took its snapshot, with a
// flag that commandSize counts. Else, and for a push with no argument that jj resolves, which no
// rewrite makes longer, it answers nil.
func (s *jjStep) checkArgMax() error {
	s.revisionSets()
	written := make([]jjGiven, len(s.push.revisions))
	extra, widest := 0, -1
	for i, a := range s.revisions {
		written[i] = s.written(i)
		if !a.read {
			continue
		}
		prefix, _ := s.push.revision(i)
		own := s.givenSize(i, written[i])
		extra += max(s.givenSize(i, jjIDsGiven(prefix, a.set)), own) - own
		if widest < 0 || len(a.set) > len(s.revisions[widest].set) {
			widest = i
		}
	}
	if widest < 0 || s.commandSize(written)+extra < jjArgMax {
		return nil
	}
	_, argument := s.push.revision(widest)
	return s.asWritten("set the author", fmt.Sprintf("revision `%s` would go to the push as the ids of its %d commits, and the push's arguments and environment would not be under %d bytes", argument, len(s.revisions[widest].set), jjArgMax))
}

// givenSize is the bytes that revision argument i, as g gives it, takes of the push's command
// (jjSize): each word it takes there (jjPush.revisionWords).
func (s *jjStep) givenSize(i int, g jjGiven) int {
	size := 0
	for word := range s.push.revisionWords(i, g.words) {
		size += jjSize(word)
	}
	return size
}

// commandSize is the bytes that Linux counts against jjArgMax (jjSize) for the push with each
// revision argument as given gives it. The arguments are next and the push's words, with the dry
// run's own (jjDryRunFlags, jjDryRunQuiet), which its dry run has beside the same words and which
// hold the --ignore-working-copy that the real push has after the step's snapshot
// (jjStep.snapshotted), and the environment is the step's (env) with wrappedJJStepped set to a
// digest (jjStepped), which the real push has beside it; so each of the two commands counts that
// many bytes or fewer.
func (s *jjStep) commandSize(given []jjGiven) int {
	return jjSize(s.words(given, false)...) + jjSize(jjDryRunFlags...) + jjSize(jjDryRunQuiet...) + jjSize(s.env...) +
		jjSize(s.next, wrappedJJStepped+"=") + hex.EncodedLen(sha256.Size)
}

// sends is the commits revision argument i sends after the step, send, and their ids, sendIDs:
// each commit of its set (revisionSet), or, for the origin of one of counted, the followed commits
// of it that no longer stand for themselves, the commit that stands for that one now, each once.
// The working-copy commit is one of counted's that stood for it after the step's read
// (jjFollowed's working), or, among the commits of its set, the one the read of that set found to
// be it: no metaedit of the step moved that one, so it still is.
func (s *jjStep) sends(i int, counted []*jjFollowed) (send []jjSent, sendIDs map[string]bool) {
	stands := make(map[string]*jjFollowed, len(counted))
	for _, f := range counted {
		stands[f.origin] = f
	}
	set := s.revisions[i].set
	sendIDs = make(map[string]bool, len(set))
	for _, c := range set {
		if f, ok := stands[c.id]; ok {
			c = jjSent{id: f.commit, working: f.working}
		}
		if !sendIDs[c.id] {
			sendIDs[c.id] = true
			send = append(send, c)
		}
	}
	return send, sendIDs
}

// jjSameSet is whether ids, commit ids each once as jj prints them, are exactly the ids of set.
func jjSameSet(ids []string, set map[string]bool) bool {
	if len(ids) != len(set) {
		return false
	}
	for _, id := range ids {
		if !set[id] {
			return false
		}
	}
	return true
}
