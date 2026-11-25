package leakguard

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"
)

// RulesSecret is the launch secret that carries the deployment's private-name rule file to every
// launch the daemon makes, and RulesFileVar the variable the guard reads the rule file's path
// from, in a role and in the daemon alike. A runtime hands each launch secret to the agent as a
// file behind `<NAME>_FILE` (internal/runtime's secretfiles.go), so the pointer a role's guard
// reads is the secret's own pointer. One is written as the other and the suffix, so the two
// cannot part.
const (
	RulesSecret  = "LEGION_PRIVATE_NAME_RULES"
	RulesFileVar = RulesSecret + "_FILE"
)

// MaxRulesFile is the most of a rule file the guard reads, and the most of the deployment's the
// daemon reads at boot (internal/daemon's readPrivateNameRulesFile), each through
// ReadRulesContent. A rule file is a page of regular expressions: a deployment's runs to a few
// kilobytes. The bound is also one a launch must be able to carry. The daemon hands the file's
// bytes to every launch as the launch secret RulesSecret, and under `runtime: kubernetes` that
// secret rides the launch's start command: one frame, which a launcher refuses past
// shimwire.MaxFrameBytes (internal/runtime/sandbox's checkStartFrame), shared with the role's
// system prompt and the launch's other files, all of it JSON-encoded, where each backslash of a
// regular expression doubles. A file near the frame's bound would fail every launch
// (runtime.PromptTooLarge). 256 KiB is far past any deployment's file and leaves that frame room,
// so no rule file meets the bound, and a pointer to a log or an image by mistake is refused rather
// than read into memory whole.
const MaxRulesFile = 256 << 10

// The bounds of a rule pass's report (RuleSet.scan), so that a report stays a page whatever the
// write is: a body can be a mebibyte of one line and a push thousands of added lines, and the
// report is read in a terminal and in a log. A match is shown to maxMatchShown bytes; the line it
// stands on, to maxContextShown bytes on either side of the part of the match shown; and the
// report, to maxLinesShown lines and one more that counts the matches it leaves out.
const (
	maxMatchShown   = 200
	maxContextShown = 120
	maxLinesShown   = 40
)

// RuleSet is a compiled rule file: one detector every record of a write is scanned with. One set
// can serve any number of writes, as the one the daemon compiles at boot serves each of its
// branch creates (RuleSet.Check): the detector keeps nothing of a scan, since Detect of a fragment
// answers its findings and holds none of them.
type RuleSet struct {
	detector *detect.Detector
	sha256   string
}

// Count is the number of rules the set holds.
func (r *RuleSet) Count() int {
	return len(r.detector.Config.Rules)
}

// SHA256 is the sha256, in hex, of the content the set was compiled from.
func (r *RuleSet) SHA256() string {
	return r.sha256
}

// LoadRules compiles content, a gitleaks rule file, into a rule set. It refuses what would
// otherwise lose a rule without a word:
//
//   - content past MaxRulesFile;
//   - content that is no TOML;
//   - a key gitleaks' rule format does not have, named in the error: gitleaks' own read ignores an
//     unknown key, so a misspelt `[[rule]]` would compile to no rule at all;
//   - an `[extend]` section: gitleaks reads an extended file through its package-level viper and
//     a package-level depth it never resets, so a third compile in one process skips the extension,
//     and it reads `extend.path` relative to the working directory, which a daemon and a role do
//     not share;
//   - a file that holds no rule, which would run nothing while it reads as cover;
//   - two rules of one id: gitleaks keeps a file's rules by id, so the second would replace the
//     first without a word;
//   - and a pattern that does not compile, a panic of regexp.MustCompile inside gitleaks'
//     Translate, recovered here into an error so that a broken file refuses the write where it
//     would otherwise end the process that read it.
//
// It asks for no rule of any id: the file is the deployment's, and what it holds is the whole of
// what the guard detects. Each call compiles content, and nothing is kept between calls: a role's
// process makes one write, which reads its rule file once (Guard.rules), and the daemon compiles
// its file once, at boot, so no process compiles one content twice.
func LoadRules(content []byte) (*RuleSet, error) {
	if len(content) > MaxRulesFile {
		return nil, fmt.Errorf("the rules are longer than %d bytes", MaxRulesFile)
	}
	sum := sha256.Sum256(content)
	return compileRules(content, hex.EncodeToString(sum[:]))
}

