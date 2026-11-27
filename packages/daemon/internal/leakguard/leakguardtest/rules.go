package leakguardtest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fixture's private-name rules. The guard runs the rule file a deployment names, and no
// Legion binary carries one, so the tests of the guard and of everything that drives it name this
// file instead, through LEGION_PRIVATE_NAME_RULES_FILE (WriteRules) or as rules compiled
// (leakguard.LoadRules) and run alone (leakguard.RuleSet.Check), as the daemon's branch create
// runs the set it compiled at boot. Every name in it is an invented word, which names nothing of
// any deployment's, and every description holds no colon, so that a report line reads as record,
// description and whatever the report adds after them.

// RulesHost is a Tailscale MagicDNS host of invented words, a machine of the fixture's invented
// tailnet, which the host rule refuses. It is spelled in two pieces so that a source file holding
// it passes .github/scripts/check-private-names.sh, which refuses any MagicDNS host written whole.
const RulesHost = "amber-kite.still-meadow.ts" + ".net"

// RulesHostClass is the description of the fixture's host rule.
const RulesHostClass = "names a machine of the invented tailnet"

// AllowedHost is the invented tailnet's example machine, which the host rule's match allowlist
// clears: the content rule with an allowlist of the fixture. It is spelled in two pieces for the
// reason RulesHost is.
const AllowedHost = "example.still-meadow.ts" + ".net"

// The two-word rule: PairName and PairWord on one line of one record, in that order and in any
// case, the shape `<name>[^\n]*<word>` a deployment writes for a name that is private only beside
// another word. Either word alone, and the two on two lines, pass.
const (
	PairName  = "velloran"
	PairWord  = "mistgrove"
	PairClass = "names the invented pair on one line"
)

// The one-word rule: Word as a word of its own, in any case, in a record's text (WordClass), and
// anywhere in a file's path (WordPathClass).
const (
	Word          = "thimbleroot"
	WordClass     = "names the invented word"
	WordPathClass = "names the invented word in its path"
)

// AllowedPath is the fixture's vendored file, whose text the word rule ignores: the path-scoped
// allowlist of the fixture. It holds no Word of its own, so the path rule does not refuse the path.
const AllowedPath = "vendor/glasswing/bundle.js"

// Rules is the whole fixture rule file, in gitleaks' rule format. Each pattern spells its literals
// as a regular expression whose own text it does not match (a [.] for a full stop), as the rule
// files a deployment writes do.
const Rules = `# leakguardtest's fixture rules, of invented names.
title = "leakguardtest fixture rules"

# A machine of the invented tailnet, and the example machine it allows.
[[rules]]
id = "host_rule"
description = "` + RulesHostClass + `"
regex = '''(?i)[[:alnum:]-]+[.]still-meadow[.]ts[.]net([^[:alnum:]]|$)'''

  [[rules.allowlists]]
  description = "the example machine names nothing"
  regexTarget = "match"
  regexes = ['''(?i)^example[.]still-meadow[.]ts[.]net[^[:alnum:]]?$''']

# The invented pair, on one line.
[[rules]]
id = "pair_rule"
description = "` + PairClass + `"
regex = '''(?i)velloran[^\n]*mistgrove'''

# The invented word, as a word, except in the vendored file.
[[rules]]
id = "word_rule"
description = "` + WordClass + `"
regex = '''(?i)(^|[^[:alnum:]])thimbleroot([^[:alnum:]]|$)'''

  [[rules.allowlists]]
  description = "the vendored file's own use of the word"
  paths = ['''^vendor/glasswing/bundle[.]js$''']

# The invented word in a file's path.
[[rules]]
id = "word_path_rule"
description = "` + WordPathClass + `"
path = '''(?i)thimbleroot'''
`

// WriteRules writes the fixture rule file (Rules) into dir and answers its path, the value a test
// gives LEGION_PRIVATE_NAME_RULES_FILE.
func WriteRules(t testing.TB, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "private-name-rules.toml")
	if err := os.WriteFile(path, []byte(Rules), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// PaddedTo is content followed by one TOML comment line that brings it to exactly size bytes: a
// rule file at a bound, or a byte past it, that holds content's rules and nothing else.
func PaddedTo(content string, size int) string {
	return content + "# " + strings.Repeat("x", size-len(content)-len("# \n")) + "\n"
}

// ReportLine is the whole line a rule pass's report writes of one finding: the record, the class
// of the rule that matched, the text it matched and the text of the record's line it stands on,
// line, with the match and that text quoted as Go quotes them.
func ReportLine(record, class, match string, line int, text string) string {
	return record + ": " + class + ": matched " + strconv.Quote(match) + " on line " + strconv.Itoa(line) + ": " + strconv.Quote(text)
}
