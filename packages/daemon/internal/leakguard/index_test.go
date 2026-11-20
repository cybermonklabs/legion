package leakguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// goldenIndex is an index the check's own writer wrote, so ParseIndex is held to the file the
// check writes rather than to this package's reading of the layout. It was written at
// 2026-11-20T09:47:01Z by the fixture command of the check's repository at commit
// 8c055c6b78288efcb30ea89b283cca44a6f0cca7, whose test-index writes through the check's own
// write_index, run from an export of that commit's leak-check directory (`git archive <that
// commit> leak-check`):
//
//	/usr/bin/python3 leak-check/index_fixture.py test-index --out sandbox-lines.idx --age 0 --neutral-names \
//	  --line 'a synthetic sandbox line that the fixture index of this test holds' \
//	  --line 'another synthetic line of the same fixture, which no grader file holds'
//
// --neutral-names writes a header that names no repository and no sandbox path, which this public
// repository's private-names check (.github/scripts/check-private-names.sh) refuses in a fixture,
// and --age 0 stamps the moment the command ran, which ParseIndex reads without judging its age.
// The list holds the line hashes of the two lines above and of the header's canary.
const goldenIndex = "testdata/sandbox-lines.idx"

// writtenLine1 is line 1 of an index of format 1, as the check's writer begins every index,
// spelled here rather than read from the package's own constant.
const writtenLine1 = "PUBLIC-LEAK-CHECK-INDEX 1\n"

// The release and the asset the builder publishes the index as, spelled here rather than read
// from the package's constants, so a read of another release or asset fails these tests.
const (
	publishedRelease = "leak-check-index"
	publishedAsset   = "sandbox-lines.idx"
)

func readGolden(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(goldenIndex)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// laidOut is an index laid out as the check's writer lays one out: line 1, then header as line 2,
// with spaces after it until the byte after its LF is at a multiple of 8, then list.
func laidOut(header string, list []byte) []byte {
	head := writtenLine1 + header
	head += strings.Repeat(" ", (8-(len(head)+1)%8)%8) + "\n"
	return append([]byte(head), list...)
}

// listOf is values as an index lists them: each an unsigned 64-bit little-endian integer.
func listOf(values ...uint64) []byte {
	list := make([]byte, 0, 8*len(values))
	for _, value := range values {
		list = binary.LittleEndian.AppendUint64(list, value)
	}
	return list
}

// builtIndex is an index built at builtAt whose list is values, ascending, with a header of every
// key the check's writer writes: the four ParseIndex reads and the ones it reads none of.
func builtIndex(builtAt time.Time, values ...uint64) []byte {
	list := listOf(values...)
	sum := sha256.Sum256(list)
	return laidOut(fmt.Sprintf(`{"built_at":%q,"canary":"public leak check index canary %032x","entries":%d,"format":1,`+
		`"generic":{"go":"none","python":"3.12.3"},"min_bytes":40,"public_trees":[],`+
		`"sandbox_files":[{"blob":"%040d","path":"sandbox/index-fixture.txt"}],"sha256":"%x"}`,
		builtAt.UTC().Format("2006-01-02T15:04:05Z"), values[0], len(values), 0, sum), list)
}

// The golden file, which the check's own writer wrote, parses: ParseIndex answers the build time
// and the list's digest its header holds, read here off its line 2 by hand, and the file whole.
func TestParseIndexReadsTheFileTheCheckWrites(t *testing.T) {
	body := readGolden(t)
	index, err := ParseIndex(body)
	if err != nil {
		t.Fatalf("the check's own index = %v, want it read", err)
	}
	wantBuiltAt := time.Date(2026, time.November, 20, 9, 47, 1, 0, time.UTC)
	const wantSHA256 = "a207622a82fceb55f095e59bd8750d5c0b9c272c4fe2dc1780f7ff3f0f68ad1e"
	if !index.BuiltAt.Equal(wantBuiltAt) || index.SHA256 != wantSHA256 {
		t.Fatalf("the check's own index read as built at %s with the list digest %s, want %s and %s",
			index.BuiltAt, index.SHA256, wantBuiltAt, wantSHA256)
	}
	if !bytes.Equal(index.Body, body) {
		t.Fatalf("the index's body is %d bytes, want the file whole, %d", len(index.Body), len(body))
	}
}

// A list that does not match its header is refused: 8 bytes past the header's entries, and one
// changed byte of the list.
func TestParseIndexRefusesAWrongList(t *testing.T) {
	golden := readGolden(t)
	longer := append(slices.Clone(golden), 1, 0, 0, 0, 0, 0, 0, 0)
	changed := slices.Clone(golden)
	changed[len(changed)-1] ^= 0x01
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"8 bytes past the header's entries", longer, "the index's list holds 4 entries and its header says 3"},
		{"one changed byte of the list", changed, "the sha256 of the index's list is not its header's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseIndex(tc.body); err == nil || err.Error() != tc.want {
				t.Fatalf("ParseIndex = %v, want %q", err, tc.want)
			}
		})
	}
}

