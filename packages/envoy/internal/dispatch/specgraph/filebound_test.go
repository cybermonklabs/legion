package specgraph

import (
	"bytes"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// The reader reads at most 16 MiB of each JSON file of a scenario, the bound that fileBound's doc
// states, and at most notes.MaxFileSize, 1 MiB, of each spec file, the bound past which `spec-check
// tree` and Dispatch's importer refuse a spec file, so that the reader refuses each spec file that
// they refuse. It refuses a file over its bound, naming the file and the bound, before it decodes or
// parses any of it, so a file that grew past any use cannot take the memory of the process. Each
// row writes one file of a scenario past its bound with white space at its end, which leaves the
// file's text a value of its form, so that the bound alone refuses it: each JSON file of carry, by
// each of the readers that read them (ReadApproved, ReadFixtureHistory, ReadCommitTies, and the
// reader of the array files and of uncited.json), a spec file of carry, and wait-cycle's
// units.json. The controls write results.json and the spec file to their bounds exactly, which
// reads.
func TestAFileOfAScenarioOverTheBoundIsRefused(t *testing.T) {
	t.Parallel()
	const jsonBound, specBound = 16 << 20, notes.MaxFileSize
	for _, row := range []struct {
		scenario, file string
		bound          int // the bound of the file
		size           int // the size that the row writes the file to
	}{
		{"carry", "approved.json", jsonBound, jsonBound + 1},
		{"carry", "subjects.json", jsonBound, jsonBound + 1},
		{"carry", "files/c2/spec.yaml", specBound, specBound + 1},
		{"carry", "history.json", jsonBound, jsonBound + 1},
		{"carry", "ties.json", jsonBound, jsonBound + 1},
		{"carry", "results.json", jsonBound, jsonBound + 1},
		{"carry", "jobs.json", jsonBound, jsonBound + 1},
		{"carry", "uncited.json", jsonBound, jsonBound + 1},
		{"wait-cycle", "units.json", jsonBound, jsonBound + 1},
		{"carry", "results.json", jsonBound, jsonBound},       // a control
		{"carry", "files/c2/spec.yaml", specBound, specBound}, // a control
	} {
		fsys := scenarioFS(t, filepath.Join("testdata", "scenarios", row.scenario))
		if strings.HasSuffix(row.file, ".yaml") {
			term := Term{ID: "bound-term", Name: "bound term", Definition: "x"}
			seed, err := term.Print()
			if err != nil {
				t.Fatal(err)
			}
			term.Definition = strings.Repeat("x", row.size-len(seed)+1)
			data, err := term.Print()
			if err != nil {
				t.Fatal(err)
			}
			if len(data) != row.size {
				t.Fatalf("%s has %d bytes, want %d", row.file, len(data), row.size)
			}
			fsys[row.file].Data = []byte(data)
		} else {
			data := fsys[row.file].Data
			fsys[row.file].Data = append(bytes.Clone(data), bytes.Repeat([]byte("\n"), row.size-len(data))...)
		}
		_, err := ReadScenario(fsys)
		var fault *ScenarioError
		switch {
		case row.size <= row.bound && err != nil:
			t.Errorf("control: %s of %s written to %d bytes, its bound: the reader refuses it: %v", row.file, row.scenario, row.size, err)
		case row.size <= row.bound:
		case !errors.As(err, &fault) || fault.File != row.file || !strings.Contains(err.Error(), strconv.Itoa(row.bound)):
			t.Errorf("%s of %s written to %d bytes: the reader gives %v, want a refusal of %s that names the bound of %d bytes", row.file, row.scenario, row.size, err, row.file, row.bound)
		}
	}
}
