package leakguard

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// TestATarballMemberNamedWithTheReadsTokenPrintsNoneOfIt: whatever answers the daemon's read of the
// check's repository in GitHub's place holds the installation token the read carried, and writes it
// into the name of a member of the tarball's leak-check directory that the pack refuses: a symbolic
// link named with the token whole (whole) or less its first byte (run), and one whose path leaves
// the directory through the token (escape). The refusal Get answers, which becomes the start's
// refusal, the claim's cause and a line of `legion claims list`, holds no run of the token, and
// still names the member, the token's place redacted. The twin shows the stand-in's echo reaches
// the member's name as archive/tar reads it, so a refusal that printed the name as it came would
// hold it. Captures leakguard.tar-member/<case>.
func TestATarballMemberNamedWithTheReadsTokenPrintsNoneOfIt(t *testing.T) {
	cases := []struct {
		name   string
		member func(bearer string) string
		says   string
	}{
		{"whole", func(bearer string) string { return "leak-check/" + bearer }, "which is neither a directory nor a regular file"},
		{"run", func(bearer string) string { return "leak-check/" + bearer[1:] }, "which is neither a directory nor a regular file"},
		{"escape", func(bearer string) string { return "leak-check/../" + bearer }, "which is no path inside leak-check/"},
	}
	linked := func(t *testing.T, member string) []byte {
		return leakguardtest.Tarball(t, map[string]leakguardtest.Member{
			leakguardtest.TarballPrefix + member: {Mode: 0o777, Link: "elsewhere"},
		})
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			if names := memberNames(t, linked(t, c.member(sourceToken))); !strings.Contains(strings.Join(names, "\n"), sourceToken[1:]) {
				t.Fatalf("the %s stand-in's tarball names %q, which holds no echo of the token, so the row cannot see a leak", c.name, names)
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var carried atomic.Value
			fake := serveCheck(t, func(w http.ResponseWriter, r *http.Request, _ string) {
				bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				carried.Store(bearer)
				_, _ = w.Write(linked(t, c.member(bearer)))
			})
			var mints atomic.Int32
			_, err := sourceOver(fake.url, &mints).Get(bound(t), theTag)
			if err == nil {
				t.Fatal("the tarball whose member the pack refuses was taken")
			}
			if bearer, _ := carried.Load().(string); bearer != sourceToken {
				t.Fatalf("the read carried the bearer %q, want the token the Source minted", bearer)
			}
			said := err.Error()
			oauth2calltest.RequireNoPiece(t, "leakguard.tar-member/"+c.name, said, sourceToken)
			if !strings.Contains(said, "holds the member") || !strings.Contains(said, c.says) {
				t.Errorf("Get = %q, want the member named and refused %s", said, c.says)
			}
		})
	}
}

// TestAServedBodyBuiltOfTheReadsTokenPrintsNoneOfIt: the stand-in in GitHub's place writes the
// read's token into the names of two regular files of the leak-check directory the pod's unpack
// cannot both write: one below the other (below), and one twice (twice). Neither is a symbolic
// link nor leaves the directory. The pack refuses each, naming the member as the rule reads it, so
// the start's refusal holds no run of the token; a pack that took them would serve a body whose
// unpack in the pod fails naming the path, the token in it, and that error is what the row reads
// then. The twin shows the error of the unpack's own file call holds the name it was given.
// Captures leakguard.served-body/<case>.
func TestAServedBodyBuiltOfTheReadsTokenPrintsNoneOfIt(t *testing.T) {
	t.Run("twin", func(t *testing.T) {
		requireTheFileCallsErrorEchoes(t)
	})
	for _, c := range []struct {
		name  string
		extra func(bearer string) []string
	}{
		{"below", func(bearer string) []string { return []string{bearer, bearer + "/x"} }},
		{"twice", func(bearer string) []string { return []string{bearer, bearer} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := serveCheck(t, func(w http.ResponseWriter, r *http.Request, _ string) {
				_, _ = w.Write(echoTarball(t, c.extra(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))...))
			})
			var mints atomic.Int32
			served, err := sourceOver(fake.url, &mints).Get(bound(t), theTag)
			if err == nil {
				_, err = unpackServed(served.Body, t.TempDir())
				if err == nil {
					t.Fatal("the pack took the members and the pod's unpack wrote them both")
				}
			}
			said := err.Error()
			oauth2calltest.RequireNoPiece(t, "leakguard.served-body/"+c.name, said, sourceToken)
			if !strings.Contains(said, "holds the member") {
				t.Errorf("Get = %q, want the pack's refusal naming the member", said)
			}
		})
	}
}

