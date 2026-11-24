// packages/envoy/internal/cipher/proxyrules/proxyrulestest/github_test.go
//
// The GitHub stand-in (github.go) does what it says it does, driven through the broker's own
// GitHub App minter. The other stand-ins' rows, and the service helpers both files use, are in
// airlocktest_test.go.
package proxyrulestest

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestTheGitHubStandAnswersTheInstallationsItWasGiven drives the broker's own GitHub App minter
// against the stand-in: the two routes answer what that minter needs, so a github-app secret
// released against this stand-in holds a token for each account the profile's scope names that
// it was given an installation on, and for no other.
func TestTheGitHubStandAnswersTheInstallationsItWasGiven(t *testing.T) {
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{Installations: map[string]int64{"acme-org": 7, "zed-org": 9}}).Answer)

	minted, err := secrets.GitHubApp{Base: service.URL()}.Mint(t.Context(), secrets.MintRequest{Value: gitHubAppValue(t), GitHub: acmeScope()})
	if err != nil {
		t.Fatalf("mint a github-app secret against the stand-in: %v", err)
	}
	var held struct {
		Installations map[string]struct {
			InstallationID int64  `json:"installation_id"`
			Token          string `json:"token"`
		} `json:"installations"`
	}
	if err := json.Unmarshal([]byte(minted.Value), &held); err != nil {
		t.Fatalf("the minted value is no JSON object of installations: %v", err)
	}
	if len(held.Installations) != 1 {
		t.Fatalf("the mint holds %d installations; want the one account its scope names", len(held.Installations))
	}
	if got := held.Installations["acme-org"]; got.InstallationID != 7 || got.Token != "ghs_standin_acme-org" {
		t.Errorf("acme-org's installation is %+v; want installation 7 and its own token", got)
	}
	if minted.Expires.IsZero() {
		t.Error("the mint has no expiry; want the expires_at the stand-in answered")
	}
	if seen := service.Requests(); len(seen) != 2 || seen[1].Path != "/app/installations/7/access_tokens" {
		t.Errorf("the stand-in was asked %v; want the installations and then acme-org's token", seen)
	}
}

