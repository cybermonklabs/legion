// packages/envoy/cmd/agent-secrets/machinecache_test.go
//
// The machine cache's own guards: a machine whose session would have nowhere to be kept trades
// nothing, and the first Google exchange audience kept stays the one every writer compares
// against, with no hard link needed to decide which writer was first.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestAMachineWithNowhereToKeepItsSessionTradesNothing: with neither XDG_STATE_HOME nor HOME, and
// no home in the password database, the machine's session could be kept nowhere, so every command
// would trade and enroll again. The machine source refuses that before any trade, naming the
// identity it found.
func TestAMachineWithNowhereToKeepItsSessionTradesNothing(t *testing.T) {
	isolateChain(t)
	original := passwdHome
	passwdHome = func() string { return "" }
	t.Cleanup(func() { passwdHome = original })
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	t.Setenv("AGENT_SECRETS_URL", broker.URL())
	metadata := startMetadataServer(t, "machine@example.iam.gserviceaccount.com")

	_, _, err := buildSigner()
	if err == nil || !strings.Contains(err.Error(), "has nowhere to be kept") || strings.Contains(err.Error(), "no broker identity for this process") {
		t.Fatalf("buildSigner = %v; want the machine source to refuse, naming that its session has nowhere to be kept", err)
	}
	if n, e, minted := len(broker.exchanged()), broker.enrolled(), metadata.metadataIdentityRequests(); n != 0 || e != 0 || minted != 0 {
		t.Fatalf("the broker saw %d exchanges and %d enrollments, and the metadata server minted %d identity tokens; want none", n, e, minted)
	}
}

// TestKeepFirstKeepsTheFirstOfManyWriters: writers that keep an audience at once each answer the
// one that was kept first, the file holds it alone in mode 0600, and nothing else is left in the
// directory but the lock that orders them. keepFirst takes no hard link, so a home on a file system
// without them keeps an audience too.
func TestKeepFirstKeepsTheFirstOfManyWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, googleAudienceFile)
	const writers = 16
	kept := make([]string, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kept[i], errs[i] = keepFirst(path, fmt.Sprintf("audience-%d", i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
		if kept[i] != kept[0] {
			t.Fatalf("writer %d answered %q and writer 0 %q; want every writer to answer the one kept first", i, kept[i], kept[0])
		}
	}
	if got, err := readTrimmed(path); err != nil || got != kept[0] {
		t.Fatalf("%s holds %q (%v); want %q", path, got, err, kept[0])
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v, mode %v; want 0600", path, err, info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := googleAudienceFile + " " + googleAudienceFile + ".lock"; strings.Join(names, " ") != want {
		t.Fatalf("the directory holds %v; want %s and nothing else", names, want)
	}
}
