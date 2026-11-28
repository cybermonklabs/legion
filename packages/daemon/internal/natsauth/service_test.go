package natsauth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/testnatscallout"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	notices "github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// daemonAccount is the ServiceAccount whose projected token the daemon presents in these tests, and
// daemonServices the hub's BROKER_NATS_SERVICES: that account under a daemon/-/* entry, which the
// auth service answers with natsgrant.Daemon() whole. daemonUser is the user it admits the daemon's
// connection as (its decision's name), which /connz?auth=true reports as that connection's
// authorized user.
const (
	daemonAccount  = "system:serviceaccount:legion-example:legion-daemon"
	daemonServices = "daemon/-/*=" + daemonAccount
	daemonUser     = "daemon/" + daemonAccount
)

// connectTimeout bounds a connect's dial and handshake, which here wait on the auth service's
// decision: room for a loaded machine, as natscred's rig tests leave their connects.
const connectTimeout = 10 * time.Second

// readServiceToken is these tests' verifier of the daemon's token in the broker's place
// (testnatscallout.Verifier): the account, issue and expiry the unsigned token names, read
// unverified, as natscred's rig reads a platform token (natscred/rig_test.go, readPlatformToken).
func readServiceToken(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false, time.Time{}, time.Time{}, errors.New("no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	var claims struct {
		Sub      string `json:"sub"`
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false, time.Time{}, time.Time{}, fmt.Errorf("claims: %w", err)
	}
	return claims.Sub, false, time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0), nil
}

// refusingVerifier is readServiceToken behind a count of refusals still to make: it refuses that
// many of the tokens it is given next, as the broker refuses a token while it cannot read the key
// set of the token's issuer, and reads every other one. The auth service asks it on goroutines of
// its own.
type refusingVerifier struct {
	mu     sync.Mutex
	refuse int
}

// refuseNext makes the verifier refuse the next n tokens it is given.
func (v *refusingVerifier) refuseNext(n int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refuse = n
}

func (v *refusingVerifier) verify(ctx context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	v.mu.Lock()
	refuse := v.refuse > 0
	if refuse {
		v.refuse--
	}
	v.mu.Unlock()
	if refuse {
		return "", false, time.Time{}, time.Time{}, errors.New("the key set of the token's issuer could not be read")
	}
	return readServiceToken(ctx, raw)
}

// writeToken writes token to path as the kubelet writes a projected token: whole, to a file of its
// own beside path, renamed over the one before, so no reader reads a part of it. The mode is a
// projected token's, 0644, which nothing that reads the token holds against it.
func writeToken(t *testing.T, path, token string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(token+"\n"), 0o644); err != nil {
		t.Fatalf("write the token beside its file: %v", err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatalf("rename the token over its file: %v", err)
	}
}

// tokenFile is a file of its own under t's temporary directory holding token, written as
// writeToken writes it.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, token)
	return path
}

// connectService is natsauth.ConnectService of the token in file, under the configuration key, to
// hub, named for t, with the options the daemon's workflow gives it (LogEvents into logs, or
// nowhere when logs is nil, and ReconnectForever) and connectTimeout, then options; it is closed
// when t ends, and a connect that fails fails t with the auth service's log.
func connectService(t *testing.T, hub *testnatscallout.Hub, file string, logs *lockedBuffer, options ...nats.Option) *nats.Conn {
	t.Helper()
	var sink io.Writer = io.Discard
	if logs != nil {
		sink = logs
	}
	conn, err := natsauth.ConnectService([]string{hub.URL()}, file, natsauth.ServiceTokenFileKey, t.Name(),
		append([]nats.Option{nats.Timeout(connectTimeout), natsauth.LogEvents(slog.New(slog.NewTextHandler(sink, nil))), natsauth.ReconnectForever()}, options...)...)
	if err != nil {
		t.Fatalf("ConnectService through the auth service: %v\nthe auth service's log:\n%s", err, hub.ServiceLog())
	}
	t.Cleanup(conn.Close)
	return conn
}

// hubConnection is one client connection as the hub's monitoring endpoint reports it with
// auth=true: the user it was authorized as, which for a client the auth service admitted is the
// name its decision gave (daemon/<account> for a daemon entry), and for a static user its public
// key (testnatscallout.Hub.StaticKeys).
type hubConnection struct {
	CID            uint64 `json:"cid"`
	Name           string `json:"name"`
	AuthorizedUser string `json:"authorized_user"`
}