// gitHubAppValue is a github-app secret's stored value for the stand-in: the App id the stand
// answers for, and a key of this row's own, which is what the minter signs its App JWT with.
func gitHubAppValue(t *testing.T) string {
	t.Helper()
	_, pemText := githubapptest.Key(t)
	value, err := json.Marshal(map[string]string{"app_id": "123456", "private_key": pemText})
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

// acmeScope is the profile's scope each GitHub row below mints for unless it says otherwise:
// contents at read on acme-org, with the metadata at read every GitHub profile names, on the
// repositories of acme-org these names name, or on every repository the installation reaches
// where it names none, released as the JSON of installations.
func acmeScope(names ...string) *secrets.GitHubScope {
	scope := &secrets.GitHubScope{
		Accounts:    []string{"acme-org"},
		Permissions: map[string]string{"contents": "read", "metadata": "read"}, Value: githubname.ValueInstallations,
	}
	for _, name := range names {
		scope.Repositories = append(scope.Repositories, "acme-org/"+name)
	}
	return scope
}

// mintedGitHubToken is the installation token a github-app release holds for owner.
func mintedGitHubToken(t *testing.T, value, owner string) string {
	t.Helper()
	var held struct {
		Installations map[string]struct {
			Token string `json:"token"`
		} `json:"installations"`
	}
	if err := json.Unmarshal([]byte(value), &held); err != nil {
		t.Fatalf("the minted value is no JSON object of installations: %v", err)
	}
	return held.Installations[owner].Token
}

// TestTheGitHubStandAnswersWhatATokenRequestAsked: the access_tokens answer is what the minter
// checks its request against (its heldBy), so the stand-in has to say what the body it was sent
// asked for - a selection of the repositories it named, and the permissions it named, with
// metadata at read beside them, which GitHub grants with every installation token whatever the
// body named - and "all" for a body that named no repositories at all. Against a stand-in that
// answered the token alone, the minter would refuse every profile's token, since it refuses a
// token that names no permissions.
func TestTheGitHubStandAnswersWhatATokenRequestAsked(t *testing.T) {
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{Installations: map[string]int64{"acme-org": 7}}).Answer)
	client := clientOf(service)

	for _, tc := range []struct {
		name, body, selection string
		repositories          []string
		permissions           map[string]string
	}{
		{
			name:         "a body naming repositories and permissions",
			body:         `{"repositories":["proofs","widgets"],"permissions":{"contents":"read","metadata":"read"}}`,
			selection:    "selected",
			repositories: []string{"proofs", "widgets"},
			permissions:  map[string]string{"contents": "read", "metadata": "read"},
		},
		{
			name:        "a body naming permissions but not metadata, which the token holds at read all the same",
			body:        `{"permissions":{"contents":"read"}}`,
			selection:   "all",
			permissions: map[string]string{"contents": "read", "metadata": "read"},
		},
		{
			name:        "a body naming neither, which GitHub answers with all the installation holds",
			selection:   "all",
			permissions: standInGitHubPermissions,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, service.URL()+"/app/installations/7/access_tokens", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			status, body := answerOf(t, client, request)
			if status != http.StatusCreated {
				t.Fatalf("the token request answered %d: %s; want 201", status, body)
			}
			var answer struct {
				Token               string `json:"token"`
				RepositorySelection string `json:"repository_selection"`
				Repositories        []struct {
					Name string `json:"name"`
				} `json:"repositories"`
				Permissions map[string]string `json:"permissions"`
			}
			if err := json.Unmarshal([]byte(body), &answer); err != nil {
				t.Fatalf("the answer is no installation token: %v", err)
			}
			names := make([]string, 0, len(answer.Repositories))
			for _, repository := range answer.Repositories {
				names = append(names, repository.Name)
			}
			if answer.RepositorySelection != tc.selection || !slices.Equal(names, tc.repositories) {
				t.Errorf("the answer is a %q selection of %v; want %q of %v", answer.RepositorySelection, names, tc.selection, tc.repositories)
			}
			if !maps.Equal(answer.Permissions, tc.permissions) {
				t.Errorf("the answer holds %v; want %v", answer.Permissions, tc.permissions)
			}
			if answer.Token == "" {
				t.Error("the answer carries no token")
			}
		})
	}
}

// TestTheGitHubStandHasTheMinterRefuseAScopeThatLeavesMetadataOut: GitHub grants metadata at read
// with every installation token, and the minter refuses a token holding a permission its request
// did not name (secrets' heldBy), so against the stand-in, as against GitHub, a scope that leaves
// metadata out releases nothing, and the same scope naming it is released. It is the local driver
// of the rule the profiles file holds every GitHub profile to (policy.ParseProfiles).
func TestTheGitHubStandHasTheMinterRefuseAScopeThatLeavesMetadataOut(t *testing.T) {
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{Installations: map[string]int64{"acme-org": 7}}).Answer)
	app := secrets.GitHubApp{Base: service.URL()}

	bare := acmeScope()
	bare.Permissions = map[string]string{"contents": "read"}
	minted, err := app.Mint(t.Context(), secrets.MintRequest{Value: gitHubAppValue(t), GitHub: bare})
	if err == nil || minted.Value != "" || secrets.MintErrorClass(err) != "upstream" || !strings.Contains(err.Error(), "did not name") {
		t.Fatalf("mint of a scope leaving metadata out = %q, %v (class %s); want nothing released, class upstream, naming the permission the request did not name", minted.Value, err, secrets.MintErrorClass(err))
	}
	if _, err := app.Mint(t.Context(), secrets.MintRequest{Value: gitHubAppValue(t), GitHub: acmeScope()}); err != nil {
		t.Fatalf("mint of the same scope naming metadata at read: %v", err)
	}
}

