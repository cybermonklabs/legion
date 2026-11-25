package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// TestAGitRemoteTheGhWrapperCannotReadIsNamedThroughTheRule: a working directory's git remote
// whose URL carries a made-up user name and password and a token in its query, and that the
// wrapper cannot read (a github.com URL naming no OWNER/REPO, url-line; an ssh remote on another
// host, ssh-elsewhere; a line of `git remote -v` whose URL holds a space, remote-v-line), refuses
// the call naming the remote with its URL through the URL rule. The refusal named the URL as
// written, or the whole line. The twin shows each URL holds the credentials and each call is
// still refused, so the row can see one printed. Captures legion.git-remote/<case>.
func TestAGitRemoteTheGhWrapperCannotReadIsNamedThroughTheRule(t *testing.T) {
	const user, password = "not-a-secret-user", "not-a-secret-password"
	credentials := user + ":" + password + "@"
	query := "?access" + "_token=" + oauth2calltest.Token
	fromRemote := func(url string) func(*testing.T) error {
		return func(*testing.T) error {
			s := &selectors{}
			return s.fromWorkingDirectory(func() ([]ghRemote, error) {
				return []ghRemote{{name: "origin", url: url}}, nil
			}, false)
		}
	}
	spaced := "https://" + credentials + "github.com/acme/wid gets" + query
	fromGit := func(t *testing.T) error {
		repository := t.TempDir()
		fixture := fixturegit.Isolated(t, t.TempDir())
		fixture.Run(repository, append([]string{"git"}, fixturegit.InitWorkTree("", "")[1:]...)...)
		fixture.Run(repository, "git", "remote", "add", "origin", spaced)
		t.Chdir(repository)
		_, err := readGitRemotes()
		return err
	}
	cases := []struct {
		name, url string
		call      func(*testing.T) error
	}{
		{"url-line", "https://" + credentials + "github.com/acme" + query, nil},
		{"ssh-elsewhere", "ssh://" + credentials + "gitlab.example/acme/widgets" + query, nil},
		{"remote-v-line", spaced, fromGit},
	}
	for i := range cases {
		if cases[i].call == nil {
			cases[i].call = fromRemote(cases[i].url)
		}
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if !strings.Contains(tc.url, user) || !strings.Contains(tc.url, oauth2calltest.Token) {
				t.Errorf("%s: the URL %q holds no credential, so the row could see none printed", tc.name, tc.url)
			}
			t.Run(tc.name, func(t *testing.T) {
				if err := tc.call(t); err == nil || !strings.Contains(err.Error(), "remote") {
					t.Errorf("the call = %v; want it refused naming the remote", err)
				}
			})
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(t)
			if err == nil {
				t.Fatalf("the remote %q was read; want the call refused", tc.url)
			}
			oauth2calltest.RequireNoPiece(t, "legion.git-remote/"+tc.name, err.Error(), user, password, oauth2calltest.Token)
		})
	}
}
