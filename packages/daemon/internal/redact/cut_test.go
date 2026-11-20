package redact

import (
	"strings"
	"testing"
)

// plantedUser and plantedQuery are what the cut tests write into a URL as its user name and its
// query (plantedURL): a user name with no password, which the push's leak check reads as no
// credential, and a query value of no real token's shape.
const (
	plantedUser  = "legion-reader"
	plantedQuery = "token=planted-query-token-1"
	plantedURL   = "https://" + plantedUser + "@h.example/p?" + plantedQuery
)

// partOf is the first four bytes in a row of planted that text holds, and "" where it holds none.
// No text a cut test writes around a URL holds four bytes in a row of either planted value.
func partOf(text, planted string) string {
	for i := 0; i+4 <= len(planted); i++ {
		if strings.Contains(text, planted[i:i+4]) {
			return planted[i : i+4]
		}
	}
	return ""
}

// fallsInside fails the test unless cut, the text as the bound alone would cut it, holds a part of
// planted but not the whole of it: the bound falls inside planted, so the case reads what the
// order of the rule and the cut does there.
func fallsInside(t *testing.T, cut, planted string) {
	t.Helper()
	if planted != "" && (partOf(cut, planted) == "" || strings.Contains(cut, planted)) {
		t.Fatalf("the bound alone cuts the text to %q, which does not end inside %q: the case tests nothing", cut, planted)
	}
}

// A URL the bound of Runes falls inside leaves no part of its user name or its query, since the
// rule reads the whole text before the cut, and a text the rule leaves alone is cut as it was
// before the rule: its first n-1 runes and "…", or kept whole at n runes.
func TestRunesCutsWhatTheURLRuleLeavesOfTheWholeText(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		n          int
		// planted is what the bound falls inside, which no part of may be left.
		planted, want string
	}{
		{
			name:    "a bound inside a URL's user name",
			text:    strings.Repeat("w", 25) + " " + plantedURL,
			n:       40,
			planted: plantedUser,
			want:    strings.Repeat("w", 25) + " https://h.exa…",
		},
		{
			name:    "a bound inside a URL's query",
			text:    strings.Repeat("w", 10) + " https://h.example/p?" + plantedQuery + " " + strings.Repeat("z", 30),
			n:       40,
			planted: plantedQuery,
			want:    strings.Repeat("w", 10) + " https://h.example/p " + strings.Repeat("z", 8) + "…",
		},
		{
			name: "a text one rune past the bound",
			text: strings.Repeat("é", 41),
			n:    40,
			want: strings.Repeat("é", 39) + "…",
		},
		{
			name: "a text of exactly the bound kept whole",
			text: strings.Repeat("é", 40),
			n:    40,
			want: strings.Repeat("é", 40),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallsInside(t, string([]rune(tc.text)[:tc.n-1]), tc.planted)
			got := Runes(tc.text, tc.n)
			if tc.planted != "" {
				if part := partOf(got, tc.planted); part != "" {
					t.Fatalf("Runes = %q, which holds %q of %q", got, part, tc.planted)
				}
			}
			if got != tc.want {
				t.Fatalf("Runes(%q, %d) = %q, want %q", tc.text, tc.n, got, tc.want)
			}
		})
	}
}

// A URL the bound of Bytes falls inside leaves no part of its user name or its query, and a text
// the rule leaves alone is cut as it was before the rule: at the rune start at or below the bound,
// with no ellipsis, and cut says whether it was.
func TestBytesCutsWhatTheURLRuleLeavesOfTheWholeText(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		n          int
		planted    string
		want       string
		wantCut    bool
	}{
		{
			name:    "a bound inside a URL's user name",
			text:    strings.Repeat("w", 25) + " " + plantedURL,
			n:       40,
			planted: plantedUser,
			want:    strings.Repeat("w", 25) + " https://h.exam",
			wantCut: true,
		},
		{
			name:    "a bound inside a URL's query",
			text:    strings.Repeat("w", 10) + " https://h.example/p?" + plantedQuery + " " + strings.Repeat("z", 30),
			n:       40,
			planted: plantedQuery,
			want:    strings.Repeat("w", 10) + " https://h.example/p " + strings.Repeat("z", 9),
			wantCut: true,
		},
		{
			name:    "a bound inside a character",
			text:    strings.Repeat("é", 25),
			n:       41,
			want:    strings.Repeat("é", 20),
			wantCut: true,
		},
		{
			name: "a text of exactly the bound kept whole",
			text: strings.Repeat("é", 20),
			n:    40,
			want: strings.Repeat("é", 20),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallsInside(t, tc.text[:min(len(tc.text), tc.n)], tc.planted)
			got, cut := Bytes(tc.text, tc.n)
			if tc.planted != "" {
				if part := partOf(got, tc.planted); part != "" {
					t.Fatalf("Bytes = %q, which holds %q of %q", got, part, tc.planted)
				}
			}
			if got != tc.want || cut != tc.wantCut {
				t.Fatalf("Bytes(%q, %d) = %q, %t; want %q, %t", tc.text, tc.n, got, cut, tc.want, tc.wantCut)
			}
		})
	}
}

