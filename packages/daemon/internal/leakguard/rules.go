package leakguard

import (
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

// ruleFile is this repository's own private-name rules, ported from
// .github/scripts/check-private-names.sh. It is embedded rather than read from disk so that no
// call site this binary serves can meet an absent or unreadable rule file: a daemon, a worker's
// gh and the image's shell all carry the same copy, compiled in.
//
//go:embed private-names.toml
var ruleFile string

// maskedBy are the content rules a path component is masked by, the three that
// check-private-names.sh's named_parts reads, under the same names: the deployment repository,
// the company, and the company's first word as a proper noun.
var maskedBy = []string{"repository_rule", "company_rule", "proper_noun_rule"}

// rules is the compiled rule file: one detector every record of every call is scanned with, and
// the three regexes a path is masked with.
type rules struct {
	detector *detect.Detector
	names    []*regexp.Regexp
}

// compiled is the rule file, compiled once per process. Every call to the rule pass takes this
// one detector: the file does not change under a running process, and compiling it per call would
// pay for ten regexes on every write.
var compiled = sync.OnceValues(compile)

// compile reads the embedded rule file and builds the detector. It recovers a panic because
// config.Translate compiles each pattern with regexp.MustCompile, which panics on a pattern it
// cannot read rather than returning an error: a daemon that embedded a broken rule file would
// otherwise die where it should refuse the write and say why.
func compile() (built *rules, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			built, err = nil, fmt.Errorf("the embedded private-name rules did not compile: %v", panicked)
		}
	}()
	// A viper of this package's own, never the package-level one: the global viper is a process's
	// single configuration, and a daemon that read its own configuration through viper would
	// share it with this.
	vp := viper.New()
	vp.SetConfigType("toml")
	if err := vp.ReadConfig(strings.NewReader(ruleFile)); err != nil {
		return nil, fmt.Errorf("the embedded private-name rules are not readable: %w", err)
	}
	var raw config.ViperConfig
	if err := vp.Unmarshal(&raw); err != nil {
		return nil, fmt.Errorf("the embedded private-name rules are not a rule file: %w", err)
	}
	cfg, err := raw.Translate()
	if err != nil {
		return nil, fmt.Errorf("the embedded private-name rules did not translate: %w", err)
	}
	names := make([]*regexp.Regexp, 0, len(maskedBy))
	for _, id := range maskedBy {
		rule, ok := cfg.Rules[id]
		if !ok || rule.Regex == nil {
			return nil, fmt.Errorf("the embedded private-name rules hold no content rule %q, which a path is masked by", id)
		}
		names = append(names, rule.Regex)
	}
	detector := detect.NewDetector(cfg)
	// The engine's own inline escape is turned off. A `gitleaks:allow` anywhere on a line makes
	// the engine drop every finding of that line, and the script these rules are ported from,
	// .github/scripts/check-private-names.sh, honours no such thing: left on, this package would
	// hand a writer an opt-out from the four classes that the check it stands in for never had,
	// and the whole of the cover would be one comment wide. The release lane's own gitleaks is
	// turned off the same way, with --ignore-gitleaks-allow (.github/scripts/release-push.sh).
	// It changes no rule and no match: the cost is that a legitimate text holding those two
	// words beside a name of one of the four classes is refused like any other, and its writer
	// rewrites the name rather than exempting the line.
	detector.IgnoreGitleaksAllow = true
	return &rules{detector: detector, names: names}, nil
}

// scan adds the report of one record to rep: one line per refusal, naming the record and the
// class and nothing else. The same class against the same record is one line however many times
// it matches, since the report says what a record names rather than how often, and a record's
// classes are sorted because the engine reads its rules out of a map and would otherwise order
// two of them differently from one run to the next. Past refusalLines, rep counts a line rather
// than holding it.
//
// The record is scanned on its own, with Detect of a fragment rather than DetectSource of a
// source: the detector accumulates the findings of a source, and a scan of several records
// through it would report the first record's finding against every record after it.
//
// A record whose text or label holds a NUL byte or is not UTF-8 is scanned again in each of its
// other readings (readings), its text and its path both, paired in order. Without its NULs, text
// of two bytes a character (UTF-16, which `git tag -F` takes) reads as the ASCII it holds. Read as
// Windows-1252 (windows1252), a commit in an encoding git cannot convert, or a file of a
// single-byte encoding, holds a separator the rules know (a no-break space, an en or em dash) as
// the character it is, where read as UTF-8 its one byte is no character at all, so a name joined
// by it would pass. The findings of every reading join the record's own, one line per class as
// ever, and the label is masked as read so (mask).
func (r *rules) scan(ctx context.Context, record Record, rep *report) error {
	var of []string
	seen := map[string]bool{}
	texts, labels := readings(record.Text), readings(record.Label)
	fragments := make([]detect.Fragment, max(len(texts), len(labels)))
	for k := range fragments {
		fragments[k] = detect.Fragment{Raw: texts[min(k, len(texts)-1)], FilePath: labels[min(k, len(labels)-1)]}
	}
	for _, fragment := range fragments {
		for _, finding := range r.detector.DetectContext(ctx, fragment) {
			line := r.mask(record.Label) + ": " + finding.Description
			if !seen[line] {
				seen[line] = true
				of = append(of, line)
			}
		}
	}
	slices.Sort(of)
	for _, line := range of {
		rep.add(line)
	}
	// A context that ended stops the detector's own scan loop and leaves it answering no
	// findings, which would read as a record that holds nothing. A bound that ran out is a setup
	// failure instead, so the write is refused rather than passed.
	return ctx.Err()
}

