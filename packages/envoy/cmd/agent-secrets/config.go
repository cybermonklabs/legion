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
// setting the client would silently ignore is a mistake the person who wrote it cannot see. A
// path that is not a regular file, a file another user than this one or root owns, and one its
// group or others can write are usage errors too, and nothing in them is read: whoever can write
// the file chooses the broker this machine proves its own identity to.
//
// HOME is the environment's, else the home the password database gives this process's user
// (homeGetenv), so a system unit that systemd starts with no HOME still reads its user's file.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
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

// passwdHome is the home directory the password database gives this process's user (os/user), ""
// where it gives none. A test replaces it.
var passwdHome = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// homeGetenv is os.Getenv, except that HOME, where the environment gives none, is passwdHome.
// systemd starts a system unit that names no User= with no HOME, and the client's own files —
// this settings file, the sign-ins, the machine cache, the helper's unit — are still its user's:
// without this a root unit read no settings file and kept no machine session, so it traded and
// enrolled on every command, and read the sign-ins relative to its working directory.
func homeGetenv(name string) string {
	value := os.Getenv(name)
	if value == "" && name == "HOME" {
		return passwdHome()
	}
	return value
}

// clientConfigPath is $HOME/.config/agent-secrets/config (homeGetenv), or "" where neither the
// environment nor the password database gives a home.
func clientConfigPath() string {
	home := homeGetenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "agent-secrets", "config")
}

// loadClientConfig reads the file into clientConfig, once per command (run). A file that is not
// there is no settings and no error; one that cannot be read, that is not a regular file, that
// another user than this one or root owns, that its group or others can write, or that holds a
// line the format does not take, is an error naming the file and the line. The file names the
// broker this machine proves its identity to, so it is held as OpenSSH holds a user's own
// configuration: whoever but its user (and root, who can change any file) could change it could
// point the machine at a broker of their own. Owning the file covers its directory too: a file
// someone else put in its place is theirs.
func loadClientConfig() error {
	clientConfig = nil
	path := clientConfigPath()
	if path == "" {
		return nil
	}
	// Stat before the open: an open of a FIFO left at the path would wait for a writer forever.
	named, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the settings file %s: %w", path, err)
	}
	if !named.Mode().IsRegular() {
		return fmt.Errorf("the settings file %s is not a regular file (%s); nothing was read from it", path, named.Mode().Type())
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read the settings file %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("read the settings file %s: %w", path, err)
	}
	if !os.SameFile(named, info) || !info.Mode().IsRegular() {
		return fmt.Errorf("the settings file %s changed while it was opened; nothing was read from it", path)
	}
	if uid, ok := fileOwner(info); ok && uid != os.Getuid() && uid != 0 {
		return fmt.Errorf("the settings file %s is owned by uid %d, not by this user (uid %d), and it names the broker this machine proves its identity to; nothing in it was read: make it yours, or remove it",
			path, uid, os.Getuid())
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return fmt.Errorf("the settings file %s can be written by its group or by others (mode %04o), and it names the broker this machine proves its identity to; nothing in it was read: run chmod go-w %s",
			path, mode, path)
	}
	data, err := io.ReadAll(file)
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
			// The key is not printed: a token pasted into the file as a line of its own reads as
			// a key up to its first '='.
			return fmt.Errorf("%s line %d sets a key this file does not take (not printed); it takes %s", path, n, strings.Join(clientConfigKeys, ", "))
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
