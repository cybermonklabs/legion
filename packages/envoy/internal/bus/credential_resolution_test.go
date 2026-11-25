package bus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
)

// The credential every bus connection presents is the one its environment names: an nkey seed, by
// file or by value with the file winning as it always has, or a service's platform token, by file
// or as this VM's Google identity token. A service token beside any other credential is refused
// naming both, since a process connects as one identity; none set is no credential.
func TestCredentialResolution(t *testing.T) {
	t.Parallel()
	seed, _ := testnats.User(t)
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
		return path
	}
	seedFile := write("seed", seed)
	tokenFile := write("token", "a.b.c")
	for _, row := range []struct {
		name    string
		env     map[string]string
		user    string // the CONNECT user the options give, ServiceUser for a service
		nkey    bool
		none    bool
		refuses []string
	}{
		{name: "none", env: map[string]string{}, none: true},
		{name: "a seed file", env: map[string]string{nkeySeedFileVariable: seedFile}, nkey: true},
		{name: "a seed", env: map[string]string{NkeySeedEnvVar: seed}, nkey: true},
		{name: "a seed file beside a seed, the file winning", env: map[string]string{nkeySeedFileVariable: seedFile, NkeySeedEnvVar: "not-read"}, nkey: true},
		{name: "a service token file", env: map[string]string{ServiceTokenFileEnvVar: tokenFile}, user: natscred.ServiceUser},
		{name: "a Google audience", env: map[string]string{ServiceGoogleAudienceEnvVar: "envoy-nats"}, user: natscred.ServiceUser},
		{name: "a token file beside an audience", env: map[string]string{ServiceTokenFileEnvVar: tokenFile, ServiceGoogleAudienceEnvVar: "envoy-nats"},
			refuses: []string{ServiceTokenFileEnvVar, ServiceGoogleAudienceEnvVar}},
		{name: "a token file beside a seed file", env: map[string]string{ServiceTokenFileEnvVar: tokenFile, nkeySeedFileVariable: seedFile},
			refuses: []string{ServiceTokenFileEnvVar, nkeySeedFileVariable}},
		{name: "an audience beside a seed", env: map[string]string{ServiceGoogleAudienceEnvVar: "envoy-nats", NkeySeedEnvVar: seed},
			refuses: []string{ServiceGoogleAudienceEnvVar, NkeySeedEnvVar}},
		{name: "an empty token file setting", env: map[string]string{ServiceTokenFileEnvVar: ""}, refuses: []string{ServiceTokenFileEnvVar}},
		{name: "a token file that is not there", env: map[string]string{ServiceTokenFileEnvVar: filepath.Join(dir, "missing")},
			refuses: []string{ServiceTokenFileEnvVar, filepath.Join(dir, "missing")}},
		{name: "an empty audience", env: map[string]string{ServiceGoogleAudienceEnvVar: " "}, refuses: []string{ServiceGoogleAudienceEnvVar}},
	} {
		t.Run(row.name, func(t *testing.T) {
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
			}
		})
	}
}
