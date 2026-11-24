package specgraph

import (
	"bytes"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The reader reads at most 16 MiB of each file of a scenario, the bound that fileBound's doc states,
// and refuses a file over it, naming the file, before it decodes any of it, so a file that grew
// past any use cannot take the memory of the process. Each row writes one file of a scenario past
// the bound with white space at its end, which leaves the file's text a value of its form, so that
// the bound alone refuses it: each JSON file of carry, by each of the readers that read them
// (ReadApproved, ReadFixtureHistory, ReadCommitTies, and the reader of the array files and of
// uncited.json), a spec file of carry, and wait-cycle's units.json. The control writes
// results.json to the bound exactly, which reads.
func TestAFileOfAScenarioOverTheBoundIsRefused(t *testing.T) {
	t.Parallel()
	const bound = 16 << 20
	for _, row := range []struct {
		scenario, file string
		size           int // the size that the row writes the file to
	}{
		{"carry", "approved.json", bound + 1},
		{"carry", "subjects.json", bound + 1},
		{"carry", "files/c2/spec.md", bound + 1},
		{"carry", "history.json", bound + 1},
		{"carry", "ties.json", bound + 1},
		{"carry", "results.json", bound + 1},
		{"carry", "jobs.json", bound + 1},
		{"carry", "uncited.json", bound + 1},
		{"wait-cycle", "units.json", bound + 1},
		{"carry", "results.json", bound}, // the control
	} {
		fsys := scenarioFS(t, filepath.Join("testdata", "scenarios", row.scenario))
		data := fsys[row.file].Data
		fsys[row.file].Data = append(bytes.Clone(data), bytes.Repeat([]byte("\n"), row.size-len(data))...)
		_, err := ReadScenario(fsys)
		var fault *ScenarioError
		switch {
		case row.size <= bound && err != nil:
			t.Errorf("control: %s of %s written to %d bytes, the bound: the reader refuses it: %v", row.file, row.scenario, row.size, err)
		case row.size <= bound:
		case !errors.As(err, &fault) || fault.File != row.file || !strings.Contains(err.Error(), strconv.Itoa(bound)):
			t.Errorf("%s of %s written to %d bytes: the reader gives %v, want a refusal of %s that names the bound of %d bytes", row.file, row.scenario, row.size, err, row.file, bound)
		}
	}
}
