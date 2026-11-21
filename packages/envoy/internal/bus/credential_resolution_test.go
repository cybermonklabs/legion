package bus

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
)

// claimsToken is an unsigned JWT whose payload is claims, which the credential reads unverified.
func claimsToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "none"}) + "." + encode(claims) + ".unsigned"
}

// metadataServer serves a Compute Engine metadata server that answers every identity token with
// token, or fails each one when token is empty, and points GCE_METADATA_HOST at it for the rest of t.
func metadataServer(t *testing.T, token string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || !strings.HasSuffix(r.URL.Path, "/instance/service-accounts/default/identity") {
			http.Error(w, "no identity here", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(token))
	}))
	t.Cleanup(server.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(server.URL, "http://"))
}

// The credential every bus connection presents is the one its environment names: an nkey seed, by
// file or by value with the file winning as it always has, or a service's platform token, by file
// or as this VM's Google identity token, read once to name the account whose reply prefix it asks
// under. A service token beside any other credential is refused naming both, since a process
// connects as one identity, and so is a token that cannot be read or names no account; none set is
// no credential.
func TestCredentialResolution(t *testing.T) {
	seed, _ := testnats.User(t)
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
		return path
	}
	const account = "system:serviceaccount:example-ns:example-listener"
	const googleAccount = "listener@example-project.iam.gserviceaccount.com"
	seedFile := write("seed", seed)
	tokenFile := write("token", claimsToken(t, map[string]any{"sub": account, "exp": 4102444800}))
	nameless := write("nameless", claimsToken(t, map[string]any{"exp": 4102444800}))
	for _, row := range []struct {
		name     string
		env      map[string]string
		metadata string // the token the metadata server answers; empty fails every request
		user     string // the CONNECT user the options give, ServiceUser for a service
		identity string // the account a service's token names, whose reply prefix it asks under
		nkey     bool
		none     bool
		refuses  []string
	}{
		{name: "none", env: map[string]string{}, none: true},
		{name: "a seed file", env: map[string]string{nkeySeedFileVariable: seedFile}, nkey: true},
		{name: "a seed", env: map[string]string{NkeySeedEnvVar: seed}, nkey: true},
		{name: "a seed file beside a seed, the file winning", env: map[string]string{nkeySeedFileVariable: seedFile, NkeySeedEnvVar: "not-read"}, nkey: true},
		{name: "a service token file", env: map[string]string{ServiceTokenFileEnvVar: tokenFile}, user: natscred.ServiceUser, identity: account},
		{name: "a Google audience", env: map[string]string{ServiceGoogleAudienceEnvVar: "envoy-nats"},
			metadata: claimsToken(t, map[string]any{"email": googleAccount, "sub": "1234", "exp": 4102444800}), user: natscred.ServiceUser, identity: googleAccount},
		{name: "a token file beside an audience", env: map[string]string{ServiceTokenFileEnvVar: tokenFile, ServiceGoogleAudienceEnvVar: "envoy-nats"},
			refuses: []string{ServiceTokenFileEnvVar, ServiceGoogleAudienceEnvVar}},
		{name: "a token file beside a seed file", env: map[string]string{ServiceTokenFileEnvVar: tokenFile, nkeySeedFileVariable: seedFile},
			refuses: []string{ServiceTokenFileEnvVar, nkeySeedFileVariable}},
		{name: "an audience beside a seed", env: map[string]string{ServiceGoogleAudienceEnvVar: "envoy-nats", NkeySeedEnvVar: seed},
			refuses: []string{ServiceGoogleAudienceEnvVar, NkeySeedEnvVar}},
		{name: "an empty token file setting", env: map[string]string{ServiceTokenFileEnvVar: ""}, refuses: []string{ServiceTokenFileEnvVar}},
		{name: "a token file that is not there", env: map[string]string{ServiceTokenFileEnvVar: filepath.Join(dir, "missing")},
			refuses: []string{ServiceTokenFileEnvVar, filepath.Join(dir, "missing")}},
		{name: "a token that names no account", env: map[string]string{ServiceTokenFileEnvVar: nameless},
			refuses: []string{ServiceTokenFileEnvVar, nameless, "names no account"}},
		{name: "an empty audience", env: map[string]string{ServiceGoogleAudienceEnvVar: " "}, refuses: []string{ServiceGoogleAudienceEnvVar}},
		{name: "an audience the metadata server mints no token for", env: map[string]string{ServiceGoogleAudienceEnvVar: "envoy-nats"},
			refuses: []string{ServiceGoogleAudienceEnvVar, "holds no token"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			metadataServer(t, row.metadata)
			option, err := credential(func(name string) (string, bool) {
				value, set := row.env[name]
				return value, set
			})
			if len(row.refuses) > 0 {
				require.Error(t, err)
				for _, name := range row.refuses {
					require.Contains(t, err.Error(), name)
				}
				require.NotContains(t, err.Error(), seed)
				return
			}
			require.NoError(t, err)
			if row.none {
				require.Nil(t, option)
				return
			}
			var options nats.Options
			require.NoError(t, option(&options))
			require.Equal(t, row.nkey, options.Nkey != "", "the options carry an nkey")
			if row.user != "" {
				require.NotNil(t, options.UserInfo)
				user, _ := options.UserInfo()
				require.Equal(t, row.user, user)
				require.True(t, options.IgnoreAuthErrorAbort)
				require.Equal(t, natscred.InboxPrefix(row.identity), options.InboxPrefix, "the replies' prefix is the account's own")
			}
		})
	}
}
