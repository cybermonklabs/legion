package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

func testKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, string(encoded)
}

// fakeGitHub is an httptest GitHub App API: one installation per seeded
// repository, token minting, the repository read the minted token proves, and
// the commit/tree/blob reads the architecture importer walks.
type fakeGitHub struct {
	t *testing.T
	// installations maps "owner/repo" to its Contents permission; a missing
	// repo answers 404.
	installations  map[string]string
	installationID int64
	tokenMints     int
	tokenExpiresAt time.Time
	publicKey      *rsa.PublicKey
	clientID       string
	// commits maps a branch name to its commit sha; a missing branch answers missingBranchStatus.
	// The commits route answers as GitHub does: the bare sha for Accept:
	// application/vnd.github.sha, the commit's JSON otherwise.
	commits map[string]string
	// missingBranchStatus is the status a branch absent from commits answers: 422 is GitHub's
	// answer for a branch that does not exist, with GitHub's body; anything else (0 included) is
	// 404 with no body.
	missingBranchStatus int
	// largeCommits names the branches whose JSON answer carries a file list that pushes it past
	// 1 MiB, as a large commit's does on GitHub.
	largeCommits map[string]bool
	// ignoresSHAMediaType answers the commits route's JSON whatever Accept asks for.
	ignoresSHAMediaType bool
	// files maps a repository path to its content; a subtree read lists the
	// paths directly inside the requested directory and each blob is served
	// under sha "blob-<path>".
	files map[string]string
	// modes overrides an entry's mode (default 100644): 120000 is a symlink.
	modes map[string]string
	// sizes overrides an entry's listed size (default the content length).
	sizes map[string]int64
	// repoEntries pads the whole-repository tree (a read without ":dir") with
	// this many entries outside the architecture directory.
	repoEntries int
	// truncated makes the tree read answer truncated=true.
	truncated bool
	// missingBlobs answer 404 by path, simulating a blob vanishing mid-fetch.
	missingBlobs map[string]bool
	// treeReads counts tree reads, by "sha:dir".
	treeReads []string
	// graphQLResponse is served verbatim (as JSON) to every POST /graphql request.
	graphQLResponse any
	// lastGraphQLQuery/lastGraphQLVariables record the most recent GraphQL request's body, for a
	// test to assert against.
	lastGraphQLQuery     string
	lastGraphQLVariables map[string]any
	// actionsPermissions/pullRequestsPermissions map "owner/repo" to that permission, for
	// DeliveryPermissions' tests; a repo absent from one of these maps gets "" (no permission).
	actionsPermissions      map[string]string
	pullRequestsPermissions map[string]string
	// installationStatuses is the Commit statuses permission every installation lookup answers,
	// and tokenPermissions the permissions every token mint answers: GitHub answers both, and the
	// two can differ while a token minted before a grant changed lives.
	installationStatuses string
	tokenPermissions     map[string]string
	// appInstallations, when non-nil, backs GET /app/installations, paginated at 100 per page; a
	// nil value answers an empty list. appInstallationsCalls counts every call made, and
	// appInstallationsRateLimitOnCall (1-indexed, 0 means never) makes that numbered call answer a
	// 403 secondary rate limit instead of a real page.
	appInstallations                []map[string]any
	appInstallationsCalls           int
	appInstallationsRateLimitOnCall int
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.verifyAppJWT(r)
		name := r.PathValue("owner") + "/" + r.PathValue("repo")
		contents, ok := f.installations[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		permissions := map[string]string{}
		if contents != "" {
			permissions["contents"] = contents
		}
		if actions := f.actionsPermissions[name]; actions != "" {
			permissions["actions"] = actions
		}
		if pullRequests := f.pullRequestsPermissions[name]; pullRequests != "" {
			permissions["pull_requests"] = pullRequests
		}
		if f.installationStatuses != "" {
			permissions["statuses"] = f.installationStatuses
		}
		response := map[string]any{
			"id":          f.installationID,
			"app_slug":    "dispatch-test",
			"permissions": permissions,
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			f.t.Errorf("encode installation: %v", err)
		}
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.verifyAppJWT(r)
		f.tokenMints++
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"token":       fmt.Sprintf("ghs_fake_%d_%d", f.installationID, f.tokenMints),
			"expires_at":  f.tokenExpiresAt.Format(time.RFC3339),
			"permissions": f.tokenPermissions,
		}); err != nil {
			f.t.Errorf("encode token: %v", err)
		}
	})
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		f.verifyAppJWT(r)
		f.appInstallationsCalls++
		if f.appInstallationsRateLimitOnCall != 0 && f.appInstallationsCalls == f.appInstallationsRateLimitOnCall {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		const perPage = 100
		start, end := (page-1)*perPage, page*perPage
		if start > len(f.appInstallations) {
			start = len(f.appInstallations)
		}
		if end > len(f.appInstallations) {
			end = len(f.appInstallations)
		}
		if err := json.NewEncoder(w).Encode(f.appInstallations[start:end]); err != nil {
			f.t.Errorf("encode installations: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_fake_") {
			f.t.Errorf("repository read used %q, want an installation token", r.Header.Get("Authorization"))
		}
		if _, ok := f.installations[r.PathValue("owner")+"/"+r.PathValue("repo")]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"full_name":%q}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	requireInstallationToken := func(r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_fake_") {
			f.t.Errorf("%s used %q, want an installation token", r.URL.Path, r.Header.Get("Authorization"))
		}
	}
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		requireInstallationToken(r)
		branch := r.PathValue("branch")
		sha, ok := f.commits[branch]
		if !ok {
			if f.missingBranchStatus == http.StatusUnprocessableEntity {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprintf(w, `{"message":"No commit found for SHA: %s","documentation_url":"https://docs.github.com/rest/commits/commits#get-a-commit","status":"422"}`, branch)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Accept") == "application/vnd.github.sha" && !f.ignoresSHAMediaType {
			w.Header().Set("Content-Type", "application/vnd.github.sha")
			fmt.Fprint(w, sha)
			return
		}
		commit := map[string]any{"sha": sha}
		if f.largeCommits[branch] {
			// 300 files of ~4 KiB patch each: about 1.2 MiB of JSON.
			patch := strings.Repeat("+a line this commit adds to the file, as its diff shows it\n", 70)
			files := make([]map[string]any, 300)
			for i := range files {
				files[i] = map[string]any{"filename": fmt.Sprintf("file-%03d.txt", i), "status": "added", "patch": patch}
			}
			commit["files"] = files
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(commit); err != nil {
			f.t.Errorf("encode commit: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		requireInstallationToken(r)
		if r.URL.RawQuery != "" {
			f.t.Errorf("tree read carries a query: %s", r.URL.RawQuery)
		}
		ref := r.PathValue("ref")
		f.treeReads = append(f.treeReads, ref)
		sha, dir, ok := strings.Cut(ref, ":")
		if !ok {
			// The whole-repository tree, GitHub-shaped (~258 bytes an entry):
			// repoEntries of padding plus every seeded file. An importer never
			// asks for it; the size is what a recursive read would have paid.
			fmt.Fprintf(w, `{"sha":%q,"truncated":%t,"tree":[`, "tree-"+sha, f.truncated)
			for i := range f.repoEntries {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				blob := fmt.Sprintf("%040d", i)
				fmt.Fprintf(w, `{"path":"packages/dispatch/web/src/features/issue/file-%d.tsx","mode":"100644","type":"blob","sha":%q,"size":%d,"url":"https://api.github.com/repos/legion/arch/git/blobs/%s"}`, i, blob, 7*i, blob)
			}
			for path := range f.files {
				fmt.Fprintf(w, `,{"path":%q,"mode":"100644","type":"blob","sha":%q,"size":%d}`, path, "blob-"+path, len(f.files[path]))
			}
			fmt.Fprint(w, `]}`)
			return
		}
		entries := []map[string]any{}
		prefix := dir + "/"
		names := []string{}
		for path := range f.files {
			if strings.HasPrefix(path, prefix) {
				names = append(names, strings.TrimPrefix(path, prefix))
			}
		}
		if len(names) == 0 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		sort.Strings(names)
		digest := sha256.New()
		seenDirs := map[string]bool{}
		for _, name := range names {
			path := prefix + name
			if first, _, nested := strings.Cut(name, "/"); nested {
				// A nested path shows up as its top-level directory entry once.
				if !seenDirs[first] {
					seenDirs[first] = true
					entries = append(entries, map[string]any{
						"path": first, "mode": "040000", "type": "tree", "sha": "tree-" + prefix + first,
					})
				}
				continue
			}
			mode := f.modes[path]
			if mode == "" {
				mode = "100644"
			}
			size, ok := f.sizes[path]
			if !ok {
				size = int64(len(f.files[path]))
			}
			fmt.Fprintf(digest, "%s %s %s\n", mode, name, f.files[path])
			entries = append(entries, map[string]any{
				"path": name, "mode": mode, "type": "blob", "sha": "blob-" + path, "size": size,
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"sha": "tree-" + sha + "-" + fmt.Sprintf("%x", digest.Sum(nil))[:12], "truncated": f.truncated, "tree": entries,
		}); err != nil {
			f.t.Errorf("encode tree: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		requireInstallationToken(r)
		path := strings.TrimPrefix(r.PathValue("sha"), "blob-")
		content, ok := f.files[path]
		if !ok || f.missingBlobs[path] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// GitHub wraps blob base64 at 60 columns.
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		var wrapped strings.Builder
		for len(encoded) > 60 {
			wrapped.WriteString(encoded[:60] + "\n")
			encoded = encoded[60:]
		}
		wrapped.WriteString(encoded + "\n")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"content": wrapped.String(), "encoding": "base64", "size": len(content),
		}); err != nil {
			f.t.Errorf("encode blob: %v", err)
		}
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		requireInstallationToken(r)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			f.t.Errorf("GraphQL request Content-Type = %q, want application/json", ct)
		}
		var decoded struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			f.t.Errorf("decode GraphQL request body: %v", err)
		}
		f.lastGraphQLQuery = decoded.Query
		f.lastGraphQLVariables = decoded.Variables
		if err := json.NewEncoder(w).Encode(f.graphQLResponse); err != nil {
			f.t.Errorf("encode GraphQL response: %v", err)
		}
	})
	return mux
}