// compileRules is LoadRules' compile of content, whose sha256 is sum.
func compileRules(content []byte, sum string) (set *RuleSet, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			set, err = nil, fmt.Errorf("the rules did not compile: %v", panicked)
		}
	}()
	// A viper of this package's own, never the package-level one: the global viper is a process's
	// single configuration, and a daemon that read its own configuration through viper would
	// share it with this.
	vp := viper.New()
	vp.SetConfigType("toml")
	if err := vp.ReadConfig(bytes.NewReader(content)); err != nil {
		return nil, fmt.Errorf("the rules are not TOML: %s", oneLine.Replace(err.Error()))
	}
	// IsSet and not the decoded Extend: an empty `[extend]` decodes to the zero Extend, and it is
	// refused all the same, since it says the file means to read another.
	if vp.IsSet("extend") {
		return nil, errors.New("the rules hold an [extend] section, which the guard does not read: write the extended rules into the file itself")
	}
	var raw config.ViperConfig
	if err := vp.UnmarshalExact(&raw); err != nil {
		return nil, fmt.Errorf("the rules are not gitleaks' rule format: %s", oneLine.Replace(err.Error()))
	}
	ids := make(map[string]bool, len(raw.Rules))
	for _, rule := range raw.Rules {
		if ids[rule.ID] {
			return nil, fmt.Errorf("the rules hold two rules of the id %q, and gitleaks would run the last of them alone", rule.ID)
		}
		ids[rule.ID] = true
	}
	cfg, err := raw.Translate()
	if err != nil {
		return nil, fmt.Errorf("the rules did not compile: %w", err)
	}
	if len(cfg.Rules) == 0 {
		return nil, errors.New("the rules hold no rule")
	}
	detector := detect.NewDetector(cfg)
	// The engine's own inline escape is turned off. A `gitleaks:allow` anywhere on a line makes
	// the engine drop every finding of that line: left on, it would hand every writer an opt-out
	// from each rule of the deployment's file, one comment wide, which no rule file can take
	// back. The release lane's own gitleaks is turned off the same way, with
	// --ignore-gitleaks-allow (.github/scripts/release-push.sh). It changes no rule and no match:
	// the cost is that a legitimate text holding those two words beside a name a rule matches is
	// refused like any other, and its writer rewrites the name rather than exempting the line.
	detector.IgnoreGitleaksAllow = true
	return &RuleSet{detector: detector, sha256: sum}, nil
}

// ReadRulesContent is the rule file at path, its bytes as they stand, which LoadRules compiles:
// the one read of a rule file, the guard's (Guard.rules) and the daemon's at boot
// (internal/daemon's readPrivateNameRulesFile) alike, bounded by MaxRulesFile (readBounded), so
// that a longer file is refused rather than compiled short. Its error is readBounded's phrase —
// "could not be opened: <why>", "could not be read: <why>", "is longer than 262144 bytes" — so
// that each caller names the file, and what named it, in its own words.
func ReadRulesContent(path string) ([]byte, error) {
	return readBounded(path, MaxRulesFile)
}

// readBounded is the file at path, its bytes as they stand, where it holds at most bound bytes:
// the one read of a file this package holds whole in memory, a rule file (ReadRulesContent) and a
// file a gh call would send (readGhFile). A file that cannot be opened or read, and one longer
// than bound, is a *boundedReadError whose message is a phrase that follows the file's name —
// "could not be opened: <why>", "could not be read: <why>", "is longer than <bound> bytes" — and
// whose error underneath is what went wrong alone (withoutPath), since the caller names the path.
//
// It reads one byte past the bound, so a file at the bound reads whole and a longer one is known
// to be longer without reading it. The stat is not asked for: a named pipe and a character device
// report a size of zero and would read on past it.
func readBounded(path string, bound int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, &boundedReadError{phrase: "could not be opened", err: withoutPath(err)}
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, int64(bound)+1))
	if err != nil {
		return nil, &boundedReadError{phrase: "could not be read", err: withoutPath(err)}
	}
	if len(content) > bound {
		return nil, &boundedReadError{phrase: fmt.Sprintf("is longer than %d bytes", bound), longer: true}
	}
	return content, nil
}