// A file whose line 1 or line 2 is not of the layout, or whose header does not hold one of the
// four keys ParseIndex reads, of its type and value, is refused by what is wrong with it. The
// control: the same header, well formed, parses.
func TestParseIndexRefusesAHeaderItCannotRead(t *testing.T) {
	list := listOf(7, 9)
	sum := sha256.Sum256(list)
	digest := `"` + hex.EncodeToString(sum[:]) + `"`
	const builtAt = `"2026-11-20T09:00:00Z"`
	keys := func(format, builtAt, entries, sha string) string {
		return `"built_at":` + builtAt + `,"entries":` + entries + `,"format":` + format + `,"sha256":` + sha
	}
	good := keys(`1`, builtAt, `2`, digest)
	if _, err := ParseIndex(laidOut("{"+good+"}", list)); err != nil {
		t.Fatalf("the control, a well-formed header = %v, want it read", err)
	}
	offGrid := writtenLine1 + "{" + good + "}"
	offGrid += strings.Repeat(" ", (8-(len(offGrid)+1)%8)%8+1) + "\n"
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"an empty file", nil, `the index's first line is not "PUBLIC-LEAK-CHECK-INDEX 1"`},
		{"line 1 of format 2", append([]byte("PUBLIC-LEAK-CHECK-INDEX 2\n{"+good+"}\n"), list...),
			`the index's first line is not "PUBLIC-LEAK-CHECK-INDEX 1"`},
		{"no line 2", []byte(writtenLine1 + "{" + good + "}"), "the index has no header line"},
		{"line 2 ending off the 8-byte grid", append([]byte(offGrid), list...),
			fmt.Sprintf("the index's header line ends at byte %d, not at a multiple of 8", len(offGrid))},
		{"a header that is not JSON", laidOut("{format:1}", list), "the index's header is not a JSON object"},
		{"a header that is JSON null", laidOut("null", list), "the index's header is not a JSON object"},
		{"a header that is not ASCII", laidOut(`{"canary":"canary `+"\u00e9"+`",`+good+"}", list), "the index's header is not ASCII"},
		{"a header of format 2", laidOut("{"+keys(`2`, builtAt, `2`, digest)+"}", list), "the index is of format 2, not 1"},
		{"no format", laidOut(`{"built_at":`+builtAt+`,"entries":2,"sha256":`+digest+`}`, list), "the index's header has no format"},
		{"a format that is a string", laidOut("{"+keys(`"1"`, builtAt, `2`, digest)+"}", list),
			`the index's header holds the format "1", which is not of its type`},
		{"entries that are null", laidOut("{"+keys(`1`, builtAt, `null`, digest)+"}", list),
			"the index's header holds the entries null, which is not of its type"},
		{"a fractional count of entries", laidOut("{"+keys(`1`, builtAt, `2.0`, digest)+"}", list),
			"the index's header holds the entries 2.0, which is not of its type"},
		{"no sha256", laidOut(`{"built_at":`+builtAt+`,"entries":2,"format":1}`, list), "the index's header has no sha256"},
		{"a build time with no T", laidOut("{"+keys(`1`, `"2026-11-20 09:00:00Z"`, `2`, digest)+"}", list),
			`the index's built_at "2026-11-20 09:00:00Z" is not a UTC time YYYY-MM-DDTHH:MM:SSZ`},
		{"a build time with a one-digit hour", laidOut("{"+keys(`1`, `"2026-11-20T9:00:00Z"`, `2`, digest)+"}", list),
			`the index's built_at "2026-11-20T9:00:00Z" is not a UTC time YYYY-MM-DDTHH:MM:SSZ`},
		{"a build time in month 13", laidOut("{"+keys(`1`, `"2026-13-20T09:00:00Z"`, `2`, digest)+"}", list),
			`the index's built_at "2026-13-20T09:00:00Z" is not a UTC time YYYY-MM-DDTHH:MM:SSZ`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseIndex(tc.body); err == nil || err.Error() != tc.want {
				t.Fatalf("ParseIndex = %v, want %q", err, tc.want)
			}
		})
	}
}

