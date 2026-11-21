package natsauth_test

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// listening is the address of a loopback listener that takes connections and answers none, closed
// when t ends: a client reaches what it does once connected, a websocket handshake among them.
func listening(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
}

// A list Connect is handed whose later entry carries a user name and a scheme no NATS server is
// written with is dialled, not refused: Connect dials what the configuration accepted, and the
// configuration (config.readNatsURLs) runs the tail rule on nats_urls, so Connect runs the pool
// and handshake rules alone. Both addresses are closed ports, so the dial fails, and the failure is
// nats.go's, not a refusal of the list.
func TestConnectDialsALaterEntryWithAUserNameAndAnotherScheme(t *testing.T) {
	closed := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		return address
	}
	conn, err := natsauth.Connect([]string{"nats://" + closed(), "x://legion@" + closed()}, "", t.Name(), nats.Timeout(time.Second), nats.NoReconnect())
	if err == nil {
		conn.Close()
		t.Fatal("Connect reached a server at a closed port")
	}
	if strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("Connect refused a list nats.go dials: %v", err)
	}
}

// printedRun is the shortest run of a credential's characters no printed text may hold: the run
// the TypeScript clients' scrub withholds (@legion/contracts/held-secrets, SCRUB_RUN).
const printedRun = 10

// printedCredential is a made-up credential, by the words a failure names it with.
type printedCredential struct{ name, value string }

// requirePrintsNoCredential fails t, naming the run, when text holds printedRun or more characters
// in a row of any of credentials.
func requirePrintsNoCredential(t *testing.T, what, text string, credentials []printedCredential) {
	t.Helper()
	for _, c := range credentials {
		for i := 0; i+printedRun <= len(c.value); i++ {
			if run := c.value[i : i+printedRun]; strings.Contains(text, run) {
				t.Errorf("%s holds %q, a run of %d characters of %s: %s", what, run, printedRun, c.name, text)
				break
			}
		}
	}
}

// madeUpSeed is the nkey seed of prefix's type whose raw seed is a hash of words, and its public key.
func madeUpSeed(t *testing.T, prefix nkeys.PrefixByte, words string) (seed, public string) {
	t.Helper()
	raw := sha256.Sum256([]byte(words))
	encoded, err := nkeys.EncodeSeed(prefix, raw[:])
	if err != nil {
		t.Fatalf("encode the made-up seed: %v", err)
	}
	pair, err := nkeys.FromSeed(encoded)
	if err != nil {
		t.Fatalf("read the made-up seed: %v", err)
	}
	if public, err = pair.PublicKey(); err != nil {
		t.Fatalf("the made-up seed's public key: %v", err)
	}
	return string(encoded), public
}

