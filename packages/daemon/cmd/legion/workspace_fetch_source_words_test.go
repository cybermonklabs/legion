package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The line `workspace-init provision` names a fresh clone's source with (workspace's
// logCacheSource) says why the cache's /tar filled it and not its bucket, and the repository
// cache's answers are its words, or those of whatever answers at its address, which holds the
// bearer the fill's /packs ask carried and the signed addresses a manifest gives. Here /packs
// answers 503 under a reason phrase that writes back the Authorization the ask carried and a signed
// address, signature and all (packs-status). The line holds no run of the bearer or of the
// signature, and still says the manifest was unavailable and how /packs answered. The case first
// shows net/http's own status line of that answer holds the bearer. Captures
// legion.fetch-source/<case>.
func TestWorkspaceInitProvisionSourceLinePrintsNoCredentialTheCacheEchoes(t *testing.T) {
	t.Parallel()
	const bearer = "cache-" + oauth2calltest.Token + "-9b2f4c"
	const signature = "eyJhbGci" + "SignedAddressQueryValue"
	const address = "https://storage.googleapis.com/example-legion-packs/acme/widgets/objects/pack/pack-0000000000000000000000000000000000000000.pack?X-Goog-Signature=" + signature
	t.Run("packs-status", func(t *testing.T) {
		t.Parallel()
		cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(workspace.CacheRepositoryHeader, "acme/widgets")
			switch r.URL.Path {
			case "/healthz":
				_, _ = w.Write([]byte(`{"ready":true,"defaultBranchOnly":true}`))
			case "/packs":
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack the stand-in's answer: %v", err)
					return
				}
				defer conn.Close()
				_, _ = io.WriteString(conn, "HTTP/1.1 503 "+r.Header.Get("Authorization")+" "+address+"\r\n"+
					workspace.CacheRepositoryHeader+": acme/widgets\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			default:
				http.Error(w, "this stand-in streams no tar", http.StatusServiceUnavailable)
			}
		}))
		t.Cleanup(cache.Close)
		ask, err := http.NewRequest(http.MethodGet, cache.URL+"/packs", nil)
		if err != nil {
			t.Fatal(err)
		}
		ask.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(ask)
		if err != nil {
			t.Fatalf("ask the stand-in's /packs: %v", err)
		}
		_ = response.Body.Close()
		if oauth2calltest.Piece(response.Status, bearer) == "" {
			t.Fatalf("net/http's status line %q holds no run of the bearer, so the case cannot see a leak", response.Status)
		}

		v := newIssueVolume(t).withRemote(t, "main")
		v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
		v.env["LEGION_REPOSITORY_CACHE_URL"] = cache.URL
		v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, bearer+"\n")
		code, stdout, stderr := v.answered(t, "workspace-init provision from a cache that echoes its bearer", v.args("LEGION-9"))
		t.Logf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		oauth2calltest.RequireNoPiece(t, "legion.fetch-source/packs-status", stdout, bearer, signature)
		if want := "workspace-init: source=/tar reason=unavailable manifest: GET " + cache.URL + "/packs answered 503 "; !strings.Contains(stdout, want) {
			t.Errorf("stdout %q, want the line naming /tar as the source because %q", stdout, want)
		}
	})
}
