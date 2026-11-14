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

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// gitHubCheckStand is the GitHub a check is driven against: the stand-in of
// internal/broker/airlock/airlocktest, on a plain (http) service of its own, with the App
// installed on acme-org (1) and ada (2). It is that stand-in rather than this package's own
// narrowingGitHub because it serves the read a released token is spent at and its revoke route
// forgets the token it revoked, so a read made with a token afterwards is refused as any other
// credential of nobody's: that is how a check's promise to hold no token past GitHub's answer is
// read below, rather than from the revoke call alone.
func gitHubCheckStand(t *testing.T) (*airlocktest.Service, *airlocktest.GitHubStand) {
	t.Helper()
	stand := airlocktest.NewGitHubStand(airlocktest.GitHubOptions{Installations: map[string]int64{"acme-org": 1, "ada": 2}})
	service, err := airlocktest.NewService(airlocktest.ServiceOptions{Plain: true})
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
func reachesWith(t *testing.T, service *airlocktest.Service, token string) int {
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

// answered is what a check and the release it stands for are compared by: the error's text, and
// "" for no error.
func answered(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestAGitHubAppCheckOfANarrowingBeforeApprovalReleasesNoToken: secrets.MintRequest.Check is what
// the broker asks this minter before anybody is asked to approve a narrowed request, and it
// promises the release's verdict without the release. For each answer GitHub gives a narrowing
// that names repositories or read-only, it makes the calls the release makes, in the same order,
// and adds the revoke of every token GitHub answered with; it hands back no value at all, and
// the error it answers is the one the release answered. A token a check minted is dead by the
// time it returns: the stand-in forgets a token it revoked, so the read a released token is spent
// at is refused with it. A narrowing to its owner alone is checked from the installations
// listing alone: the check sends no token request, so GitHub holds no token with every
// permission of the installation for it, and it still refuses an account the App has no
// installation on. A check that names no narrowing is a programming error, refused before GitHub
// is asked anything.
func TestAGitHubAppCheckOfANarrowingBeforeApprovalReleasesNoToken(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	service, stand := gitHubCheckStand(t)
	app := secrets.GitHubApp{Base: service.URL(), HTTP: http.DefaultClient}
	value := githubAppValue(t, "123456", key)
	// grace is an account the github-owners tag lists and the App is not installed on.
	owners := []string{"acme-org", "ada", "grace"}
	lean := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"lean"}, ReadOnly: true}
	const (
		installations = "GET /app/installations"
		tokenRequest  = "POST /app/installations/2/access_tokens"
		revoke        = "DELETE /installation/token"
	)

	for _, tc := range []struct {
		name      string
		narrowing *secrets.GitHubNarrowing
		// answer is what the stand-in answers each token request with while this row runs.
		answer airlocktest.GitHubAnswer
		class  string // the class of the error the release answers, "" where it serves the narrowing
		// noInstallation is whether that error is the App with no installation on the account
		// (secrets.ErrMintNoInstallation), the one refusal of GitHub's the broker refuses a
		// request for besides the narrowing's own.
		noInstallation bool
		releaseCalls   []string // the calls the release makes to GitHub
		checkCalls     []string // the calls the check makes
	}{
		{
			name:         "a narrowing GitHub serves",
			narrowing:    lean,
			answer:       airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensOrdinary},
			releaseCalls: []string{installations, tokenRequest},
			checkCalls:   []string{installations, tokenRequest, revoke},
		},
		{
			name:      "a narrowing GitHub refuses",
			narrowing: lean,
			// The read-only half is asked for once more without actions, which this answer
			// refuses too, so neither run holds a token and neither revokes one.
			answer:       airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensRefused},
			class:        "narrowing",
			releaseCalls: []string{installations, tokenRequest, tokenRequest},
			checkCalls:   []string{installations, tokenRequest, tokenRequest},
		},
		{
			name:      "a token GitHub mints wider than the narrowing",
			narrowing: lean,
			// The release revokes the token it will not hand over, and the check revokes the
			// same token before it judges it, so the two make the same calls.
			answer:       airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensWider},
			class:        "upstream",
			releaseCalls: []string{installations, tokenRequest, revoke},
			checkCalls:   []string{installations, tokenRequest, revoke},
		},
		{
			name:      "a narrowing to an account alone, which the App is installed on",
			narrowing: &secrets.GitHubNarrowing{Owner: "ada"},
			// The release asks for the token with no body. The check asks for none: GitHub's
			// answer to such a request could refuse the App but never the narrowing.
			answer:       airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensOrdinary},
			releaseCalls: []string{installations, tokenRequest},
			checkCalls:   []string{installations},
		},
		{
			name:           "a narrowing to an account alone, which the App is not installed on",
			narrowing:      &secrets.GitHubNarrowing{Owner: "grace"},
			answer:         airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensOrdinary},
			class:          "refused",
			noInstallation: true,
			releaseCalls:   []string{installations},
			checkCalls:     []string{installations},
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

			release, releaseErr := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: owners, GitHubNarrowing: tc.narrowing})
			class := ""
			if releaseErr != nil {
				class = secrets.MintErrorClass(releaseErr)
			}
			if class != tc.class || errors.Is(releaseErr, secrets.ErrMintNoInstallation) != tc.noInstallation {
				t.Fatalf("the release answered %v (class %q), want class %q, the App with no installation %t", releaseErr, class, tc.class, tc.noInstallation)
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

			checked, checkErr := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: owners, GitHubNarrowing: tc.narrowing, Check: true})
			if answered(checkErr) != answered(releaseErr) || errors.Is(checkErr, secrets.ErrMintNoInstallation) != tc.noInstallation {
				t.Fatalf("the check answered %v, want the release's own answer, %v", checkErr, releaseErr)
			}
			if checked.Value != "" || !checked.Expires.IsZero() {
				t.Fatalf("the check handed back %q, expiring %v; want no value at all", checked.Value, checked.Expires)
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

	t.Run("a check that names no narrowing", func(t *testing.T) {
		seen := len(service.Requests())
		checked, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: owners, Check: true})
		if checked.Value != "" || secrets.MintErrorClass(err) != "refused" || !strings.Contains(err.Error(), "narrowing") {
			t.Fatalf("a check with no narrowing = %q, %v (class %s); want nothing released and class refused, naming the narrowing",
				checked.Value, err, secrets.MintErrorClass(err))
		}
		if made := service.Requests()[seen:]; len(made) != 0 {
			t.Fatalf("the check made %d calls to GitHub, want none: it is refused before any", len(made))
		}
	})
}