// serveIndex is the release stand-in for the check's repository and the index's release, admitting
// sourceToken alone.
func serveIndex(t *testing.T) *leakguardtest.Releases {
	t.Helper()
	return leakguardtest.ServeReleases(t, CheckSlug, publishedRelease, sourceToken)
}

// indexSourceOver is an IndexSource reading api with sourceToken.
func indexSourceOver(api string) *IndexSource {
	return &IndexSource{API: api, Token: func(context.Context) (string, error) { return sourceToken, nil }}
}

// indexReader is a read of the index, of IndexSource.Read's shape.
type indexReader func(context.Context) (Index, error)

// onceCached is read behind a cache that keeps its first answer for good, as Source keeps the read
// of a tag.
func onceCached(read indexReader) indexReader {
	var once sync.Once
	var kept Index
	var err error
	return func(ctx context.Context) (Index, error) {
		once.Do(func() { kept, err = read(ctx) })
		return kept, err
	}
}

// readTwice publishes a and reads the index, then publishes b and reads it again, through the
// reader over makes of a fresh stand-in, and answers the two indexes and how many times the
// stand-in was asked for the release and for its asset.
func readTwice(t *testing.T, a, b []byte, over func(api string) indexReader) (first, second Index, releaseReads, assetReads int) {
	t.Helper()
	releases := serveIndex(t)
	read := over(releases.URL)
	releases.Publish(publishedAsset, a)
	first, err := read(bound(t))
	if err != nil {
		t.Fatalf("the first read: %v", err)
	}
	releases.Publish(publishedAsset, b)
	if second, err = read(bound(t)); err != nil {
		t.Fatalf("the second read: %v", err)
	}
	for _, asked := range releases.Asked() {
		switch {
		case strings.Contains(asked.Path, "/releases/tags/"):
			releaseReads++
		case strings.Contains(asked.Path, "/releases/assets/"):
			assetReads++
		}
	}
	return first, second, releaseReads, assetReads
}

