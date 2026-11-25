// packages/envoy/cmd/cipher/machinecache_test.go
//
// The machine cache's own guards: a machine whose session would have nowhere to be kept trades
// nothing, and the first Google exchange audience kept stays the one every writer compares
// against, with no hard link needed to decide which writer was first.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
)

// TestAMachineWithNowhereToKeepItsSessionTradesNothing: with neither XDG_STATE_HOME nor HOME, and
// no home in the password database, the machine's session could be kept nowhere, so every command
// would trade and enroll again. The machine source refuses that before any trade, naming the
// identity it found.
func TestAMachineWithNowhereToKeepItsSessionTradesNothing(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at machinecache_test.go:28); it signals the test process or changes its signal handling (main.buildSigner, main.buildChainSigner, main.selectSigner, main.signerChain, main.machineChainSource, main.registerMachine, main.registerMachineSession, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (passwdHome set at machinecache_test.go:26).
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

	_, _, err := buildSigner(io.Discard)
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
const googleKeptFile = "google-exchange-audience"

func TestKeepFirstKeepsTheFirstOfManyWriters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, googleKeptFile)
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
	if want := googleKeptFile + " " + googleKeptFile + ".lock"; strings.Join(names, " ") != want {
		t.Fatalf("the directory holds %v; want %s and nothing else", names, want)
	}
}

// TestAnAudienceKeptBeforeTheRegistryIsStillChecked: a machine that kept its broker's Google
// exchange audience with a client from before the identity registry wrote it to
// google-exchange-audience in the machine cache. The registry's detector names that same file
// (KeptFile), so a broker that now names another audience is refused in the same words, and the
// audience that machine kept is still the one its tokens are minted for.
func TestAnAudienceKeptBeforeTheRegistryIsStillChecked(t *testing.T) {
	useBareEnvironment(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := machineCacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What the older client wrote: the file name it used, holding the audience it kept.
	if err := os.WriteFile(filepath.Join(dir, "google-exchange-audience"), []byte("agent-secrets-vm"), 0o600); err != nil {
		t.Fatal(err)
	}
	detector := googleDetector(t)
	base := cipheraddr.New("https://broker.example")
	if err := checkAudience(base, detector, "agent-secrets-vm"); err != nil {
		t.Fatalf("the audience this machine kept before the registry is refused now: %v", err)
	}
	err := checkAudience(base, detector, "another-audience")
	if err == nil {
		t.Fatal("a broker naming another audience was taken, so a machine could mint a token for someone else's service")
	}
	for _, want := range []string{
		"names \"another-audience\" as its Google exchange audience",
		"first read \"agent-secrets-vm\" there",
		"kept in google-exchange-audience in " + machineCacheWords,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q, want it holding %q", err, want)
		}
	}
}
