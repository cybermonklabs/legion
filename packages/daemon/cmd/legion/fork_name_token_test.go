package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// TestAForkFullNameThatHoldsTheReadsTokenPrintsNoneOfIt: whatever answers the fork read in
// GitHub's place echoes the token the read carried into a full name of the shape
// `owner/<token>`, which ghrepo.Parse takes (a repository's name may hold "-" and "_") and which no
// GitHub token form matches (twin). The repository's own full name (full-name) and a parent's
// (upstream) are named as no repository, never printed, where the refusal printed them. The
// control: each refusal still names the read and says what it found. Captures
// legion.fork-name/<case>.
func TestAForkFullNameThatHoldsTheReadsTokenPrintsNoneOfIt(t *testing.T) {
	const token = "not-a-secret-token-7Qz4kM2p"
	echoed := "outside-author/" + token
	repo := ghrepo.MustParse("outside-author/sample-project")
	t.Run("twin", func(t *testing.T) {
		if named, err := ghrepo.Parse("GitHub's full_name", echoed); err != nil || !strings.Contains(named.String(), token) {
			t.Fatalf("ghrepo.Parse(%q) = %v, %v; want it taken whole, so the row can see it printed", echoed, named, err)
		}
	})
	t.Run("full-name", func(t *testing.T) {
		_, err := readForkAnswer(leakguard.CurlResponse{Stdout: "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\n\r\n" +
			`{"full_name":"` + echoed + `","fork":true}`}, repo, token)
		said := errorText(err)
		oauth2calltest.RequireNoPiece(t, "legion.fork-name/full-name", said, token)
		if want := "GitHub answered the read of " + repo.String() + " with a full_name that names no repository"; !strings.Contains(said, want) {
			t.Errorf("readForkAnswer = %q; want %q in it", said, want)
		}
	})
	t.Run("upstream", func(t *testing.T) {
		_, _, err := forkAnswer{Parent: forkUpstream{FullName: echoed}}.dir(t.TempDir(), repo, token)
		said := errorText(err)
		oauth2calltest.RequireNoPiece(t, "legion.fork-name/upstream", said, token)
		if want := "the parent, whose full_name names no repository GitHub could hold"; !strings.Contains(said, want) {
			t.Errorf("dir = %q; want %q in it", said, want)
		}
	})
}
