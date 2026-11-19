package leakguard

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The embedded rules compile, and they are the eight private-names.toml holds: the four classes
// read out of a text and the three of them read out of a path, plus the proper noun. A rule file
// that did not compile would be a panic inside a daemon rather than a refusal, which is why
// compile recovers one; this is the test that says the file this binary carries needs no
// recovery.
func TestTheEmbeddedRulesCompile(t *testing.T) {
	engine, err := compiled()
	if err != nil {
		t.Fatalf("the embedded rules did not compile: %v", err)
	}
	ids := make([]string, 0, len(engine.detector.Config.Rules))
	for id := range engine.detector.Config.Rules {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	want := []string{
		"company_rule", "company_rule_path",
		"private_host_rule",
		"proper_noun_rule", "proper_noun_rule_path",
		"repository_rule", "repository_rule_path",
		"tailnet_host_rule",
	}
	if !slices.Equal(ids, want) {
		t.Errorf("the rules are %q, want %q", ids, want)
	}
	if len(engine.names) != len(maskedBy) {
		t.Errorf("a path is masked by %d rules, want %d", len(engine.names), len(maskedBy))
	}
}

// The rules pass the names they are written to pass: the reserved example tailnet, which names
// nothing, and a record with nothing of ours in it.
func TestTheRulesPassWhatTheyAreWrittenToPass(t *testing.T) {
	engine, err := compiled()
	if err != nil {
		t.Fatal(err)
	}
	clean := []Record{
		{Label: "the title", Text: "a machine on example-tailnet.ts" + ".net"},
		{Label: "docs/testing.md", Text: "a line of ordinary prose"},
		{Label: "ref legion/INVENTED-1", Text: "legion/INVENTED-1"},
	}
	lines, err := engine.scan(context.Background(), clean)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("the report is %q, want nothing", lines)
	}
}

// The host rule reads a host, not a Lean or OCaml name (dispatch://INFRA-3332). A name holding a
// capital letter, then a segment spelled exactly Internal, passes: Lake's and Rocq's module paths,
// and a Lean function's own namespace. So does Lean's own option of the cmdline snapshots under
// its Lean namespace, and no name near it. Every other host is refused, whatever the case of its
// labels: at the start of a record, on a later line, in italics, right after a reserved example
// host, and with a capital letter in its first label while the segment is not spelled Internal.
// The texts are assembled from pieces, as every refusable literal of these tests is, so that no
// line of this file names one whole, and so that a guard still reading a namespace as a host
// passes the file too.
func TestThePrivateHostRuleReadsAHostNotANamespace(t *testing.T) {
	engine, err := compiled()
	if err != nil {
		t.Fatal(err)
	}
	const (
		label   = "internal"
		capital = "Internal"
		upper   = "INTERNAL"
		class   = "names a private internal host"
	)
	scan := func(t *testing.T, record Record) []string {
		t.Helper()
		lines, err := engine.scan(context.Background(), []Record{record})
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}
	t.Run("a namespace passes", func(t *testing.T) {
		for _, text := range []string{
			"import Std." + capital + ".UV.System",
			"open Std." + capital + ".UV in",
			"  if Environ." + capital + ".is_above_prop env q then",
			"  let invert = Inductiveops." + capital + ".should_invert_case env sigma ci in",
			"  (Environ." + capital + ".View.view answer_env).Environ." + capital + ".View.env_modtypes",
			"      Classes." + capital + ".add_instance ~locality inst",
			"import Std.Async",
			// The lines of leanprover/lean4's src the namespace alone would leave refused: a
			// function's namespace (Init/Internal/Order/While.lean, twice, and both
			// SpecLemmas.lean of Std's Do and WP), and the option (Lean/Elab/Frontend.lean).
			"This module proves the one-step unfolding lemma `repeatM." + capital + ".eq_of_monadTail`,",
			"  rw [repeatM." + capital + ".eq_of_monadTail]; unfold repeatM.body; rw [bind_assoc]",
			"    rw [_root_.repeatM." + capital + ".eq_of_monadTail (f := f) a]",
			"  let opts := Lean." + label + ".cmdlineSnapshots.setIfNotSet opts incrSaveFileName?.isNone",
		} {
			if lines := scan(t, Record{Label: "the body", Text: text}); len(lines) != 0 {
				t.Errorf("%q: the report is %q, want nothing", text, lines)
			}
		}
	})
	t.Run("a reserved example host and the vendored property pass", func(t *testing.T) {
		for _, record := range []Record{
			{Label: "the body", Text: "https://listener." + label + ".example"},
			{Label: "the body", Text: "listener." + label + ".example"},
			{Label: "the body", Text: "line one\nlistener." + label + ".example"},
			{Label: "the body", Text: "nats://nats." + label + ".example.com:4222"},
			{Label: "the body", Text: "Point it at api." + label + ".example."},
			{Label: "the body", Text: "now https://ENVOY." + upper + ".EXAMPLE"},
			{Label: "packages/claude-envoy/dist/envoy-channel.js", Text: "        this." + label + ".push(sv);"},
		} {
			if lines := scan(t, record); len(lines) != 0 {
				t.Errorf("%q: the report is %q, want nothing", record.Text, lines)
			}
		}
	})
	t.Run("a host is refused", func(t *testing.T) {
		for _, text := range []string{
			"https://beacon." + label + ".quietforge",
			"https://beacon." + upper + ".quietforge",
			"https://beacon." + capital + ".quietforge",
			"https://BEACON." + upper + ".QUIETFORGE",
			"https://beacon." + label + ".Quietforge.Net",
			"beacon." + label + ".quietforge",
			"line one\nbeacon." + label + ".quietforge",
			"see _beacon." + label + ".quietforge_ in italics",
			"NATS_URLS=a." + label + ".example,b." + label + ".quietforge",
			"ip-10-0-0-7." + label + ".quietforge.net:443",
			"https://dispatch." + label + ".quietforge.com/",
			"this." + label + ".push(value)",
			"Beacon." + label + ".quietforge answers on 443.",
			"BEACON." + label + ".quietforge",
			"fooBar." + label + ".quietforge",
			"Beacon." + upper + ".quietforge",
			// Near names of the one Lean option the rule allows: another option, a longer name,
			// a name glued in front, and a host after its method.
			"Lean." + label + ".otherName",
			"Lean." + label + ".cmdlineSnapshotsX.get opts",
			"xLean." + label + ".cmdlineSnapshots",
			"Lean." + label + ".cmdlineSnapshots.get.beacon",
		} {
			if lines, want := scan(t, Record{Label: "the body", Text: text}), []string{"the body: " + class}; !slices.Equal(lines, want) {
				t.Errorf("%q: the report is %q, want %q", text, lines, want)
			}
		}
	})
}