// verifyAppJWT checks the Authorization JWT verifies against the fixture
// public key and carries the claims GitHub requires.
func (f *fakeGitHub) verifyAppJWT(r *http.Request) {
	f.t.Helper()
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		f.t.Errorf("app endpoint called with %s, want a bearer JWT",
			githubapptest.DescribeAuthorization(r.Header.Get("Authorization")))
		return
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		f.t.Errorf("JWT has %d segments, want 3", len(parts))
		return
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		f.t.Errorf("decode JWT header: %v", err)
		return
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "RS256" || header.Typ != "JWT" {
		f.t.Errorf("JWT header %s, want RS256/JWT", headerJSON)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.t.Errorf("decode JWT signature: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		f.t.Errorf("JWT signature does not verify against the fixture key: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		f.t.Errorf("decode JWT claims: %v", err)
		return
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		f.t.Errorf("parse JWT claims: %v", err)
		return
	}
	if claims.Iss != f.clientID {
		f.t.Errorf("JWT iss %q, want client ID %q", claims.Iss, f.clientID)
	}
	now := time.Now().Unix()
	if claims.Iat > now-30 {
		f.t.Errorf("JWT iat %d is not backdated (now %d)", claims.Iat, now)
	}
	if claims.Exp <= now || claims.Exp > now+10*60 {
		f.t.Errorf("JWT exp %d outside (now, now+10m] (now %d)", claims.Exp, now)
	}
}

func newTestClient(t *testing.T, fake *fakeGitHub) *Client {
	t.Helper()
	key, pemText := testKeyPEM(t)
	fake.publicKey = &key.PublicKey
	fake.clientID = "Iv1.testclient"
	base := githubtest.Server(t, githubtest.Spec{Handle: fake.handler().ServeHTTP})
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client == nil {
		t.Fatal("new client: got nil for a configured app")
	}
	return client
}
