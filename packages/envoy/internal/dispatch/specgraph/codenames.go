package specgraph

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// The five signs of code in a text ported from level_check.py's code_signs, and three more below
// (R4: no text of The Spec names a file, a package or a symbol, but a check's test and its job).
var (
	backtickSpan        = regexp.MustCompile("`[^`\n]+`")
	fileEnding          = regexp.MustCompile(`(?i)\.(?:go|ts|tsx|js|jsx|mjs|cjs|py|md|yaml|yml|json|jsonl|toml|sh|bash|sql|rs|lean|v|txt|html|css|proto|lock|mod|cfg|ini|env)$`)
	slashBetweenLetters = regexp.MustCompile(`[A-Za-z]/[A-Za-z]`)
	endsWithCall        = regexp.MustCompile(`[\p{L}\p{N}_]\(\)`)
	underscoreBetween   = regexp.MustCompile(`[A-Za-z0-9]_[A-Za-z0-9]`)
)

// Two more shapes of a symbol, lowerCamel and packageSymbol. A plain word or a product name of one
// of these shapes, such as mmHg, ownCloud or works.Then, is refused as code too. A word with a
// capital at the start of each part, such as ReadSet, unless it has the form of a Go test's name,
// and a bare package word, such as pmdoc, are no sign, since product names such as GitHub and
// JetStream, and plain words, have those shapes.
var (
	// lowerCamel is a name in lower camel case, such as readSet: a small letter and one or more
	// small letters or digits, then a capital letter and a small letter. A brand or a unit with one
	// small letter before its capital, such as iPhone, eBay or kHz, or with capitals after it, such
	// as macOS or gRPC, is no code; one with more small letters, such as mmHg or ownCloud, is.
	lowerCamel = regexp.MustCompile(`^[a-z][a-z0-9]+[A-Z][a-z][A-Za-z0-9]*$`)
	// packageSymbol is a package and its exported symbol, such as pmdoc.ReadSet: a lower-case word,
	// a dot and a capital letter. An abbreviation such as e.g. and a host such as example.com have
	// no capital after the dot; a stop with no space after it, as in works.Then, has one.
	packageSymbol = regexp.MustCompile(`^[a-z][a-z0-9]*\.[A-Z][A-Za-z0-9_]*$`)
)

// productNames are plain names of products that are not code where a sign matches them.
var productNames = []string{"GitHub", "PostgreSQL", "OpenAI", "JavaScript", "TypeScript", "Kubernetes", "Node.js", "Next.js", "Vue.js"}

// CodeNames is each name of code that text holds, in the order found: first each span in
// backticks, backticks included, then each word of the rest with a file ending, a slash between two
// ASCII letters, an ending "()", or an underscore between two ASCII letters or digits, or that has
// the form of a name in lower camel case (lowerCamel) or of a package and its symbol
// (packageSymbol), and else each name in the word that has the form of a Go test's name
// (goTestName). The quotes and brackets of every script around a word, punctuation after it and a
// possessive "'s" are not part of it, and a product name that productNames lists, such as Node.js,
// is no code. Any other word of one of these shapes is, a plain word or a product name such as
// I/O, Three.js or TestFlight included.
func CodeNames(text string) []string {
	found := backtickSpan.FindAllString(text, -1)
	for _, token := range strings.Fields(backtickSpan.ReplaceAllString(text, " ")) {
		word := strings.TrimLeftFunc(token, func(r rune) bool { return strings.ContainsRune(`"'<`, r) || quoteOrBracket(r) })
		if endsWithCall.MatchString(word) {
			found = append(found, strings.TrimRightFunc(word, func(r rune) bool {
				return strings.ContainsRune(`.,;:!?"'`, r) || unicode.Is(unicode.Pf, r)
			}))
			continue
		}
		word = strings.TrimRightFunc(word, func(r rune) bool { return strings.ContainsRune(`"'>.,;:!?`, r) || quoteOrBracket(r) })
		word = strings.TrimSuffix(strings.TrimSuffix(word, "'s"), "\u2019s")
		if word == "" || slices.Contains(productNames, word) {
			continue
		}
		if fileEnding.MatchString(word) || slashBetweenLetters.MatchString(word) || underscoreBetween.MatchString(word) ||
			lowerCamel.MatchString(word) || packageSymbol.MatchString(word) {
			found = append(found, word)
			continue
		}
		for _, name := range nameRun.FindAllString(word, -1) {
			if goTestName(name) {
				found = append(found, name)
			}
		}
	}
	return found
}

// quoteOrBracket reports whether r is a quote or a bracket of any script: an opening or closing
// quote (Pi, Pf) or an opening or closing bracket (Ps, Pe), such as “ ” « » „ 「 」 ( ).
func quoteOrBracket(r rune) bool {
	return unicode.In(r, unicode.Pi, unicode.Pf, unicode.Ps, unicode.Pe)
}

// nameRun is a run of letters, digits and underscores: each other character ends a name.
var nameRun = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// goTestName reports whether name has the form of the name of a top-level Go test, the one form of
// a test's name that the checker knows, since go is the one runner whose selector it reads
// (ParseSelector): Test, then a capital letter, a digit or an underscore, then letters, digits and
// underscores. The word Test alone is a plain word, and so are Tests and Testament.
func goTestName(name string) bool {
	return name != "Test" && topLevelTestName.MatchString(name)
}

// slug is an id's form: lower-case letters and digits, in words joined by single hyphens.
var slug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// positionWord is a word that states a position: digits alone ("step-3") or one letter and digits
// ("s3", "r12"). A word such as "oauth2" or "ipv6" does not.
var positionWord = regexp.MustCompile(`^(?:[0-9]+|[a-z][0-9]+)$`)

// IsID reports whether s is a valid id of a node: words of lower-case ASCII letters and digits
// joined by single hyphens, with no word that states a position, since an id never changes and a
// position does.
func IsID(s string) bool {
	if !slug.MatchString(s) {
		return false
	}
	for _, word := range strings.Split(s, "-") {
		if positionWord.MatchString(word) {
			return false
		}
	}
	return true
}