// TestTheGitHubStandHoldsThePermissionsItWasGiven: what an installation holds is what the minter
// cuts a profile's permissions to (secrets' cut), so the installations listing names each owner's
// own map of GitHubOptions.Permissions, and an owner with no entry holds standInGitHubPermissions.
// The token route answers 422, as GitHub does, to a request naming a permission at a level past
// what that owner's map holds, to one naming a permission the map does not name at all, and to
// workflows at read, a level GitHub never gives it; it mints a request within the map, and each
// owner's own map decides that owner's requests. Driven through the broker's own minter, a profile
// asking past the map is cut to it, and the release names what the cut left out.
func TestTheGitHubStandHoldsThePermissionsItWasGiven(t *testing.T) {
	acme := map[string]string{"contents": "read", "metadata": "read", "workflows": "write"}
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{
		Installations: map[string]int64{"acme-org": 7, "zed-org": 9},
		Permissions:   map[string]map[string]string{"acme-org": acme},
	}).Answer)
	client := clientOf(service)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, service.URL()+"/app/installations", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, body := answerOf(t, client, request)
	var listed []struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		Permissions map[string]string `json:"permissions"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &listed) != nil {
		t.Fatalf("the installations listing answered %d: %s; want 200 and a list of installations", status, body)
	}
	held := map[string]map[string]string{}
	for _, installation := range listed {
		held[installation.Account.Login] = installation.Permissions
	}
	if len(held) != 2 || !maps.Equal(held["acme-org"], acme) || !maps.Equal(held["zed-org"], standInGitHubPermissions) {
		t.Fatalf("the listing says the installations hold %v; want acme-org's own map, and zed-org the stand-in's default", held)
	}

	for _, tc := range []struct {
		name, installation, body string
		want                     int
	}{
		{"a permission within acme-org's map", "7", `{"permissions":{"contents":"read"}}`, http.StatusCreated},
		{"workflows at the write acme-org holds it at", "7", `{"permissions":{"workflows":"write","metadata":"read"}}`, http.StatusCreated},
		{"a permission at a level past acme-org's map", "7", `{"permissions":{"contents":"write"}}`, http.StatusUnprocessableEntity},
		{"a permission acme-org's map does not name", "7", `{"permissions":{"pull_requests":"read"}}`, http.StatusUnprocessableEntity},
		{"workflows at read, a level GitHub never gives it", "7", `{"permissions":{"workflows":"read"}}`, http.StatusUnprocessableEntity},
		{"that same level past acme-org's map, for zed-org, which holds the default", "9", `{"permissions":{"contents":"write","pull_requests":"write"}}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, service.URL()+"/app/installations/"+tc.installation+"/access_tokens", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if status, body := answerOf(t, client, request); status != tc.want {
				t.Fatalf("the token request answered %d: %s; want %d", status, body, tc.want)
			}
		})
	}

	t.Run("the broker's minter, for a profile asking past acme-org's map", func(t *testing.T) {
		scope := acmeScope()
		scope.Permissions = map[string]string{"contents": "write", "issues": "read", "metadata": "read", "workflows": "write"}
		minted, err := secrets.GitHubApp{Base: service.URL()}.Mint(t.Context(), secrets.MintRequest{Value: gitHubAppValue(t), GitHub: scope})
		if err != nil {
			t.Fatalf("mint a profile asking past acme-org's map: %v", err)
		}
		if left := minted.LeftOut["acme-org"]; len(minted.LeftOut) != 1 || !slices.Equal(left, []string{"contents:write", "issues"}) {
			t.Fatalf("the release says the cut left out %v; want contents at write and issues, of acme-org alone", minted.LeftOut)
		}
		asked := service.Requests()
		last := asked[len(asked)-1]
		var sent struct {
			Permissions map[string]string `json:"permissions"`
		}
		if last.Path != "/app/installations/7/access_tokens" || json.Unmarshal([]byte(last.Body), &sent) != nil ||
			!maps.Equal(sent.Permissions, map[string]string{"contents": "read", "metadata": "read", "workflows": "write"}) {
			t.Fatalf("the stand-in was last asked %s %s with %s; want acme-org's token asking for contents at read, metadata at read and workflows at write", last.Method, last.Path, last.Body)
		}
	})
}

