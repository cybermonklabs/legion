// packages/envoy/cmd/agent-secrets/machinecache.go
//
// The machine cache: the session a machine registered for itself under its own cloud identity,
// kept in ${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine (a 0700 directory holding
// key.pem and enrollment, each 0600), so the next use command of this machine signs with it and
// trades nothing. It is the chain's last source, read below a person's sign-in, so a person who
// runs `agent-secrets login` acts as themselves from their next command on while the machine's
// session stays for every process no person signed in for.
//
// A client before #4143 kept that session in the launcher's key directory, marked with a
// machine-session file, where the chain's first source would read it as a launcher's.
// migrateKeyDirMachineSession moves it here once.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// machineCacheDir is ${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine, beside the
// sign-ins (brokerclient.SignIns): state a reboot keeps, unlike the key directory, which is
// runtime state. It is "" where neither variable names a directory.
func machineCacheDir() string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "agent-secrets", "machine")
}

// machineCacheHolds reports whether dir holds a whole session: both key.pem and enrollment. A
// cache left with one of the two by a write that was cut short holds none, and the next
// registration writes both again.
func machineCacheHolds(dir string) bool {
	return dir != "" && exists(filepath.Join(dir, keyFile)) && exists(filepath.Join(dir, enrollmentFile))
}

// readMachineCache is the signer of the session the machine cache holds, or nil and no error
// where it holds none. A session there that cannot be read is an error naming the file: the
// source is present but broken.
func readMachineCache() (*fileSigner, error) {
	dir := machineCacheDir()
	if !machineCacheHolds(dir) {
		return nil, nil
	}
	key, err := loadKey(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	enrollment := filepath.Join(dir, enrollmentFile)
	id, err := readTrimmed(enrollment)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("%s: empty", enrollment)
	}
	return &fileSigner{key: key, enrollmentID: id, register: machineReregister(key)}, nil
}

// writeMachineCache keeps key and enrollmentID as the machine's session, each file written whole
// (writeFileAtomic) in mode 0600 in a 0700 directory.
func writeMachineCache(key *ecdsa.PrivateKey, enrollmentID string) error {
	dir := machineCacheDir()
	if dir == "" {
		return errors.New("this machine's session has nowhere to be kept: neither XDG_STATE_HOME nor HOME names a directory")
	}
	pemBytes, err := encodeKeyPEM(key)
	if err != nil {
		return err
	}
	if err := ensureMachineCacheDir(dir); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, keyFile), pemBytes, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, enrollmentFile), []byte(enrollmentID+"\n"), 0o600)
}

// ensureMachineCacheDir makes dir, and holds it to this user alone whether it made it or not.
func ensureMachineCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make the machine cache %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("hold the machine cache %s to this user: %w", dir, err)
	}
	return nil
}

// registerMachineSession trades identity, this machine's own, for a launcher credential,
// registers key under it as this machine's session, and keeps the two in the machine cache.
func registerMachineSession(ctx context.Context, identity brokerclient.Source, key *ecdsa.PrivateKey) (string, error) {
	base := brokerBaseURL()
	if base == "" {
		return "", fmt.Errorf("this machine proves %s, so %s must name the secrets broker it proves it to", identity.Describe(), brokerURLVar)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	client, err := brokerclient.New(brokerclient.Config{
		BrokerURL: base, Key: key, Subject: identity,
		Session: brokerclient.TradeSession(os.Getenv, ""), HTTP: brokerclient.HTTPClient(),
	})
	if err != nil {
		return "", err
	}
	token, err := client.Token(ctx)
	if err != nil {
		return "", err
	}
	if token.CredentialID == "" {
		return "", errors.New("the broker took this machine's identity but named no launcher credential, so no session can be registered under it")
	}
	enrollmentID, err := enrollUnderCredential(ctx, base, key, thumbprint, token.CredentialID, "this machine")
	if err != nil {
		return "", err
	}
	if err := writeMachineCache(key, enrollmentID); err != nil {
		return "", err
	}
	return enrollmentID, nil
}

// machineReregister is how the machine's cached session is made again when the broker says its
// enrollment is not live: the machine's identity is found again (machineSource) and traded
// afresh, and key is registered under the credential that trade mints, into the cache.
func machineReregister(key *ecdsa.PrivateKey) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		identity, skipped, err := machineSource(ctx)
		if err != nil {
			return "", err
		}
		if identity == nil {
			return "", fmt.Errorf("this machine's session in %s is not live, and the machine proves no identity to register it again: %s", machineCacheDir(), joinSkipped(skipped))
		}
		return registerMachineSession(ctx, identity, key)
	}
}

// migrateKeyDirMachineSession moves a machine session a client before #4143 kept in the key
// directory, marked machineSessionMarker, into the machine cache: into an empty cache by rename,
// or by copy and remove where the two are on different file systems; where the cache already
// holds a session, the key directory's is removed and the cache's kept. The marker goes last, so
// a move cut short is finished by the next command. A key directory with no marker is a
// launcher's, and nothing here touches it.
func migrateKeyDirMachineSession(keyDir string) error {
	marker := filepath.Join(keyDir, machineSessionMarker)
	if !exists(marker) {
		return nil
	}
	cache := machineCacheDir()
	if cache == "" {
		return fmt.Errorf("%s holds a machine session to move to the machine cache, and neither XDG_STATE_HOME nor HOME names a directory for it", keyDir)
	}
	keep := !machineCacheHolds(cache)
	if keep {
		if err := ensureMachineCacheDir(cache); err != nil {
			return err
		}
	}
	for _, name := range []string{keyFile, enrollmentFile} {
		src := filepath.Join(keyDir, name)
		if !exists(src) {
			continue
		}
		if keep {
			if err := moveFile(src, filepath.Join(cache, name)); err != nil {
				return fmt.Errorf("move this machine's session from %s to the machine cache %s: %w", keyDir, cache, err)
			}
			continue
		}
		if err := os.Remove(src); err != nil {
			return fmt.Errorf("remove the machine session %s left, which the machine cache %s replaces: %w", keyDir, cache, err)
		}
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", marker, err)
	}
	return nil
}

// moveFile moves src to dst in mode 0600: a rename, else a whole copy and the source removed.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return os.Chmod(dst, 0o600)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(dst, data, 0o600); err != nil {
		return err
	}
	return os.Remove(src)
}
