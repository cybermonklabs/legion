package leakguard

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The fixture rules compile, and they are the four leakguardtest.Rules holds: the host, the pair,
// the word in a text and the word in a path. A rule file that did not compile would be a panic
// inside the process that read it rather than a refusal, which is why LoadRules recovers one; this
// is the test that says the fixture every other test runs needs no recovery.
func TestTheFixtureRulesCompile(t *testing.T) {
	rules := fixtureRules(t)
	ids := make([]string, 0, len(rules.detector.Config.Rules))
	for id := range rules.detector.Config.Rules {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if want := []string{"host_rule", "pair_rule", "word_path_rule", "word_rule"}; !slices.Equal(ids, want) {
		t.Errorf("the rules are %q, want %q", ids, want)
	}
	if rules.Count() != len(ids) {
		t.Errorf("the set counts %d rules, want %d", rules.Count(), len(ids))
	}
}

// Each fixture rule refuses what it is written for and nothing beside it: the host and not the
// example machine, the pair on one line and not on two, the word as a word and not inside another,
// and the word in a record's path by the path rule, whose finding stands before the record's lines.
func TestTheFixtureRulesMatchWhatTheyAreWrittenFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record Record
		// want is the report, "<record>: <class>" a line, and empty where the record passes.
		want []string
	}{
		{
			name:   "a machine of the invented tailnet",
			record: Record{Label: "the title", Text: "a machine on " + leakguardtest.RulesHost},
			want:   []string{"the title: " + leakguardtest.RulesHostClass},
		},
		{
			name:   "the example machine",
			record: Record{Label: "the title", Text: "a machine on " + leakguardtest.AllowedHost},
		},
		{
			name:   "the pair on one line",
			record: Record{Label: "the body", Text: strings.ToUpper(leakguardtest.PairName) + " keeps the " + leakguardtest.PairWord + " ledger"},
			want:   []string{"the body: " + leakguardtest.PairClass},
		},
		{
			name:   "the pair on two lines",
			record: Record{Label: "the body", Text: leakguardtest.PairName + " keeps\nthe " + leakguardtest.PairWord + " ledger"},
		},
		{
			name:   "the pair's name alone",
			record: Record{Label: "the body", Text: "a note on " + leakguardtest.PairName},
		},
		{
			name:   "the word as a word",
			record: Record{Label: "the body", Text: "the " + strings.ToUpper(leakguardtest.Word) + " runbook"},
			want:   []string{"the body: " + leakguardtest.WordClass},
		},
		{
			name:   "the word inside another word",
			record: Record{Label: "the body", Text: "a " + leakguardtest.Word + "s ledger"},
		},
		{
			name: "the word in a path",
			record: Record{
				Label: "docs/" + leakguardtest.Word + "-notes.md",
				Text:  "docs/" + leakguardtest.Word + "-notes.md",
				Path:  "docs/" + leakguardtest.Word + "-notes.md",
			},
			want: []string{
				"docs/" + leakguardtest.Word + "-notes.md: " + leakguardtest.WordPathClass,
				"docs/" + leakguardtest.Word + "-notes.md: " + leakguardtest.WordClass,
			},
		},
		{
			name:   "the word in a label that is no path",
			record: Record{Label: "docs/" + leakguardtest.Word + "-notes.md", Text: "a line of ordinary prose"},
		},
		{
			name:   "ordinary prose",
			record: Record{Label: "docs/testing.md", Text: "a line of ordinary prose"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, err := fixtureRules(t).scan(context.Background(), []Record{tc.record})
			if err != nil {
				t.Fatal(err)
			}
			reports(t, lines, tc.want...)
		})
	}
}

// A path-scoped allowlist clears its one path and no other: the word rule ignores the vendored
// file's own use of the word, and the same line in a file beside it is refused. The allowlist reads
// a record's Path and never its label, so a record labelled with the vendored file's path and
// holding no Path, which is what a gh call's body file of that name is, is refused too.
func TestAPathScopedAllowlistClearsItsPathAlone(t *testing.T) {
	const line = "var " + leakguardtest.Word + " = require('./lantern')"
	rules := fixtureRules(t)
	if err := rules.Check(bound(t), "the write", Record{Label: leakguardtest.AllowedPath, Text: line, Path: leakguardtest.AllowedPath}); err != nil {
		t.Fatalf("the vendored file's own line: %v", err)
	}
	const beside = "vendor/glasswing/other.js"
	reports(t, refusal(t, rules.Check(bound(t), "the write", Record{Label: beside, Text: line, Path: beside})).Lines, beside+": "+leakguardtest.WordClass)
	reports(t, refusal(t, rules.Check(bound(t), "the write", Record{Label: leakguardtest.AllowedPath, Text: line})).Lines,
		leakguardtest.AllowedPath+": "+leakguardtest.WordClass)
}