// TestTheGitHubStandReleasesAProfilesTokenAndRefusesAWiderOne drives the broker's own minter at
// the stand-in, which is the assertion the rows above rest on: a profile's scope the stand-in
// answers as GitHub does is released, so every check of this stack has a token to spend, of named
// repositories or of every one; and the token the widening option answers is refused with
// nothing released, which is the path a check cannot otherwise drive, since GitHub itself never
// widens.
func TestTheGitHubStandReleasesAProfilesTokenAndRefusesAWiderOne(t *testing.T) {
	value := gitHubAppValue(t)
	for _, tc := range []struct {
		name   string
		answer GitHubTokenAnswer
		scope  *secrets.GitHubScope
		class  string
	}{
		{name: "the repositories the scope named", scope: acmeScope("proofs")},
		{name: "the permissions the profile named, on every repository", scope: acmeScope()},
		{name: "every repository, for a scope that named one", answer: GitHubTokensWider, class: "upstream", scope: acmeScope("proofs")},
		{name: "a grant the profile did not name, for a scope of every repository", answer: GitHubTokensWider, class: "upstream", scope: acmeScope()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newService(t, ServiceOptions{Plain: true})
			service.AnswerWith(NewGitHubStand(GitHubOptions{
				Installations: map[string]int64{"acme-org": 7}, Answer: GitHubAnswer{Mode: tc.answer},
			}).Answer)
			minted, err := secrets.GitHubApp{Base: service.URL()}.Mint(t.Context(), secrets.MintRequest{Value: value, GitHub: tc.scope})
			if tc.class == "" {
				if err != nil {
					t.Fatalf("mint a profile's token against the stand-in: %v", err)
				}
				if token := mintedGitHubToken(t, minted.Value, "acme-org"); token == "" {
					t.Error("the release holds no token for acme-org")
				}
				return
			}
			if err == nil || secrets.MintErrorClass(err) != tc.class || minted.Value != "" {
				t.Fatalf("mint against the widened answer = %q, %v (class %s); want nothing released and class %s",
					minted.Value, err, secrets.MintErrorClass(err), tc.class)
			}
		})
	}
}

// TestTheGitHubStandRefusesEveryTokenWhenItIsAskedTo: the refusing option answers the 422 GitHub
// answers a body it will not mint for, and the minter must read that as the caller's scope
// rather than as the upstream's trouble, so no profile's token driven against it is released.
// The refusal names what the scope likely got wrong: its repositories where it names some, and
// else the permissions it asked for.
func TestTheGitHubStandRefusesEveryTokenWhenItIsAskedTo(t *testing.T) {
	value := gitHubAppValue(t)
	for _, tc := range []struct {
		name  string
		scope *secrets.GitHubScope
		names string
	}{
		{"a scope that named repositories", acmeScope("proofs"), "proofs"},
		{"a scope of every repository", acmeScope(), "contents: read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newService(t, ServiceOptions{Plain: true})
			service.AnswerWith(NewGitHubStand(GitHubOptions{
				Installations: map[string]int64{"acme-org": 7}, Answer: GitHubAnswer{Mode: GitHubTokensRefused},
			}).Answer)
			minted, err := secrets.GitHubApp{Base: service.URL()}.Mint(t.Context(), secrets.MintRequest{Value: value, GitHub: tc.scope})
			if err == nil {
				t.Fatalf("mint against the refusing stand-in released %q; want a refusal", minted.Value)
			}
			if minted.Value != "" || secrets.MintErrorClass(err) != "narrowing" ||
				!strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("mint = %q, %v (class %s); want nothing released and class narrowing naming the 422 and %s",
					minted.Value, err, secrets.MintErrorClass(err), tc.names)
			}
		})
	}
}