// Each record is scanned on its own: a record that names something private is reported against
// itself and against no record after it. Detect of one fragment answers that fragment's
// findings, where DetectSource would hand the detector's whole run back and report the first
// record's finding against every later one.
func TestARecordIsReportedAgainstItselfAlone(t *testing.T) {
	engine, err := compiled()
	if err != nil {
		t.Fatal(err)
	}
	lines, err := engine.scan(context.Background(), []Record{
		{Label: "the first record", Text: "a host at " + theHost},
		{Label: "the second record", Text: "nothing of ours"},
		{Label: "the third record", Text: "nothing of ours either"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"the first record: " + theClass}; !slices.Equal(lines, want) {
		t.Errorf("the report is %q, want %q", lines, want)
	}
}

// A context that has run out is a setup failure, never a record read as clean: the detector's own
// scan loop stops at a context's end and answers no finding, which would otherwise read as a
// record holding nothing.
func TestARuleScanThatRanOutOfTimeRefuses(t *testing.T) {
	engine, err := compiled()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.scan(ctx, []Record{{Label: "the title", Text: "a host at " + theHost}}); err == nil {
		t.Fatal("a scan under a context that ended answered no error")
	}
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	if err := guard.Rules(ctx, Record{Label: "the title", Text: "nothing of ours"}); err == nil {
		t.Fatal("Rules under a context that ended passed the write")
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
// `gitleaks:allow` comment, and .github/scripts/check-private-names.sh, the script these rules
// are ported from, honours no such comment: a write that carries one is refused exactly as a
// write that does not. The comment is spelled in two pieces, as the hosts here are, so that no
// line of this file carries an escape of its own.
func TestTheInlineAllowCommentIsInert(t *testing.T) {
	const escape = "gitleaks" + ":allow"
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	err := guard.Rules(bound(t), Record{
		Label: "commit 1a2b3c4 message",
		Text:  "moved the run to " + theHost + " # " + escape,
	})
	want := []string{"commit 1a2b3c4 message: " + theClass}
	if refused := refusal(t, err); !slices.Equal(refused.Lines, want) {
		t.Fatalf("the report is %q, want %q", refused.Lines, want)
	}
}
