package leakguard

import (
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

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

// maskedBy are the content rules a path component is masked by, the three
// check-private-names.sh's own `names` reads (its lines 140-143): the deployment repository, the
// company, and the company's first word as a proper noun.
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
func compile() (compiled *rules, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			compiled, err = nil, fmt.Errorf("the embedded private-name rules did not compile: %v", panicked)
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
	return &rules{detector: detect.NewDetector(cfg), names: names}, nil
}

// scan is the report of records: one line per refusal, naming the record and the class and
// nothing else. The same class against the same record is one line however many times it
// matches, since the report says what a record names rather than how often, and a record's
// classes are sorted because the engine reads its rules out of a map and would otherwise order
// two of them differently from one run to the next.
//
// Each record is scanned on its own, with Detect of a fragment rather than DetectSource of a
// source: the detector accumulates the findings of a source, and a scan of several records
// through it would report the first record's finding against every record after it.
func (r *rules) scan(ctx context.Context, records []Record) ([]string, error) {
	var lines []string
	for _, record := range records {
		var of []string
		seen := map[string]bool{}
		for _, finding := range r.detector.DetectContext(ctx, detect.Fragment{Raw: record.Text, FilePath: record.Label}) {
			line := r.mask(record.Label) + ": " + finding.Description
			if !seen[line] {
				seen[line] = true
				of = append(of, line)
			}
		}
		slices.Sort(of)
		lines = append(lines, of...)
		// A context that ended stops the detector's own scan loop and leaves it answering no
		// findings, which would read as a record that holds nothing. A bound that ran out is a
		// setup failure instead, so the write is refused rather than passed.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

// mask is label with each slash-separated component the repository or company rules match
// replaced by "<name>", the masking .github/scripts/check-private-names.sh does (its lines
// 145-154). A label that names no file has no component to mask and comes back as it was.
func (r *rules) mask(label string) string {
	parts := strings.Split(label, "/")
	for i, part := range parts {
		for _, name := range r.names {
			if name.MatchString(part) {
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
