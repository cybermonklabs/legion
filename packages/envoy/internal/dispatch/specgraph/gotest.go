package specgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// An entry keeps an output of maxOutput bytes whole. Of a longer output it keeps the first
// outputStart bytes, which name the test, and the last outputEnd, which say how it ended.
const (
	maxOutput   = 64 << 10
	outputStart = 4 << 10
	outputEnd   = maxOutput - outputStart
)

// outcomeCached is the Outcome of each run of a package whose result go test replayed from its
// test cache (TestOutcome).
const outcomeCached = "cached"

// TestOutcome is one run of one top-level Go test, as a go test -json report gives it: its
// package, its name, its outcome and its output.
//
// Outcome is the run's one end event of its own, "pass", "fail" or "skip", when go test's own end
// line of that kind for the test, the frame "--- PASS: <name> (…)", "--- FAIL: …" or "--- SKIP: …",
// comes before it in the run, and the report holds the end of the run's package. Else it is
// "could_not_run":
//   - for a run with no end event, as when go test killed its test at its -timeout or the test's
//     process ended inside it (os.Exit with a status that is not 0, a fatal error of the runtime);
//   - for a run whose end has no such frame before it, as the end event that go tool test2json
//     writes at its exit, run over a test binary, for the test it named last;
//   - for a run with two or more end events, as when a test printed an end line with the marker
//     of the testing package's own lines;
//   - for each run of a name that starts while another run of that name in its package has not
//     ended, and for that other run, since the report then cannot say whose end an end event is:
//     two parallel tests of one name, in package p and in p_test, and a test that prints a run line
//     of its own name with the marker;
//   - and for each run of a package whose own end event, the pass, fail or skip event with no Test
//     that go test's converter writes last for the package, does not follow the run's start in the
//     report. A report that ends before it, as a capture cut short, cannot say whether go test
//     replayed the package from its cache, since the summary line that says so comes just before
//     that end. go tool test2json, run over a test binary that stops inside a test, writes that end
//     as an end event of the test it named last, so such a report holds no run of its package.
//
// Every run of a package whose result go test replayed from its test cache is "cached", whatever
// its end event: such a run is an earlier run's account, and the test did not run at the commit
// of the report. go test then prints its summary line of the package with "(cached)" in place of
// the elapsed time, "ok  \t<import path>\t(cached)", and what came after the time after it, as
// "\tcoverage: 50.0% of statements" under -cover or " [no tests to run]", and replays the run's
// events as they were. The cache does not see a file that a test reads through a child process,
// so such a test can fail when it runs. The reader reads that line whole, though go test's
// converter writes a line of more than 1,024 bytes, as the summary line of a long import path, in
// two or more output events (ReadGoTestJSON). The line has no marker, so a test that prints its
// own package's cached summary line makes each run of its package in that go test run read as
// replayed from the cache, which reads as not run: the event of a test's name does not tell the
// two apart, since go's own summary line can come in such an event too, after a TestMain that
// prints a run line.
//
// In the two forms of report that ReadGoTestJSON reads, what a test prints with no marker decides
// no outcome, go test's kill line and its end lines included. A test's pass stays when a goroutine
// that outlives the test fails the package later.
//
// Output is the output events that go test's converter gave the run, its subtests' and the
// testing package's own lines included, which can hold a parallel sibling's line or go test's kill
// trace. It is whole when it is 64 KiB or less. Of a longer output it holds the first 4 KiB, then
// the line "[The middle of the output is cut here: <n> of its <size> bytes.]", then the last
// 60 KiB; each part starts and ends at a whole UTF-8 character, so a part can hold up to 3 bytes
// less. The reader reads each line of the output before it cuts, so a cut does not hide an end
// line.
type TestOutcome struct {
	Package, Test, Outcome, Output string
}

// event is the part of one go test -json event that the report reads (go doc test2json).
type event struct {
	Action, Package, Test, Output, OutputType string
}

// run is one entry while the report is read: its end events of its own so far, the kinds of go
// test's end lines of it so far, what its test wrote, and the run of go test over its package.
type run struct {
	outcome TestOutcome
	ends    int
	lines   endKinds // the end lines of the test seen so far
	counted bool     // its last end event came after an end line of its kind
	overlap bool     // it overlaps another run of its name
	size    int      // how many bytes of output the test wrote
	start   []byte   // the first outputStart bytes of them
	end     []byte   // the bytes after start, the last outputEnd of them at least
	pkg     *pkgRun  // the run of go test over its package that it is part of
}

