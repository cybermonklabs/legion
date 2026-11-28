package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// `nats_daemon_service_token_file` names the file of the projected ServiceAccount token the
// daemon's own NATS connection presents to the hub's auth service (natsauth.DaemonCredential), and
// is read as every other path key is: a relative path is resolved against the configuration file's
// directory, as `nats_daemon_nkey_seed_file`'s is, so the setting does not depend on the directory
// the daemon was started from, and an absolute one is kept as written. A file that names none
// leaves it empty. A value written blank is refused at load naming the key (readNonEmptyString),
// rather than resolved into the configuration file's directory, which holds no token.
func TestLoadReadsTheDaemonServiceTokenFileUnderTheConfigDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		// want is the path, where the configuration file's directory stands for {dir}.
		want string
	}{
		{name: "none"},
		{name: "a relative path, under the file's directory", line: "nats_daemon_service_token_file: secrets/nats-token", want: "{dir}/secrets/nats-token"},
		{name: "an absolute path, as written", line: "nats_daemon_service_token_file: /var/run/secrets/legion/nats-token", want: "/var/run/secrets/legion/nats-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := minimalFile
			if tc.line != "" {
				body += tc.line + "\n"
			}
			path := writeConfigFile(t, body)

			cfg, err := Load(path, noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if want := strings.ReplaceAll(tc.want, "{dir}", filepath.Dir(path)); cfg.NatsDaemonServiceTokenFile != want {
				t.Errorf("NatsDaemonServiceTokenFile = %q, want %q", cfg.NatsDaemonServiceTokenFile, want)
			}
		})
	}
	for _, blank := range []string{`""`, `" "`} {
		t.Run("blank: "+blank, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+"nats_daemon_service_token_file: "+blank+"\n"), noEnv)
			if want := "nats_daemon_service_token_file must not be empty"; err == nil || err.Error() != want {
				t.Fatalf("Load = %v, want %q", err, want)
			}
		})
	}
}
