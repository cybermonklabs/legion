package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

// A made-up credential pasted where a file's path goes is named in no refusal of the broker's two
// file readers: each names the setting and why it could not read the file (fileUnread), never the
// path, as one that does not exist, one it may not read, a directory, an empty file and a file
// holding no seed show.
func TestTheBrokersFileRefusalsNameNoPath(t *testing.T) {
	const pasted = "madeUpBrokerCredential0123456789"
	dir := t.TempDir()
	write := func(name, contents string, mode os.FileMode) string {
		path := filepath.Join(dir, pasted+"-"+name)
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	unreadable := write("unreadable", "x\n", 0o000)
	empty, noSeed := write("empty", "\n", 0o600), write("no-seed", "not a seed\n", 0o600)
	directory := filepath.Join(dir, pasted+"-directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, want string
		read       func() error
	}{
		{"a secret file that does not exist", "BROKER_X_FILE names a file that cannot be read: it does not exist",
			func() error { _, err := secretFile("BROKER_X_FILE", filepath.Join(dir, pasted)); return err }},
		{"a secret file that is a directory", "BROKER_X_FILE names a file that cannot be read: it is a directory",
			func() error { _, err := secretFile("BROKER_X_FILE", directory); return err }},
		{"an empty secret file", "BROKER_X_FILE names a file that is empty",
			func() error { _, err := secretFile("BROKER_X_FILE", empty); return err }},
		{"a seed file that does not exist", "BROKER_NATS_SEED_FILE names a file that cannot be read: it does not exist",
			func() error {
				_, err := ReadSeedFile("BROKER_NATS_SEED_FILE", filepath.Join(dir, pasted), nkeys.PrefixByteUser)
				return err
			}},
		{"a seed file holding no seed", "BROKER_NATS_SEED_FILE names a file that holds no nkey seed",
			func() error {
				_, err := ReadSeedFile("BROKER_NATS_SEED_FILE", noSeed, nkeys.PrefixByteUser)
				return err
			}},
		{"a seed file it may not read", "BROKER_NATS_SEED_FILE names a file that cannot be read: permission is denied",
			func() error {
				_, err := ReadSeedFile("BROKER_NATS_SEED_FILE", unreadable, nkeys.PrefixByteUser)
				return err
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Contains(row.want, "permission") && os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 file")
			}
			err := row.read()
			if err == nil || err.Error() != row.want {
				t.Fatalf("the refusal is %v, want %q", err, row.want)
			}
			for i := 0; i+10 <= len(pasted); i++ {
				if strings.Contains(err.Error(), pasted[i:i+10]) {
					t.Fatalf("the refusal %q holds %q of the pasted credential", err, pasted[i:i+10])
				}
			}
		})
	}
}