// pkgRun is one run of go test over one package while the report is read, from the package's
// start event to its own end event, the pass, fail or skip event with no Test: whether that end
// came, whether the package's summary line said that go test replayed it from its test cache, and
// the line of the package's output that has not ended yet.
type pkgRun struct {
	pkg           string // the package's import path
	ended, cached bool
	line          []byte // the first bytes of the open line, as many as keep
	open          bool   // a line has started and not ended
	frame         bool   // the open line is go test's own, of frame events
	keep          int    // how many bytes of the open line the rules read
	owner         *run   // the run whose own frame the open line is, or nil
}

// output reads out, the Output of one output event of the package, frame or not, of the run owner
// (nil for an event that is no frame of a top-level test). A line ends at its line feed, never at
// the end of an event: go test's converter writes a line of more than 1,024 bytes in two or more
// events. The converter also ends a line with no line feed where a frame starts after a test's
// output, so an open line ends unread at an event of the other kind: each line that a rule reads,
// the summary line and an end line, ends with its line feed.
func (p *pkgRun) output(out string, frame bool, owner *run) {
	if out == "" {
		return
	}
	if p.open && p.frame != frame {
		p.open = false
	}
	for {
		text, rest, ended := strings.Cut(out, "\n")
		switch {
		case !p.open && ended:
			// The whole line is in this event: it is read where it is.
			p.read(text, owner)
		case !p.open:
			p.open, p.frame, p.owner = true, frame, owner
			p.keep = p.lineKeep(owner)
			p.line = append(p.line[:0], text[:min(len(text), p.keep)]...)
		default:
			p.line = append(p.line, text[:min(len(text), p.keep-len(p.line))]...)
			if ended {
				p.open = false
				p.read(string(p.line), p.owner)
			}
		}
		if !ended || rest == "" {
			return
		}
		out = rest
	}
}

// read applies the rules of the reader to one whole line of the package: go test's summary line of
// the package replayed from its cache, and the end line of owner's test.
func (p *pkgRun) read(line string, owner *run) {
	if cachedLine(line, p.pkg) {
		p.cached = true
	}
	if owner != nil {
		owner.lines |= endLine(line, owner.outcome.Test)
	}
}

// lineKeep is how many bytes of a line of the package, and of a frame of the run owner, the rules
// read, each from the line's start: the summary line up to the "\t(cached)" after the import path,
// and an end line up to the " (" after the name, its kind PASS, FAIL or SKIP being four bytes.
func (p *pkgRun) lineKeep(owner *run) int {
	keep := len("ok  \t") + len(p.pkg) + len("\t(cached)")
	if owner != nil {
		keep = max(keep, len("--- PASS: ")+len(owner.outcome.Test)+len(" ("))
	}
	return keep
}

// endKinds is a set of end actions, one bit for each of pass, fail and skip.
type endKinds uint8

const (
	passEnd endKinds = 1 << iota
	failEnd
	skipEnd
)

// endKind is the bit of an end action, or 0 for an action that ends no test.
func endKind(action string) endKinds {
	switch action {
	case "pass":
		return passEnd
	case "fail":
		return failEnd
	case "skip":
		return skipEnd
	}
	return 0
}

// endLine is the bit of the kind of go test's own end line of the top-level test name in out, as
// the testing package writes it ("--- PASS: <name> (0.00s)"), or 0 when out is no such line.
func endLine(out, name string) endKinds {
	rest, ok := strings.CutPrefix(out, "--- ")
	if !ok {
		return 0
	}
	kind, rest, ok := strings.Cut(rest, ": ")
	if rest, found := strings.CutPrefix(rest, name); !ok || !found || !strings.HasPrefix(rest, " (") {
		return 0
	}
	switch kind {
	case "PASS":
		return passEnd
	case "FAIL":
		return failEnd
	case "SKIP":
		return skipEnd
	}
	return 0
}

// nameRuns is what the reader holds of one top-level name of one package: the run that the name's
// events belong to, and how many of its runs have not ended.
type nameRuns struct {
	latest *run
	open   int
}

