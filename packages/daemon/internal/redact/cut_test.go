package redact

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"
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
// with no ellipsis, and cut says whether it was. A run of bytes that start no character is cut at
// the bound, not stepped back over to the text's start.
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
			// The bound falls two bytes into a three-byte character, the start two bytes back.
			name:    "a bound inside a three-byte character",
			text:    strings.Repeat("€", 20),
			n:       41,
			want:    strings.Repeat("€", 13),
			wantCut: true,
		},
		{
			// The bound falls three bytes into a four-byte character, the start as far back as
			// Bytes looks, utf8.UTFMax-1 bytes.
			name:    "a bound inside a four-byte character",
			text:    strings.Repeat("𝄞", 15),
			n:       43,
			want:    strings.Repeat("𝄞", 10),
			wantCut: true,
		},
		{
			name:    "a bound inside a run of bytes that start no character",
			text:    strings.Repeat("\x80", 10),
			n:       9,
			want:    strings.Repeat("\x80", 9),
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

// TestBytesHoldsEveryBoundary generates the whole cut boundary. For each length from eight below
// through eight above 128, each character width 1 to 4 starts at every byte that ends at or
// crosses it. The malformed row holds the bounded look-back. The URL row is long before the rule
// and short after it. The invariants are a longest whole-character prefix of URLCredentials(text),
// no longer than the bound, and the cut bit that length decides.
func TestBytesHoldsEveryBoundary(t *testing.T) {
	const (
		bound = 128
		span  = 8
	)
	for _, edge := range bytesBoundaryCases(bound, span) {
		t.Run(edge.name, func(t *testing.T) {
			redacted := URLCredentials(edge.text)
			want, wantCut := bytesWholePrefixAtMost(redacted, bound)
			got, cut := Bytes(edge.text, bound)
			if !strings.HasPrefix(redacted, got) || len(got) > bound || got != want || cut != wantCut {
				t.Fatalf("Bytes of %s, %d bytes before redaction and %d after, = %q, %t; want longest whole prefix %q, %t",
					edge.name, len(edge.text), len(redacted), got, cut, want, wantCut)
			}
		})
	}
}

type bytesBoundaryCase struct {
	name string
	text string
}

func bytesBoundaryCases(bound, span int) []bytesBoundaryCase {
	cases := []bytesBoundaryCase{}
	for total := bound - span; total <= bound+span; total++ {
		for _, char := range []string{"x", "é", "€", "𝄞"} {
			if total < bound && total >= len(char) {
				start := total - len(char)
				cases = append(cases, bytesBoundaryCase{
					name: fmt.Sprintf("%d bytes, a %d-byte character ending at byte %d", total, len(char), total),
					text: strings.Repeat("a", start) + char,
				})
			}
			for start := max(0, bound-len(char)); start <= bound; start++ {
				suffix := total - start - len(char)
				if suffix < 0 {
					continue
				}
				cases = append(cases, bytesBoundaryCase{
					name: fmt.Sprintf("%d bytes, a %d-byte character at byte %d", total, len(char), start),
					text: strings.Repeat("a", start) + char + strings.Repeat("z", suffix),
				})
			}
		}
	}
	cases = append(cases,
		bytesBoundaryCase{
			name: "a malformed-byte run at the bound",
			text: strings.Repeat("a", bound-span) + strings.Repeat("\x80", span*2),
		},
		bytesBoundaryCase{
			name: "a URL redacted under the bound",
			text: "https://" + strings.Repeat("u", bound+span*2) + "@h.example/p",
		},
	)
	return cases
}

func bytesWholePrefixAtMost(text string, bound int) (string, bool) {
	if len(text) <= bound {
		return text, false
	}
	end := 0
	for at := range text {
		if at > bound {
			break
		}
		end = at
	}
	return text[:end], true
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
// no part of either: Read drops the run the bound stopped inside, which holds the URL's "://" and
// which the rule alone reads as a URL with no userinfo. A read the bound did not cut is kept
// whole, through the rule, and so is one the bound cut just after a byte that ends a URL, and one
// whose last run, after its last byte that ends a URL or with none, holds no "://": no URL's
// userinfo or query lies in such a run.
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
			name: "a read that stopped inside a run with no end and no \"://\"",
			text: "unfinished",
			cut:  true,
			want: "unfinished",
		},
		{
			name: "a read that stopped inside a run with no \"://\" after a URL",
			text: "moved to " + plantedURL + " unfinish",
			cut:  true,
			want: "moved to https://h.example/p unfinish",
		},
		{
			name: "a read that stopped inside a run with no end that holds \"://\"",
			text: "https://h.example/unfinished",
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

// Read of a text cut at any point, as a read its bound cut there, keeps no part of a user name or
// a query that the URL rule removes from the whole text, and of a text the rule leaves alone it
// keeps a start of that text: for 2000 texts built under a fixed seed from words, white space,
// quotes, "https://", a bare "://", "@", "?", a URL's user name, host, path, query and fragment
// with and without an "https://" before it, and bytes that start no character, at each of their
// cut points. The rule alone after such a cut keeps the start of a user name as a host, or the end
// of a query as a path, and the test fails where no cut of the texts meets that.
func TestReadOfAnyCutKeepsNoPartOfWhatTheURLRuleRemovesFromTheWholeText(t *testing.T) {
	random := rand.New(rand.NewPCG(4193, 4463))
	leaks := 0
	for n := range 2000 {
		query := fmt.Sprintf("token=planted-query-token-%d", n)
		rest := plantedUser + "@h.example/p?" + query + "#frag"
		pieces := []string{"moved", "to", " ", "\n", "\u00a0", `"`, "'", "<", ">", "https://", "://", "@", "?",
			"é", "\xff", "\x80", rest, "https://" + rest, "https://" + rest}
		var text strings.Builder
		for range 1 + random.IntN(12) {
			text.WriteString(pieces[random.IntN(len(pieces))])
		}
		whole := text.String()
		ruled := URLCredentials(whole)
		var removed []string
		for _, planted := range []string{plantedUser, query} {
			if partOf(ruled, planted) == "" {
				removed = append(removed, planted)
			}
		}
		for c := range len(whole) + 1 {
			got := Read(whole[:c], true)
			if ruled == whole && !strings.HasPrefix(whole, got) {
				t.Fatalf("Read(%q, true) = %q, want a start of %q, which the rule leaves alone", whole[:c], got, whole)
			}
			if len(removed) == 0 {
				continue
			}
			alone := URLCredentials(whole[:c])
			for _, planted := range removed {
				if part := partOf(got, planted); part != "" {
					t.Fatalf("Read(%q, true) = %q, which holds %q of %q, which the rule removes from %q", whole[:c], got, part, planted, whole)
				}
				if partOf(alone, planted) != "" {
					leaks++
				}
			}
		}
	}
	if leaks == 0 {
		t.Fatal("the rule alone keeps a part of what it removes from the whole text after no cut of the texts: the test tests nothing")
	}
}

// ReadBounded answers the first limit bytes of a read and whether its bound cut them: cut where
// there was more to read than limit, kept whole and not cut at limit bytes or fewer, and cut where
// the read ended on an error, which it answers with what came before the error.
func TestReadBoundedTellsAReadItsBoundCutFromOneReadWhole(t *testing.T) {
	reset := errors.New("connection reset mid-body")
	for _, tc := range []struct {
		name    string
		r       io.Reader
		want    string
		wantCut bool
		wantErr error
	}{
		{name: "a body longer than the limit", r: strings.NewReader("0123456789"), want: "01234567", wantCut: true},
		{name: "a body of exactly the limit", r: strings.NewReader("01234567"), want: "01234567"},
		{name: "a body shorter than the limit", r: strings.NewReader("0123"), want: "0123"},
		{
			name: "a read that ended on an error", r: io.MultiReader(strings.NewReader("0123"), iotest.ErrReader(reset)),
			want: "0123", wantCut: true, wantErr: reset,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, cut, err := ReadBounded(tc.r, 8)
			if string(kept) != tc.want || cut != tc.wantCut || !errors.Is(err, tc.wantErr) {
				t.Fatalf("ReadBounded = %q, %t, %v; want %q, %t, %v", kept, cut, err, tc.want, tc.wantCut, tc.wantErr)
			}
		})
	}
}