// TestThePodsUnpackNamesNoMemberOfTheBodyItWasServed: a body that is not the one PackTarballManifest
// makes reaches the pod's unpack with a member named with the read's token: a regular file below
// another (below), one whose path leaves the directory (escape) and a symbolic link (link). The
// unpack's error, which the launcher hands the daemon as the cause, names the member by its place
// and kind and the failed call by its operation and the system's reason, and holds no run of the
// token. The twin shows the error of the unpack's own file call holds the name it was given.
// Captures leakguard.served-unpack/<case>.
func TestThePodsUnpackNamesNoMemberOfTheBodyItWasServed(t *testing.T) {
	t.Run("twin", func(t *testing.T) {
		requireTheFileCallsErrorEchoes(t)
	})
	for _, c := range []struct {
		name    string
		members map[string]leakguardtest.Member
		says    string
	}{
		{"below", map[string]leakguardtest.Member{
			sourceToken:        {Body: "a file\n", Mode: 0o644},
			sourceToken + "/x": {Body: "below a file\n", Mode: 0o644},
		}, "member 2, a regular file, could not be written: mkdir: not a directory"},
		{"escape", map[string]leakguardtest.Member{
			"../" + sourceToken: {Body: "escaped\n", Mode: 0o644},
		}, "member 1, a regular file, is no path inside the check's directory"},
		{"link", map[string]leakguardtest.Member{
			sourceToken: {Mode: 0o777, Link: "elsewhere"},
		}, "member 1, a symbolic link, is neither a directory nor a regular file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := unpackServed(leakguardtest.Tarball(t, c.members), t.TempDir())
			if err == nil {
				t.Fatal("the pod's unpack took a body PackTarballManifest does not make")
			}
			said := err.Error()
			oauth2calltest.RequireNoPiece(t, "leakguard.served-unpack/"+c.name, said, sourceToken)
			if said != c.says {
				t.Errorf("unpackServed = %q, want %q", said, c.says)
			}
		})
	}
}

// requireTheFileCallsErrorEchoes fails t unless the error of the file call the pod's unpack makes
// for a member below a regular file, both named with the read's token, holds the token: the twin of
// the rows that read the unpack's error.
func requireTheFileCallsErrorEchoes(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sourceToken), []byte("a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := os.MkdirAll(filepath.Join(dir, sourceToken, "x"), 0o755)
	if err == nil || !strings.Contains(err.Error(), sourceToken) {
		t.Fatalf("os.MkdirAll below a file = %v, which holds no echo of the token, so the rows cannot see a leak", err)
	}
}

// echoTarball is a tarball shaped as GitHub's of a check that runs (theCheck answering its help),
// with a regular file at each of extra after its files, under the leak-check directory, in the order
// given: written here, since GitHubTarball's map holds each name once, in path order.
func echoTarball(t *testing.T, extra ...string) []byte {
	t.Helper()
	var raw bytes.Buffer
	zipped := gzip.NewWriter(&raw)
	archive := tar.NewWriter(zipped)
	add := func(name string, typeflag byte, mode int64, body string) {
		if err := archive.WriteHeader(&tar.Header{Name: leakguardtest.TarballPrefix + name, Typeflag: typeflag, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("leak-check/", tar.TypeDir, 0o755, "")
	for name, m := range theCheck(answersItsHelp) {
		add("leak-check/"+name, tar.TypeReg, m.Mode, m.Body)
	}
	for _, name := range extra {
		add("leak-check/"+name, tar.TypeReg, 0o644, "an echo\n")
	}
	for _, closer := range []io.Closer{archive, zipped} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return raw.Bytes()
}

// memberNames is every member's name in the gzipped tarball raw, as archive/tar reads it.
func memberNames(t *testing.T, raw []byte) []string {
	t.Helper()
	zipped, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(zipped)
	var names []string
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}