// hubConnections is every client connection the hub's monitoring endpoint lists at /connz?query.
func hubConnections(t *testing.T, hub *testnatscallout.Hub, query string) []hubConnection {
	t.Helper()
	monitor := hub.MonitorURL()
	if monitor == "" {
		t.Fatal("the hub's server is stopped: it has no monitoring endpoint to read")
	}
	response, err := http.Get(monitor + "/connz?" + query)
	if err != nil {
		t.Fatalf("read the hub's /connz?%s: %v", query, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the hub's /connz?%s answered %s", query, response.Status)
	}
	var connz struct {
		Connections []hubConnection `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
		t.Fatalf("decode the hub's /connz?%s: %v", query, err)
	}
	return connz.Connections
}

// authorizedAs is the user the hub authorized conn's open connection as, read from its monitoring
// endpoint by the client id the server gave that connection.
func authorizedAs(t *testing.T, hub *testnatscallout.Hub, conn *nats.Conn) string {
	t.Helper()
	cid, err := conn.GetClientID()
	if err != nil {
		t.Fatalf("the connection's client id: %v", err)
	}
	listed := hubConnections(t, hub, "auth=true&cid="+strconv.FormatUint(cid, 10))
	if len(listed) != 1 || listed[0].CID != cid {
		t.Fatalf("the hub lists %d open connections for client id %d, want the connection's one", len(listed), cid)
	}
	return listed[0].AuthorizedUser
}

// decidedConnections is each connection the hub lists at /connz?auth=true&state=all, open or
// closed, but its two static users' (testnatscallout.Hub.StaticKeys: the admin's and the auth
// service's own): every connection the auth service decided, by the client id the server gave it.
func decidedConnections(t *testing.T, hub *testnatscallout.Hub) map[uint64]hubConnection {
	t.Helper()
	static := hub.StaticKeys()
	decided := map[uint64]hubConnection{}
	for _, each := range hubConnections(t, hub, "auth=true&state=all") {
		if !slices.Contains(static, each.AuthorizedUser) {
			decided[each.CID] = each
		}
	}
	return decided
}

// requireEveryDecidedConnectionIsTheDaemon fails t naming each of decided that the hub authorized
// as anyone but the daemon (daemonUser).
func requireEveryDecidedConnectionIsTheDaemon(t *testing.T, decided map[uint64]hubConnection) {
	t.Helper()
	for _, each := range decided {
		if each.AuthorizedUser != daemonUser {
			t.Errorf("the hub lists connection %d %q authorized as %q; every connection but the hub's two static users' should be the daemon's, authorized as %s",
				each.CID, each.Name, each.AuthorizedUser, daemonUser)
		}
	}
}

// requirePresentedInOrder fails t unless every token the auth service was given (given) is one of
// held, the tokens the file held in the order it held them, and they were given in that order:
// each CONNECT presented the token the file held when it read it, and none made after a rewrite
// presented a token from before it.
func requirePresentedInOrder(t *testing.T, given []string, held ...string) {
	t.Helper()
	last := 0
	for i, token := range given {
		at := slices.Index(held, token)
		switch {
		case at < 0:
			t.Errorf("CONNECT %d of %d presented a token the file never held", i+1, len(given))
		case at < last:
			t.Errorf("CONNECT %d of %d presented the file's token %d of %d after another CONNECT had presented its token %d: a token from before a rewrite was presented after it",
				i+1, len(given), at+1, len(held), last+1)
		default:
			last = at
		}
	}
}

// presentations is how many of given are token.
func presentations(given []string, token string) int {
	n := 0
	for _, each := range given {
		if each == token {
			n++
		}
	}
	return n
}

// linesNaming is each line of text that holds every one of words, case aside.
func linesNaming(text string, words ...string) []string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		lower := strings.ToLower(line)
		if !slices.ContainsFunc(words, func(word string) bool { return !strings.Contains(lower, strings.ToLower(word)) }) {
			lines = append(lines, line)
		}
	}
	return lines
}

// permissionRefusals is every line in which the daemon's log (LogEvents) or the hub's server log
// names a permission the server refused the daemon's connection, the subject among them, joined.
func permissionRefusals(logs *lockedBuffer, hub *testnatscallout.Hub) string {
	lines := linesNaming(logs.String(), "NATS refused the daemon a permission")
	lines = append(lines, linesNaming(hub.ServerLog(), "Publish Violation")...)
	lines = append(lines, linesNaming(hub.ServerLog(), "Subscription Violation")...)
	return strings.Join(lines, "\n")
}

// The daemon's own credential is its service token when either form of the setting names a file
// (natsauth.DaemonCredential): the key, nats_daemon_service_token_file (the path legion.yaml's
// directory resolved, config.Config.NatsDaemonServiceTokenFile), wins over the variable,
// NATS_DAEMON_SERVICE_TOKEN_FILE, as the seed's key wins over its variables; the variable alone is
// the token too; and the variable set but empty is refused naming it, never read as no setting.
// Each answer names the file, the setting that named it and the account the file's token names,
// and presents no seed, though a pane seed is set beside it. With neither set the answer is
// today's: the daemon seed by its key or its variable, else the pane seed, else nothing.
func TestTheDaemonServiceTokenFileResolvesByItsKeyThenItsVariable(t *testing.T) {
	t.Parallel()
	const (
		keyAccount      = "system:serviceaccount:legion-example:named-by-the-key"
		variableAccount = "system:serviceaccount:legion-example:named-by-the-variable"
	)
	fromKey := tokenFile(t, testnkeys.ServiceToken(keyAccount, time.Now().Add(time.Hour)))
	fromVariable := tokenFile(t, testnkeys.ServiceToken(variableAccount, time.Now().Add(time.Hour)))
	daemonSeed, paneSeed := testnkeys.UserSeed(t), testnkeys.UserSeed(t)
	seeds := map[string]string{"the daemon seed": daemonSeed, "the pane seed": paneSeed}
	urls := []string{"nats://127.0.0.1:4222"}
	for _, tc := range []struct {
		name                         string
		seedFile, tokenFile          string
		env                          map[string]string
		paneSeed                     string
		source                       natsauth.Source
		file, setting, account, seed string
	}{
		{
			name: "the key, over the variable", tokenFile: fromKey,
			env: map[string]string{natsauth.ServiceTokenFileVariable: fromVariable}, paneSeed: paneSeed,
			source: natsauth.SourceServiceToken, file: fromKey, setting: natsauth.ServiceTokenFileKey, account: keyAccount,
		},
		{
			name: "the variable alone", env: map[string]string{natsauth.ServiceTokenFileVariable: fromVariable}, paneSeed: paneSeed,
			source: natsauth.SourceServiceToken, file: fromVariable, setting: natsauth.ServiceTokenFileVariable, account: variableAccount,
		},
		{
			name: "neither, beside the daemon seed's key", seedFile: testnkeys.SeedFile(t, daemonSeed), paneSeed: paneSeed,
			source: natsauth.SourceDaemonSeed, seed: daemonSeed,
		},
		{
			name: "neither, beside the daemon seed's variable", env: map[string]string{natsauth.DaemonSeedVariable: daemonSeed}, paneSeed: paneSeed,
			source: natsauth.SourceDaemonSeed, seed: daemonSeed,
		},
		{name: "neither, beside the pane seed alone", paneSeed: paneSeed, source: natsauth.SourcePaneSeed, seed: paneSeed},
		{name: "neither, and no seed", source: natsauth.SourceNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential, err := natsauth.DaemonCredential(tc.seedFile, tc.tokenFile, urls, tc.paneSeed, env(tc.env))
			if err != nil {
				t.Fatalf("DaemonCredential refused the case: %v", err)
			}
			if credential.Source() != tc.source {
				t.Errorf("DaemonCredential's source = %q, want %q", credential.Source(), tc.source)
			}
			if file, setting, account := credential.ServiceToken(); file != tc.file || setting != tc.setting || account != tc.account {
				t.Errorf("DaemonCredential's service token = file %q, setting %q, account %q; want %q, %q, %q", file, setting, account, tc.file, tc.setting, tc.account)
			}
			if credential.Seed() != tc.seed {
				t.Errorf("DaemonCredential presents %s, want %s", seedNamed(credential.Seed(), seeds), seedNamed(tc.seed, seeds))
			}
		})
	}
	t.Run("the variable set but empty", func(t *testing.T) {
		_, err := natsauth.DaemonCredential("", "", urls, paneSeed, env(map[string]string{natsauth.ServiceTokenFileVariable: ""}))
		if err == nil {
			t.Fatalf("DaemonCredential took %s set but empty as no setting", natsauth.ServiceTokenFileVariable)
		}
		if !strings.Contains(err.Error(), natsauth.ServiceTokenFileVariable) {
			t.Errorf("the refusal of %s set but empty does not name it: %v", natsauth.ServiceTokenFileVariable, err)
		}
		requirePrintsNoCredential(t, "the refusal", err.Error(), []printedCredential{{"the pane seed", paneSeed}})
	})
}

// seedNamed is which of seeds (each by its name) seed is, so a failure says which seed a
// credential presents without printing it.
func seedNamed(seed string, seeds map[string]string) string {
	if seed == "" {
		return "no seed"
	}
	for name, each := range seeds {
		if each == seed {
			return name
		}
	}
	return "a seed the test never set"
}

// The service token is refused beside each setting of the daemon seed, on presence alone and
// before any seed is read (natsauth.RefuseServiceTokenBesideDaemonSeed, which DaemonCredential runs
// first): the token's key or its variable beside nats_daemon_nkey_seed_file,
// NATS_DAEMON_NKEY_SEED_FILE or NATS_DAEMON_NKEY_SEED, six rows, each refused naming the two
// settings set, in the bus's words. Each seed setting here names a file that does not exist or
// holds a value that is no seed, and each token setting a file that does not exist, so a resolver
// that read either before it refused would answer that read's error instead: the refusal is made on
// what is set, not on what it holds. A seed variable set but empty is set. Where more than one
// setting of a side is set, the refusal names the one that wins: the token's key over its variable;
// the seed's key, then its file variable, then its value variable. No refusal carries a path or the
// value set. The controls: the token alone, and a daemon seed setting alone, are no refusal.
func TestTheDaemonServiceTokenIsRefusedBesideEachDaemonSeedSetting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token-that-does-not-exist")
	seedPath := filepath.Join(dir, "seed-that-does-not-exist")
	const notASeed = "madeUpValueThatIsNoNkeySeed0123456789"
	urls := []string{"nats://127.0.0.1:4222"}
	for _, tc := range []struct {
		name                string
		seedFile, tokenFile string
		env                 map[string]string
		token, seed         string
	}{
		{"the key beside the seed's key", seedPath, tokenPath, nil, natsauth.ServiceTokenFileKey, natsauth.DaemonSeedFileKey},
		{"the key beside the seed's file variable", "", tokenPath,
			map[string]string{natsauth.DaemonSeedFileVariable: seedPath}, natsauth.ServiceTokenFileKey, natsauth.DaemonSeedFileVariable},
		{"the key beside the seed's value variable", "", tokenPath,
			map[string]string{natsauth.DaemonSeedVariable: notASeed}, natsauth.ServiceTokenFileKey, natsauth.DaemonSeedVariable},
		{"the variable beside the seed's key", seedPath, "",
			map[string]string{natsauth.ServiceTokenFileVariable: tokenPath}, natsauth.ServiceTokenFileVariable, natsauth.DaemonSeedFileKey},
		{"the variable beside the seed's file variable", "", "",
			map[string]string{natsauth.ServiceTokenFileVariable: tokenPath, natsauth.DaemonSeedFileVariable: seedPath},
			natsauth.ServiceTokenFileVariable, natsauth.DaemonSeedFileVariable},
		{"the variable beside the seed's value variable", "", "",
			map[string]string{natsauth.ServiceTokenFileVariable: tokenPath, natsauth.DaemonSeedVariable: notASeed},
			natsauth.ServiceTokenFileVariable, natsauth.DaemonSeedVariable},
		{"the key beside the seed's file variable set but empty", "", tokenPath,
			map[string]string{natsauth.DaemonSeedFileVariable: ""}, natsauth.ServiceTokenFileKey, natsauth.DaemonSeedFileVariable},
		{"the variable beside the seed's value variable set but empty", "", "",
			map[string]string{natsauth.ServiceTokenFileVariable: tokenPath, natsauth.DaemonSeedVariable: ""},
			natsauth.ServiceTokenFileVariable, natsauth.DaemonSeedVariable},
		{"both token settings beside all three seed settings: the key and the seed's key win", seedPath, tokenPath,
			map[string]string{natsauth.ServiceTokenFileVariable: tokenPath, natsauth.DaemonSeedFileVariable: seedPath, natsauth.DaemonSeedVariable: notASeed},
			natsauth.ServiceTokenFileKey, natsauth.DaemonSeedFileKey},
		{"the variable beside both seed variables: the seed's file variable wins", "", "",
			map[string]string{natsauth.ServiceTokenFileVariable: tokenPath, natsauth.DaemonSeedFileVariable: seedPath, natsauth.DaemonSeedVariable: notASeed},
			natsauth.ServiceTokenFileVariable, natsauth.DaemonSeedFileVariable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := fmt.Sprintf("%s and %s are both set; the daemon's own NATS connection presents one credential, so set one", tc.token, tc.seed)
			presence := natsauth.RefuseServiceTokenBesideDaemonSeed(tc.seedFile, tc.tokenFile, env(tc.env))
			_, resolved := natsauth.DaemonCredential(tc.seedFile, tc.tokenFile, urls, "", env(tc.env))
			for _, refusal := range []struct {
				what string
				err  error
			}{{"RefuseServiceTokenBesideDaemonSeed", presence}, {"DaemonCredential", resolved}} {
				if refusal.err == nil {
					t.Errorf("%s accepted %s beside %s", refusal.what, tc.token, tc.seed)
					continue
				}
				if !strings.Contains(refusal.err.Error(), want) {
					t.Errorf("%s = %v\nwant the refusal naming both settings: %s", refusal.what, refusal.err, want)
				}
				for _, path := range []string{tokenPath, seedPath} {
					if strings.Contains(refusal.err.Error(), path) {
						t.Errorf("%s's refusal names the path %s: %v", refusal.what, path, refusal.err)
					}
				}
				requirePrintsNoCredential(t, refusal.what+"'s refusal", refusal.err.Error(), []printedCredential{{"the value set as the seed", notASeed}})
			}
		})
	}
	t.Run("control: the token alone, or a daemon seed setting alone, is no refusal", func(t *testing.T) {
		if err := natsauth.RefuseServiceTokenBesideDaemonSeed("", tokenPath, env(nil)); err != nil {
			t.Errorf("RefuseServiceTokenBesideDaemonSeed refused the token's key alone: %v", err)
		}
		if err := natsauth.RefuseServiceTokenBesideDaemonSeed("", "", env(map[string]string{natsauth.ServiceTokenFileVariable: tokenPath})); err != nil {
			t.Errorf("RefuseServiceTokenBesideDaemonSeed refused the token's variable alone: %v", err)
		}
		if err := natsauth.RefuseServiceTokenBesideDaemonSeed(seedPath, "", env(map[string]string{natsauth.DaemonSeedFileVariable: seedPath, natsauth.DaemonSeedVariable: notASeed})); err != nil {
			t.Errorf("RefuseServiceTokenBesideDaemonSeed refused the daemon seed's settings with no token set: %v", err)
		}
	})
}

// A token file the daemon cannot use is refused when the daemon resolves its credential at its
// start (natsauth.ServiceIdentity, which DaemonCredential runs), naming the setting that named it,
// through the bus's reader (natscred.ServiceFile) and natscred.Identity: a file that cannot be read
// by the setting and the system's reason alone, never its path, since the path is where a person
// who pasted the token itself puts it; a file that is empty by its path; a file whose token names
// no account by the setting and the path. The token pasted where the path goes is refused as a
// file that cannot be read, and no refusal holds a run of printedRun characters of it or of the
// token that names no account. Under either setting, a file holding a token that names an account
// answers that account.
func TestAnUnusableDaemonServiceTokenFileIsRefusedNamingItsSetting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	token := testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour))
	unnamedToken := testnkeys.ServiceToken("", time.Now().Add(time.Hour))
	good := tokenFile(t, token)
	missing := filepath.Join(dir, "token-that-does-not-exist")
	empty := filepath.Join(dir, "empty-token")
	writeToken(t, empty, "")
	unnamed := filepath.Join(dir, "token-naming-no-account")
	writeToken(t, unnamed, unnamedToken)
	credentials := []printedCredential{{"the token", token}, {"the token that names no account", unnamedToken}}
	urls := []string{"nats://127.0.0.1:4222"}
	for _, setting := range []string{natsauth.ServiceTokenFileKey, natsauth.ServiceTokenFileVariable} {
		resolve := func(path string) (natsauth.Credential, error) {
			if setting == natsauth.ServiceTokenFileKey {
				return natsauth.DaemonCredential("", path, urls, "", env(nil))
			}
			return natsauth.DaemonCredential("", "", urls, "", env(map[string]string{natsauth.ServiceTokenFileVariable: path}))
		}
		t.Run(setting, func(t *testing.T) {
			if account, err := natsauth.ServiceIdentity(good, setting); err != nil || account != daemonAccount {
				t.Errorf("ServiceIdentity of a usable file = %q, %v; want %q", account, err, daemonAccount)
			}
			if credential, err := resolve(good); err != nil {
				t.Errorf("DaemonCredential refused a usable file: %v", err)
			} else if _, _, account := credential.ServiceToken(); account != daemonAccount {
				t.Errorf("DaemonCredential's account of a usable file = %q, want %q", account, daemonAccount)
			}
			for _, tc := range []struct {
				name, path   string
				names, hides []string
			}{
				{"a file that does not exist", missing, []string{setting, syscall.ENOENT.Error()}, []string{missing}},
				{"the token pasted where the path goes", token, []string{setting, syscall.ENOENT.Error()}, nil},
				{"an empty file", empty, []string{setting, empty}, nil},
				{"a file whose token names no account", unnamed, []string{setting, unnamed}, nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, identified := natsauth.ServiceIdentity(tc.path, setting)
					_, resolved := resolve(tc.path)
					for _, refusal := range []struct {
						what string
						err  error
					}{{"ServiceIdentity", identified}, {"DaemonCredential", resolved}} {
						if refusal.err == nil {
							t.Errorf("%s accepted %s", refusal.what, tc.name)
							continue
						}
						for _, want := range tc.names {
							if !strings.Contains(refusal.err.Error(), want) {
								t.Errorf("%s's refusal of %s does not name %q: %v", refusal.what, tc.name, want, refusal.err)
							}
						}
						for _, hidden := range tc.hides {
							if strings.Contains(refusal.err.Error(), hidden) {
								t.Errorf("%s's refusal of %s names the path %s: %v", refusal.what, tc.name, hidden, refusal.err)
							}
						}
						requirePrintsNoCredential(t, refusal.what+"'s refusal", refusal.err.Error(), credentials)
					}
				})
			}
		})
	}
}

// Beside the service token, a nats_urls entry that carries userinfo is refused (D5): nats.go
// presents a URL's own userinfo on the CONNECT and never asks for the token then, so the daemon
// would connect as that user and not as user service with its token. DaemonCredential and
// ConnectService each refuse it before any dial (no host here resolves), naming the entry by its
// position in the list alone, as an unreadable entry is named, and printing no part of it: a
// user-only entry, which nats.go presents as a token, and a user:password one. The control is the
// list without the userinfo, which DaemonCredential answers with the token.
func TestTheDaemonServiceTokenIsRefusedBesideANATSURLWithUserinfo(t *testing.T) {
	t.Parallel()
	const (
		madeUpUser = "madeUpNatsUser"
		madeUpWord = "nats-password"
		host       = "userinfo-entry.example:4222"
	)
	file := tokenFile(t, testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour)))
	credentials := []printedCredential{{"the URL's user", madeUpUser}, {"the URL's password", madeUpWord}}
	for _, tc := range []struct {
		name     string
		urls     []string
		position string
	}{
		{"a user alone, second of three",
			[]string{"nats://first.example:4222", "nats://" + madeUpUser + "@" + host, "nats://third.example:4222"}, "2 of 3"},
		{"a user and a password, third of three",
			[]string{"nats://first.example:4222", "nats://second.example:4222", "nats://legion:" + madeUpWord + "@" + host}, "3 of 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, resolved := natsauth.DaemonCredential("", file, tc.urls, "", env(nil))
			conn, connected := natsauth.ConnectService(tc.urls, file, natsauth.ServiceTokenFileKey, t.Name(), nats.Timeout(time.Second))
			if connected == nil {
				conn.Close()
			}
			for _, refusal := range []struct {
				what string
				err  error
			}{{"DaemonCredential", resolved}, {"ConnectService", connected}} {
				if refusal.err == nil {
					t.Errorf("%s accepted a NATS URL with userinfo beside the service token", refusal.what)
					continue
				}
				if !strings.Contains(refusal.err.Error(), tc.position) {
					t.Errorf("%s's refusal does not name the entry by its position, %s: %v", refusal.what, tc.position, refusal.err)
				}
				if strings.Contains(refusal.err.Error(), host) {
					t.Errorf("%s's refusal prints the entry's host: %v", refusal.what, refusal.err)
				}
				requirePrintsNoCredential(t, refusal.what+"'s refusal", refusal.err.Error(), credentials)
			}
		})
	}
	t.Run("control: the list without userinfo", func(t *testing.T) {
		credential, err := natsauth.DaemonCredential("", file, []string{"nats://first.example:4222", "nats://" + host}, "", env(nil))
		if err != nil || credential.Source() != natsauth.SourceServiceToken {
			t.Fatalf("DaemonCredential with no userinfo in the list = source %q, %v; want the service token", credential.Source(), err)
		}
	})
}

// createNotificationStream creates ENVOY_NOTIFICATIONS, the daemon's stream, as the hub's admin,
// as the daemon's grant tests create it (internal/daemon's createDaemonNotificationStream).
func createNotificationStream(t *testing.T, admin *nats.Conn) {
	t.Helper()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("open JetStream as the hub's admin: %v", err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create the notification stream as the hub's admin: %v", err)
	}
}

// The daemon's service-token connection is admitted by the real NATS auth service as the daemon,
// with today's grant whole: ConnectService presents user service and the token its file holds
// (natscred.Service), the hub sends the CONNECT to the auth service, which is given that token,
// verifies it with the test's verifier and admits it under the daemon/-/* entry that lists its
// account. The hub's monitoring endpoint reports the connection authorized as daemon/<account>, as
// it does every connection it lists but its two static users', and the service has refused nothing
// as no_credential. Under natsgrant.Daemon() the connection subscribes the role-lane exception
// subject subscribeNoticeExceptions takes and receives the admin's publish on it, and a JetStream
// STREAM.INFO request on ENVOY_NOTIFICATIONS is answered on _INBOX, the reply subjects the grant
// lets it subscribe (D7: no inbox prefix of its own). A refused subscription or publish fails the
// test naming the subject, from the daemon's log (LogEvents) and the server's.
//
// The control is a connection with no credential (natsauth.Connect with no seed): the hub sends
// it to the auth service too, which refuses it, counting a no_credential refusal, and the connect
// fails with the authorization violation.
func TestAServiceTokenConnectionIsAdmittedByTheAuthServiceAsTheDaemon(t *testing.T) {
	t.Parallel()
	hub := testnatscallout.Start(t, daemonServices, readServiceToken)
	admin := hub.Admin(t)
	createNotificationStream(t, admin)
	token := testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour))
	logs := &lockedBuffer{}
	conn := connectService(t, hub, tokenFile(t, token), logs)

	given := hub.Tokens()
	if len(given) == 0 || slices.ContainsFunc(given, func(each string) bool { return each != token }) {
		t.Fatalf("the auth service was given %d tokens, %d of them the file's; want the file's token at every CONNECT",
			len(given), presentations(given, token))
	}
	if user := authorizedAs(t, hub, conn); user != daemonUser {
		t.Fatalf("the hub authorized the service-token connection as %q, want %s", user, daemonUser)
	}
	requireEveryDecidedConnectionIsTheDaemon(t, decidedConnections(t, hub))
	if refused := hub.Refused(t, "no_credential"); refused != 0 {
		t.Fatalf("the auth service refused %d connections as no_credential, want none\n%s", refused, hub.ServiceLog())
	}

	exceptions := "notifications.envoy.exceptions." + notices.RoleTopicPrefix + "example"
	received := make(chan []byte, 1)
	subscription, err := conn.Subscribe(exceptions, func(msg *nats.Msg) {
		select {
		case received <- msg.Data:
		default:
		}
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", exceptions, err)
	}
	defer subscription.Unsubscribe()
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription to %s: %v", exceptions, err)
	}
	if err := admin.Publish(exceptions, []byte("retry this notice")); err != nil {
		t.Fatalf("publish the role-lane exception as the admin: %v", err)
	}
	if err := admin.Flush(); err != nil {
		t.Fatalf("flush the role-lane exception: %v", err)
	}
	var got []byte
	testwait.Eventually(t, "the daemon receiving the role-lane exception, or a refusal of its subscription", func() bool {
		select {
		case got = <-received:
			return true
		default:
			return permissionRefusals(logs, hub) != ""
		}
	})
	if got == nil {
		t.Fatalf("the auth service's grant refused the daemon's subscription to %s:\n%s", exceptions, permissionRefusals(logs, hub))
	}
	if string(got) != "retry this notice" {
		t.Fatalf("the role-lane exception = %q, want %q", got, "retry this notice")
	}

	const streamInfo = "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS"
	asking, cancel := context.WithTimeout(testwait.Context(t), connectTimeout)
	defer cancel()
	reply, err := conn.RequestWithContext(asking, streamInfo, nil)
	if err != nil {
		t.Fatalf("%s went unanswered on the service-token connection: %v\n%s", streamInfo, err, permissionRefusals(logs, hub))
	}
	if !strings.HasPrefix(reply.Subject, nats.InboxPrefix) {
		t.Errorf("%s was answered on %s, want a subject under %s>, the replies natsgrant.Daemon() lets the daemon subscribe", streamInfo, reply.Subject, nats.InboxPrefix)
	}
	var info struct {
		Config struct {
			Name string `json:"name"`
		} `json:"config"`
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &info); err != nil || info.Error != nil || info.Config.Name != "ENVOY_NOTIFICATIONS" {
		t.Fatalf("%s answered %s (decode error %v), want ENVOY_NOTIFICATIONS's info", streamInfo, reply.Data, err)
	}
	if refusals := permissionRefusals(logs, hub); refusals != "" {
		t.Fatalf("the daemon's connection was refused a permission:\n%s", refusals)
	}
	requirePrintsNoCredential(t, "the daemon's log", logs.String(), []printedCredential{{"the token", token}})

	t.Run("control: no credential", func(t *testing.T) {
		control, err := natsauth.Connect([]string{hub.URL()}, "", t.Name(), nats.Timeout(connectTimeout))
		if err == nil {
			control.Close()
			t.Fatal("the hub admitted a connection that presented no credential")
		}
		if !errors.Is(err, nats.ErrAuthorization) {
			t.Fatalf("the connect with no credential failed with %v, want the authorization violation", err)
		}
		testwait.Eventually(t, "the auth service counting a no_credential refusal", func() bool {
			return hub.Refused(t, "no_credential") >= 1
		})
	})
}

// The connection reads its token file again at every CONNECT: the kubelet rewrites a projected
// token in place well before it lapses, and the auth service admits the user it answers only until
// the token it was given lapses, so a reconnect presents the token the file holds then. Here the
// file is rewritten with a later token (whole, by a rename, as the kubelet does) and the connection
// is cut (ForceReconnect): the auth service is given the later token at the reconnect, after the
// first one at the connect and never the first after the rewrite, and admits the reconnect as the
// daemon; the connection ends connected, as a new client of the hub's.
func TestAServiceTokenConnectionReadsItsFileAgainAtEachConnectThroughTheAuthService(t *testing.T) {
	t.Parallel()
	hub := testnatscallout.Start(t, daemonServices, readServiceToken)
	first := testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour))
	later := testnkeys.ServiceToken(daemonAccount, time.Now().Add(50*time.Minute))
	file := tokenFile(t, first)
	conn := connectService(t, hub, file, nil)
	before, err := conn.GetClientID()
	if err != nil {
		t.Fatalf("the connection's client id: %v", err)
	}

	writeToken(t, file, later)
	if err := conn.ForceReconnect(); err != nil {
		t.Fatalf("cut the connection: %v", err)
	}
	testwait.Eventually(t, "the reconnect presenting the file's later token, connected", func() bool {
		given := hub.Tokens()
		return conn.IsConnected() && len(given) >= 2 && given[len(given)-1] == later
	})
	given := hub.Tokens()
	if given[0] != first {
		t.Errorf("the first CONNECT presented another token than the file held")
	}
	requirePresentedInOrder(t, given, first, later)
	after, err := conn.GetClientID()
	if err != nil {
		t.Fatalf("the reconnected connection's client id: %v", err)
	}
	if after == before {
		t.Errorf("the connection kept client id %d across the cut: it never reconnected", before)
	}
	if user := authorizedAs(t, hub, conn); user != daemonUser {
		t.Errorf("the hub authorized the reconnect as %q, want %s", user, daemonUser)
	}
}

// keeperBound is how long a test waits for natscred's keeper to move a connection to a later
// token: it wakes ServiceRenewBefore before the token it presented lapses, two seconds after the
// test began here, reads the file each second and reconnects once its probe is admitted, so a few
// seconds suffice, and the bound leaves a loaded machine room. It is far inside the first token's
// life, which is what a keeper that never runs is measured against.
const keeperBound = 30 * time.Second

// The connection moves to a later token before the one it presented lapses (natscred's keeper,
// which ConnectService starts beside the connection and the connection's close ends): the auth
// service answers the daemon's user with its token's expiry, and the server closes the connection
// when that user expires. Here the first token lapses natscred.ServiceRenewBefore and two seconds
// from now, and the kubelet's rewrite of the file is a later token; the auth service is given the
// later token by the keeper's probe and again by the reconnect the keeper makes once the probe is
// admitted, well before the first lapses, and the connection is connected after them, admitted as
// the daemon. The control is a connection whose keeper does not run: it never presents the later
// token, and the wait fails at keeperBound naming it.
func TestAServiceTokenConnectionMovesToALaterTokenBeforeItLapsesThroughTheAuthService(t *testing.T) {
	t.Parallel()
	hub := testnatscallout.Start(t, daemonServices, readServiceToken)
	first := testnkeys.ServiceToken(daemonAccount, time.Now().Add(natscred.ServiceRenewBefore+2*time.Second))
	file := tokenFile(t, first)
	conn := connectService(t, hub, file, nil)

	later := testnkeys.ServiceToken(daemonAccount, time.Now().Add(50*time.Minute))
	writeToken(t, file, later)
	testwait.WithinPolling(t, keeperBound, 100*time.Millisecond,
		"the keeper's probe and its reconnect presenting the later token before the first lapsed, the connection connected after them", func() bool {
			return presentations(hub.Tokens(), later) >= 2 && conn.IsConnected()
		})
	given := hub.Tokens()
	if given[0] != first {
		t.Errorf("the first CONNECT presented another token than the file held")
	}
	requirePresentedInOrder(t, given, first, later)
	if user := authorizedAs(t, hub, conn); user != daemonUser {
		t.Errorf("the hub authorized the keeper's reconnect as %q, want %s", user, daemonUser)
	}
}

// Every connection a service-token connection opens presents the token its file holds at that
// moment, and each is admitted as the daemon: the class "every NATS connection the daemon opens"
// (R1) at the connection's level, whose members are its first connect, each reconnect nats.go
// makes and the keeper's probe dial. In one run the connection connects with a token that lapses
// soon; the keeper probes with the kubelet's later token and reconnects with it (as
// TestAServiceTokenConnectionMovesToALaterTokenBeforeItLapsesThroughTheAuthService); then the file
// is rewritten again and the connection cut (as
// TestAServiceTokenConnectionReadsItsFileAgainAtEachConnectThroughTheAuthService). Each token the
// auth service was given is one the file held, given in the order the file held them, so none came
// from anywhere else or outlived its rewrite; the service refused nothing as no_credential; and the
// hub lists, open or closed, one connection for each token the service was given, the keeper's
// closed probe (named for the connection, with " probe") among them, every one of them but the
// hub's two static users' (the admin and the auth service's own) authorized as daemon/<account>.
func TestEveryConnectionOfAServiceTokenConnectionPresentsTheTokenThroughTheAuthService(t *testing.T) {
	t.Parallel()
	hub := testnatscallout.Start(t, daemonServices, readServiceToken)
	first := testnkeys.ServiceToken(daemonAccount, time.Now().Add(natscred.ServiceRenewBefore+2*time.Second))
	file := tokenFile(t, first)
	conn := connectService(t, hub, file, nil)

	renewed := testnkeys.ServiceToken(daemonAccount, time.Now().Add(55*time.Minute))
	writeToken(t, file, renewed)
	testwait.WithinPolling(t, keeperBound, 100*time.Millisecond, "the keeper's probe and its reconnect presenting the renewed token", func() bool {
		return presentations(hub.Tokens(), renewed) >= 2 && conn.IsConnected()
	})

	rewritten := testnkeys.ServiceToken(daemonAccount, time.Now().Add(50*time.Minute))
	writeToken(t, file, rewritten)
	if err := conn.ForceReconnect(); err != nil {
		t.Fatalf("cut the connection: %v", err)
	}
	testwait.Eventually(t, "the reconnect presenting the rewritten token, connected", func() bool {
		given := hub.Tokens()
		return conn.IsConnected() && len(given) > 0 && given[len(given)-1] == rewritten
	})

	probe := t.Name() + " probe"
	testwait.Eventually(t, "the hub listing the keeper's closed probe "+strconv.Quote(probe), func() bool {
		return slices.ContainsFunc(hubConnections(t, hub, "auth=true&state=closed"), func(each hubConnection) bool { return each.Name == probe })
	})
	given := hub.Tokens()
	requirePresentedInOrder(t, given, first, renewed, rewritten)
	if refused := hub.Refused(t, "no_credential"); refused != 0 {
		t.Errorf("the auth service refused %d connections as no_credential, want none\n%s", refused, hub.ServiceLog())
	}
	// The server moves a closing connection to its closed list on a goroutine of its own, so the
	// inventory is read until it lists a connection for each token given, or a while has passed.
	var decided map[uint64]hubConnection
	testwait.Holds(connectTimeout, 50*time.Millisecond, func() bool {
		decided = decidedConnections(t, hub)
		return len(decided) >= len(given)
	})
	if len(decided) != len(given) {
		t.Errorf("the hub lists %d connections the auth service decided, open or closed, and the service was given %d tokens: one connection for each CONNECT that presented one",
			len(decided), len(given))
	}
	if !slices.ContainsFunc(slices.Collect(maps.Values(decided)), func(each hubConnection) bool { return each.Name == probe }) {
		t.Errorf("the inventory of decided connections holds no keeper's probe %q", probe)
	}
	requireEveryDecidedConnectionIsTheDaemon(t, decided)
}

// After it has connected, a service-token connection tries a refused reconnect again until the
// auth service admits its token, and never closes: the service refuses a token while it cannot
// read the key set of the token's issuer ("service token not verified"), the server tells the
// client only that it was refused (an authorization violation), nats.go reports each refused
// reconnect to the connection's asynchronous error handler, and natscred.Service's
// IgnoreAuthErrorAbort keeps it from closing after the same refusal twice. Here the connection is
// connected, then the verifier refuses the next three CONNECTs and the connection is cut
// (ForceReconnect): it presents its token four times, about two seconds apart (nats.go's reconnect
// wait), and is admitted as the daemon on the fourth, and the refusals of that outage write one
// LogEvents line naming the authorization violation (ConnectService's refusal report: the first of
// an outage, then at most one a minute). A second outage after that reconnect writes its own first
// line. A connection that closes fails the test naming the close and the CONNECTs it made.
func TestAServiceTokenConnectionRetriesARefusedReconnectThroughTheAuthService(t *testing.T) {
	t.Parallel()
	verifier := &refusingVerifier{}
	hub := testnatscallout.Start(t, daemonServices, verifier.verify)
	token := testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour))
	logs := &lockedBuffer{}
	reconnected, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	conn := connectService(t, hub, tokenFile(t, token), logs,
		signalled(make(chan struct{}, 1), make(chan struct{}, 1), reconnected, closed))
	ended := testwait.Context(t)

	for outage := 1; outage <= 2; outage++ {
		select {
		case <-reconnected:
		default:
		}
		asked := len(hub.Tokens())
		verifier.refuseNext(3)
		if err := conn.ForceReconnect(); err != nil {
			t.Fatalf("outage %d: cut the connection: %v", outage, err)
		}
		select {
		case <-reconnected:
		case <-closed:
			t.Fatalf("outage %d: the connection closed after %d refused CONNECTs: nats.go gave up after the same refusal twice\n%s",
				outage, len(hub.Tokens())-asked, logs.String())
		case <-ended.Done():
			t.Fatalf("outage %d: the connection never reconnected; it made %d CONNECTs since the cut\n%s", outage, len(hub.Tokens())-asked, logs.String())
		}
		if made := len(hub.Tokens()) - asked; made != 4 {
			t.Errorf("outage %d: the connection presented its token on %d CONNECTs, want 4: three refused, then the one admitted", outage, made)
		}
		if conn.IsClosed() || !conn.IsConnected() {
			t.Fatalf("outage %d: the connection is not connected after its reconnect (closed: %t)", outage, conn.IsClosed())
		}
		if user := authorizedAs(t, hub, conn); user != daemonUser {
			t.Errorf("outage %d: the hub authorized the reconnect as %q, want %s", outage, user, daemonUser)
		}
		if lines := linesNaming(logs.String(), "authorization violation"); len(lines) != outage {
			t.Fatalf("after outage %d the log holds %d lines naming the refusal, want %d: the first refusal of each outage, and no other of the three\n%s",
				outage, len(lines), outage, logs.String())
		}
	}
	requirePrintsNoCredential(t, "the daemon's log", logs.String(), []printedCredential{{"the token", token}})
}

// startAttempt is the start's attempt at its first NATS connect, as internal/daemon's
// readinessAttempt makes it over workflow.connect: dial, and an error the credential waits out
// (Credential.WaitsOut) is transient, logged with its text and tried again, and every other is a
// refusal. It counts each attempt in attempts and closes a connection it made when t ends; past
// limit attempts it ends the run's context through stop, so a wait that never stops fails the
// test rather than hanging it.
func startAttempt(t *testing.T, credential natsauth.Credential, attempts *int, limit int, stop context.CancelFunc, dial func() (*nats.Conn, error)) func(context.Context) bootprobe.Outcome {
	return func(context.Context) bootprobe.Outcome {
		*attempts++
		if *attempts > limit {
			stop()
		}
		conn, err := dial()
		if err == nil {
			t.Cleanup(conn.Close)
			return bootprobe.Outcome{Passed: true}
		}
		if credential.WaitsOut(err) {
			return bootprobe.Outcome{Detail: err.Error()}
		}
		return bootprobe.Outcome{Refusal: err}
	}
}

// The start's NATS wait waits out a refusal of the service token and stops at a refusal of a seed
// or of no credential, on the boot's own mechanism: bootprobe.Run, as the daemon's "connect Envoy
// NATS" probe runs it, given the wait decision of the credential natsauth.DaemonCredential resolved
// (Credential.WaitsOut) in place of natsauth.Unreachable and an attempt that dials the daemon's
// first connect, on a short retry of the test's own. The auth service refuses a token while it
// cannot read the key set of the token's issuer, and the server answers that refusal as it answers
// every other (an authorization violation), so the start waits out each refusal of the token.
//
//   - The service token (ConnectService): the verifier refuses the first three CONNECTs; Run tries
//     again after each, logging each refused attempt with the refusal, and returns once the fourth
//     is admitted.
//   - A daemon seed (natsauth.Connect with the seed resolved): the hub sends an nkey user it does
//     not list to the auth service, which refuses it as no_credential; Run returns that refusal
//     after one attempt, as before.
//   - No credential: refused as no_credential too; Run returns it after one attempt.
func TestTheStartsWaitWaitsOutARefusedServiceTokenAndStopsAtARefusedSeedThroughTheAuthService(t *testing.T) {
	t.Parallel()
	retry := bootprobe.Retry{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond}

	t.Run("the service token", func(t *testing.T) {
		t.Parallel()
		verifier := &refusingVerifier{}
		verifier.refuseNext(3)
		hub := testnatscallout.Start(t, daemonServices, verifier.verify)
		urls := []string{hub.URL()}
		token := testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour))
		credential, err := natsauth.DaemonCredential("", tokenFile(t, token), urls, "", env(nil))
		if err != nil {
			t.Fatalf("DaemonCredential: %v", err)
		}
		if credential.Source() != natsauth.SourceServiceToken {
			t.Fatalf("DaemonCredential's source = %q, want %q", credential.Source(), natsauth.SourceServiceToken)
		}
		logs := &lockedBuffer{}
		ctx, stop := context.WithCancel(testwait.Context(t))
		defer stop()
		attempts := 0
		err = bootprobe.Run(ctx, "connect Envoy NATS", retry, slog.New(slog.NewTextHandler(logs, nil)),
			startAttempt(t, credential, &attempts, 4, stop, func() (*nats.Conn, error) {
				file, setting, _ := credential.ServiceToken()
				return natsauth.ConnectService(urls, file, setting, t.Name(), nats.Timeout(connectTimeout))
			}))
		if err != nil {
			t.Fatalf("the start's wait stopped after %d attempts at %v; it should wait out each refusal of the service token\n%s", attempts, err, logs.String())
		}
		if attempts != 4 {
			t.Errorf("the start made %d attempts, want 4: three refused, then the one admitted", attempts)
		}
		if made := len(hub.Tokens()); made != 4 {
			t.Errorf("the auth service was given the token on %d CONNECTs, want 4", made)
		}
		waited := linesNaming(logs.String(), "boot probe failed transiently")
		if len(waited) != 3 {
			t.Errorf("the start logged %d refused attempts, want 3\n%s", len(waited), logs.String())
		}
		for _, line := range waited {
			if len(linesNaming(line, "authorization violation")) == 0 {
				t.Errorf("a refused attempt's line does not name the refusal: %s", line)
			}
		}
		requirePrintsNoCredential(t, "the start's log", logs.String(), []printedCredential{{"the token", token}})
	})

	for _, tc := range []struct {
		name   string
		seed   string
		source natsauth.Source
	}{
		{"a daemon seed", testnkeys.UserSeed(t), natsauth.SourceDaemonSeed},
		{"no credential", "", natsauth.SourceNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hub := testnatscallout.Start(t, daemonServices, readServiceToken)
			urls := []string{hub.URL()}
			seedFile := ""
			if tc.seed != "" {
				seedFile = testnkeys.SeedFile(t, tc.seed)
			}
			credential, err := natsauth.DaemonCredential(seedFile, "", urls, "", env(nil))
			if err != nil {
				t.Fatalf("DaemonCredential: %v", err)
			}
			if credential.Source() != tc.source || credential.Seed() != tc.seed {
				t.Fatalf("DaemonCredential's source = %q, with %s; want %q, with the case's", credential.Source(),
					seedNamed(credential.Seed(), map[string]string{"the daemon seed": tc.seed}), tc.source)
			}
			logs := &lockedBuffer{}
			ctx, stop := context.WithCancel(testwait.Context(t))
			defer stop()
			attempts := 0
			err = bootprobe.Run(ctx, "connect Envoy NATS", retry, slog.New(slog.NewTextHandler(logs, nil)),
				startAttempt(t, credential, &attempts, 1, stop, func() (*nats.Conn, error) {
					return natsauth.Connect(urls, credential.Seed(), t.Name(), nats.Timeout(connectTimeout))
				}))
			if err == nil {
				t.Fatal("the start's wait connected with a credential the auth service refuses")
			}
			if attempts != 1 {
				t.Fatalf("the start made %d attempts, want the one whose refusal stops it: a refusal of %s is waited out (%v)\n%s", attempts, tc.name, err, logs.String())
			}
			if !errors.Is(err, nats.ErrAuthorization) {
				t.Errorf("the start's wait stopped at %v, want the authorization violation", err)
			}
			testwait.Eventually(t, "the auth service counting a no_credential refusal", func() bool {
				return hub.Refused(t, "no_credential") >= 1
			})
			if tc.seed != "" {
				credentials := []printedCredential{{"the daemon seed", tc.seed}}
				requirePrintsNoCredential(t, "the start's refusal", err.Error(), credentials)
				requirePrintsNoCredential(t, "the start's log", logs.String(), credentials)
			}
		})
	}
}

// ConnectService's refusal report (D6), on a clock of the test's own over LogEvents' own lines:
// after the connect, nats.go reports each refused reconnect to the connection's asynchronous error
// handler with no subscription, about every two seconds, so the report lets the first refusal of
// an outage through and then at most one a minute, as the Envoy bus logs its failed reconnects
// (internal/bus, reconnectErrThrottle). The first refusal passes; one ten seconds later is dropped;
// one a minute after the first passes, and one thirty seconds after that is dropped; a reconnect,
// which LogEvents still logs, starts a new outage, whose first refusal passes at once; a report
// that names a subscription (a permission violation) and an error that is not a refusal (a slow
// consumer) pass each time, and leave the outage's minute as it was.
func TestTheAuthServicesRefusalOfTheServiceTokenIsLoggedOnceAMinute(t *testing.T) {
	t.Parallel()
	var out lockedBuffer
	start := time.Date(2026, time.November, 28, 12, 0, 0, 0, time.UTC)
	now := start
	var o nats.Options
	if err := natsauth.LogEvents(slog.New(slog.NewTextHandler(&out, nil)))(&o); err != nil {
		t.Fatalf("LogEvents: %v", err)
	}
	if err := natsauth.ReportRefusals(func() time.Time { return now })(&o); err != nil {
		t.Fatalf("ReportRefusals: %v", err)
	}
	refuse := func(at time.Duration, want int, what string) {
		t.Helper()
		now = start.Add(at)
		o.AsyncErrorCB(nil, nil, nats.ErrAuthorization)
		if got := len(linesNaming(out.String(), "authorization violation")); got != want {
			t.Fatalf("%s: the log holds %d lines naming the refusal, want %d\n%s", what, got, want, out.String())
		}
	}
	refuse(0, 1, "the first refusal of an outage")
	refuse(10*time.Second, 1, "a refusal ten seconds after the first")
	refuse(time.Minute, 2, "a refusal a minute after the first")
	refuse(90*time.Second, 2, "a refusal thirty seconds after the one a minute in")

	o.ReconnectedCB(nil)
	if got := len(linesNaming(out.String(), "NATS connection restored")); got != 1 {
		t.Fatalf("the log holds %d lines of the reconnect, want LogEvents' one\n%s", got, out.String())
	}
	refuse(90*time.Second, 3, "the first refusal after a reconnect, in the same second")

	now = start.Add(91 * time.Second)
	violation := fmt.Errorf("%w: %s", nats.ErrPermissionViolation, `Permissions Violation for Subscription to "notifications.example"`)
	subscription := &nats.Subscription{Subject: "notifications.example"}
	for range 2 {
		o.AsyncErrorCB(nil, subscription, violation)
		o.AsyncErrorCB(nil, nil, nats.ErrSlowConsumer)
	}
	if got := len(linesNaming(out.String(), "NATS refused the daemon a permission")); got != 2 {
		t.Errorf("the log holds %d lines of a subscription's permission violation reported twice, want 2\n%s", got, out.String())
	}
	if got := len(linesNaming(out.String(), "slow consumer")); got != 2 {
		t.Errorf("the log holds %d lines of a slow consumer reported twice, want 2\n%s", got, out.String())
	}
	refuse(92*time.Second, 3, "a refusal two seconds after the outage's first, with other reports between")
}

// The daemon's own credential is the token, one seed or nothing, never two
// (natsauth.DaemonCredential): with the token set it presents no seed, not the daemon seed and not
// the pane seed in its place, and the pane seed's settings stay allowed beside the token, since the
// daemon hands the pane seed to the agents it launches (readLaunchSecrets) and does not connect
// with it. Its whole choice as a table: the token beside a pane seed given by each of its three
// settings (nats_nkey_seed_file, whose seed readLaunchSecrets reads and hands over,
// NATS_NKEY_SEED_FILE and NATS_NKEY_SEED) answers the token and no seed, with no refusal, of the
// presence rule either; the token alone answers the token; and with no token, today's order: a
// daemon seed beside a pane seed answers the daemon seed, a pane seed alone the pane seed, and
// nothing nothing.
func TestTheDaemonsCredentialChoiceTakesTheServiceTokenOverThePaneSeed(t *testing.T) {
	t.Parallel()
	file := tokenFile(t, testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour)))
	daemonSeed, paneSeed := testnkeys.UserSeed(t), testnkeys.UserSeed(t)
	seeds := map[string]string{"the daemon seed": daemonSeed, "the pane seed": paneSeed}
	paneSeedFile := testnkeys.SeedFile(t, paneSeed)
	urls := []string{"nats://127.0.0.1:4222"}
	for _, tc := range []struct {
		name                string
		seedFile, tokenFile string
		env                 map[string]string
		paneSeed            string
		source              natsauth.Source
		seed, account       string
	}{
		{name: "the token beside the pane seed's key", tokenFile: file, paneSeed: paneSeed,
			source: natsauth.SourceServiceToken, account: daemonAccount},
		{name: "the token beside NATS_NKEY_SEED_FILE", tokenFile: file, paneSeed: paneSeed,
			env:    map[string]string{natsauth.SeedFileVariable: paneSeedFile},
			source: natsauth.SourceServiceToken, account: daemonAccount},
		{name: "the token beside NATS_NKEY_SEED", tokenFile: file, paneSeed: paneSeed,
			env:    map[string]string{natsauth.SeedVariable: paneSeed},
			source: natsauth.SourceServiceToken, account: daemonAccount},
		{name: "the token alone", tokenFile: file, source: natsauth.SourceServiceToken, account: daemonAccount},
		{name: "a daemon seed beside a pane seed", seedFile: testnkeys.SeedFile(t, daemonSeed), paneSeed: paneSeed,
			env:    map[string]string{natsauth.SeedVariable: paneSeed},
			source: natsauth.SourceDaemonSeed, seed: daemonSeed},
		{name: "a pane seed alone", paneSeed: paneSeed, env: map[string]string{natsauth.SeedVariable: paneSeed},
			source: natsauth.SourcePaneSeed, seed: paneSeed},
		{name: "nothing", source: natsauth.SourceNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := natsauth.RefuseServiceTokenBesideDaemonSeed(tc.seedFile, tc.tokenFile, env(tc.env)); err != nil {
				t.Errorf("RefuseServiceTokenBesideDaemonSeed refused the case: %v", err)
			}
			credential, err := natsauth.DaemonCredential(tc.seedFile, tc.tokenFile, urls, tc.paneSeed, env(tc.env))
			if err != nil {
				t.Fatalf("DaemonCredential refused the case: %v", err)
			}
			if credential.Source() != tc.source {
				t.Errorf("DaemonCredential's source = %q, want %q", credential.Source(), tc.source)
			}
			if credential.Seed() != tc.seed {
				t.Errorf("DaemonCredential presents %s, want %s", seedNamed(credential.Seed(), seeds), seedNamed(tc.seed, seeds))
			}
			if _, _, account := credential.ServiceToken(); account != tc.account {
				t.Errorf("DaemonCredential's service token names account %q, want %q", account, tc.account)
			}
		})
	}
}