// TestTheGitHubStandRefusesOnePartOfATokenRequest: the two answers that refuse one part of a
// request and mint every other. The repository answer refuses the scope naming its repository,
// mints the one naming another, and mints the request that names no repository at all - the
// probe that finds the repositories to be the refused part, which the broker's check makes by
// asking again with fewer repositories (requests' narrowingCause). The permissions answer refuses
// a request naming permissions, which every profile's token request does (secrets' cut), so it
// refuses every scope the minter mints, of named repositories or of every one; and it mints a
// request naming none, which only a caller other than the minter sends.
func TestTheGitHubStandRefusesOnePartOfATokenRequest(t *testing.T) {
	value := gitHubAppValue(t)
	refusedRepository := GitHubAnswer{Mode: GitHubTokensRefusedRepository, Repository: "proofs"}
	refusedPermissions := GitHubAnswer{Mode: GitHubTokensRefusedPermissions}
	for _, tc := range []struct {
		name    string
		answer  GitHubAnswer
		scope   *secrets.GitHubScope
		refused bool
	}{
		{name: "the repository the answer names", answer: refusedRepository, refused: true, scope: acmeScope("proofs")},
		{name: "another repository, under that same answer", answer: refusedRepository, scope: acmeScope("ledger")},
		{name: "a request naming no repository, under that same answer", answer: refusedRepository, scope: acmeScope()},
		{name: "the permissions a scope of every repository names", answer: refusedPermissions, refused: true, scope: acmeScope()},
		{name: "the permissions a scope of named repositories names too", answer: refusedPermissions, refused: true, scope: acmeScope("proofs")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newService(t, ServiceOptions{Plain: true})
			service.AnswerWith(NewGitHubStand(GitHubOptions{
				Installations: map[string]int64{"acme-org": 7}, Answer: tc.answer,
			}).Answer)
			minted, err := secrets.GitHubApp{Base: service.URL()}.Mint(t.Context(), secrets.MintRequest{Value: value, GitHub: tc.scope})
			if !tc.refused {
				if err != nil {
					t.Fatalf("mint the scope the answer leaves alone: %v", err)
				}
				if token := mintedGitHubToken(t, minted.Value, "acme-org"); token == "" {
					t.Error("the release holds no token for acme-org")
				}
				return
			}
			if minted.Value != "" || secrets.MintErrorClass(err) != "narrowing" ||
				err == nil || !strings.Contains(err.Error(), "422") {
				t.Fatalf("mint against the %s = %q, %v (class %s); want nothing released and class narrowing naming the 422",
					tc.name, minted.Value, err, secrets.MintErrorClass(err))
			}
		})
	}

	t.Run("a request naming repositories alone, under the permissions answer", func(t *testing.T) {
		service := newService(t, ServiceOptions{Plain: true})
		service.AnswerWith(NewGitHubStand(GitHubOptions{
			Installations: map[string]int64{"acme-org": 7}, Answer: refusedPermissions,
		}).Answer)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, service.URL()+"/app/installations/7/access_tokens", strings.NewReader(`{"repositories":["proofs"]}`))
		if err != nil {
			t.Fatal(err)
		}
		if status, body := answerOf(t, clientOf(service), request); status != http.StatusCreated {
			t.Fatalf("a token request naming no permissions answered %d: %s; want 201, the part the answer leaves alone", status, body)
		}
	})
}

// TestTheGitHubStandsAnswerIsSetWhileItRuns: the answers one after another against one
// running stand, which is what a check of a profile drives on one stack - the ordinary mint,
// the 422 that releases nothing, the token wider than asked that the minter revokes, and the
// ordinary mint again as that check's negative control - and the one outage, which a minter
// that makes one attempt fails at as the upstream's and a minter with a Retry mints through at
// its second attempt, after which the stand answers as ordinary again with no call to Answers.
// A stand built with no option starts on the ordinary answer.
func TestTheGitHubStandsAnswerIsSetWhileItRuns(t *testing.T) {
	service := newService(t, ServiceOptions{Plain: true})
	stand := NewGitHubStand(GitHubOptions{Installations: map[string]int64{"acme-org": 7}})
	service.AnswerWith(stand.Answer)
	value := gitHubAppValue(t)
	scope := acmeScope("proofs")
	// retry waits no real time: the row is about what the stand answers, not the backoff.
	retry := &secrets.Retry{Sleep: func(context.Context, time.Duration) error { return nil }}
	mint := func(retried bool) (secrets.Minted, error) {
		app := secrets.GitHubApp{Base: service.URL()}
		if retried {
			app.Retry = retry
		}
		return app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHub: scope})
	}

	for _, tc := range []struct {
		name    string
		answer  GitHubTokenAnswer
		retried bool
		class   string
		// tokenCalls is how many token requests the row makes, when it is not one.
		tokenCalls int
	}{
		{name: "the answer it starts on"},
		{name: "the refusal", answer: GitHubTokensRefused, class: "narrowing"},
		{name: "the token wider than asked", answer: GitHubTokensWider, class: "upstream"},
		{name: "the ordinary answer again", answer: GitHubTokensOrdinary},
		{name: "the outage, to a minter that makes one attempt", answer: GitHubTokensFailOnce, class: "upstream"},
		{name: "the answer after the outage"},
		{name: "the outage, to a minter that retries", answer: GitHubTokensFailOnce, retried: true, tokenCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.answer != "" {
				stand.Answers(GitHubAnswer{Mode: tc.answer})
			}
			before := len(service.Requests())
			minted, err := mint(tc.retried)
			if want, asked := max(tc.tokenCalls, 1), tokenRequests(service.Requests()[before:]); asked != want {
				t.Errorf("the mint against the %s made %d token requests; want %d", tc.name, asked, want)
			}
			if tc.class == "" {
				if err != nil {
					t.Fatalf("mint against the %s: %v", tc.name, err)
				}
				if token := mintedGitHubToken(t, minted.Value, "acme-org"); token == "" {
					t.Error("the release holds no token for acme-org")
				}
				return
			}
			if minted.Value != "" || secrets.MintErrorClass(err) != tc.class {
				t.Fatalf("mint against the %s = %q, %v (class %s); want nothing released and class %s",
					tc.name, minted.Value, err, secrets.MintErrorClass(err), tc.class)
			}
		})
	}
}