// A refusal shows what matched within its bounds, whatever the write is. A mebibyte of one line
// shows its match and a window of the line around it, each end of the window cut on a rune
// boundary; a match of 500 bytes is cut at 200, and one with a rune across byte 200 is cut before
// that rune; sixty findings show forty lines and one that counts the rest; a newline and an escape
// character in a match print quoted. Two matches of one line, far apart, are each shown at their
// own place, on the first line and on a later one, which holds the engine to the column
// RuleSet.place reads; and a match the engine begins at the newline before its line is shown on
// that line.
func TestRefusalShowsMatchWithinItsBounds(t *testing.T) {
	const (
		host = leakguardtest.RulesHost
		word = leakguardtest.Word
	)
	shows := func(t *testing.T, rules *RuleSet, text string, want ...string) {
		t.Helper()
		got := refusal(t, rules.Check(bound(t), "the write", Record{Label: "the body", Text: text})).Lines
		if !slices.Equal(got, want) {
			t.Errorf("the report is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}

	t.Run("a mebibyte of one line", func(t *testing.T) {
		// A two-byte rune stands across each end of the window: 120 bytes before the match falls
		// inside the first, and 120 bytes after it inside the second, so each end is cut on the
		// rune's far side.
		before := strings.Repeat("a", 281) + "é" + strings.Repeat("a", 118) + " "
		match := host + " "
		after := strings.Repeat("b", 119) + "é"
		text := before + match + after + strings.Repeat("b", 1<<20-len(before)-len(match)-len(after))
		if len(text) != 1<<20 || strings.Contains(text, "\n") || len(before) <= 300 {
			t.Fatalf("the text is %d bytes with its match at byte %d, want a mebibyte of one line with the match past byte 300", len(text), len(before))
		}
		shows(t, fixtureRules(t), text,
			leakguardtest.ReportLine("the body", leakguardtest.RulesHostClass, match, 1, "…"+strings.Repeat("a", 118)+" "+match+strings.Repeat("b", 119)+"…"))
	})
	t.Run("a match of 500 bytes", func(t *testing.T) {
		match := leakguardtest.PairName + strings.Repeat("x", 483) + leakguardtest.PairWord
		if len(match) != 500 {
			t.Fatalf("the match is %d bytes, want 500", len(match))
		}
		shown := leakguardtest.PairName + strings.Repeat("x", 192)
		// The line shows 120 bytes past the part of the match shown, which here is more of the
		// match.
		shows(t, fixtureRules(t), "a note: "+match+" ends here",
			leakguardtest.ReportLine("the body", leakguardtest.PairClass, shown+"…", 1, "a note: "+shown+strings.Repeat("x", 120)+"…"))
	})
	t.Run("a match cut inside a rune", func(t *testing.T) {
		match := leakguardtest.PairName + strings.Repeat("x", 191) + "é" + strings.Repeat("x", 290) + leakguardtest.PairWord
		if len(match) != 500 || !strings.HasPrefix(match[199:], "é") {
			t.Fatalf("the match is %d bytes, want 500 with a rune at bytes 199 and 200", len(match))
		}
		shown := leakguardtest.PairName + strings.Repeat("x", 191)
		shows(t, fixtureRules(t), "a note: "+match+" ends here",
			leakguardtest.ReportLine("the body", leakguardtest.PairClass, shown+"…", 1, "a note: "+shown+"é"+strings.Repeat("x", 118)+"…"))
	})
	t.Run("sixty findings", func(t *testing.T) {
		lines := make([]string, 60)
		for i := range lines {
			lines[i] = fmt.Sprintf("machine %d answers at %s", i+1, host)
		}
		want := make([]string, 0, 41)
		for i := range 40 {
			want = append(want, leakguardtest.ReportLine("the body", leakguardtest.RulesHostClass, host, i+1, lines[i]))
		}
		shows(t, fixtureRules(t), strings.Join(lines, "\n"), append(want, "… and 20 more matches not shown")...)
	})
	t.Run("an escape character in a match", func(t *testing.T) {
		// The word rule's match takes in the character on either side of the word, here an
		// escape, which the report writes as Go writes it in a quoted string.
		shows(t, fixtureRules(t), "a \x1b"+word+"\x1b b",
			`the body: `+leakguardtest.WordClass+`: matched "\x1b`+word+`\x1b" on line 1: "a \x1b`+word+`\x1b b"`)
	})
	t.Run("a newline in a match", func(t *testing.T) {
		// No fixture rule matches across a line, so this one is the test's own. The line shown
		// is the one the match starts on, up to its end.
		spanning, err := LoadRules([]byte("[[rules]]\nid = \"spanning_rule\"\ndescription = \"names the invented pair across a line\"\n" +
			"regex = '''(?i)" + leakguardtest.PairName + `\s+` + leakguardtest.PairWord + "'''\n"))
		if err != nil {
			t.Fatal(err)
		}
		shows(t, spanning, "the "+leakguardtest.PairName+"\n"+leakguardtest.PairWord+" ledger",
			`the body: names the invented pair across a line: matched "`+leakguardtest.PairName+`\n`+leakguardtest.PairWord+`" on line 1: "the `+leakguardtest.PairName+`"`)
	})
	t.Run("two matches of one line", func(t *testing.T) {
		// Two of the word's matches stand 300 bytes apart on each line, and they are the same
		// text: placed anywhere but where each stands, the two would write one line twice, and the
		// report would hold one line where it holds two.
		line := "x " + word + " " + strings.Repeat("y", 300) + " " + word + " z"
		first := "x " + word + " " + strings.Repeat("y", 120) + "…"
		second := "…" + strings.Repeat("y", 120) + " " + word + " z"
		match := " " + word + " "
		shows(t, fixtureRules(t), line+"\n"+line,
			leakguardtest.ReportLine("the body", leakguardtest.WordClass, match, 1, first),
			leakguardtest.ReportLine("the body", leakguardtest.WordClass, match, 1, second),
			leakguardtest.ReportLine("the body", leakguardtest.WordClass, match, 2, first),
			leakguardtest.ReportLine("the body", leakguardtest.WordClass, match, 2, second))
	})
	t.Run("a match that begins at the newline before its line", func(t *testing.T) {
		// The word rule's match takes in the newline before the word, which the engine trims off
		// the match and not off where it says the match starts.
		shows(t, fixtureRules(t), "a first line\n"+word+" starts the second",
			leakguardtest.ReportLine("the body", leakguardtest.WordClass, word+" ", 2, word+" starts the second"))
	})
}

// A record's label is shown on its report line's one line, and bounded, whatever the label holds:
// a newline in a field's name, and a `\` and an escape character, are written as Go writes them
// in a quoted string; a label of a kilobyte is shown as its first 60 bytes and its last 140 with
// "…" between, each cut between two runes, of the label as written or as escaped; and a label of
// printable text within the bound, `"` and all, is shown as it was written.
func TestRefusalShowsMatchUnderALabelOnOneBoundedLine(t *testing.T) {
	const text = "a host at " + leakguardtest.RulesHost
	for _, tc := range []struct{ name, label, shown string }{
		{"a newline", "the gh call's field a\nb", `the gh call's field a\nb`},
		{"a backslash and an escape character", "the body \\ \x1b", `the body \\ \x1b`},
		{"a kilobyte", strings.Repeat("h", 500) + strings.Repeat("t", 524), strings.Repeat("h", 60) + "…" + strings.Repeat("t", 140)},
		{
			"a kilobyte with a rune across each cut",
			strings.Repeat("h", 59) + "é" + strings.Repeat("m", 900) + "é" + strings.Repeat("t", 139),
			strings.Repeat("h", 59) + "…" + strings.Repeat("t", 139),
		},
		{"a kilobyte of newlines", strings.Repeat("\n", 1024), strings.Repeat(`\n`, 30) + "…" + strings.Repeat(`\n`, 70)},
		{"printable text", `the body, JSON ."a b" (name)`, `the body, JSON ."a b" (name)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := refusal(t, fixtureRules(t).Check(bound(t), "the write", Record{Label: tc.label, Text: text})).Lines
			want := leakguardtest.ReportLine(tc.shown, leakguardtest.RulesHostClass, leakguardtest.RulesHost, 1, text)
			if len(lines) != 1 || lines[0] != want {
				t.Errorf("the report is %q, want the one line %q", lines, want)
			}
		})
	}
}

// A document's base label past 400 bytes is cut once, before its walk (cutLabel), to at most 400
// bytes, and the cut shows as the whole label shows. For a label of 4 KB of ASCII, of runes of
// two, three and four bytes with one across each cut, of newlines and `\`, which show as two bytes
// each, and of bytes that are no UTF-8 in runs too long for a rune's start to be found, shownText
// of the cut with a JSON path after it is shownText of the whole label with that path: a path of
// a few bytes, and one past the bound by itself. A cut of a label of UTF-8 is UTF-8, and a label
// of 400 bytes is kept whole.
func TestACutLabelShowsAsTheWholeLabel(t *testing.T) {
	longPath := jsonLabel("", slices.Repeat([]string{jqMember(strings.Repeat("n", 100))}, 12))
	if len(longPath) <= maxTextShown {
		t.Fatalf("the long path is %d bytes, want it past %d", len(longPath), maxTextShown)
	}
	for _, tc := range []struct{ name, label string }{
		{"ASCII", strings.Repeat("abcdefgh", 512)},
		{"runes of several bytes", "a" + strings.Repeat("é漢𝄞", 455)},
		{"newlines and backslashes", strings.Repeat("a\nb\\", 1024)},
		{"bytes that are no UTF-8", "abcdef" + strings.Repeat("\x80\x80\x80\x80\x80\x80\x80\x80\xff", 454) + "abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.label) != 4096 {
				t.Fatalf("the label is %d bytes, want 4096", len(tc.label))
			}
			cut := cutLabel(tc.label)
			if len(cut) > 2*maxTextShown {
				t.Errorf("the cut label is %d bytes, want at most %d", len(cut), 2*maxTextShown)
			}
			if utf8.ValidString(tc.label) && !utf8.ValidString(cut) {
				t.Errorf("the cut label %q cuts a rune of a label of UTF-8", cut)
			}
			for _, path := range []string{", JSON .body", longPath} {
				if got, want := shownText(cut+path), shownText(tc.label+path); got != want {
					t.Errorf("the cut label with %q after it shows as %q, want %q", path, got, want)
				}
			}
		})
	}
	within := strings.Repeat("w", 2*maxTextShown)
	if cut := cutLabel(within); cut != within {
		t.Errorf("a label of %d bytes is cut to %q, want it whole", len(within), cut)
	}
}

// Each record is scanned on its own: a record that names something private is reported against
// itself and against no record after it. Detect of one fragment answers that fragment's
// findings, where DetectSource would hand the detector's whole run back and report the first
// record's finding against every later one.
func TestARecordIsReportedAgainstItselfAlone(t *testing.T) {
	lines, err := fixtureRules(t).scan(context.Background(), []Record{
		{Label: "the first record", Text: "a host at " + leakguardtest.RulesHost},
		{Label: "the second record", Text: "nothing of ours"},
		{Label: "the third record", Text: "nothing of ours either"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reports(t, lines, "the first record: "+leakguardtest.RulesHostClass)
}

// A context that has run out is a setup failure, never a record read as clean: the detector's own
// scan loop stops at a context's end and answers no finding, which would otherwise read as a
// record holding nothing.
func TestARuleScanThatRanOutOfTimeRefuses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixtureRules(t).scan(ctx, []Record{{Label: "the title", Text: "a host at " + leakguardtest.RulesHost}}); err == nil {
		t.Fatal("a scan under a context that ended answered no error")
	}
	if err := fixtureRules(t).Check(ctx, "the write", Record{Label: "the title", Text: "nothing of ours"}); err == nil {
		t.Fatal("Check under a context that ended passed the write")
	} else {
		setupFailure(t, err)
	}
}

// A remote URL names the repository the push would reach, in the shapes a remote is written in;
// one that names no repository on github.com names none, and the guard scans such a write as if
// it were public rather than skipping it. A URL whose two names are no repository GitHub serves
// names none either: the guard refuses a slug its exemption could not cover (Guard.scanThrough),
// so a URL that answered one would refuse the push over the spelling of its remote.
func TestRepoFromURL(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://github.com/invented-org/invented-repo.git", "invented-org/invented-repo"},
		{"https://github.com/invented-org/invented-repo", "invented-org/invented-repo"},
		{"https://x-access-token:secret@github.com/invented-org/invented-repo.git", "invented-org/invented-repo"},
		{"ssh://git@github.com/invented-org/invented-repo.git", "invented-org/invented-repo"},
		{"git@github.com:invented-org/invented-repo.git", "invented-org/invented-repo"},
		{"https://github.com/invented-org/invented-repo/", "invented-org/invented-repo"},
		{"https://example.invalid/invented-org/invented-repo.git", ""},
		{"/a/local/path.git", ""},
		{"https://github.com/invented-org", ""},
		{"https://github.com/-invented-org/invented-repo", ""},
		{"https://github.com/invented-org/../invented-repo", ""},
	} {
		if got := RepoFromURL(tc.url); got != tc.want {
			t.Errorf("RepoFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// The engine's own inline escape is inert. gitleaks drops every finding of a line that holds a
// `gitleaks:allow` comment, and the guard honours no such comment: a write that carries one is
// refused exactly as a write that does not. The comment is spelled in two pieces, as the hosts
// here are, so that no line of this file carries an escape of its own.
func TestTheInlineAllowCommentIsInert(t *testing.T) {
	const escape = "gitleaks" + ":allow"
	err := fixtureRules(t).Check(bound(t), "the write", Record{
		Label: "commit 1a2b3c4 message",
		Text:  "moved the run to " + leakguardtest.RulesHost + " # " + escape,
	})
	reports(t, refusal(t, err).Lines, "commit 1a2b3c4 message: "+leakguardtest.RulesHostClass)
}
