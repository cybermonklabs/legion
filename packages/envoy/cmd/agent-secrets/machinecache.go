// packages/envoy/cmd/agent-secrets/machinecache.go
//
// The machine cache: the session a machine registered for itself under its own cloud identity,
// kept in ${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine (a 0700 directory holding
// key.pem and enrollment, each 0600), so the next use command of this machine signs with it and
// trades nothing. It is the chain's last source, read below a person's sign-in, so a person who
// runs `agent-secrets login` acts as themselves from their next command on while the machine's
// session stays for every process no person signed in for. HOME is homeGetenv's, so a system unit
// that systemd starts with no HOME keeps its session under its user's home.
//
// Every registration makes a key of its own, the first and each one after the broker says the
// session is not live, so a copy of key.pem (a home backup, a disk kept across a reboot) signs
// for no enrollment after the one it was registered for. A flock on the directory's lock file
// pairs the two files: a registration holds it exclusively from its read of the cache to its
// write, so processes that find the cache empty at once register one session between them, and a
// read holds it shared, so no process signs with one registration's key and another's enrollment.
//
// The directory also keeps the first Google exchange audience the broker named
// (googleAudienceFile, holdGoogleAudience), since the broker's settings, which are
// unauthenticated, choose the audience of the identity token this machine mints and hands over.
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
	"github.com/sjawhar/envoy/internal/redact"
)

const (
	// machineCacheLock is the file in the machine cache whose flock pairs key.pem and enrollment
	// (lockMachineCache).
	machineCacheLock = "lock"
	// googleAudienceFile is the file in the machine cache that keeps the first Google exchange
	// audience this machine read from its broker (holdGoogleAudience).
	googleAudienceFile = "google-exchange-audience"
)

