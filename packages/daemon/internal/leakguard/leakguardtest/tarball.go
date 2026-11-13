package leakguardtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"maps"
	"slices"
	"testing"
)

// TarballPrefix is the one directory GitHub's tarball of the check's repository holds every
// member under, `<owner>-<repo>-<sha>/`.
const TarballPrefix = "theorem-labs-agent-infra-84d835ba5d00f97212253ffa221898f1f234a772/"

// checkDirectory is the directory of the check's repository the check's files are under. The
// guard spells it for itself (leakguard.checkDirectory, unexported).
const checkDirectory = "leak-check"

// Member is one member of a test tarball: a regular file holding Body, or a symbolic link to Link
// where Link is set, at Mode.
type Member struct {
	Body string
	Mode int64
	Link string
}

// GitHubTarball is a gzipped tarball shaped as GitHub's tarball of the check's repository: under
// TarballPrefix, the leak-check directory's own entry, one file of the repository outside that
// directory (README.md), and every member of check under the directory at its path, in path
// order. A path that leaves the directory is written as it is, so a test can hand the pack one.
func GitHubTarball(t testing.TB, check map[string]Member) []byte {
	t.Helper()
	return gzipTar(t, func(archive *tar.Writer) {
		if err := archive.WriteHeader(&tar.Header{Name: TarballPrefix + checkDirectory + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		writeMember(t, archive, TarballPrefix+"README.md", Member{Body: "the rest of the repository\n", Mode: 0o644})
		for _, name := range slices.Sorted(maps.Keys(check)) {
			writeMember(t, archive, TarballPrefix+checkDirectory+"/"+name, check[name])
		}
	})
}

// Tarball is a gzipped tarball of members alone, each at its own whole path, in path order: the
// shape of a tarball GitHub's is not, such as one with no leak-check directory.
func Tarball(t testing.TB, members map[string]Member) []byte {
	t.Helper()
	return gzipTar(t, func(archive *tar.Writer) {
		for _, name := range slices.Sorted(maps.Keys(members)) {
			writeMember(t, archive, name, members[name])
		}
	})
}

// gzipTar writes a gzipped tarball whose members write adds, and answers its bytes.
func gzipTar(t testing.TB, write func(*tar.Writer)) []byte {
	t.Helper()
	var raw bytes.Buffer
	zipped := gzip.NewWriter(&raw)
	archive := tar.NewWriter(zipped)
	write(archive)
	for _, closer := range []interface{ Close() error }{archive, zipped} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return raw.Bytes()
}

// writeMember adds m to archive under name.
func writeMember(t testing.TB, archive *tar.Writer, name string, m Member) {
	t.Helper()
	header := &tar.Header{Name: name, Mode: m.Mode, Size: int64(len(m.Body))}
	if m.Link != "" {
		header = &tar.Header{Name: name, Mode: m.Mode, Typeflag: tar.TypeSymlink, Linkname: m.Link}
	}
	if err := archive.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if m.Link == "" {
		if _, err := archive.Write([]byte(m.Body)); err != nil {
			t.Fatal(err)
		}
	}
}