// readings is s as the rules read it: s itself; where it holds a NUL byte, s without its NULs, so
// text of two bytes a character (UTF-16, which `git tag -F` takes) reads as the ASCII it holds;
// and each of those that is not UTF-8 read as Windows-1252 as well (windows1252). Text with no
// NUL that is UTF-8 is read once.
func readings(s string) []string {
	read := []string{s}
	if strings.IndexByte(s, 0) >= 0 {
		read = append(read, strings.ReplaceAll(s, "\x00", ""))
	}
	for _, r := range read[:len(read):len(read)] {
		if !utf8.ValidString(r) {
			read = append(read, windows1252(r))
		}
	}
	return read
}

// windows1252 is text read byte by byte as Windows-1252, the superset of Latin-1 that most text
// of a single-byte encoding is written in: a byte below 0x80 is itself, one of 0x80 to 0x9F the
// character Windows-1252 gives it (cp1252High; the C1 control of its value where it gives none),
// and one from 0xA0 the Latin-1 character of its value. Every byte is a character, so it answers
// for any text, and the copy is at most three times text's size.
func windows1252(text string) string {
	var read strings.Builder
	read.Grow(len(text) * 2)
	for i := range len(text) {
		c := text[i]
		switch {
		case c < 0x80:
			read.WriteByte(c)
		case c < 0xA0 && cp1252High[c-0x80] != 0:
			read.WriteRune(cp1252High[c-0x80])
		default:
			read.WriteRune(rune(c))
		}
	}
	return read.String()
}

// cp1252High is what Windows-1252 gives the bytes 0x80 to 0x9F, and 0 for the five it leaves
// undefined (0x81, 0x8D, 0x8F, 0x90, 0x9D).
var cp1252High = [32]rune{
	0x20AC, 0, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017D, 0,
	0, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0, 0x017E, 0x0178,
}

// pass is one rule pass over records that arrive one at a time, as a push's diff is read: it
// scans each record as it arrives and keeps nothing of it but its refusal lines, so its memory is
// the record in hand, the detector's own cost of scanning it, and at most refusalLines refusal
// lines and a count of the rest. Whatever reads the records holds one line of its own beside
// that. None of it grows with the size of the push's files.
type pass struct {
	ctx    context.Context
	engine *rules
	report report
	// err is a scan that did not finish: the context ended under it. Every add after it answers
	// it, so that the reader of the records stops.
	err error
}

// add scans one record.
func (p *pass) add(record Record) error {
	if p.err == nil {
		p.err = p.engine.scan(p.ctx, record, &p.report)
	}
	return p.err
}

// end answers the pass as a write of what: nil where no record matched, a refusal naming the
// records that did, at most refusalLines of them and a count of the rest, and a setup failure
// where a scan did not finish.
func (p *pass) end(what string) error {
	if p.err != nil {
		return &SetupFailure{What: what, Why: "the private-name rules did not finish", Err: p.err}
	}
	if len(p.report.lines) == 0 {
		return nil
	}
	return &Refusal{What: what, By: byRules, Lines: p.report.all()}
}

// refusalLines is the most lines a refusal of the rule pass names. A push that adds a file at a
// path a path rule refuses is refused once for each line it adds there, so the report of a push
// of a million such lines would otherwise be a million lines, held in memory and printed. Past
// this many, the report counts the lines it leaves out in a last line of its own; below it, the
// report is every line, as it always was.
const refusalLines = 1000

// report is the lines of a refusal as the rule pass builds them: at most refusalLines of them,
// and a count of the ones past that.
type report struct {
	lines   []string
	leftOut int
}

// add is line added to the report, or counted where the report is full.
func (r *report) add(line string) {
	if len(r.lines) < refusalLines {
		r.lines = append(r.lines, line)
		return
	}
	r.leftOut++
}

// all is the report's lines, and a last line counting the lines left out where there are any.
func (r *report) all() []string {
	if r.leftOut == 0 {
		return r.lines
	}
	lines := "lines"
	if r.leftOut == 1 {
		lines = "line"
	}
	return append(r.lines, fmt.Sprintf("(%d more %s left out: this report names the first %d)", r.leftOut, lines, refusalLines))
}

// mask is label with each slash-separated component the repository or company rules match
// replaced by "<name>", the masking .github/scripts/check-private-names.sh does (its named_parts
// function, which its mask applies). A component is tested in every reading the scan gives it
// (readings), so a component that one of them refuses is masked. A label that
// names no file has no component to mask and comes back as it was.
func (r *rules) mask(label string) string {
	parts := strings.Split(label, "/")
	for i, part := range parts {
		read := readings(part)
		for _, name := range r.names {
			if slices.ContainsFunc(read, name.MatchString) {
				parts[i] = "<name>"
				break
			}
		}
	}
	return strings.Join(parts, "/")
}

// maskPath is mask for a caller with no engine in hand, such as the message of a setup failure
// that names a repository slug. Where the rules themselves could not be compiled it answers
// "<name>" for the whole path: a guard that cannot read its own rules cannot tell what is safe to
// print.
func maskPath(path string) string {
	engine, err := compiled()
	if err != nil {
		return "<name>"
	}
	return engine.mask(path)
}