// The end Tail keeps of a URL that starts before the kept window holds no part of that URL's
// query or its user name, since the rule reads the whole text before the cut, and a text the rule
// leaves alone is cut as it was before the rule: its last n bytes, moved forward to a rune's
// start, with the count of the bytes before them.
func TestTailKeepsTheEndOfWhatTheURLRuleLeavesOfTheWholeText(t *testing.T) {
	path := strings.Repeat("p", 30)
	for _, tc := range []struct {
		name, text string
		n          int
		planted    string
		want       string
		wantCut    int
	}{
		{
			name:    "a window that starts inside a URL's query",
			text:    "start https://" + plantedUser + "@h.example/" + path + "?" + plantedQuery + " end",
			n:       20,
			planted: plantedQuery,
			want:    strings.Repeat("p", 16) + " end",
			wantCut: len("start https://h.example/") + 14,
		},
		{
			name:    "a window that starts inside a URL's user name",
			text:    strings.Repeat("x", 30) + " https://" + plantedUser + "@h.example/" + path,
			n:       len("ader@h.example/") + len(path),
			planted: plantedUser,
			want:    "ps://h.example/" + path,
			wantCut: 34,
		},
		{
			name:    "a window that starts inside a character",
			text:    strings.Repeat("é", 20) + "x",
			n:       20,
			want:    strings.Repeat("é", 9) + "x",
			wantCut: 22,
		},
		{
			name:    "a window of bytes that start no character",
			text:    strings.Repeat("\x80", 30),
			n:       20,
			want:    strings.Repeat("\x80", 20),
			wantCut: 10,
		},
		{
			name: "a text inside the bound kept whole",
			text: "fatal: the remote end hung up unexpectedly",
			n:    64,
			want: "fatal: the remote end hung up unexpectedly",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallsInside(t, tc.text[max(0, len(tc.text)-tc.n):], tc.planted)
			got, cut := Tail(tc.text, tc.n)
			for _, planted := range []string{plantedUser, plantedQuery} {
				if part := partOf(got, planted); part != "" {
					t.Fatalf("Tail = %q, which holds %q of %q", got, part, planted)
				}
			}
			if got != tc.want || cut != tc.wantCut {
				t.Fatalf("Tail(%q, %d) = %q, %d; want %q, %d", tc.text, tc.n, got, cut, tc.want, tc.wantCut)
			}
		})
	}
}

// A read its bound stopped inside a URL, before its user name's "@" or inside its query, keeps
// no part of either: Read drops the run the bound stopped inside, which the rule alone reads as a
// URL with no userinfo. A read the bound did not cut is kept whole, through the rule, and so is
// one the bound cut just after a byte that ends a URL.
func TestReadDropsTheRunABoundedReadStoppedInside(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		cut        bool
		planted    string
		want       string
	}{
		{
			name:    "a read that stopped inside a URL's user name",
			text:    "moved to " + plantedURL[:len("https://legion-rea")],
			cut:     true,
			planted: plantedUser,
			want:    "moved to ",
		},
		{
			name:    "a read that stopped inside a URL's query",
			text:    "moved to https://h.example/p?" + plantedQuery[:len("token=planted-qu")],
			cut:     true,
			planted: plantedQuery,
			want:    "moved to ",
		},
		{
			name: "a read that stopped inside a run with no end",
			text: "unfinished",
			cut:  true,
			want: "",
		},
		{
			name: "a read that stopped just after a line's end",
			text: "the first line\n",
			cut:  true,
			want: "the first line\n",
		},
		{
			name: "a read its bound did not cut",
			text: "moved to " + plantedURL,
			want: "moved to https://h.example/p",
		},
		{
			name: "a read its bound did not cut, with no end",
			text: "unfinished",
			want: "unfinished",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallsInside(t, tc.text, tc.planted)
			got := Read(tc.text, tc.cut)
			if tc.planted != "" {
				if part := partOf(got, tc.planted); part != "" {
					t.Fatalf("Read = %q, which holds %q of %q", got, part, tc.planted)
				}
			}
			if got != tc.want {
				t.Fatalf("Read(%q, %t) = %q, want %q", tc.text, tc.cut, got, tc.want)
			}
		})
	}
}
