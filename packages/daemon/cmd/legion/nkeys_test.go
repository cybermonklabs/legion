package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

// The nkey seeds this package's tests hand `legion` are made here, not by internal/testnats,
// because that package links testcontainers-go, whose package init walks every process on the
// machine: ten seconds and a gigabyte of allocation on a box running 25,000 of them. Many tests
// here start this test binary again as `legion` (TestMain), and each start pays every package
// init linked into it, so testnats made one `legion gh` start cost what its init cost, and a test
// of twenty-two such calls outlast its deadline. No test here starts a NATS server.

// nkeyUser is a fresh nkey user's seed and public key.
func nkeyUser(t testing.TB) (seed, public string) {
	t.Helper()
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create nkey user: %v", err)
	}
	raw, err := user.Seed()
	if err != nil {
		t.Fatalf("user seed: %v", err)
	}
	if public, err = user.PublicKey(); err != nil {
		t.Fatalf("user public key: %v", err)
	}
	return string(raw), public
}

// nkeyUserSeed is a fresh nkey user's seed (nkeyUser without the public key).
func nkeyUserSeed(t testing.TB) string {
	t.Helper()
	seed, _ := nkeyUser(t)
	return seed
}

// nkeyAccount is a fresh nkey account's seed: a valid nkey seed, but not a user's.
func nkeyAccount(t testing.TB) string {
	t.Helper()
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("create nkey account: %v", err)
	}
	raw, err := account.Seed()
	if err != nil {
		t.Fatalf("account seed: %v", err)
	}
	return string(raw)
}

// seedFile writes contents to a 0600 file of its own under t's temporary directory and returns
// its path.
func seedFile(t testing.TB, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nats.seed")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the seed file: %v", err)
	}
	return path
}

// This test binary, which the package's tests start again as `legion`, links no module of
// testcontainers-go, so no start of it pays that library's package init (the comment above
// nkeyUser). It reads the modules the binary was built from, as `go version -m` prints them.
func TestThisTestBinaryLinksNoTestcontainers(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("this test binary carries no build information to read its modules from")
	}
	if len(info.Deps) == 0 {
		t.Fatal("this test binary's build information lists no module, so it cannot say what is linked")
	}
	for _, dep := range info.Deps {
		if strings.HasPrefix(dep.Path, "github.com/testcontainers/") {
			t.Errorf("this test binary links %s %s, and every start of it as `legion` pays that module's package init; `go list -deps -test -f '{{.ImportPath}} {{.Imports}}' ./cmd/legion/` names the package that imports it", dep.Path, dep.Version)
		}
	}
}
