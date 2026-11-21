package leakguard

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unsafe"

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

// rules is the compiled rule file: one detector every record of every call is scanned with, the
// three regexes a path is masked with, and every rule's content pattern, which the blob record
// writer checks each line of a blob with (anyLineMatches).
type rules struct {
	detector *detect.Detector
	names    []*regexp.Regexp
	patterns []*regexp.Regexp
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
	var patterns []*regexp.Regexp
	for _, rule := range cfg.Rules {
		if rule.Regex != nil {
			patterns = append(patterns, rule.Regex)
		}
	}
	return &rules{detector: detector, names: names, patterns: patterns}, nil
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
		for _, finding := range r.detector.DetectContext(ctx, detectFragment(record)) {
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

// detectFragment is record as the detector reads it: the text, at the path its label names.
func detectFragment(record Record) detect.Fragment {
	return detect.Fragment{Raw: record.Text, FilePath: record.Label}
}

// anyLineMatches is whether any rule's content pattern matches any line of blob, each line formed
// as a push's diff forms an added line (Guard.readPatch): the blob split at each "\n", and every
// "\r" and "\n" at a line's end taken off. It is stricter than the rule pass on every count: it
// reads the blank lines a diff's reader leaves out, applies a rule's pattern whatever its path,
// keywords and entropy say, and applies none of its allowlists, each of which only takes a finding
// away. So a blob it answers false for holds no line a push's rule pass could refuse for its
// content. It reads no decoded form: Guard.recordKey gives no key to rules that decode.
func (r *rules) anyLineMatches(blob []byte) bool {
	for len(blob) > 0 {
		line := blob
		if end := bytes.IndexByte(blob, '\n'); end >= 0 {
			line, blob = blob[:end], blob[end+1:]
		} else {
			blob = nil
		}
		line = bytes.TrimRight(line, "\r\n")
		for _, pattern := range r.patterns {
			if pattern.Match(line) {
				return true
			}
		}
	}
	return false
}

// batchBytes is the most record text one batch of the rule pass holds, each record's header
// counted at recordHeader bytes beside its text, so that a batch of short lines is bounded by its
// records as well as by their bytes. A record longer than this is a batch of its own.
const batchBytes = 64 << 20

// recordHeader is what a record costs a batch beyond its text: its two string headers. The label
// is not counted: every line record of one file carries the one path string its diff header was
// read into.
const recordHeader = int(unsafe.Sizeof(Record{}))

// pass is one rule pass over records that arrive one at a time, as a push's diff is read: it
// holds at most one batch of them (batchBytes, or the Guard's own bound in a test), scans a batch
// once it is full, and keeps nothing of a scanned batch but its refusal lines. The lines come out
// as scan answers them for all the records at once, since a batch is scanned in the order its
// records arrived and scan reports each record on its own.
//
// Its memory is one batch, plus the detector's own cost of scanning one record of it, plus the
// refusal lines; whatever reads the records holds one line of its own beside that. None of it
// grows with the size of the push.
type pass struct {
	ctx    context.Context
	engine *rules
	limit  int
	// scanned, where it is not nil, is handed each batch before it is scanned: a test's seam
	// (Guard.onBatch), which reads what a batch held.
	scanned func(batch []Record)
	batch   []Record
	size    int
	lines   []string
	// err is a scan that did not finish: the context ended under it. Every add after it answers
	// it, so that the reader of the records stops.
	err error
}

// add takes one record into the pass, scanning the batch first where the record would take it past
// the bound, and scanning the record alone where it is past the bound by itself.
func (p *pass) add(record Record) error {
	if p.err != nil {
		return p.err
	}
	cost := len(record.Text) + recordHeader
	if len(p.batch) > 0 && p.size+cost > p.limit {
		if err := p.flush(); err != nil {
			return err
		}
	}
	p.batch = append(p.batch, record)
	p.size += cost
	if p.size >= p.limit {
		return p.flush()
	}
	return nil
}

// flush scans the batch and lets it go.
func (p *pass) flush() error {
	if len(p.batch) == 0 {
		return p.err
	}
	if p.scanned != nil {
		p.scanned(p.batch)
	}
	lines, err := p.engine.scan(p.ctx, p.batch)
	// The records are let go whatever the scan answered: the slice is kept for the next batch,
	// and its old entries would otherwise keep each scanned text alive.
	clear(p.batch)
	p.batch, p.size = p.batch[:0], 0
	if err != nil {
		p.err = err
		return err
	}
	p.lines = append(p.lines, lines...)
	return nil
}

// end scans what is left and answers the pass as a write of what: nil where no record matched, a
// refusal naming every record that did, and a setup failure where a scan did not finish.
func (p *pass) end(what string) error {
	if err := p.flush(); err != nil {
		return &SetupFailure{What: what, Why: "the private-name rules did not finish", Err: err}
	}
	if len(p.lines) == 0 {
		return nil
	}
	return &Refusal{What: what, By: byRules, Lines: p.lines}
}

// mask is label with each slash-separated component the repository or company rules match
// replaced by "<name>", the masking .github/scripts/check-private-names.sh does (its named_parts
// function, which its mask applies). A label that names no file has no component to mask and comes
// back as it was.
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