// boundedReadError is readBounded's refusal of a file: phrase, which follows the file's name; the
// error underneath it, nil for a file longer than the bound; and longer, true for that file alone,
// which a caller that says more of that refusal reads (readGhFile), so that what it adds rests on
// a field that names the refusal and not on an error that happens to be nil. Its message is the
// phrase, then that error after a colon.
type boundedReadError struct {
	phrase string
	err    error
	longer bool
}

func (e *boundedReadError) Error() string {
	if e.err == nil {
		return e.phrase
	}
	return e.phrase + ": " + e.err.Error()
}

func (e *boundedReadError) Unwrap() error {
	return e.err
}

// withoutPath is err reduced to what went wrong where it is a *fs.PathError, whose own message
// repeats the path: the message err goes into names the file already.
func withoutPath(err error) error {
	var path *fs.PathError
	if errors.As(err, &path) {
		return path.Err
	}
	return err
}

// Check is the rule pass over records and nothing else: no shared check, no visibility read, and
// no read of any environment. It answers nil where no rule matches, a *Refusal of what, the write
// in the caller's words, whose report is RuleSet.scan's, and a *SetupFailure of what where the
// pass could not finish. It is the last step of a text the guard runs a rule over (Text) — a push
// and a gh call run the same pass a record at a time, as git writes a push's records and as the
// call's files are read (Guard.Push, GhCall) — and the whole of the check of a write with no
// repository behind it: the daemon's branch create (internal/ghbranch's Create), which holds a
// ref name and no checkout at all, and runs the set the daemon compiled at boot, so that the
// create depends on no rule file its process's environment may name.
func (r *RuleSet) Check(ctx context.Context, what string, records ...Record) error {
	pass := r.newPass(ctx)
	for _, record := range records {
		if pass.add(record) != nil {
			break
		}
	}
	return pass.end(what)
}

// pass is one rule pass of a set over records that arrive one at a time, as a push's diff and a gh
// call's files are read: it scans each record as it arrives and keeps nothing of it but its
// report, so its memory is the record in hand, the detector's own cost of scanning it, and the
// report (shownReport): at most maxLinesShown lines and a count of the rest. Whatever reads the
// records holds one line of its own beside that. None of it grows with the size of the push's
// files.
type pass struct {
	ctx    context.Context
	rules  *RuleSet
	report shownReport
	// err is a scan that did not finish: the context ended under it. Every add after it answers
	// it, so that the reader of the records stops.
	err error
}

// newPass is a rule pass of the set under ctx, for records read one at a time.
func (r *RuleSet) newPass(ctx context.Context) *pass {
	return &pass{ctx: ctx, rules: r}
}

// add scans one record.
func (p *pass) add(record Record) error {
	if p.err == nil {
		p.err = p.rules.scan(p.ctx, record, &p.report)
	}
	return p.err
}

// end answers the pass as a write of what: nil where no record matched, a refusal whose report is
// the pass's (shownReport.lines), and a setup failure where a scan did not finish.
func (p *pass) end(what string) error {
	if p.err != nil {
		return &SetupFailure{What: what, Why: "the private-name rules did not finish", Err: p.err}
	}
	lines := p.report.lines()
	if len(lines) == 0 {
		return nil
	}
	return &Refusal{What: what, By: byRules, Lines: lines}
}

