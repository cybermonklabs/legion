package bus

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// takingListener is the address of a loopback listener that takes connections and answers none,
// closed when t ends: a client reaches what it does once connected, a websocket handshake among
// them.
func takingListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
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
	require.NoError(t, err)
	pair, err := nkeys.FromSeed(encoded)
	require.NoError(t, err)
	public, err = pair.PublicKey()
	require.NoError(t, err)
	return string(encoded), public
}

// madeUpServiceToken is a platform identity token of fixed made-up claims, an unsigned JWT with a
// made-up signature naming a listener's account and lapsing at the start of 2099.
func madeUpServiceToken() string {
	segment := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signature := sha256.Sum256([]byte("a made-up signature"))
	return segment(map[string]string{"alg": "RS256", "kid": "made-up-key", "typ": "JWT"}) + "." +
		segment(map[string]any{"sub": "system:serviceaccount:example-ns:example-listener", "iat": 4070905200, "exp": 4070908800}) + "." +
		base64.RawURLEncoding.EncodeToString(signature[:])
}

// lookupOf is a lookup over exactly the variables values names, whatever the shell running the
// test set.
func lookupOf(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, set := values[name]
		return value, set
	}
}

// TestTheBusCredentialPrintsNoSeedOrToken: the bus prints no part of the NATS credential its
// environment names. A made-up nkey seed, an account's seed, the seed with a letter mistyped and a
// made-up service token go where a person could put them, and the error of each refusal
// credential.go makes - both identities set, a variable set but empty, a file missing, unreadable
// or empty, a value that is not an nkey seed, a seed that is not a user's - holds no run of
// printedRun or more characters of any of them, and names the variable, and the file where there
// is one. A seed's public key may print. The same holds for a server URL nats.go would read other
// than as written, on a run that may reach remote NATS (ENVOY_ALLOW_REMOTE_NATS=1, as deployments
// set it), each refused naming the URL by its position alone: one naming the token as its user
// that does not parse, one with the token in its query, ones whose password holds a "/", "?", "#"
// or space (which url.Parse reads as a port it quotes, or as userinfo it cannot read), one naming a
// token that holds a "/" as its user (which parses, the token's head its host), and the pieces of
// URLs a comma in a credential split, as every caller splits its list: a password's, a password's
// whose head is all digits and a token's, whose second piece names no scheme, and a token's whose
// second piece names one nats.go does not dial; and a websocket URL with the token in its query
// whose path stops parsing once nats.go's handshake decodes it, and the same URL written with no
// scheme after a first ws:// URL, which nats.go gives the pool's ws scheme. The bus trims its list
// before nats.go reads Servers as written, so a URL with white space around it connects. Then
// every line the bus logs through slog while test servers refuse the connection holds none of
// them: a seed's user the restarted server no longer admits, a service token the server refuses from the
// first connect, and a token written as the server URL's user name - refused; admitted by a
// server that then reports a slow consumer, stops and starts again; and dialled where nothing
// answers. Each flow's lines are required, so a run that logged nothing cannot pass, and every
// client is closed and its close logged before the lines are read. The rows that read an
// unreadable file skip under root, which reads a 0000 file.
func TestTheBusCredentialPrintsNoSeedOrToken(t *testing.T) {
	seed, public := madeUpSeed(t, nkeys.PrefixByteUser, "a made-up user seed")
	account, _ := madeUpSeed(t, nkeys.PrefixByteAccount, "a made-up account seed")
	letter := byte('A')
	if seed[20] == letter {
		letter = 'B'
	}
	mistyped := seed[:20] + string(letter) + seed[21:]
	token := madeUpServiceToken()
	// Passwords that hold a "/", "?", "#", space or comma, which end url.Parse's authority or its
	// URL early or split the URL in a caller's list (about four in ten base64 credentials hold a
	// "/"), and a token holding a "/".
	slashed := sha256.Sum256([]byte("a made-up NATS credential holding a slash"))
	tail := base64.RawURLEncoding.EncodeToString(slashed[:12])
	slashedPassword, queryPassword := "madeUpNatsPassword/"+tail, "madeUpNatsPassword?"+tail
	fragmentPassword, spacedPassword := "madeUpNatsPassword#"+tail, "madeUpNatsPassword "+tail
	commaPassword := "madeUpNatsPassword," + tail
	// A password holding a comma after a head of digits, which url.Parse reads as a port once the
	// list is split, a token holding a comma, whose head it reads as the host, and a password that
	// parses, written with white space around its URL.
	digitsPassword, commaToken := "8472915603918274,"+tail, "madeUpNatsToken,"+tail
	plainPassword := "madeUpNatsPasswordPlain" + tail
	slashedToken := "madeUpNatsToken/" + base64.RawURLEncoding.EncodeToString(slashed[12:24])
	// A token holding a comma followed by a scheme, whose second piece names one nats.go does not
	// dial.
	schemedToken := "madeUpNatsToken,x://" + tail
	credentials := []printedCredential{
		{"the made-up seed", seed}, {"the made-up account seed", account}, {"the mistyped seed", mistyped}, {"the made-up service token", token},
		{"the made-up password holding a /", slashedPassword}, {"the made-up password holding a ?", queryPassword},
		{"the made-up password holding a #", fragmentPassword}, {"the made-up password holding a space", spacedPassword},
		{"the made-up password holding a comma", commaPassword}, {"the made-up token holding a /", slashedToken},
		{"the made-up password holding a comma after digits", digitsPassword}, {"the made-up token holding a comma", commaToken},
		{"the made-up password that parses", plainPassword}, {"the made-up token holding a comma and a scheme", schemedToken},
	}
	unparsed := []string{"NATS server URL 1 of 1 cannot be read", "does not parse as a URL"}
	remote := map[string]string{AllowRemoteEnvVar: "1"}
	// withPassword is a server URL whose user legion has password.
	withPassword := func(password string) string { return "nats://legion:" + password + "@127.0.0.1:1" }

	dir := t.TempDir()
	write := func(name, contents string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		return path
	}
	unreadable := func(name, contents string) string {
		path := write(name, contents)
		require.NoError(t, os.Chmod(path, 0))
		return path
	}
	tokenFile, seedFile := write("service.token", token), write("nats.seed", seed)
	emptyToken, emptySeed := write("empty.token", " \n"), write("empty.seed", "\n")
	missingToken, missingSeed := filepath.Join(dir, "missing.token"), filepath.Join(dir, "missing.seed")
	unreadableToken, unreadableSeed := unreadable("unreadable.token", token), unreadable("unreadable.seed", seed)
	mistypedFile, tokenAsSeedFile, accountFile := write("mistyped.seed", mistyped), write("token-as.seed", token), write("account.seed", account)

	t.Run("each refusal of a credential or a server URL", func(t *testing.T) {
		for _, row := range []struct {
			name       string
			urls       []string
			env        map[string]string
			unreadable bool
			names      []string
		}{
			{name: "a service token file beside a seed", env: map[string]string{ServiceTokenFileEnvVar: tokenFile, NkeySeedEnvVar: seed},
				names: []string{ServiceTokenFileEnvVar, NkeySeedEnvVar, "one identity"}},
			{name: "a service token file beside a Google audience", env: map[string]string{ServiceTokenFileEnvVar: tokenFile, ServiceGoogleAudienceEnvVar: token},
				names: []string{ServiceTokenFileEnvVar, ServiceGoogleAudienceEnvVar, "one identity"}},
			{name: "a Google audience beside a seed file", env: map[string]string{ServiceGoogleAudienceEnvVar: token, nkeySeedFileVariable: seedFile},
				names: []string{ServiceGoogleAudienceEnvVar, nkeySeedFileVariable, "one identity"}},
			{name: "NATS_SERVICE_TOKEN_FILE set but empty", env: map[string]string{ServiceTokenFileEnvVar: ""},
				names: []string{ServiceTokenFileEnvVar, "set but empty"}},
			{name: "NATS_SERVICE_GOOGLE_AUDIENCE set but blank", env: map[string]string{ServiceGoogleAudienceEnvVar: " "},
				names: []string{ServiceGoogleAudienceEnvVar, "set but empty"}},
			{name: "NATS_NKEY_SEED_FILE set but empty", env: map[string]string{nkeySeedFileVariable: "", NkeySeedEnvVar: seed},
				names: []string{nkeySeedFileVariable, "set but empty"}},
			{name: "NATS_NKEY_SEED set but blank", env: map[string]string{NkeySeedEnvVar: "  "},
				names: []string{NkeySeedEnvVar, "set but empty"}},
			{name: "a service token file that is not there", env: map[string]string{ServiceTokenFileEnvVar: missingToken},
				names: []string{ServiceTokenFileEnvVar, missingToken, "no such file"}},
			{name: "a service token file that cannot be read", env: map[string]string{ServiceTokenFileEnvVar: unreadableToken}, unreadable: true,
				names: []string{ServiceTokenFileEnvVar, unreadableToken, "permission denied"}},
			{name: "an empty service token file", env: map[string]string{ServiceTokenFileEnvVar: emptyToken},
				names: []string{ServiceTokenFileEnvVar, emptyToken, "is empty"}},
			{name: "a seed file that is not there", env: map[string]string{nkeySeedFileVariable: missingSeed, NkeySeedEnvVar: seed},
				names: []string{nkeySeedFileVariable, missingSeed, "could not be read"}},
			{name: "a seed file that cannot be read", env: map[string]string{nkeySeedFileVariable: unreadableSeed}, unreadable: true,
				names: []string{nkeySeedFileVariable, unreadableSeed, "permission denied"}},
			{name: "an empty seed file", env: map[string]string{nkeySeedFileVariable: emptySeed},
				names: []string{nkeySeedFileVariable, emptySeed, "is empty"}},
			{name: "the service token where the seed goes", env: map[string]string{NkeySeedEnvVar: token},
				names: []string{NkeySeedEnvVar, "does not hold a valid nkey seed"}},
			{name: "the seed mistyped", env: map[string]string{NkeySeedEnvVar: mistyped},
				names: []string{NkeySeedEnvVar, "does not hold a valid nkey seed"}},
			{name: "a seed file holding the service token", env: map[string]string{nkeySeedFileVariable: tokenAsSeedFile},
				names: []string{nkeySeedFileVariable, tokenAsSeedFile, "does not hold a valid nkey seed"}},
			{name: "a seed file holding the seed mistyped", env: map[string]string{nkeySeedFileVariable: mistypedFile},
				names: []string{nkeySeedFileVariable, mistypedFile, "does not hold a valid nkey seed"}},
			{name: "an account's seed", env: map[string]string{NkeySeedEnvVar: account},
				names: []string{NkeySeedEnvVar, "not a user's"}},
			{name: "a seed file holding an account's seed", env: map[string]string{nkeySeedFileVariable: accountFile},
				names: []string{nkeySeedFileVariable, accountFile, "not a user's"}},
			{name: "a server URL naming the token as its user that does not parse", urls: []string{"nats://" + token + "%zz@127.0.0.1:1"},
				env: remote, names: unparsed},
			{name: "a server URL whose password holds a /", urls: []string{"nats://legion:" + slashedPassword + "@127.0.0.1:1"},
				env: remote, names: unparsed},
			{name: "a server URL whose password holds a ?", urls: []string{"nats://legion:" + queryPassword + "@127.0.0.1:1"},
				env: remote, names: unparsed},
			{name: "a server URL whose password holds a #", urls: []string{"nats://legion:" + fragmentPassword + "@127.0.0.1:1"},
				env: remote, names: unparsed},
			{name: "a server URL whose password holds a space", urls: []string{"nats://legion:" + spacedPassword + "@127.0.0.1:1"},
				env: remote, names: unparsed},
			{name: "a server URL whose password holds a comma, split as every caller splits its list", urls: strings.Split(withPassword(commaPassword), ","),
				env: remote, names: []string{"NATS server URL 1 of 2 cannot be read", "does not parse as a URL"}},
			{name: "a server URL whose password holds a comma after digits, split", urls: strings.Split(withPassword(digitsPassword), ","),
				env: remote, names: []string{"NATS server URL 2 of 2 cannot be read", "names none of the schemes nats, tls, ws and wss", "(%2C)"}},
			{name: "a server URL naming a token that holds a comma as its user, split", urls: strings.Split("nats://"+commaToken+"@127.0.0.1:1", ","),
				env: remote, names: []string{"NATS server URL 2 of 2 cannot be read", "names none of the schemes nats, tls, ws and wss", "(%2C)"}},
			{name: "a server URL naming a token that holds a comma and a scheme as its user, split", urls: strings.Split("nats://"+schemedToken+"@127.0.0.1:1", ","),
				env: remote, names: []string{"NATS server URL 2 of 2 cannot be read", "names none of the schemes nats, tls, ws and wss", "(%2C)"}},
			{name: "a server URL naming a token that holds a / as its user", urls: []string{"nats://" + slashedToken + "@127.0.0.1:1"},
				env: remote, names: []string{"NATS server URL 1 of 1 cannot be read", `an "@" stands after its host`}},
			{name: "a server URL that does not parse with the token in its query", urls: []string{"nats://127.0.0.1:1/%zz?token=" + token},
				names: unparsed},
			// A listener that takes the connection, so nats.go reaches its websocket handshake, as it
			// does against a server, where the handshake's parse error quotes the URL it rebuilt.
			{name: "a websocket URL with the token in its query whose path stops parsing once decoded", urls: []string{"ws://" + takingListener(t) + "/%25zz?token=" + token},
				names: []string{"NATS server URL 1 of 1 cannot be read", "does not parse once decoded", "%2525"}},
			{name: "a ws list's later URL with no scheme, the token in its query, whose path stops parsing once decoded",
				urls:  []string{"ws://127.0.0.1:1", takingListener(t) + "/%25zz?token=" + token},
				names: []string{"NATS server URL 2 of 2 cannot be read", "does not parse once decoded", "%2525"}},
		} {
			t.Run(row.name, func(t *testing.T) {
				if row.unreadable && os.Geteuid() == 0 {
					t.Skip("root reads a 0000 file")
				}
				urls := row.urls
				if urls == nil {
					urls = []string{"nats://127.0.0.1:1"}
				}
				nc, err := Dial("credential-printed-test", urls, lookupOf(row.env))
				if err == nil {
					nc.Close()
					t.Fatal("Dial connected with a credential it should refuse")
				}
				printed := err.Error()
				requirePrintsNoCredential(t, "the error", printed, credentials)
				for _, name := range row.names {
					require.Contains(t, printed, name, "the error does not name what it is about")
				}
			})
		}
		// The URLs these rules let through. A later URL that carries a user name and names a NATS
		// scheme; the first URL of a list, which names none, with a user name (index 0 is no comma's
		// tail); and a websocket URL whose path parses once decoded.
		for _, passes := range [][]string{
			{"nats://127.0.0.1:4222", "tls://legion:" + plainPassword + "@127.0.0.1:4223"},
			{"legion:" + plainPassword + "@127.0.0.1:4222"},
			{"ws://127.0.0.1:4222/nats%2Fsub?compress=1"},
		} {
			_, err := checkedNATSURLs(passes)
			require.NoError(t, err, "a list the rules let through was refused")
		}
		// The list is trimmed once, and the trimmed list is what every check and nats.go read: a URL
		// with white space around it whose password parses is dialled, in the background, and no
		// parse error quotes it.
		trimmed, err := checkedNATSURLs([]string{" nats://127.0.0.1:4222 ", "\t127.0.0.1:4223\n"})
		require.NoError(t, err)
		require.Equal(t, []string{"nats://127.0.0.1:4222", "127.0.0.1:4223"}, trimmed)
		untrimmed, err := Dial("credential-printed-test", []string{" " + withPassword(plainPassword) + " "}, lookupOf(nil), WithConnectInBackground())
		require.NoError(t, err, "a URL with white space around it was refused")
		untrimmed.Close()
	})

	t.Run("every line logged while a server refuses the connection", func(t *testing.T) {
		logs := captureLogs(t)
		logged := func(name, message string) bool {
			for _, record := range logs.records(t) {
				if record["name"] == name && record["msg"] == message {
					return true
				}
			}
			return false
		}
		requireLogged := func(what string, lines ...[2]string) {
			t.Helper()
			missing := func() (absent [][2]string) {
				for _, line := range lines {
					if !logged(line[0], line[1]) {
						absent = append(absent, line)
					}
				}
				return absent
			}
			if !testwait.Holds(30*time.Second, 100*time.Millisecond, func() bool { return len(missing()) == 0 }) {
				t.Fatalf("%s: no %q line logged within 30s, so the check below sees nothing of it:\n%s", what, missing(), logs)
			}
		}
		withUser := func(t *testing.T, raw, user string) string {
			t.Helper()
			parsed, err := url.Parse(raw)
			require.NoError(t, err)
			parsed.User = url.User(user)
			return parsed.String()
		}

		// The seed's user, admitted at first, is one the restarted server no longer has: nats.go
		// reconnects, is refused twice and closes the connection (TestANkeyConnectionRefusedTwice-
		// ClosesAsAtTheMergeBase).
		server, seedURL := testnats.StartNkeyAuthorizedServer(t, public)
		seeded, err := Dial("printed-seed-refused", []string{seedURL}, lookupOf(map[string]string{NkeySeedEnvVar: seed}))
		require.NoError(t, err)
		defer seeded.Close()
		_, other := testnats.User(t)
		server.Restart(t, fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ { nkey: %q } ]\n}\n", other))
		requireLogged("the refused seed's disconnect, refusal and close",
			[2]string{"printed-seed-refused", "envoy nats disconnected"}, [2]string{"printed-seed-refused", "envoy nats async error"},
			[2]string{"printed-seed-refused", "envoy nats connection closed"})

		// A service token the server refuses from the first connect, in the background, which logs
		// each refusal until the connection connects (reportRefusalsUntilConnected).
		_, serviceURL := testnats.StartConfig(t, "authorization {\n  users = [ { user: \"service\", password: \"another-service-password\" } ]\n}\n", nil, false)
		serviced, err := Dial("printed-token-refused", []string{serviceURL}, lookupOf(map[string]string{ServiceTokenFileEnvVar: tokenFile}), WithConnectInBackground())
		require.NoError(t, err)
		defer serviced.Close()

		// The same token written as the server URL's user name, which nats.go presents as a token.
		_, tokenServerURL := testnats.StartConfig(t, "authorization {\n  token: \"another-nats-token\"\n}\n", nil, false)
		refusedInURL, err := Dial("printed-url-token-refused", []string{withUser(t, tokenServerURL, token)}, lookupOf(nil), WithConnectInBackground())
		require.NoError(t, err)
		defer refusedInURL.Close()
		requireLogged("the refusals of the service token and of the URL's token",
			[2]string{"printed-token-refused", "envoy nats authorization refused"}, [2]string{"printed-url-token-refused", "envoy nats authorization refused"})

		// The token in the URL admitted: its connect, a slow consumer's asynchronous error with the
		// subscription's subject, the disconnect while the server is stopped, and the reconnect once
		// it starts again. testnats holds the stopped server's port and refuses each connection
		// there after accepting it, which nats.go reports to no callback.
		admitting, admittingURL := testnats.StartConfig(t, fmt.Sprintf("authorization {\n  token: %q\n}\n", token), nil, false)
		admittedInURL, err := Dial("printed-url-token-admitted", []string{withUser(t, admittingURL, token)}, lookupOf(nil), WithConnectInBackground())
		require.NoError(t, err)
		defer admittedInURL.Close()
		requireLogged("the URL token's connect", [2]string{"printed-url-token-admitted", "envoy nats connected"})
		slow, err := admittedInURL.SubscribeSync("printed.slow")
		require.NoError(t, err)
		require.NoError(t, slow.SetPendingLimits(1, -1))
		for range 5 {
			require.NoError(t, admittedInURL.Publish("printed.slow", []byte("{}")))
		}
		require.NoError(t, admittedInURL.Flush())
		requireLogged("the slow consumer", [2]string{"printed-url-token-admitted", "envoy nats async error"})
		admitting.Stop(t)
		requireLogged("the stopped server's disconnect", [2]string{"printed-url-token-admitted", "envoy nats disconnected"})
		admitting.Start(t)
		requireLogged("the reconnect", [2]string{"printed-url-token-admitted", "envoy nats reconnected"})

		// The token in the URL of a server nothing answers at: each failed dial is logged.
		unanswered, err := Dial("printed-url-token-unanswered", []string{"nats://" + token + "@127.0.0.1:1"}, lookupOf(nil), WithConnectInBackground())
		require.NoError(t, err)
		defer unanswered.Close()
		requireLogged("the failed dial", [2]string{"printed-url-token-unanswered", "envoy nats reconnect attempt failed"})

		// Every client closed, and its close logged, before the lines are read.
		for _, nc := range []*nats.Conn{seeded, serviced, refusedInURL, admittedInURL, unanswered} {
			nc.Close()
		}
		requireLogged("every client's close",
			[2]string{"printed-token-refused", "envoy nats connection closed"}, [2]string{"printed-url-token-refused", "envoy nats connection closed"},
			[2]string{"printed-url-token-admitted", "envoy nats connection closed"}, [2]string{"printed-url-token-unanswered", "envoy nats connection closed"})
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			requirePrintsNoCredential(t, "the logged line", line, credentials)
		}
	})
}