// tokenRequests is how many of seen are access_tokens requests, the call the stand's outage
// answers.
func tokenRequests(seen []Request) int {
	count := 0
	for _, request := range seen {
		if request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/access_tokens") {
			count++
		}
	}
	return count
}

// TestTheGitHubStandAnswersARepositoryOnlyToATokenThatReachesIt: a check spends a token of named
// repositories at GitHub, so the stand-in has to answer the repository that token reaches and
// refuse every other one - another repository of the same account, and a repository of another
// account - and refuse a credential it never minted at all. A token of a scope that named no
// repository reaches every repository of its own account and none of anyone else's, which is
// the other scope it mints.
func TestTheGitHubStandAnswersARepositoryOnlyToATokenThatReachesIt(t *testing.T) {
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{Installations: map[string]int64{"acme-org": 7, "zed-org": 9}}).Answer)
	app := secrets.GitHubApp{Base: service.URL()}
	value := gitHubAppValue(t)

	named, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHub: acmeScope("proofs")})
	if err != nil {
		t.Fatalf("mint a token of one repository: %v", err)
	}
	// The record a check reads the scope back from: the token request named that one repository
	// and nothing else.
	asked := service.Requests()
	if last := asked[len(asked)-1]; last.Path != "/app/installations/7/access_tokens" || !strings.Contains(last.Body, `"repositories":["proofs"]`) {
		t.Fatalf("the stand-in was last asked %s %s with %s; want the token request for proofs alone", last.Method, last.Path, last.Body)
	}
	wide, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHub: acmeScope()})
	if err != nil {
		t.Fatalf("mint a token of every repository: %v", err)
	}
	namedToken := mintedGitHubToken(t, named.Value, "acme-org")
	wideToken := mintedGitHubToken(t, wide.Value, "acme-org")

	client := clientOf(service)
	for _, tc := range []struct {
		name, path, credential, full string
		want                         int
	}{
		{"the repository the scope names", "/repos/acme-org/proofs", "Bearer " + namedToken, "acme-org/proofs", http.StatusOK},
		{"that repository under GitHub's other scheme", "/repos/acme-org/proofs", "token " + namedToken, "acme-org/proofs", http.StatusOK},
		{"another repository of the same account", "/repos/acme-org/widgets", "Bearer " + namedToken, "", http.StatusNotFound},
		{"a repository of another account", "/repos/zed-org/proofs", "Bearer " + namedToken, "", http.StatusNotFound},
		{"a credential this stand-in never minted", "/repos/acme-org/proofs", "Bea" + "rer ghs_no_token_of_this_runs", "", http.StatusUnauthorized},
		{"any repository of the account, for a token that named none", "/repos/acme-org/widgets", "Bearer " + wideToken, "acme-org/widgets", http.StatusOK},
		{"another account's repository, for that same token", "/repos/zed-org/proofs", "Bearer " + wideToken, "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, service.URL()+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", tc.credential)
			status, body := answerOf(t, client, request)
			if status != tc.want {
				t.Fatalf("the read of %s answered %d: %s; want %d", tc.path, status, body, tc.want)
			}
			if tc.full != "" && !strings.Contains(body, `"full_name":"`+tc.full+`"`) {
				t.Errorf("the repository answered %s; want the one named %s", body, tc.full)
			}
		})
	}
}