// scan adds the report of one record to rep, which writes a line it already shows once and keeps
// to its bound (shownReport): one line per distinct finding,
//
//	<record>: <class>: matched "<match>" on line <n>: "<line>"
//
// naming the record by its label, on one line and bounded (shownText), the class by the
// description the deployment's rule gives it, and then the text that matched and the line of the
// record it stands on. Both are quoted (strconv.Quote), so that a newline or a control character
// in them cannot break a report apart, and both are cut to the bounds above on a rune boundary,
// each cut marked "…" inside the quotes. <n> counts from the record's first line in its file
// (Record.FirstLine). A path rule's finding stands on no line, and is "<record>: <class>: matched
// the path".
//
// The report shows the text because the agent that would make the write wrote it: there is
// nothing in it to hide from the agent the refusal is shown to, and this repository's public CI
// never prints the guard's report.
//
// A line the report shows is written once however many findings write it (shownReport): a path
// rule matches every record of a file, and two refs of one push can carry two commits that add one
// line at one place. Past the report's bound, each line is counted each time it comes. Lines come
// in the order of the records, then of the line in a record, then of the class and of the match,
// since the engine reads its rules out of a map and would otherwise order two findings of one line
// differently from one run to the next.
//
// The record is scanned on its own, with Detect of a fragment rather than DetectSource of a
// source: the detector accumulates the findings of a source, and a scan of several records
// through it would report the first record's finding against every record after it.
func (r *RuleSet) scan(ctx context.Context, record Record, rep *shownReport) error {
	findings := r.detector.DetectContext(ctx, detect.Fragment{Raw: record.Text, FilePath: record.Path})
	// A context that ended stops the detector's own scan loop and leaves it answering no
	// findings, which would read as a record that holds nothing. A bound that ran out is a setup
	// failure instead, so the write is refused rather than passed.
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}
	text := newRecordText(record.Text)
	places := make([]placed, len(findings))
	for i := range findings {
		places[i] = r.place(text, &findings[i])
	}
	slices.SortFunc(places, func(a, b placed) int {
		return cmp.Or(
			cmp.Compare(a.line, b.line),
			strings.Compare(a.finding.Description, b.finding.Description),
			strings.Compare(a.finding.Match, b.finding.Match),
			cmp.Compare(a.at, b.at),
		)
	})
	for _, p := range places {
		rep.add(text.reportLine(record, p))
	}
	return nil
}

// shownReport is a report being written: the lines it shows and the count of the matches past
// maxLinesShown. A line already among the shown lines is not written or counted again, which needs
// only those lines, so a path rule's finding that every line of a file repeats is one line of the
// report. A line past the bound is counted each time it comes and nothing of it is kept, so the
// count is of matches, not of distinct lines, and the report holds at most maxLinesShown lines and
// one count however many findings a write has: a rule pass over a push keeps nothing of a record
// it has scanned beyond them (pass).
type shownReport struct {
	shown []string
	left  int
}

func (s *shownReport) add(line string) {
	if slices.Contains(s.shown, line) {
		return
	}
	if len(s.shown) < maxLinesShown {
		s.shown = append(s.shown, line)
		return
	}
	s.left++
}

// lines is the report: the lines shown, and where any were left out, one more that counts them.
func (s *shownReport) lines() []string {
	if s.left == 0 {
		return s.shown
	}
	return append(s.shown, fmt.Sprintf("… and %d more matches not shown", s.left))
}

// recordText is a record's text and the offset of each of its newlines, read once for a record
// with a finding to place, so that placing each of many findings costs a search and not a read of
// the text. Lines count from 0 here, as the engine counts them.
type recordText struct {
	text     string
	newlines []int
}

func newRecordText(text string) *recordText {
	t := &recordText{text: text}
	for at := 0; ; {
		i := strings.IndexByte(text[at:], '\n')
		if i < 0 {
			return t
		}
		t.newlines = append(t.newlines, at+i)
		at += i + 1
	}
}

// start is the offset line k starts at, for k from 0 to the number of newlines.
func (t *recordText) start(k int) int {
	if k == 0 {
		return 0
	}
	return t.newlines[k-1] + 1
}

// end is the offset line k ends at: its newline, or the end of the text for the last line.
func (t *recordText) end(k int) int {
	if k < len(t.newlines) {
		return t.newlines[k]
	}
	return len(t.text)
}

// lineOf is the line the byte at offset at stands on.
func (t *recordText) lineOf(at int) int {
	k, _ := slices.BinarySearch(t.newlines, at)
	return k
}

// placed is one finding of a record and where it stands: line is the line of the record its match
// starts on, and -1 for a path rule's finding, which stands on none; at is the offset its match
// starts at; and found is whether the match was found there at all (RuleSet.place).
type placed struct {
	finding  *report.Finding
	line, at int
	found    bool
}

