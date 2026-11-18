package fixtureenv_test

import (
	"net/http"
	"net/url"
	"testing"
)

// The loopback remote counts only a request of a push's shape, so another process's request
// leaves the "want none" of TestIsolatedReachesNoNetworkRemoteWhateverTheRepositoryAllows intact:
// a metadata server's identity read, the root, and another repository's ref discovery are each
// answered 404 and counted as nothing. The request a push that left the environment sends, the
// remote's own ref discovery, is still counted, so the test still fails on such a push.
func TestTheLoopbackRemoteCountsOnlyAPushsOwnRequests(t *testing.T) {
	remote, requests := loopbackRemote(t)
	parsed, err := url.Parse(remote)
	if err != nil {
		t.Fatal(err)
	}
	origin := parsed.Scheme + "://" + parsed.Host
	get := func(address string) int {
		t.Helper()
		response, err := http.Get(address)
		if err != nil {
			t.Fatalf("GET %s: %v", address, err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	for _, path := range []string{"/computeMetadata/v1/instance/service-accounts/default/identity", "/", "/other.git/info/refs?service=git-receive-pack"} {
		if status := get(origin + path); status != http.StatusNotFound {
			t.Fatalf("GET %s from another process: status %d, want 404", path, status)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("the remote counted %d requests of other processes, want none", n)
	}
	if status := get(remote + "/info/refs?service=git-receive-pack"); status != http.StatusForbidden {
		t.Fatalf("a push's ref discovery: status %d, want the remote's 403", status)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("the remote counted %d requests after a push's ref discovery, want that one", n)
	}
}