// Each Read reads GitHub again and keeps nothing: with index A published and then index B, two
// reads answer A and then B, and each asks for the release and then its asset. The control is the
// same two reads through a reader that keeps its first answer, as Source keeps a tag's: it answers
// A twice and asks GitHub once, and the second assertion refuses it, so that assertion can tell a
// cache from a read.
func TestIndexSourceReadsEachTime(t *testing.T) {
	a := builtIndex(time.Date(2026, time.November, 20, 9, 7, 0, 0, time.UTC), 11, 12)
	b := builtIndex(time.Date(2026, time.November, 20, 10, 7, 0, 0, time.UTC), 21, 22)
	secondIsB := func(second Index) error {
		if !bytes.Equal(second.Body, b) {
			return fmt.Errorf("the second read answered the index built at %s, want B, built at 10:07:00", second.BuiltAt.Format(time.TimeOnly))
		}
		return nil
	}

	first, second, releaseReads, assetReads := readTwice(t, a, b, func(api string) indexReader { return indexSourceOver(api).Read })
	if !bytes.Equal(first.Body, a) {
		t.Fatalf("the first read answered the index built at %s, want A, built at 09:07:00", first.BuiltAt.Format(time.TimeOnly))
	}
	if err := secondIsB(second); err != nil {
		t.Fatal(err)
	}
	if releaseReads != 2 || assetReads != 2 {
		t.Fatalf("two reads asked for the release %d times and for its asset %d times, want each twice", releaseReads, assetReads)
	}

	_, cached, releaseReads, assetReads := readTwice(t, a, b, func(api string) indexReader { return onceCached(indexSourceOver(api).Read) })
	if secondIsB(cached) == nil {
		t.Fatal("the control: a reader that keeps its first answer passed the second assertion, which so cannot tell a cache from a read")
	}
	if releaseReads != 1 || assetReads != 1 {
		t.Fatalf("the control: a reader that keeps its first answer asked for the release %d times and for its asset %d times, want each once", releaseReads, assetReads)
	}
}

// The read carries the daemon's token in its own two requests: the stand-in sees the bearer on the
// release request and on the asset request, and the asset request asks for the asset's bytes
// (Accept: application/octet-stream) rather than its JSON.
func TestIndexSourceKeepsTheTokenInItsOwnRequest(t *testing.T) {
	releases := serveIndex(t)
	published := builtIndex(time.Date(2026, time.November, 20, 9, 7, 0, 0, time.UTC), 5)
	id := releases.Publish(publishedAsset, published)
	got, err := indexSourceOver(releases.URL).Read(bound(t))
	if err != nil {
		t.Fatalf("the read: %v", err)
	}
	if !bytes.Equal(got.Body, published) {
		t.Fatalf("the read answered %d bytes, want the published index's %d", len(got.Body), len(published))
	}
	want := []leakguardtest.Asked{
		{Path: "/repos/" + CheckSlug + "/releases/tags/leak-check-index", Authorization: "Bearer " + sourceToken, Accept: "application/vnd.github+json"},
		{Path: fmt.Sprintf("/repos/%s/releases/assets/%d", CheckSlug, id), Authorization: "Bearer " + sourceToken, Accept: "application/octet-stream"},
	}
	if asked := releases.Asked(); !slices.Equal(asked, want) {
		t.Fatalf("the read made the requests %+v, want %+v", asked, want)
	}
}

// indexRefusal is the sentence of a Read refused for why.
func indexRefusal(why string) string {
	return "the daemon could not read sandbox-lines.idx of the release leak-check-index of " + CheckSlug + ": " + why
}

// A 401 is GitHub no longer taking the token the read was made with: the read calls Forget once,
// so the next mint is a new token, which the next read is made with. A read refused otherwise
// forgets nothing.
func TestIndexSourceForgetsATokenGitHubAnswers401(t *testing.T) {
	releases := serveIndex(t)
	published := builtIndex(time.Date(2026, time.November, 20, 9, 7, 0, 0, time.UTC), 5)
	var mints, forgets atomic.Int32
	source := &IndexSource{API: releases.URL, Forget: func() { forgets.Add(1) }, Token: func(context.Context) (string, error) {
		if mints.Add(1) == 1 {
			return "ghs_revoked", nil
		}
		return sourceToken, nil
	}}
	releases.Publish(publishedAsset, published)
	_, err := source.Read(bound(t))
	if want := indexRefusal(`GitHub answered the read of the release with HTTP 401: {"message":"Bad credentials"}`); err == nil || err.Error() != want {
		t.Fatalf("the read with a token GitHub refuses = %v, want %q", err, want)
	}
	if got := forgets.Load(); got != 1 {
		t.Fatalf("the read answered 401 forgot the token %d times, want once", got)
	}
	got, err := source.Read(bound(t))
	if err != nil || !bytes.Equal(got.Body, published) {
		t.Fatalf("the read after the 401 = %d bytes, %v; want the published index", len(got.Body), err)
	}

	releases.Publish(publishedAsset+".new", published)
	if _, err := source.Read(bound(t)); err == nil {
		t.Fatal("a read of a release with no index asset was not refused")
	}
	if got := forgets.Load(); got != 1 {
		t.Fatalf("a read refused for another reason forgot the token, %d forgets in all, want 1", got)
	}
}