// place is where finding stands in text. A finding of a rule with no pattern is a path rule's,
// which reads the record's Path alone.
//
// The engine says where a match starts as a line counted from 0 (StartLine) and a column counted
// from the newline before that line, so that a line's first byte is column 1 on the first line and
// column 2 on every later one (StartColumn); and it trims the newlines a match begins with off the
// match's text and not off its place. TestPushRefusalShowsMatch holds the engine to the line, and
// TestRefusalShowsMatchWithinItsBounds to the column. Where the match stands at the place that
// reading gives, that is its place, which tells apart two matches of one line. Where it does not,
// it is placed at the first place it stands on or after the start of the line the engine names,
// and where it stands nowhere, at the start of that line, with no part of the line its own. The
// engine decodes nothing for this package (its MaxDecodeDepth is 0), so each match is text of the
// record's, and that last place is one no finding of this engine is put in.
func (r *RuleSet) place(text *recordText, finding *report.Finding) placed {
	if r.detector.Config.Rules[finding.RuleID].Regex == nil {
		return placed{finding: finding, line: -1}
	}
	k := min(max(finding.StartLine, 0), len(text.newlines))
	start := text.start(k)
	at := start + finding.StartColumn - 1
	if k > 0 {
		at--
	}
	for at >= 0 && at < len(text.text) && text.text[at] == '\n' {
		at++
	}
	if at < 0 || at > len(text.text) || !strings.HasPrefix(text.text[at:], finding.Match) {
		i := strings.Index(text.text[start:], finding.Match)
		if i < 0 {
			return placed{finding: finding, line: k, at: start}
		}
		at = start + i
	}
	return placed{finding: finding, line: text.lineOf(at), at: at, found: true}
}

// reportLine is the line of record's report that p writes (RuleSet.scan). The record is named by
// its label as shownText shows it. The line shown is the match's own line, from at most
// maxContextShown bytes before the match to at most maxContextShown bytes after the part of it
// shown, so that a match as long as its line shows a bounded line too.
func (t *recordText) reportLine(record Record, p placed) string {
	head := shownText(record.Label) + ": " + p.finding.Description + ": matched "
	if p.line < 0 {
		return head + "the path"
	}
	shown := p.finding.Match
	if len(shown) > maxMatchShown {
		shown = shown[:runeBack(shown, maxMatchShown, 0)]
	}
	match := shown
	if len(shown) < len(p.finding.Match) {
		match += "…"
	}
	begin, finish := t.start(p.line), t.end(p.line)
	end := p.at
	if p.found {
		end = min(p.at+len(shown), finish)
	}
	from := runeOn(t.text, max(begin, p.at-maxContextShown), p.at)
	to := runeBack(t.text, min(finish, end+maxContextShown), end)
	line := t.text[from:to]
	if from > begin {
		line = "…" + line
	}
	if to < finish {
		line += "…"
	}
	return head + strconv.Quote(match) + " on line " + strconv.Itoa(max(record.FirstLine, 1)+p.line) + ": " + strconv.Quote(line)
}

// The bounds of a text of the caller's as a report or a setup failure shows it (shownText): a text
// whose shown form is longer than maxTextShown bytes is shown as its first textShownBefore bytes
// and its last textShownAfter, with "…" between.
const (
	maxTextShown    = 200
	textShownBefore = 60
	textShownAfter  = 140
)