// TestTheGitHubStandRevokesATokenItMinted: a caller gives a token back as the token itself, which
// is what the broker's minter does with a token it will not release, so the stand-in has to
// answer that route as GitHub does: 204 with no body for a token it minted, which reaches
// nothing afterwards, and 401 for a credential it never minted, as its repository route answers
// one.
func TestTheGitHubStandRevokesATokenItMinted(t *testing.T) {
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{Installations: map[string]int64{"acme-org": 7}}).Answer)
	minted, err := secrets.GitHubApp{Base: service.URL()}.Mint(t.Context(), secrets.MintRequest{
		Name: "legion-github-app", Value: gitHubAppValue(t), GitHub: acmeScope(),
	})
	if err != nil {
		t.Fatalf("mint a github-app secret against the stand-in: %v", err)
	}
	token := mintedGitHubToken(t, minted.Value, "acme-org")
	client := clientOf(service)
	// ask is one call to the stand-in with credential, which is the whole of what the two routes
	// below decide on.
	ask := func(method, path, credential string) (int, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, service.URL()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+credential)
		return answerOf(t, client, request)
	}

	if status, body := ask(http.MethodGet, "/repos/acme-org/proofs", token); status != http.StatusOK {
		t.Fatalf("the read with the minted token answered %d: %s; want 200 before the revoke", status, body)
	}
	if status, body := ask(http.MethodDelete, "/installation/token", token); status != http.StatusNoContent || strings.TrimSpace(body) != "" {
		t.Fatalf("the revoke answered %d: %q; want 204 and no body", status, body)
	}
	if status, body := ask(http.MethodGet, "/repos/acme-org/proofs", token); status != http.StatusUnauthorized {
		t.Fatalf("the read with the revoked token answered %d: %s; want 401, as a credential this stand-in never minted reads", status, body)
	}
	if status, body := ask(http.MethodDelete, "/installation/token", "ghs_no_token_of_this_runs"); status != http.StatusUnauthorized {
		t.Fatalf("the revoke of a credential this stand-in never minted answered %d: %s; want 401", status, body)
	}
}

// TestTheGitHubStandAnswersTheMintersOwnRevoke: the widening option is the one path that makes
// the broker's minter revoke a token, so it is what proves the revoke route serves that minter.
// The release is still refused with nothing released, the revoke is the last call the stand-in
// saw, and the run logs no revoke failure, which a route the stand-in did not serve would leave
// behind.
func TestTheGitHubStandAnswersTheMintersOwnRevoke(t *testing.T) {
	var logged logtest.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{
		Installations: map[string]int64{"acme-org": 7}, Answer: GitHubAnswer{Mode: GitHubTokensWider},
	}).Answer)

	minted, err := secrets.GitHubApp{Base: service.URL()}.Mint(t.Context(), secrets.MintRequest{
		Name: "legion-github-app", Value: gitHubAppValue(t), GitHub: acmeScope("proofs"),
	})
	if minted.Value != "" || secrets.MintErrorClass(err) != "upstream" {
		t.Fatalf("mint against the widened answer = %q, %v (class %s); want nothing released and class upstream",
			minted.Value, err, secrets.MintErrorClass(err))
	}
	seen := service.Requests()
	if last := seen[len(seen)-1]; last.Method != http.MethodDelete || last.Path != "/installation/token" {
		t.Fatalf("the stand-in was last asked %s %s; want the revoke of the token the minter refused", last.Method, last.Path)
	}
	if line := logged.Lines("revoke"); line != "" {
		t.Errorf("the run logged %q; want no revoke failure, since the stand-in answered the minter's revoke", line)
	}
}

