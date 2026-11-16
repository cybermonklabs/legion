// packages/envoy/cmd/agent-secrets/config.go
//
// The client's own settings file, ~/.config/agent-secrets/config, as the AWS CLI reads
// ~/.aws/config: a process started with no environment of its own — a systemd unit, a cron job —
// finds the broker and this machine's sign-in client there in place of the variables it was not
// given. A variable that is set and not empty wins over the file, so a shell, a session and a test
// that name a broker keep it.
//
// The file is KEY=value lines, with `#` comments and blank lines. It is read as it stands: no
// quotes are taken off, no `export` is understood and nothing is expanded, so what a line says is
// what the client uses. It takes the settings that say where the broker is and who this machine
// and its person are, and no others; any other key is a usage error naming the line, since a
// setting the client would silently ignore is a mistake the person who wrote it cannot see.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/brokerclient"
)

// clientConfigKeys are the settings the file takes. The machine identity's own settings are
// among them because `agent-secrets login`, and a sign-in's renewal, present the machine as the
// actor: without them a sign-in made in a shell could not renew in a unit.
var clientConfigKeys = []string{
	brokerclient.BrokerURLVar,
	brokerclient.SignInIssuerVar,
	brokerclient.SignInClientIDVar,
	brokerclient.SignInClientSecretFileVar,
	brokerclient.IdentityVar,
	brokerclient.MachineTokenFileVar,
	brokerclient.MachineAudienceVar,
	brokerclient.PoolAudienceVar,
	brokerclient.PoolAccountVar,
}

// clientConfig is what loadClientConfig read from the file, nil before it ran or where there is
// no file.
var clientConfig map[string]string

// clientConfigPath is $HOME/.config/agent-secrets/config, or "" where HOME is unset.
func clientConfigPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "agent-secrets", "config")
}

// loadClientConfig reads the file into clientConfig, once per command (run). A file that is not
// there is no settings and no error; one that cannot be read, or that holds a line the format does
// not take, is an error naming the file and the line.
func loadClientConfig() error {
	clientConfig = nil
	path := clientConfigPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the settings file %s: %w", path, err)
	}
	settings := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		switch {
		case !ok:
			return fmt.Errorf("%s line %d is not KEY=value", path, n)
		case !slices.Contains(clientConfigKeys, key):
			return fmt.Errorf("%s line %d sets %q, which this file does not take; it takes %s", path, n, key, strings.Join(clientConfigKeys, ", "))
		}
		settings[key] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read the settings file %s: %w", path, err)
	}
	clientConfig = settings
	return nil
}

// clientGetenv is a setting as this client reads it: the environment's value when it is set and
// not empty, else the settings file's, else "". It reads like os.Getenv, so it is the getenv the
// brokerclient readers of these settings are handed.
func clientGetenv(name string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return clientConfig[name]
}