// machineCacheDir is ${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine, beside the
// sign-ins (brokerclient.SignIns): state a reboot keeps, unlike the key directory, which is
// runtime state, and so state a cron job of a user with no runtime directory has too. HOME is
// homeGetenv's. It is "" where neither variable nor the password database names a directory.
func machineCacheDir() string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home := homeGetenv("HOME")
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
// where it holds none, read under the cache's shared lock. A session there that cannot be read
// is an error naming the file: the source is present but broken.
func readMachineCache() (*fileSigner, error) {
	dir := machineCacheDir()
	if !machineCacheHolds(dir) {
		return nil, nil
	}
	unlock, err := lockMachineCache(dir, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readMachineCacheHeld(dir)
}

// readMachineCacheHeld is readMachineCache's read of dir, by a caller already holding its lock.
func readMachineCacheHeld(dir string) (*fileSigner, error) {
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
	return &fileSigner{key: key, enrollmentID: id, register: machineReregister}, nil
}

// registerMachine is this machine's session made under the machine cache's exclusive lock, held
// from its read of the cache to its write. A session the cache holds whose enrollment is not
// stale — any where stale is "", the first registration, and one another process registered in
// the meantime where it is not — is the answer, and nothing is traded. Otherwise the identity
// machineSource finds is traded, and a key made for this registration alone is registered under
// the credential that trade mints and kept in the cache. It answers a sourceAbsent where the
// machine proves no identity, and refuses, before anything is traded, a machine whose session
// would have nowhere to be kept, since every command would then trade and enroll again.
func registerMachine(ctx context.Context, stale string) (*fileSigner, error) {
	dir := machineCacheDir()
	if dir == "" {
		identity, skipped, err := machineSource(ctx, false)
		switch {
		case err != nil:
			return nil, err
		case identity == nil:
			return nil, absent("%s", joinSkipped(skipped))
		}
		return nil, fmt.Errorf("this machine proves %s, and its session has nowhere to be kept: neither XDG_STATE_HOME nor HOME names a directory, and the password database gives this user no home", identity.Describe())
	}
	if err := ensureMachineCacheDir(dir); err != nil {
		return nil, err
	}
	unlock, err := lockMachineCache(dir, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cached, err := readMachineCacheHeld(dir)
	if err != nil {
		return nil, err
	}
	if cached != nil && cached.enrollmentID != stale {
		return cached, nil
	}
	identity, skipped, err := machineSource(ctx, true)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, absent("%s", joinSkipped(skipped))
	}
	key, err := proof.NewKey()
	if err != nil {
		return nil, err
	}
	id, err := registerMachineSession(ctx, dir, identity, key)
	if err != nil {
		return nil, err
	}
	return &fileSigner{key: key, enrollmentID: id, register: machineReregister}, nil
}

// machineReregister is fileSigner.register for the machine's own session, which the broker said
// is not live as stale: registerMachine's session, made again under a fresh key unless another
// process of this machine already did.
func machineReregister(ctx context.Context, stale string) (*ecdsa.PrivateKey, string, error) {
	signer, err := registerMachine(ctx, stale)
	if errors.Is(err, errSourceAbsent) {
		return nil, "", fmt.Errorf("this machine's session in %s is not live, and the machine proves no identity to register it again: %v", machineCacheDir(), err)
	}
	if err != nil {
		return nil, "", err
	}
	return signer.key, signer.enrollmentID, nil
}

// writeMachineCache keeps key and enrollmentID as the machine's session in dir, each file written
// whole (writeFileAtomic) in mode 0600 in a 0700 directory, by a caller holding the cache's
// exclusive lock.
func writeMachineCache(dir string, key *ecdsa.PrivateKey, enrollmentID string) error {
	pemBytes, err := encodeKeyPEM(key)
	if err != nil {
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
// registers key under it as this machine's session, and keeps the two in the machine cache dir,
// whose exclusive lock the caller holds.
func registerMachineSession(ctx context.Context, dir string, identity brokerclient.Source, key *ecdsa.PrivateKey) (string, error) {
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
	if err := writeMachineCache(dir, key, enrollmentID); err != nil {
		return "", err
	}
	return enrollmentID, nil
}

// holdGoogleAudience refuses a Google exchange audience other than the first this machine read
// from its broker at base, which the machine cache keeps (googleAudienceFile). The broker's GET
// /v1/settings is unauthenticated, and its answer chooses the audience of the identity token this
// machine mints and hands over, so a broker that later names another audience — a wrong
// AGENT_SECRETS_URL, a broker someone else now answers for — is refused before any token is minted
// for someone else's service. keep writes the audience where none is kept yet; `identity
// --source` reads and keeps nothing. With no machine cache directory there is nothing to compare
// against and nowhere to keep it.
func holdGoogleAudience(base, audience string, keep bool) error {
	dir := machineCacheDir()
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, googleAudienceFile)
	kept, err := readTrimmed(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !keep:
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := ensureMachineCacheDir(dir); err != nil {
			return err
		}
		if kept, err = keepFirst(path, audience); err != nil {
			return fmt.Errorf("keep the broker's Google exchange audience in %s: %w", path, err)
		}
	case err != nil:
		return err
	}
	if kept != audience {
		return fmt.Errorf("the broker at %s names %q as its Google exchange audience, and this machine first read %q there (kept in %s): a broker's audience does not change, so this machine mints no identity token for another one. If the broker's audience did change, remove %s and run the command again",
			redact.URL(base), audience, kept, path, path)
	}
	return nil
}

// keepFirst writes value to path unless a file is already there, and answers what path holds. It
// links a finished temporary file into place, which fails rather than replace a file another
// process kept first, so the first of two at once is the one both compare against.
func keepFirst(path, value string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), path); err == nil {
		return value, nil
	} else if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return readTrimmed(path)
}

// migrateKeyDirMachineSession moves a machine session a client before #4143 kept in the key
// directory, marked machineSessionMarker, into the machine cache, under the cache's exclusive
// lock: into an empty cache by rename, or by copy and remove where the two are on different file
// systems; where the cache already holds a session, the key directory's is removed and the
// cache's kept. The marker goes last, so a move cut short is finished by the next command, and a
// second command that waited on the lock finds no marker and moves nothing. A key directory with
// no marker is a launcher's, and nothing here touches it.
func migrateKeyDirMachineSession(keyDir string) error {
	marker := filepath.Join(keyDir, machineSessionMarker)
	if !exists(marker) {
		return nil
	}
	cache := machineCacheDir()
	if cache == "" {
		return fmt.Errorf("%s holds a machine session to move to the machine cache, and neither XDG_STATE_HOME nor HOME names a directory for it, nor does the password database give this user a home", keyDir)
	}
	if err := ensureMachineCacheDir(cache); err != nil {
		return err
	}
	unlock, err := lockMachineCache(cache, true)
	if err != nil {
		return err
	}
	defer unlock()
	if !exists(marker) {
		return nil
	}
	keep := !machineCacheHolds(cache)
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