// TestTheGitHubStandsRoutesAnswerByWhatATokenHolds: the three routes a GitHub profile's reach is
// read at answer by the permissions the token holds, as GitHub does: a deploy-key list needs
// administration, an organization's installations need organization_administration, and a
// commit status needs statuses at write, with 422 for a commit other than GitHubCommitSHA, the one
// every repository holds. A token of another account, or of other repositories, finds nothing
// there; a credential the stand never minted, and one the minter gave back, are refused as GitHub
// refuses bad credentials. What a token holds is what the token route answered it holds.
func TestTheGitHubStandsRoutesAnswerByWhatATokenHolds(t *testing.T) {
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{
		Installations: map[string]int64{"acme-org": 7, "ada": 8},
		Permissions: map[string]map[string]string{
			"acme-org": {"metadata": "read", "administration": "read", "organization_administration": "read", "statuses": "write"},
		},
		DistinctTokens: true,
	}).Answer)
	client := clientOf(service)
	// call is one request of the stand-in with token as its bearer, none for "", made for t.
	call := func(t *testing.T, method, path, token, body string) (int, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, service.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		return answerOf(t, client, request)
	}
	mint := func(asked string) string {
		t.Helper()
		status, body := call(t, http.MethodPost, "/app/installations/7/access_tokens", "", asked)
		var answer struct {
			Token string `json:"token"`
		}
		if status != http.StatusCreated || json.Unmarshal([]byte(body), &answer) != nil || answer.Token == "" {
			t.Fatalf("the token request %s answered %d %s; want 201 and a token", asked, status, body)
		}
		return answer.Token
	}
	administrator := mint(`{"repositories":["proofs"],"permissions":{"administration":"read"}}`)
	reader := mint(`{"repositories":["proofs"],"permissions":{"metadata":"read"}}`)
	organization := mint(`{"permissions":{"organization_administration":"read"}}`)
	statusWriter := mint(`{"repositories":["proofs"],"permissions":{"statuses":"write"}}`)
	statusReader := mint(`{"repositories":["proofs"],"permissions":{"statuses":"read"}}`)
	givenBack := mint(`{"repositories":["proofs"],"permissions":{"administration":"read"}}`)
	if status, body := call(t, http.MethodDelete, "/installation/token", givenBack, ""); status != http.StatusNoContent {
		t.Fatalf("the revoke answered %d %s; want 204", status, body)
	}

	const state = `{"state":"success","context":"stand-in"}`
	commit := "/repos/acme-org/proofs/statuses/" + GitHubCommitSHA
	for _, tc := range []struct {
		name, token, method, path, body string
		want                            int
	}{
		{"a deploy-key list, to a token holding administration", administrator, http.MethodGet, "/repos/acme-org/proofs/keys", "", http.StatusOK},
		{"a deploy-key list, to a token without it", reader, http.MethodGet, "/repos/acme-org/proofs/keys", "", http.StatusForbidden},
		{"a deploy-key list of a repository outside the token", administrator, http.MethodGet, "/repos/acme-org/widgets/keys", "", http.StatusNotFound},
		{"a deploy-key list of another account's repository", administrator, http.MethodGet, "/repos/ada/proofs/keys", "", http.StatusNotFound},
		{"an organization's installations, to a token holding organization_administration", organization, http.MethodGet, "/orgs/acme-org/installations", "", http.StatusOK},
		{"an organization's installations, to a token without it", administrator, http.MethodGet, "/orgs/acme-org/installations", "", http.StatusForbidden},
		{"another organization's installations", organization, http.MethodGet, "/orgs/ada/installations", "", http.StatusNotFound},
		{"a commit status, to a token holding statuses at write", statusWriter, http.MethodPost, commit, state, http.StatusCreated},
		{"a commit status for a commit the repository does not hold", statusWriter, http.MethodPost, "/repos/acme-org/proofs/statuses/" + strings.Repeat("0", 40), state, http.StatusUnprocessableEntity},
		{"a commit status, to a token holding statuses at read", statusReader, http.MethodPost, commit, state, http.StatusForbidden},
		{"a credential the stand never minted", "ghs_minted_by_nobody", http.MethodGet, "/repos/acme-org/proofs/keys", "", http.StatusUnauthorized},
		{"a token the minter gave back", givenBack, http.MethodGet, "/repos/acme-org/proofs/keys", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := call(t, tc.method, tc.path, tc.token, tc.body); status != tc.want {
				t.Errorf("%s %s answered %d %s; want %d", tc.method, tc.path, status, body, tc.want)
			}
		})
	}
}
