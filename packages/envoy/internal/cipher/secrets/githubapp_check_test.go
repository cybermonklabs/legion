package secrets_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// gitHubCheckStand is the GitHub a check is driven against: the stand-in of
// internal/cipher/proxyrules/proxyrulestest, on a plain (http) service of its own, with the App
// installed on acme-org (1) and ada (2). It is that stand-in rather than this package's own
// profileGitHub because it serves the read a released token is spent at and its revoke route
// forgets the token it revoked, so a read made with a token afterwards is refused as any other
// credential of nobody's: that is how a check's promise to hold no token past GitHub's answer is
// read below, rather than from the revoke call alone.
func gitHubCheckStand(t *testing.T) (*proxyrulestest.Service, *proxyrulestest.GitHubStand) {
	t.Helper()
	stand := proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: map[string]int64{"acme-org": 1, "ada": 2}})
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(service.Close)
	service.AnswerWith(stand.Answer)
	return service, stand
}

// reachesWith is the status the stand-in answers a read of ada's lean repository made with
// token, as the caller a release hands a token to makes it: 200 for a token it minted that
// reaches the repository, and 401 once that token was revoked.
func reachesWith(t *testing.T, service *proxyrulestest.Service, token string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, service.URL()+"/repos/ada/lean", nil)
	if err != nil {
		t.Fatalf("build the read: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("read ada/lean with the token GitHub minted: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// errorText is what a check and the release it stands for are compared by: the error's text, and
// "" for no error. It is named for its answer rather than `answered`, which retry_test.go's own
// helper of that name takes in this package's tests.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestAGitHubAppCheckOfAProfileBeforeApprovalReleasesNoToken: secrets.MintRequest.Check is what
// the broker asks this minter before anybody is asked to approve a request for a github-app
// profile, and it promises the release's verdict without the release. For each answer GitHub
// gives a profile's scope, of named repositories or of every repository the installation
// reaches, it makes the calls the release makes, in the same order, and adds the revoke of every
// token GitHub answered with: a scope of every repository is minted and revoked too, since every
// token request names the profile's permissions and GitHub's answer can refuse them. It hands
// back no value at all, nor the entries the cut left out, and the error it answers is the one the
// release answered. A token a check minted is dead by the time it returns: the stand-in forgets
// a token it revoked, so the read a released token is spent at is refused with it. A scope whose
// account the App has no installation on, and one whose permissions the installation holds none
// of but metadata, are refused from the installations listing alone, before any token request,
// as the two refusals the broker refuses a request for besides GitHub's 422
// (secrets.ErrMintNoInstallation and secrets.ErrMintEmptyCut). A check that carries no scope is
// a programming error, refused before GitHub is asked anything.
func TestAGitHubAppCheckOfAProfileBeforeApprovalReleasesNoToken(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	service, stand := gitHubCheckStand(t)
	app := secrets.GitHubApp{Base: service.URL(), HTTP: http.DefaultClient}
	value := githubAppValue(t, "123456", key)
	lean := readScope("ada")
	lean.Repositories = []string{"ada/lean"}
	// leanWithIssues asks for issues too, which the stand-in's installation does not hold, so the
	// cut leaves it out and the release names it.
	leanWithIssues := readScope("ada")
	leanWithIssues.Repositories = []string{"ada/lean"}
	leanWithIssues.Permissions = map[string]string{"contents": "read", "issues": "write", "metadata": "read"}
	// administration is a permission the stand-in's installation does not hold at all, so the cut
	// keeps metadata alone, which every installation holds: an empty cut.
	noneHeld := readScope("ada")
	noneHeld.Permissions = map[string]string{"administration": "write", "metadata": "read"}
	const (
		installations = "GET /app/installations"
		tokenRequest  = "POST /app/installations/2/access_tokens"
		revoke        = "DELETE /installation/token"
	)

	for _, tc := range []struct {
		name  string
		scope *secrets.GitHubScope
		// answer is what the stand-in answers each token request with while this row runs.
		answer proxyrulestest.GitHubAnswer
		class  string // the class of the error the release answers, "" where it serves the scope
		// wraps is the one of the two refusals the broker refuses a request for before GitHub
		// is asked for a token (secrets.ErrMintNoInstallation, secrets.ErrMintEmptyCut) that
		// error wraps, and nil for neither.
		wraps error
		// leftOut is what the release says the cut left out.
		leftOut      map[string][]string
		releaseCalls []string // the calls the release makes to GitHub
		checkCalls   []string // the calls the check makes
	}{
		{
			name:         "a scope GitHub serves",
			scope:        lean,
			answer:       proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensOrdinary},
			releaseCalls: []string{installations, tokenRequest},
			checkCalls:   []string{installations, tokenRequest, revoke},
		},
		{
			name:  "a scope GitHub refuses",
			scope: lean,
			// The one token request is refused, so neither run holds a token and neither revokes
			// one.
			answer:       proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensRefused},
			class:        "narrowing",
			releaseCalls: []string{installations, tokenRequest},
			checkCalls:   []string{installations, tokenRequest},
		},
		{
			name:  "a token GitHub mints wider than the scope",
			scope: lean,
			// The release revokes the token it will not hand over, and the check revokes the
			// same token before it judges it, so the two make the same calls.
			answer:       proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensWider},
			class:        "upstream",
			releaseCalls: []string{installations, tokenRequest, revoke},
			checkCalls:   []string{installations, tokenRequest, revoke},
		},
		{
			name:  "a scope of every repository of an account the App is installed on",
			scope: readScope("ada"),
			// The check asks for the token as the release does, with the profile's permissions,
			// and revokes it: GitHub's answer to that request can refuse the permissions.
			answer:       proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensOrdinary},
			releaseCalls: []string{installations, tokenRequest},
			checkCalls:   []string{installations, tokenRequest, revoke},
		},
		{
			name:         "a scope the cut leaves part of",
			scope:        leanWithIssues,
			answer:       proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensOrdinary},
			leftOut:      map[string][]string{"ada": {"issues"}},
			releaseCalls: []string{installations, tokenRequest},
			checkCalls:   []string{installations, tokenRequest, revoke},
		},
		{
			// grace is an account the App is not installed on.
			name:         "a scope of an account the App is not installed on",
			scope:        readScope("grace"),
			answer:       proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensOrdinary},
			class:        "refused",
			wraps:        secrets.ErrMintNoInstallation,
			releaseCalls: []string{installations},
			checkCalls:   []string{installations},
		},
		{
			name:         "a scope whose permissions the installation holds none of but metadata",
			scope:        noneHeld,
			answer:       proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensOrdinary},
			class:        "narrowing",
			wraps:        secrets.ErrMintEmptyCut,
			releaseCalls: []string{installations},
			checkCalls:   []string{installations},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stand.Answers(tc.answer)
			seen := len(service.Requests())
			callsSince := func() []string {
				requests := service.Requests()
				made := []string{}
				for _, request := range requests[seen:] {
					made = append(made, request.Method+" "+request.Path)
				}
				seen = len(requests)
				return made
			}
			// wrapsAsWanted is whether err wraps the refusal the row names of the two, and not
			// the other.
			wrapsAsWanted := func(err error) bool {
				for _, refusal := range []error{secrets.ErrMintNoInstallation, secrets.ErrMintEmptyCut} {
					if errors.Is(err, refusal) != (tc.wraps == refusal) {
						return false
					}
				}
				return true
			}

			release, releaseErr := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHub: tc.scope})
			class := ""
			if releaseErr != nil {
				class = secrets.MintErrorClass(releaseErr)
			}
			if class != tc.class || !wrapsAsWanted(releaseErr) {
				t.Fatalf("the release answered %v (class %q), want class %q wrapping %v", releaseErr, class, tc.class, tc.wraps)
			}
			if !reflect.DeepEqual(release.LeftOut, tc.leftOut) {
				t.Fatalf("the release says the cut left out %v, want %v", release.LeftOut, tc.leftOut)
			}
			if made := callsSince(); !reflect.DeepEqual(made, tc.releaseCalls) {
				t.Fatalf("the release made the calls %v, want %v", made, tc.releaseCalls)
			}
			// The token the release handed over, which the check mints again below: a read made
			// with it is what says whether the check left it live.
			var token string
			if releaseErr == nil {
				var handed mintedGitHubApp
				if err := json.Unmarshal([]byte(release.Value), &handed); err != nil {
					t.Fatalf("the release's value: %v", err)
				}
				token = handed.Installations["ada"].Token
				if status := reachesWith(t, service, token); status != http.StatusOK {
					t.Fatalf("a read made with the released token answered %d, want 200: a release hands over a token that works", status)
				}
				seen = len(service.Requests())
			}

			checked, checkErr := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHub: tc.scope, Check: true})
			if errorText(checkErr) != errorText(releaseErr) || !wrapsAsWanted(checkErr) {
				t.Fatalf("the check answered %v, want the release's own answer, %v", checkErr, releaseErr)
			}
			if checked.Value != "" || !checked.Expires.IsZero() || checked.LeftOut != nil {
				t.Fatalf("the check handed back %q, expiring %v, leaving out %v; want no value at all", checked.Value, checked.Expires, checked.LeftOut)
			}
			if made := callsSince(); !reflect.DeepEqual(made, tc.checkCalls) {
				t.Fatalf("the check made the calls %v, want %v", made, tc.checkCalls)
			}
			// A check that asked for no token minted none to read with, and the token the
			// release handed over is the release's own.
			if token == "" || !slices.Contains(tc.checkCalls, tokenRequest) {
				return
			}
			if status := reachesWith(t, service, token); status != http.StatusUnauthorized {
				t.Fatalf("a read made with the token the check minted answered %d, want 401: a check holds no token past GitHub's answer", status)
			}
		})
	}

	t.Run("a check that carries no scope", func(t *testing.T) {
		seen := len(service.Requests())
		checked, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, Check: true})
		if checked.Value != "" || secrets.MintErrorClass(err) != "refused" || !strings.Contains(err.Error(), "scope") {
			t.Fatalf("a check with no scope = %q, %v (class %s); want nothing released and class refused, naming the scope",
				checked.Value, err, secrets.MintErrorClass(err))
		}
		if made := service.Requests()[seen:]; len(made) != 0 {
			t.Fatalf("the check made %d calls to GitHub, want none: it is refused before any", len(made))
		}
	})
}