// TestTheDaemonsNATSAuthPrintsNoSeedOrToken: the daemon's NATS connection prints no part of its
// credential. A made-up nkey seed, a second one no server admits, an account's seed, the seed with
// a letter mistyped and a made-up NATS token go where a person could put them, and the error of
// each refusal of a seed (Seed, DaemonSeed, SeedFile, Connect and PublicKey: a file missing,
// unreadable, empty, no regular file or open to others, a variable set but empty, a value that is
// no nkey seed, a seed that is not a user's) holds no run of printedRun or more characters of any
// of them, and names the key or variable, and the file where there is one. A seed's public key may
// print. Then every line LogEvents writes while the daemon connects to test servers and is refused
// holds none of them either: the seed no server admits, refused until the connection closes; a
// subscription and a publish the server refuses the admitted seed; a slow consumer's asynchronous
// error; and a connection whose server URL names the token as its user, lost and restored, and
// lost and closed. A server URL whose userinfo url.Parse cannot read as userinfo is refused naming
// none of it: one that does not parse, ones whose password holds a "/", "?", "#" or space (which
// url.Parse reads as a port it quotes, or which end a URL rule's authority or URL early), and one
// naming a token that holds a "/" as its user (which parses, the token's head its host).
func TestTheDaemonsNATSAuthPrintsNoSeedOrToken(t *testing.T) {
	seed, public := madeUpSeed(t, nkeys.PrefixByteUser, "a made-up user seed")
	stranger, _ := madeUpSeed(t, nkeys.PrefixByteUser, "a made-up user seed no server admits")
	account, _ := madeUpSeed(t, nkeys.PrefixByteAccount, "a made-up account seed")
	letter := byte('A')
	if seed[20] == letter {
		letter = 'B'
	}
	mistyped := seed[:20] + string(letter) + seed[21:]
	tokenHash := sha256.Sum256([]byte("a made-up NATS token"))
	token := "made-up-nats-token-" + hex.EncodeToString(tokenHash[:16])
	// Passwords that hold a "/", "?", "#", space or comma, which end url.Parse's authority or its
	// URL early or split the list nats.go reads (about four in ten base64 credentials hold a "/"),
	// and a token holding a "/".
	tail := hex.EncodeToString(tokenHash[16:])
	slashedPassword, queryPassword := "madeUpNatsPassword/"+tail, "madeUpNatsPassword?"+tail
	fragmentPassword, spacedPassword := "madeUpNatsPassword#"+tail, "madeUpNatsPassword "+tail
	commaPassword := "madeUpNatsPassword," + tail
	slashedToken := "madeUpNatsToken/" + tail
	credentials := []printedCredential{
		{"the made-up seed", seed}, {"the made-up seed no server admits", stranger}, {"the made-up account seed", account},
		{"the mistyped seed", mistyped}, {"the made-up NATS token", token},
		{"the made-up password holding a /", slashedPassword}, {"the made-up password holding a ?", queryPassword},
		{"the made-up password holding a #", fragmentPassword}, {"the made-up password holding a space", spacedPassword},
		{"the made-up password holding a comma", commaPassword}, {"the made-up token holding a /", slashedToken},
	}

	dir := t.TempDir()
	write := func(name, contents string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	missing, directory := filepath.Join(dir, "missing.seed"), filepath.Join(dir, "a-directory.seed")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	unreadable, empty := write("unreadable.seed", seed, 0), write("empty.seed", " \n", 0o600)
	open, tokenFile := write("open.seed", seed, 0o644), write("token.seed", token, 0o600)
	mistypedFile, accountFile := write("mistyped.seed", mistyped, 0o600), write("account.seed", account, 0o600)

	t.Run("each refusal of a seed", func(t *testing.T) {
		seedOf := func(file string, values map[string]string) func() error {
			return func() error { _, err := natsauth.Seed(file, env(values)); return err }
		}
		daemonSeedOf := func(file string, values map[string]string) func() error {
			return func() error { _, err := natsauth.DaemonSeed(file, env(values)); return err }
		}
		// connectTo is Connect handed server, refused before any dial, and its error.
		connectTo := func(server string) func() error {
			return func() error {
				conn, err := natsauth.Connect([]string{server}, "", t.Name())
				if err == nil {
					conn.Close()
				}
				return err
			}
		}
		unparsed := []string{"the NATS server URL 1 of 1 cannot be read", "does not parse as a URL"}
		for _, row := range []struct {
			name       string
			printed    func() error
			unreadable bool
			names      []string
		}{
			{name: "a key file that is not there", printed: seedOf(missing, map[string]string{natsauth.SeedVariable: seed}),
				names: []string{natsauth.SeedFileKey, missing, "could not be read"}},
			{name: "a key file that cannot be read", printed: seedOf(unreadable, nil), unreadable: true,
				names: []string{natsauth.SeedFileKey, unreadable, "permission denied"}},
			{name: "an empty key file", printed: seedOf(empty, nil), names: []string{natsauth.SeedFileKey, empty, "is empty"}},
			{name: "a key file that is a directory", printed: seedOf(directory, nil),
				names: []string{natsauth.SeedFileKey, directory, "not a regular file"}},
			{name: "a key file others can read", printed: seedOf(open, nil), names: []string{natsauth.SeedFileKey, open, "readable by its group or others"}},
			{name: "a key file holding the token", printed: seedOf(tokenFile, nil),
				names: []string{natsauth.SeedFileKey, tokenFile, "does not hold a valid nkey seed"}},
			{name: "a key file holding the seed mistyped", printed: seedOf(mistypedFile, nil),
				names: []string{natsauth.SeedFileKey, mistypedFile, "does not hold a valid nkey seed"}},
			{name: "a key file holding an account's seed", printed: seedOf(accountFile, nil),
				names: []string{natsauth.SeedFileKey, accountFile, "not a user's"}},
			{name: "NATS_NKEY_SEED_FILE set but empty", printed: seedOf("", map[string]string{natsauth.SeedFileVariable: "", natsauth.SeedVariable: seed}),
				names: []string{natsauth.SeedFileVariable, "set but empty"}},
			{name: "NATS_NKEY_SEED_FILE naming a file others can read", printed: seedOf("", map[string]string{natsauth.SeedFileVariable: open}),
				names: []string{natsauth.SeedFileVariable, open, "readable by its group or others"}},
			{name: "NATS_NKEY_SEED set but blank", printed: seedOf("", map[string]string{natsauth.SeedVariable: "  "}),
				names: []string{natsauth.SeedVariable, "set but empty"}},
			{name: "the token where the seed goes", printed: seedOf("", map[string]string{natsauth.SeedVariable: token}),
				names: []string{natsauth.SeedVariable, "does not hold a valid nkey seed"}},
			{name: "the seed mistyped", printed: seedOf("", map[string]string{natsauth.SeedVariable: mistyped}),
				names: []string{natsauth.SeedVariable, "does not hold a valid nkey seed"}},
			{name: "an account's seed", printed: seedOf("", map[string]string{natsauth.SeedVariable: account}),
				names: []string{natsauth.SeedVariable, "not a user's"}},
			{name: "the daemon's key file holding the token", printed: daemonSeedOf(tokenFile, nil),
				names: []string{natsauth.DaemonSeedFileKey, tokenFile, "does not hold a valid nkey seed"}},
			{name: "NATS_DAEMON_NKEY_SEED_FILE set but empty", printed: daemonSeedOf("", map[string]string{natsauth.DaemonSeedFileVariable: ""}),
				names: []string{natsauth.DaemonSeedFileVariable, "set but empty"}},
			{name: "NATS_DAEMON_NKEY_SEED_FILE naming a file others can read", printed: daemonSeedOf("", map[string]string{natsauth.DaemonSeedFileVariable: open}),
				names: []string{natsauth.DaemonSeedFileVariable, open, "readable by its group or others"}},
			{name: "the daemon's seed mistyped", printed: daemonSeedOf("", map[string]string{natsauth.DaemonSeedVariable: mistyped}),
				names: []string{natsauth.DaemonSeedVariable, "does not hold a valid nkey seed"}},
			{name: "an account's seed as the daemon's", printed: daemonSeedOf("", map[string]string{natsauth.DaemonSeedVariable: account}),
				names: []string{natsauth.DaemonSeedVariable, "not a user's"}},
			{name: "the operator's seed file others can read", printed: func() error { _, err := natsauth.SeedFile(open); return err },
				names: []string{natsauth.SeedFileKey, open, "readable by its group or others"}},
			{name: "the operator's seed file holding the token", printed: func() error { _, err := natsauth.SeedFile(tokenFile); return err },
				names: []string{natsauth.SeedFileKey, tokenFile, "does not hold a valid nkey seed"}},
			{name: "Connect handed the seed mistyped", printed: func() error {
				conn, err := natsauth.Connect([]string{"nats://127.0.0.1:1"}, mistyped, t.Name())
				if err == nil {
					conn.Close()
				}
				return err
			}, names: []string{"the NATS nkey seed", "does not hold a valid nkey seed"}},
			{name: "Connect handed a server URL naming the token as its user that does not parse",
				printed: connectTo("nats://" + token + "%zz@127.0.0.1:1"), names: unparsed},
			{name: "Connect handed a server URL whose password holds a /",
				printed: connectTo("nats://legion:" + slashedPassword + "@127.0.0.1:1"), names: unparsed},
			{name: "Connect handed a server URL whose password holds a ?",
				printed: connectTo("nats://legion:" + queryPassword + "@127.0.0.1:1"), names: unparsed},
			{name: "Connect handed a server URL whose password holds a #",
				printed: connectTo("nats://legion:" + fragmentPassword + "@127.0.0.1:1"), names: unparsed},
			{name: "Connect handed a server URL whose password holds a space",
				printed: connectTo("nats://legion:" + spacedPassword + "@127.0.0.1:1"), names: unparsed},
			{name: "Connect handed a server URL whose password holds a comma",
				printed: connectTo("nats://legion:" + commaPassword + "@127.0.0.1:1"),
				names:   []string{"the NATS server URL 1 of 1 cannot be read", "it holds a comma", "list each server as its own nats_urls entry"}},
			{name: "Connect handed a server URL naming a token that holds a / as its user",
				printed: connectTo("nats://" + slashedToken + "@127.0.0.1:1"),
				names:   []string{"the NATS server URL 1 of 1 cannot be read", `an "@" stands after its host`}},
			{name: "Connect handed a server URL that does not parse with the token in its query",
				printed: connectTo("nats://127.0.0.1:1/%zz?token=" + token), names: unparsed},
			// A listener that takes the connection, so nats.go reaches its websocket handshake, as it
			// does against a server, where the handshake's parse error quotes the URL it rebuilt.
			{name: "Connect handed a websocket URL with the token in its query whose path stops parsing once decoded",
				printed: connectTo("ws://" + listening(t) + "/%25zz?token=" + token),
				names:   []string{"the NATS server URL 1 of 1 cannot be read", "does not parse once decoded", "%2525"}},
			// A ws list's later URL with no scheme: nats.go dials it as ws, so the handshake rule
			// runs on it too, as natsurl.CheckEntries runs it for the list.
			{name: "Connect handed a ws list whose later URL has no scheme, the token in its query, and a path that stops parsing",
				printed: func() error {
					conn, err := natsauth.Connect([]string{"ws://127.0.0.1:1", listening(t) + "/%25zz?token=" + token}, "", t.Name())
					if err == nil {
						conn.Close()
					}
					return err
				},
				names: []string{"the NATS server URL 2 of 2 cannot be read", "does not parse once decoded", "%2525"}},
			{name: "PublicKey of the token", printed: func() error { _, err := natsauth.PublicKey(token); return err },
				names: []string{"the NATS nkey seed", "does not hold a valid nkey seed"}},
			{name: "PublicKey of an account's seed", printed: func() error { _, err := natsauth.PublicKey(account); return err },
				names: []string{"the NATS nkey seed", "not a user's"}},
		} {
			t.Run(row.name, func(t *testing.T) {
				if row.unreadable && os.Geteuid() == 0 {
					t.Skip("root reads a 0000 file")
				}
				err := row.printed()
				if err == nil {
					t.Fatal("the seed was not refused")
				}
				printed := err.Error()
				requirePrintsNoCredential(t, "the error", printed, credentials)
				for _, name := range row.names {
					if !strings.Contains(printed, name) {
						t.Errorf("the error %q does not name %q", printed, name)
					}
				}
			})
		}
	})

	t.Run("every line LogEvents writes while a server refuses the connection", func(t *testing.T) {
		var out lockedBuffer
		log := slog.New(slog.NewTextHandler(&out, nil))
		logged := func(message string) int { return strings.Count(out.String(), `msg="`+message+`"`) }
		channel := func() chan struct{} { return make(chan struct{}, 1) }

		// The seed no server admits, retried until nats.go closes the connection on the same
		// refusal twice (TestADaemonSeedRefusedTwiceCloses).
		const denied, deniedPublish = "legion.printed.denied", "legion.printed.denied-publish"
		_, nkeyURL := testnats.StartNkeyAuthorizedUsers(t, testnats.Image, testnats.NkeyUser{
			Public:      public,
			Permissions: `{ subscribe: { deny: ["` + denied + `"] }, publish: { deny: ["` + deniedPublish + `"] } }`,
		})
		refusedClosed := channel()
		refused, err := natsauth.Connect([]string{nkeyURL}, stranger, t.Name()+" refused",
			nats.RetryOnFailedConnect(true), natsauth.ReconnectForever(), nats.ReconnectWait(50*time.Millisecond),
			natsauth.LogEvents(log), signalled(channel(), channel(), channel(), refusedClosed))
		if err != nil {
			t.Fatalf("Connect with RetryOnFailedConnect: %v", err)
		}
		t.Cleanup(refused.Close)
		wait(t, refusedClosed, "the refused seed's close", &out)

		// The admitted seed, refused a subscription and a publish, and a slow consumer.
		admitted, err := natsauth.Connect([]string{nkeyURL}, seed, t.Name()+" admitted", nats.Timeout(5*time.Second),
			natsauth.LogEvents(log), signalled(channel(), channel(), channel(), channel()))
		if err != nil {
			t.Fatalf("connect as the admitted seed's user: %v", err)
		}
		t.Cleanup(admitted.Close)
		if _, err := admitted.SubscribeSync(denied); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := admitted.Publish(deniedPublish, []byte("{}")); err != nil {
			t.Fatalf("publish: %v", err)
		}
		const slow = "legion.printed.slow"
		sub, err := admitted.SubscribeSync(slow)
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := sub.SetPendingLimits(1, -1); err != nil {
			t.Fatalf("set pending limits: %v", err)
		}
		for range 5 {
			if err := admitted.Publish(slow, []byte("{}")); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
		if err := admitted.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, "the two refused permissions and the slow consumer logged", func() bool {
			return logged("NATS refused the daemon a permission: its NATS user lacks that grant") >= 2 && logged("NATS reported an asynchronous error") >= 1
		})

		// The token as the server URL's user name, which nats.go presents as the CONNECT's token:
		// one connection lost and restored, one lost and closed.
		upstream := strings.TrimPrefix(testnats.URL(t), "nats://")
		restoring, closing := dropProxy(t, upstream), dropProxy(t, upstream)
		lost, restored, closed := channel(), channel(), channel()
		reconnecting, err := natsauth.Connect([]string{"nats://" + token + "@" + restoring.addr}, "", t.Name()+" restored", nats.Timeout(5*time.Second),
			nats.ReconnectWait(10*time.Millisecond), nats.ReconnectJitter(0, 0),
			natsauth.LogEvents(log), signalled(channel(), lost, restored, channel()))
		if err != nil {
			t.Fatalf("connect with the token in the URL: %v", err)
		}
		t.Cleanup(reconnecting.Close)
		ending, err := natsauth.Connect([]string{"nats://" + token + "@" + closing.addr}, "", t.Name()+" closed", nats.Timeout(5*time.Second), nats.NoReconnect(),
			natsauth.LogEvents(log), signalled(channel(), channel(), channel(), closed))
		if err != nil {
			t.Fatalf("connect with the token in the URL: %v", err)
		}
		t.Cleanup(ending.Close)
		restoring.drop()
		wait(t, lost, "the lost connection", &out)
		wait(t, restored, "the restored connection", &out)
		closing.drop()
		wait(t, closed, "the closed connection", &out)
		reconnecting.Close()

		logs := out.String()
		for _, want := range []struct {
			what string
			ok   bool
		}{
			{"the refused seed's close, with the server's refusal", strings.Contains(logs, `msg="NATS connection closed" error="nats: Authorization Violation"`)},
			{"the lost connection", logged("NATS connection lost") >= 1},
			{"the restored connection, naming its server", strings.Contains(logs, `msg="NATS connection restored" server=`) && strings.Contains(logs, restoring.addr)},
			{"two closes", logged("NATS connection closed") >= 2},
		} {
			if !want.ok {
				t.Errorf("no line of %s, so the check below sees nothing of it:\n%s", want.what, logs)
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
			requirePrintsNoCredential(t, "the logged line", line, credentials)
		}
	})
}