// write adds out to what the run's test wrote.
func (r *run) write(out string) {
	r.size += len(out)
	n := min(outputStart-len(r.start), len(out))
	r.start = append(r.start, out[:n]...)
	r.end = append(r.end, out[n:]...)
	if len(r.end) > 2*outputEnd {
		r.end = append(r.end[:0], r.end[len(r.end)-outputEnd:]...)
	}
}

// output is what TestOutcome.Output keeps of what the run's test wrote. Each event's Output is
// valid UTF-8, since encoding/json decodes a string so, and so is what the test wrote: the first
// part ends before a character that its last bytes cut, and the last part starts after one.
func (r *run) output() string {
	if r.size <= maxOutput {
		return string(r.start) + string(r.end)
	}
	start := r.start
	last := len(start) - 1
	for last > 0 && !utf8.RuneStart(start[last]) {
		last--
	}
	if !utf8.FullRune(start[last:]) {
		start = start[:last]
	}
	end := r.end[len(r.end)-outputEnd:]
	for len(end) > 0 && !utf8.RuneStart(end[0]) {
		end = end[1:]
	}
	return fmt.Sprintf("%s\n[The middle of the output is cut here: %d of its %d bytes.]\n%s", start, r.size-len(start)-len(end), r.size, end)
}

// ReadGoTestJSON reads the report that go test -json writes, or that go tool test2json writes for a
// test binary run with -test.v=test2json, and gives one entry for each run of a top-level test, in
// the order the tests started. A name that runs twice in one package, as under -count=2 or as a
// test defined in both package p and package p_test, gives two entries; the events of that name
// after its second start belong to the second. The outcome is the run's one end event of its own,
// never a subtest's, and that end counts only after go test's own end line of its kind for the test
// (TestOutcome).
//
// The reader reads exactly those two forms of report: only in them does the marker separate go
// test's own lines from what a test prints. A report of another form, as of a test binary run with
// -test.v=true or the text of go test -v through go tool test2json, lets a test write go test's
// lines with no marker. go test -json -v makes such a report: -json gives the test binary
// -test.v=test2json, and the -v after it gives -test.v=true, which wins; go test -json with
// -test.v=true given to the binary after -args makes one too. So in such a report a test can print
// the run and end lines of any test of its package, its own or another's, with no marker, and make
// that test read as run or passed, and a test that prints with no line feed gets no end event and
// reads as not run (TestStatedLimitsOfTheReader; another test's lines, through the command:
// TestCitationsStatedLimitAForgedRun of cmd/spec-check). The reader cannot refuse such a report by
// its content: of a package of passing tests with no subtest and no error line, its events are
// those of go test -json. So a step that makes a report for CI runs go test -json with no -v and no
// -test.v.
//
// The report is the test binary's own account, and the reader trusts its order: a report that
// someone changed or put in another order can give a run the wrong outcome. In those two forms, a
// line that a test prints with no marker is output, whatever it says. But the reader cannot tell a
// line that the binary prints with the marker, the byte 0x16, which an honest test never prints and
// a test that forges the lines of the testing package can, or before its first test, from the
// testing package's own, so the binary can still print a whole run, of a test that does not exist
// or of one that does, and a test that prints a run line of its own name with the marker gives two
// entries, each could_not_run.
// And in a report of go test -json, a test that prints, with the marker, go test's end line of its
// own name, or of another test of its package that is running, can still give that test an outcome
// when that test then does not end by itself: for its own name, the kind of that line, pass, fail
// or skip, when go test then kills it or its process ends with a status that is not 0; for another
// test, a parallel test that prints the pass line of a running test that go test then kills gives
// that test pass. A report of go tool test2json whose test binary stops inside a test holds no end
// of its package (TestOutcome), so there such a line gives no outcome.
//
// A value that is no JSON object, a null and an object with no Action are refused, and so is a
// report that holds a run of a test and no frame, as a converter of a Go before 1.27 writes, which
// marks no line as go test's own. Any other report that names no test, an empty one included,
// reads as a run of no tests: MatchTests then gives no entry for a check, and a reporter must read
// a check with no entry as a check with no result, which R9 of the spec reads as unknown and never
// true. Events of a package and not of a test (no Test), events of a test that never started, and
// the actions that end no test (start, pause, cont, attr and the rest) are passed over, but for
// the package's start and its own end event, which bound each run of go test over it, and go
// test's summary line of a package replayed from its test cache, which makes each run of that
// package "cached" (TestOutcome).
//
// The reader reads a line whole, the summary line of a package and the end line of a test: it joins
// the output events of each package in the order they come, and a line ends at its line feed, never
// at the end of an event, since go test's converter writes a line of more than 1,024 bytes, as one
// of a long import path or test name, in two or more events. The converter ends a line with no line
// feed too, where a frame starts after a test's output, as after a test that prints with no line
// feed at its end, and so does the reader: what comes before a frame is no part of its line.
func ReadGoTestJSON(r io.Reader) ([]TestOutcome, error) {
	var runs []*run
	names := map[[2]string]*nameRuns{} // package and top-level name
	packages := map[string]*pkgRun{}   // the run of go test over each package that has not ended
	framed := false                    // the report holds a frame
	decoder := json.NewDecoder(r)
	for {
		var e event
		if err := decoder.Decode(&e); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("specgraph: the go test report holds no event at byte %d: %w", decoder.InputOffset(), err)
		}
		if e.Action == "" {
			return nil, fmt.Errorf("specgraph: the go test report holds no event at byte %d: a value with no Action", decoder.InputOffset())
		}
		frame := e.OutputType == "frame"
		framed = framed || frame
		p := packages[e.Package]
		if p == nil || (e.Test == "" && e.Action == "start") {
			p = &pkgRun{pkg: e.Package}
			packages[e.Package] = p
		}
		if e.Test == "" {
			switch e.Action {
			case "output":
				p.output(e.Output, frame, nil)
			case "pass", "fail", "skip":
				// The package's own end: a later event of the package is of another run of go test over it.
				p.ended = true
				delete(packages, e.Package)
			}
			continue
		}
		top, _, sub := strings.Cut(e.Test, "/")
		key := [2]string{e.Package, top}
		name := names[key]
		if e.Action == "run" && !sub {
			if name == nil {
				name = &nameRuns{}
				names[key] = name
			}
			this := &run{outcome: TestOutcome{Package: e.Package, Test: top}, pkg: p}
			if name.open > 0 {
				// A run of the name is open, so the two runs overlap. Each earlier run of this group was
				// marked when the group's second run started, so only the latest and this one need it.
				name.latest.overlap = true
				this.overlap = true
			}
			name.open++
			name.latest = this
			runs = append(runs, this)
			continue
		}
		if e.Action == "output" {
			var owner *run // the run whose end line this event can hold
			if frame && !sub && name != nil {
				owner = name.latest
			}
			p.output(e.Output, frame, owner)
		}
		if name == nil {
			continue
		}
		this := name.latest
		switch e.Action {
		case "output":
			this.write(e.Output)
		case "pass", "fail", "skip":
			if !sub {
				this.ends++
				this.counted = this.lines&endKind(e.Action) != 0
				this.outcome.Outcome = e.Action
				name.open = max(name.open-1, 0)
			}
		}
	}
	if len(runs) > 0 && !framed {
		return nil, errors.New("specgraph: the go test report holds a run of a test and no frame, as a converter of a Go before 1.27 writes: the report needs Go 1.27 or later")
	}
	outcomes := make([]TestOutcome, 0, len(runs))
	for _, this := range runs {
		switch {
		case this.pkg.cached:
			this.outcome.Outcome = outcomeCached
		case !this.pkg.ended || this.ends != 1 || !this.counted || this.overlap:
			this.outcome.Outcome = "could_not_run"
		}
		this.outcome.Output = this.output()
		outcomes = append(outcomes, this.outcome)
	}
	return outcomes, nil
}

// cachedLine is whether line is go test's summary line of the package pkg that it replayed from its
// test cache, "ok  \t<pkg>\t(cached)" and what follows. It cuts each part in turn, so a line builds
// no string.
func cachedLine(line, pkg string) bool {
	rest, ok := strings.CutPrefix(line, "ok  \t")
	if ok {
		rest, ok = strings.CutPrefix(rest, pkg)
	}
	return ok && strings.HasPrefix(rest, "\t(cached)")
}