// A release that holds no index the daemon can push is refused by what it holds, in a sentence
// that names the asset, the release and the repository and never the token: no release yet, the
// moment of a publish when the release holds the new upload alone, an asset over 16 MiB, and an
// asset that is no index. An asset of exactly 16 MiB is read, and refused only as no index.
func TestIndexSourceRefusesAReleaseWithNoIndex(t *testing.T) {
	for _, tc := range []struct {
		name    string
		publish func(*leakguardtest.Releases)
		why     string
	}{
		{"no release yet", func(*leakguardtest.Releases) {},
			`GitHub answered the read of the release with HTTP 404: {"message":"Not Found","documentation_url":"https://docs.github.com/rest/releases/releases#get-a-release-by-tag-name","status":"404"}`},
		{"the new upload alone", func(r *leakguardtest.Releases) {
			r.Publish(publishedAsset+".new", builtIndex(time.Date(2026, time.November, 20, 9, 7, 0, 0, time.UTC), 5))
		}, "the release holds no asset sandbox-lines.idx"},
		{"an asset over 16 MiB", func(r *leakguardtest.Releases) {
			r.Publish(publishedAsset, bytes.Repeat([]byte("x"), 16<<20+1))
		}, "GitHub's answer for the asset is over the 16 MiB this daemon reads"},
		{"an asset of 16 MiB", func(r *leakguardtest.Releases) {
			r.Publish(publishedAsset, bytes.Repeat([]byte("x"), 16<<20))
		}, `the index's first line is not "PUBLIC-LEAK-CHECK-INDEX 1"`},
		{"an asset that is no index", func(r *leakguardtest.Releases) {
			r.Publish(publishedAsset, []byte("not an index\n"))
		}, `the index's first line is not "PUBLIC-LEAK-CHECK-INDEX 1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases := serveIndex(t)
			tc.publish(releases)
			_, err := indexSourceOver(releases.URL).Read(bound(t))
			if want := indexRefusal(tc.why); err == nil || err.Error() != want {
				t.Fatalf("the read = %v, want %q", err, want)
			}
			if strings.Contains(err.Error(), sourceToken) {
				t.Fatalf("the refusal %q carries the token", err)
			}
		})
	}
}

// underUmask sets the process's file mode creation mask to mask for the rest of the test, so a mode
// the test reads back is the mode the code asked for under that mask rather than under whatever
// mask the test was started with, and puts the old mask back when the test ends. The mask is the
// whole process's, and no test of this package runs in parallel.
func underUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

// namesIn is the names dir holds, sorted.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// pastBuild is a build time at minute 7 of hour on 2026-11-19, a day before these tests were
// written: every install test but the one of build times ahead of the clock builds its indexes at
// such a time, so the clock is past each of them, as it is past every index the builder publishes.
func pastBuild(hour int) time.Time {
	return time.Date(2026, time.November, 19, hour, 7, 0, 0, time.UTC)
}

// An index replaces what the folder holds where that is an older index, nothing, a file that is no
// index, or a symlink, even one to a later index: a role can put a symlink there, so it is
// replaced, never followed or written through. The index lands whole, as a regular file of mode
// 0644, and nothing else is left in the folder.
func TestInstallIndexReplacesAnOlderFile(t *testing.T) {
	underUmask(t, 0o022)
	older := builtIndex(pastBuild(9), 1, 2)
	newer := builtIndex(pastBuild(10), 3, 4)
	later := builtIndex(pastBuild(11), 5, 6)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, later, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		held func(t *testing.T, path string)
	}{
		{"an older index", func(t *testing.T, path string) {
			if err := os.WriteFile(path, older, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"no index", func(*testing.T, string) {}},
		{"a file that is no index", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("half an ind"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlink to a later index", func(t *testing.T, path string) {
			if err := os.Symlink(elsewhere, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "sandbox-lines.idx")
			tc.held(t, path)
			replaced, err := InstallIndex(dir, newer)
			if err != nil || !replaced {
				t.Fatalf("InstallIndex over %s = %v, %v; want it replaced", tc.name, replaced, err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode() != 0o644 {
				t.Fatalf("the installed index is of mode %v, want a regular file of mode 0644", info.Mode())
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, newer) {
				t.Fatalf("the folder's index holds %d bytes (%v), want the newer index", len(got), err)
			}
			if names := namesIn(t, dir); !slices.Equal(names, []string{"sandbox-lines.idx"}) {
				t.Fatalf("the folder holds %q, want the index alone", names)
			}
		})
	}
	if got, err := os.ReadFile(elsewhere); err != nil || !bytes.Equal(got, later) {
		t.Fatalf("the file the symlink named holds %d bytes (%v), want it as it was", len(got), err)
	}
}

// An index does not replace one built at its own time or later: the folder keeps the index it
// holds, and nothing else is left in it.
func TestInstallIndexKeepsANewerFile(t *testing.T) {
	arriving := builtIndex(pastBuild(9), 1, 2)
	for _, tc := range []struct {
		name string
		held []byte
	}{
		{"a newer index", builtIndex(pastBuild(10), 3, 4)},
		{"an index of the same build time", builtIndex(pastBuild(9), 5, 6)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "sandbox-lines.idx")
			if err := os.WriteFile(path, tc.held, 0o644); err != nil {
				t.Fatal(err)
			}
			replaced, err := InstallIndex(dir, arriving)
			if err != nil || replaced {
				t.Fatalf("InstallIndex over %s = %v, %v; want it kept", tc.name, replaced, err)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, tc.held) {
				t.Fatalf("the folder's index holds %d bytes (%v), want %s as it was", len(got), err, tc.name)
			}
			if names := namesIn(t, dir); !slices.Equal(names, []string{"sandbox-lines.idx"}) {
				t.Fatalf("the folder holds %q, want the index alone", names)
			}
		})
	}
}

// A body that is no index is refused before anything is written, and an install whose rename
// fails leaves no new file behind: what the folder held stays as it was.
func TestInstallIndexRefusesAndLeavesNoNewFile(t *testing.T) {
	index := builtIndex(pastBuild(9), 1, 2)
	t.Run("a body that is no index", func(t *testing.T) {
		dir := t.TempDir()
		changed := slices.Clone(index)
		changed[len(changed)-1] ^= 0x01
		_, err := InstallIndex(dir, changed)
		if want := "the leak check index could not be installed in " + dir + ": the sha256 of the index's list is not its header's"; err == nil || err.Error() != want {
			t.Fatalf("InstallIndex = %v, want %q", err, want)
		}
		if names := namesIn(t, dir); len(names) != 0 {
			t.Fatalf("the folder holds %q, want nothing", names)
		}
	})
	t.Run("a directory where the index goes", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "sandbox-lines.idx")
		if err := os.MkdirAll(filepath.Join(path, "inside"), 0o755); err != nil {
			t.Fatal(err)
		}
		if replaced, err := InstallIndex(dir, index); err == nil || replaced {
			t.Fatalf("InstallIndex over a directory = %v, %v; want it refused", replaced, err)
		}
		if names := namesIn(t, dir); !slices.Equal(names, []string{"sandbox-lines.idx"}) {
			t.Fatalf("the folder holds %q, want the directory alone", names)
		}
		if info, err := os.Lstat(path); err != nil || !info.IsDir() {
			t.Fatalf("the directory where the index goes is %v (%v), want it as it was", info, err)
		}
	})
}

// The check reads an index built more than 10 minutes ahead of the clock as a header it cannot
// read, so such an index is never installed and never kept: one built 11 minutes ahead is refused
// before anything is written, with the current index the folder holds left as it was, and a held
// index built a day ahead is no index, which a current one replaces. The control: an index built
// 9 minutes ahead is installed. The build times are read off the clock, to the second, as the
// builder writes them, so each case is as far ahead of InstallIndex's own reading of the clock as
// its name says, less the moments between the two readings.
func TestInstallIndexTakesNoIndexBuiltAheadOfTheClock(t *testing.T) {
	underUmask(t, 0o022)
	now := time.Now()
	current := builtIndex(now, 1, 2)
	for _, tc := range []struct {
		name     string
		held     []byte
		arriving time.Time
		// refused is the start of the refusal's cause, after the directory; empty is an install.
		refused string
	}{
		{name: "a held index a day ahead, under a current one", held: builtIndex(now.Add(24*time.Hour), 3, 4), arriving: now},
		{name: "an index 11 minutes ahead, over a current one", held: current, arriving: now.Add(11 * time.Minute),
			refused: "the index's built_at " + now.Add(11*time.Minute).UTC().Format("2006-01-02T15:04:05Z") +
				" is more than 10 minutes ahead of this machine's clock, "},
		{name: "the control: an index 9 minutes ahead, over a current one", held: current, arriving: now.Add(9 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "sandbox-lines.idx")
			if err := os.WriteFile(path, tc.held, 0o644); err != nil {
				t.Fatal(err)
			}
			arriving := builtIndex(tc.arriving, 5, 6)
			replaced, err := InstallIndex(dir, arriving)
			want := arriving
			if tc.refused == "" {
				if err != nil || !replaced {
					t.Fatalf("InstallIndex = %v, %v; want it installed", replaced, err)
				}
			} else {
				prefix := "the leak check index could not be installed in " + dir + ": " + tc.refused
				if err == nil || replaced || !strings.HasPrefix(err.Error(), prefix) {
					t.Fatalf("InstallIndex = %v, %v; want it refused with %q and the clock's reading", replaced, err, prefix)
				}
				want = tc.held
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("the folder's index holds %d bytes (%v), want %d: the index InstallIndex should leave there", len(got), err, len(want))
			}
			if names := namesIn(t, dir); !slices.Equal(names, []string{"sandbox-lines.idx"}) {
				t.Fatalf("the folder holds %q, want the index alone", names)
			}
		})
	}
}

// The marker is an empty regular file of its own name in the folder, which MarkIndexPushed makes,
// mode 0755, where it is absent. A second mark, a launcher that started again, leaves it so. A
// symlink at the marker's name is refused, never written through.
func TestMarkIndexPushedWritesTheMarker(t *testing.T) {
	underUmask(t, 0o022)
	dir := filepath.Join(t.TempDir(), "home", ".cache", "public-leak-check")
	marker := filepath.Join(dir, "sandbox-lines.idx.pushed")
	for range 2 {
		if err := MarkIndexPushed(dir); err != nil {
			t.Fatalf("MarkIndexPushed: %v", err)
		}
		folder, err := os.Stat(dir)
		if err != nil || folder.Mode() != os.ModeDir|0o755 {
			t.Fatalf("the folder is %v (%v), want a directory of mode 0755", folder, err)
		}
		info, err := os.Lstat(marker)
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			t.Fatalf("the marker is %v (%v), want an empty regular file", info, err)
		}
	}

	linked := filepath.Join(t.TempDir(), "public-leak-check")
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linked, "sandbox-lines.idx.pushed")); err != nil {
		t.Fatal(err)
	}
	err := MarkIndexPushed(linked)
	if want := "the leak check index's pushed marker could not be written in " + linked + ": " +
		filepath.Join(linked, "sandbox-lines.idx.pushed") + " is a symlink"; err == nil || err.Error() != want {
		t.Fatalf("MarkIndexPushed over a symlink = %v, want %q", err, want)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("the symlink's target = %v, want it never made", err)
	}
}
