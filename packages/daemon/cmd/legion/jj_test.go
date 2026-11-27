package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A revision argument that the step gives in more than one word repeats its option as the role
// spelled it (jjStep.words, jjPush.revisionWords): the word of the option stands again before each
// word after the first of a value that follows it, and each word of a value attached to its option
// carries the option. Every other word stays as the role wrote it, and the dry run's words leave
// out those that stay on the real push alone.
func TestARevisionArgumentInSeveralWordsRepeatsItsOption(t *testing.T) {
	t.Parallel()
	s := &jjStep{push: readJJPush([]string{"--quiet", "git", "push", "-r", "a", "--revisions=b", "--named", "nm=c", "-cd"})}
	if len(s.push.revisions) != 4 {
		t.Fatalf("readJJPush read %d revision arguments, want 4", len(s.push.revisions))
	}
	given := []jjGiven{
		{words: []string{"1 | 2", "3"}, ids: []string{"1", "2", "3"}},
		{words: []string{"--revisions=4", "--revisions=5"}, ids: []string{"4", "5"}},
		{words: []string{"nm=6"}, ids: []string{"6"}},
		{words: []string{"-cd"}},
	}
	dry := []string{"git", "push", "-r", "1 | 2", "-r", "3", "--revisions=4", "--revisions=5", "--named", "nm=6", "-cd"}
	if got := s.words(given, true); !slices.Equal(got, dry) {
		t.Errorf("the dry run's words are %q, want %q", got, dry)
	}
	if got, want := s.words(given, false), append([]string{"--quiet"}, dry...); !slices.Equal(got, want) {
		t.Errorf("the real push's words are %q, want %q", got, want)
	}
}

// A list of ids that one revset of a read, or one word of a revision argument that the step gives
// the push as the ids of its commits, would hold over jjWordBound bytes goes to as many words as
// keep each under the bound (jjWords): each starts with the prefix and ends with the suffix, and
// they hold every term once, in order. A term too long for a word under the bound is a word alone,
// and a list that fits is one word: a word one byte under the bound holds every id after its
// prefix, and the ids of a word that would be of the bound's length, or one byte over it, go to two
// words, each with the prefix.
func TestTheStepsReadsOfAListOfIDsStayUnderTheBound(t *testing.T) {
	t.Parallel()
	const prefix, suffix = "(::(", ") ~ ::remote_bookmarks()) & mutable()"
	terms := make([]string, 3001)
	for i := range terms {
		terms[i] = fmt.Sprintf("commit_id(%040x)", i)
	}
	long := strings.Repeat("x", jjWordBound)
	ids := make([]string, jjWordBound/len(fmt.Sprintf("%040x | ", 0)))
	for i := range ids {
		ids[i] = fmt.Sprintf("%040x", i)
	}
	// behind is a prefix of a revision's word that puts the word of every id behind it at size
	// bytes.
	behind := func(size int) string { return strings.Repeat("n", size-len(strings.Join(ids, " | "))-1) + "=" }
	for _, row := range []struct {
		name           string
		prefix, suffix string
		terms          []string
		words          int
	}{
		{name: "3,001 commit ids", prefix: prefix, suffix: suffix, terms: terms, words: 2},
		{name: "one commit id", prefix: prefix, suffix: suffix, terms: terms[:1], words: 1},
		{name: "a term over the bound between two commit ids", prefix: prefix, suffix: suffix, terms: []string{terms[0], long, terms[1]}, words: 3},
		{name: "a revision's ids one byte under the bound", prefix: behind(jjWordBound - 1), terms: ids, words: 1},
		{name: "a revision's ids at the bound", prefix: behind(jjWordBound), terms: ids, words: 2},
		{name: "a revision's ids one byte over the bound", prefix: behind(jjWordBound + 1), terms: ids, words: 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			words := jjWords(row.prefix, row.terms, row.suffix)
			if len(words) != row.words {
				t.Errorf("jjWords made %d words, want %d", len(words), row.words)
			}
			var got []string
			for _, word := range words {
				if len(word) >= jjWordBound && !strings.Contains(word, long) {
					t.Errorf("jjWords made a word of %d bytes, want under %d", len(word), jjWordBound)
				}
				inner, prefixed := strings.CutPrefix(word, row.prefix)
				inner, suffixed := strings.CutSuffix(inner, row.suffix)
				if !prefixed || !suffixed {
					t.Fatalf("jjWords made %.80q…, want each word %.80q, the terms, then %q", word, row.prefix, row.suffix)
				}
				got = append(got, strings.Split(inner, " | ")...)
			}
			if !slices.Equal(got, row.terms) {
				t.Errorf("jjWords' words hold %d terms, want the %d given, each once, in order", len(got), len(row.terms))
			}
		})
	}
}