// shownText is text the caller wrote — a record's label, a path, a repository slug, an option —
// as a report line or a setup failure prints it: on one line, and bounded. Every rune
// strconv.IsPrint rejects, every byte that is no UTF-8, and `\`, are written as strconv.Quote
// writes them, without the quotes around them (shownRune), so that a newline in a field's name or
// in a path cannot break the line it stands on, and a `\` of the text reads apart from an escape
// written here. Every other rune, `"` among them, stands as it was written, so that a text of
// printable runes within the bound prints exactly as it was written. A text whose shown form is
// longer than maxTextShown bytes is cut to its first textShownBefore and its last textShownAfter
// bytes of that form, each cut between two runes, with "…" between: a label holds a path, or a
// field's name, of any length the caller wrote, and the line it stands on is read in a terminal
// and in a log. The text is read no further than the bound needs.
func shownText(text string) string {
	width, head := 0, 0
	for i := 0; i < len(text); {
		_, size := utf8.DecodeRuneInString(text[i:])
		width += len(shownRune(text[i : i+size]))
		if width > maxTextShown {
			tail, kept := len(text), 0
			for tail > head {
				_, size := utf8.DecodeLastRuneInString(text[head:tail])
				if kept += len(shownRune(text[tail-size : tail])); kept > textShownAfter {
					break
				}
				tail -= size
			}
			return shownRunes(text[:head]) + "…" + shownRunes(text[tail:])
		}
		i += size
		if width <= textShownBefore {
			head = i
		}
	}
	return shownRunes(text)
}

// shownRunes is text with each of its runes written as shownRune writes it.
func shownRunes(text string) string {
	var shown strings.Builder
	for i := 0; i < len(text); {
		_, size := utf8.DecodeRuneInString(text[i:])
		shown.WriteString(shownRune(text[i : i+size]))
		i += size
	}
	return shown.String()
}

// shownRune is one rune of a text, or one byte of it that is no UTF-8, as shownText writes it:
// as strconv.Quote writes it, without the quotes, where it is `\`, a byte that is no UTF-8 or a
// rune strconv.IsPrint rejects, and as it stands otherwise.
func shownRune(unit string) string {
	r, size := utf8.DecodeRuneInString(unit)
	if r != '\\' && (r != utf8.RuneError || size > 1) && strconv.IsPrint(r) {
		return unit
	}
	quoted := strconv.Quote(unit)
	return quoted[1 : len(quoted)-1]
}

// cutLabel is label, the base of the label of each string a document holds (jsonFields,
// graphQLRecords), cut once before the document is read: where it is longer than 2*maxTextShown
// bytes, its first maxTextShown bytes and its last maxTextShown, each cut on a rune boundary
// (runeBack, runeOn) and the two joined with nothing; else the whole label. Each string's label is
// the base and the string's path after it, and the base of an `--input <path>` body is the path
// as the argv wrote it, of any length: a path of 4 KB and a mebibyte of one-letter strings would
// otherwise build a gigabyte of labels.
//
// The cut shows no report line differently. A label is shown only through shownText, which shows
// a label whose shown form is past maxTextShown bytes as the first textShownBefore bytes and the
// last textShownAfter bytes of that form, and a shown byte is never fewer than the raw byte it
// shows. So the label and its cut, with any path after either, are both past that bound and both
// cut by shownText: the head it shows comes from the first textShownBefore raw bytes and the tail
// from the last textShownAfter, each read with at most utf8.UTFMax bytes more to decode a rune.
// The cut keeps at least maxTextShown-utf8.UTFMax+1 bytes of each end of the label, which holds
// both, and the joined middle is never shown. It cuts and never escapes: shownText escapes the
// label it shows, and a label escaped here too would print a newline as `\\n`.
func cutLabel(label string) string {
	if len(label) <= 2*maxTextShown {
		return label
	}
	return label[:runeBack(label, maxTextShown, 0)] + label[runeOn(label, len(label)-maxTextShown, len(label)):]
}

// runeBack is the offset i of s moved back to the start of the rune it falls inside, and no
// further back than floor. It moves at most utf8.UTFMax-1 bytes, the furthest a rune's start
// stands behind one of its bytes, so that a run of bytes that is no UTF-8 is cut where it falls.
func runeBack(s string, i, floor int) int {
	for n := 1; n < utf8.UTFMax && i > floor && i < len(s) && !utf8.RuneStart(s[i]); n++ {
		i--
	}
	return i
}

// runeOn is the offset i of s moved on to the start of the next rune where it falls inside one,
// and no further on than ceiling, as runeBack moves back.
func runeOn(s string, i, ceiling int) int {
	for n := 1; n < utf8.UTFMax && i < ceiling && i < len(s) && !utf8.RuneStart(s[i]); n++ {
		i++
	}
	return i
}
