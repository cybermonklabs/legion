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

// TestOutcome is one run of one top-level Go test, as a go test -json report gives it: its
// package, its name, its outcome and its output.
//
// Outcome is the run's one end event of its own, "pass", "fail" or "skip", when go test's own end
// line of that kind for the test, the frame "--- PASS: <name> (…)", "--- FAIL: …" or "--- SKIP: …",
// comes before it in the run. Else it is "could_not_run":
//   - for a run with no end event, as when go test killed its test at its -timeout or the test's
//     process ended inside it (os.Exit with a status that is not 0, a fatal error of the runtime);
//   - for a run whose end has no such frame before it, as the end event that go tool test2json
//     writes at its exit, run over a test binary, for the test it named last;
//   - for a run with two or more end events, as when a test printed an end line with the marker
//     of the testing package's own lines;
//   - and for each run of a name that starts while another run of that name in its package has
//     not ended, and for that other run, since the report then cannot say whose end an end event
//     is: two parallel tests of one name, in package p and in p_test, and a test that prints a run
//     line of its own name with the marker.
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
// less. The reader finds an end line in each output event before it cuts, so a cut does not hide
// one.
type TestOutcome struct {
	Package, Test, Outcome, Output string
}

// event is the part of one go test -json event that the report reads (go doc test2json).
type event struct {
	Action, Package, Test, Output, OutputType string
}

// run is one entry while the report is read: its end events of its own so far, the kinds of go
// test's end lines of it so far, and what its test wrote.
type run struct {
	outcome TestOutcome
	ends    int
	lines   endKinds // the end lines of the test seen so far
	counted bool     // its last end event came after an end line of its kind
	overlap bool     // it overlaps another run of its name
	size    int      // how many bytes of output the test wrote
	start   []byte   // the first outputStart bytes of them
	end     []byte   // the bytes after start, the last outputEnd of them at least
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
// -test.v=true or the text of go test -v through go tool test2json, lets a test write an end line
// with no marker, and the reader cannot see which form made a report. In CI, the job's own go test
// -json command makes the report that the reporter reads.
//
// The report is the test binary's own account, and the reader trusts its order: a report that
// someone changed or put in another order can give a run the wrong outcome. In those two forms, a
// line that a test prints with no marker is output, whatever it says. But the reader cannot tell a
// line that the binary prints with the marker, or before its first test, from the testing
// package's own, so the binary can still print a whole run, of a test that does not exist or of one
// that does, and a test that prints a run line of its own name with the marker gives two entries,
// each could_not_run.
// And a test that prints, with the marker, go test's end line of its own name, or of another test
// of its package that is running, can still give that test an outcome when that test then does not
// end by itself. For its own name: in a report of go test -json, the kind of that line, pass, fail
// or skip, when go test then kills it or its process ends with a status that is not 0; in a report
// of go tool test2json, fail for a fail line when its process then ends with an error, and pass for
// a pass line when it then calls os.Exit(0). For another test: a parallel test that prints the pass
// line of a running test that go test then kills gives that test pass, in both forms.
//
// A value that is no JSON object, a null and an object with no Action are refused, and so is a
// report that holds a run of a test and no frame, as a converter of a Go before 1.27 writes, which
// marks no line as go test's own. Any other report that names no test, an empty one included,
// reads as a run of no tests: MatchTests then gives no entry for a check, and a reporter must read
// a check with no entry as a check with no result, which R9 of the spec reads as unknown and never
// true. Events of a package and not of a test (no Test), events of a test that never started, and
// the actions that end no test (start, pause, cont, attr and the rest) are passed over.
func ReadGoTestJSON(r io.Reader) ([]TestOutcome, error) {
	var runs []*run
	names := map[[2]string]*nameRuns{} // package and top-level name
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
		framed = framed || e.OutputType == "frame"
		if e.Test == "" {
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
			this := &run{outcome: TestOutcome{Package: e.Package, Test: top}}
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
		if name == nil {
			continue
		}
		this := name.latest
		switch e.Action {
		case "output":
			if !sub && e.OutputType == "frame" {
				this.lines |= endLine(e.Output, top)
			}
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
		if this.ends != 1 || !this.counted || this.overlap {
			this.outcome.Outcome = "could_not_run"
		}
		this.outcome.Output = this.output()
		outcomes = append(outcomes, this.outcome)
	}
	return outcomes, nil
}
