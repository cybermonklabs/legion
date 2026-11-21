package githubapp

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// The set's two bounds refuse a listing before any blob read: one file more than notes.MaxFiles,
// and listed sizes summing one byte more than notes.MaxSetSize, though each file is within
// notes.MaxFileSize. Every blob is missing, so a read would fail with GitHub's 404. A listing at
// each bound reads whole.
func TestDirFilesEnforceTheSetBounds(t *testing.T) {
	const dir = ".dispatch/architecture"
	small := func(count int) map[string]string {
		files := map[string]string{}
		for i := range count {
			files[fmt.Sprintf("%s/n%04d.md", dir, i)] = "x"
		}
		return files
	}
	atCap := strings.Repeat("x", notes.MaxFileSize)
	summing := func(total int) (map[string]string, map[string]int64) {
		files, sizes := map[string]string{}, map[string]int64{}
		for i := 0; total > 0; i++ {
			path := fmt.Sprintf("%s/big-%02d.md", dir, i)
			size := min(total, notes.MaxFileSize)
			files[path], sizes[path] = atCap[:size], int64(size)
			total -= size
		}
		return files, sizes
	}
	missing := func(files map[string]string) map[string]bool {
		gone := map[string]bool{}
		for path := range files {
			gone[path] = true
		}
		return gone
	}
	overSum, overSizes := summing(notes.MaxSetSize + 1)
	for _, row := range []struct {
		name  string
		files map[string]string
		sizes map[string]int64
		want  string
	}{
		{name: "one file over MaxFiles", files: small(notes.MaxFiles + 1), want: "set too large: " + dir + " holds 1001 files (limit 1000)"},
		{name: "one byte over MaxSetSize", files: overSum, sizes: overSizes, want: "set too large: " + dir + " holds 16777217 bytes in its files (limit 16777216)"},
	} {
		fake := &fakeGitHub{t: t, files: row.files, sizes: row.sizes, missingBlobs: missing(row.files)}
		client, token := newReaderFixture(t, fake)
		if _, _, err := readDir(t, client, token); !errors.Is(err, notes.ErrSetTooLarge) || err.Error() != row.want {
			t.Errorf("%s: err=%v, want notes.ErrSetTooLarge %q, refused before any blob read", row.name, err, row.want)
		}
	}
	atSum, _ := summing(notes.MaxSetSize)
	for name, files := range map[string]map[string]string{"MaxFiles files": small(notes.MaxFiles), "files summing MaxSetSize": atSum} {
		fake := &fakeGitHub{t: t, files: files}
		client, token := newReaderFixture(t, fake)
		if _, read, err := readDir(t, client, token); err != nil || len(read) != len(files) {
			t.Errorf("%s: %d files read, err=%v; want all %d", name, len(read), err, len(files))
		}
	}
}
