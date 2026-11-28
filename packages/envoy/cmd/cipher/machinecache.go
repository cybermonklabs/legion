// packages/envoy/cmd/cipher/machinecache.go
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
// The directory also keeps the first Google exchange audience the broker named for which this
// machine found an identity to mint a token (googleAudienceFile, checkGoogleAudience,
// keepGoogleAudience), since the broker's settings, which are unauthenticated, choose the audience
// of the identity token this machine mints and hands over.
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
	"strconv"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
)

const (
	// machineCacheLock is the file in the machine cache whose flock pairs key.pem and enrollment
	// (lockMachineCache).
	machineCacheLock = "lock"
	// googleAudienceFile is the file in the machine cache that keeps the first Google exchange
	// audience this machine found an identity for (keepGoogleAudience).
	googleAudienceFile = "google-exchange-audience"
)

var errLockWouldBlock = errors.New("the machine cache lock is held")

// machineCacheDir is ${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine, beside the
// sign-ins (cipherclient.SignIns): state a reboot keeps, unlike the key directory, which is
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

// machineCacheName is how a message names the machine cache: by where it is, never by its path,
// since it holds the machine session's key.pem. machineCacheKeyFile names that key.pem (loadKey),
// machineCacheEnrollment the enrollment file beside it, and machineCacheAudienceFile the kept
// Google exchange audience (googleAudienceFile).
const (
	machineCacheName         = "the machine cache (${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine)"
	machineCacheKeyFile      = "key.pem in " + machineCacheName
	machineCacheEnrollment   = "the enrollment file in " + machineCacheName
	machineCacheAudienceFile = googleAudienceFile + " in " + machineCacheName
)

// readMachineCache is the signer of the session the machine cache holds, or nil and no error
// where it holds none, read under the cache's shared lock. A session there that cannot be read
// is an error naming the file: the source is present but broken.
func readMachineCache() (*fileSigner, error) {
	dir := machineCacheDir()
	if !machineCacheHolds(dir) {
		return nil, nil
	}
	unlock, err := lockMachineCache(dir, false, true)
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
	key, err := loadKey(filepath.Join(dir, keyFile), machineCacheKeyFile)
	if err != nil {
		return nil, err
	}
	id, err := readEnrollment(filepath.Join(dir, enrollmentFile), machineCacheEnrollment)
	if err != nil {
		return nil, err
	}
	return &fileSigner{key: key, enrollmentID: id, register: machineReregister}, nil
}

// readMachineCacheNow is readMachineCache for heldSigner, which must not wait: it takes the
// cache's shared lock only where no writer holds it. A registration that holds the exclusive lock
// across its token trade and enrollment (registerMachine) makes the cache hold nothing now, a
// sourceAbsent, rather than a wait for that registration. Its signer never trades a machine
// identity again: a lapsed session names the machine's session and agent-secrets self as its cure.
func readMachineCacheNow() (*fileSigner, error) {
	dir := machineCacheDir()
	if !machineCacheHolds(dir) {
		return nil, absent("%s holds no session", machineCacheName)
	}
	unlock, err := lockMachineCache(dir, false, false)
	switch {
	case errors.Is(err, errLockWouldBlock):
		return nil, absent("another command is registering this machine's session in %s", machineCacheName)
	case err != nil:
		return nil, err
	}
	defer unlock()
	signer, err := readMachineCacheHeld(dir)
	switch {
	case err != nil:
		return nil, err
	case signer == nil:
		return nil, absent("%s holds no session", machineCacheName)
	}
	signer.register = func(context.Context, string) (*ecdsa.PrivateKey, string, error) {
		return nil, "", fmt.Errorf("this machine's session in %s is not live; this secret form does not trade a machine identity here: run agent-secrets self", machineCacheName)
	}
	return signer, nil
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
	unlock, err := lockMachineCache(dir, true, true)
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
		return nil, "", fmt.Errorf("this machine's session in %s is not live, and the machine proves no identity to register it again: %v", machineCacheName, err)
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
	if err := writeFileAtomic(filepath.Join(dir, keyFile), pemBytes); err != nil {
		return fmt.Errorf("write %s: %w", machineCacheKeyFile, redact.File(err))
	}
	if err := writeFileAtomic(filepath.Join(dir, enrollmentFile), []byte(enrollmentID+"\n")); err != nil {
		return fmt.Errorf("write %s: %w", machineCacheEnrollment, redact.File(err))
	}
	return nil
}

// ensureMachineCacheDir makes dir, and holds it to this user alone whether it made it or not.
func ensureMachineCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", machineCacheName, redact.File(err))
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("hold %s to this user: %w", machineCacheName, redact.File(err))
	}
	return nil
}

// lockMachineCache takes the flock of the machine cache dir's lock file (lockFile), exclusive to
// register a session and shared to read one. wait is false only for a held lookup.
func lockMachineCache(dir string, exclusive, wait bool) (func(), error) {
	return lockFile(filepath.Join(dir, machineCacheLock), exclusive, wait)
}

// registerMachineSession trades identity, this machine's own, for a launcher credential,
// registers key under it as this machine's session, and keeps the two in the machine cache dir,
// whose exclusive lock the caller holds.
func registerMachineSession(ctx context.Context, dir string, identity cipherclient.Source, key *ecdsa.PrivateKey) (string, error) {
	base := brokerBaseURL()
	if base.Empty() {
		return "", fmt.Errorf("this machine proves %s, so %s must name the secrets broker it proves it to", identity.Describe(), brokerURLVar)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	client, err := cipherclient.New(cipherclient.Config{
		BrokerURL: base, Key: key, Subject: identity,
		Session: cipherclient.TradeSession(os.Getenv, ""), HTTP: cipherclient.HTTPClient(),
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

// checkGoogleAudience refuses a Google exchange audience other than the one this machine kept from
// its broker at base (googleAudienceFile, keepGoogleAudience), and passes any audience where none
// is kept or there is no machine cache directory. The broker's GET /v1/settings is
// unauthenticated, and its answer chooses the audience of the identity token this machine mints
// and hands over, so a broker that later names another audience — a wrong AGENT_SECRETS_URL, a
// broker someone else now answers for — is refused before any token is minted for someone else's
// service. It writes nothing: a machine with no identity of its own mints nothing, so it keeps no
// audience, and a later audience or broker still gets the chain's no-source refusal.
func checkGoogleAudience(base cipheraddr.Address, audience string) error {
	dir := machineCacheDir()
	if dir == "" {
		return nil
	}
	kept, err := readTrimmed(filepath.Join(dir, googleAudienceFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("read %s: %w", machineCacheAudienceFile, redact.File(err))
	}
	return sameGoogleAudience(base, audience, kept)
}

// keepGoogleAudience keeps audience as the one this machine's identity tokens are minted for,
// once machineSource has found an identity to mint one and before anything is minted, unless an
// audience is kept already, and refuses it where another writer kept a different one first.
func keepGoogleAudience(base cipheraddr.Address, audience string) error {
	dir := machineCacheDir()
	if dir == "" {
		return nil
	}
	if err := ensureMachineCacheDir(dir); err != nil {
		return err
	}
	kept, err := keepFirst(filepath.Join(dir, googleAudienceFile), audience)
	if err != nil {
		return fmt.Errorf("keep the broker's Google exchange audience in %s: %w", machineCacheAudienceFile, redact.File(err))
	}
	return sameGoogleAudience(base, audience, kept)
}

// sameGoogleAudience is the refusal of audience, which the broker at base names, where this
// machine kept another one (machineCacheAudienceFile). Both audiences are text a broker's settings
// answer wrote, so it prints them as base lets that text print (cipheraddr.Address.Words).
func sameGoogleAudience(base cipheraddr.Address, audience, kept string) error {
	if kept != audience {
		return fmt.Errorf("the broker at %s names %s as its Google exchange audience, and this machine first read %s there (kept in %s): a broker's audience does not change, so this machine mints no identity token for another one. If the broker's audience did change, remove %s and run the command again",
			base, base.Words(strconv.Quote(audience)), base.Words(strconv.Quote(kept)), machineCacheAudienceFile, machineCacheAudienceFile)
	}
	return nil
}

// keepFirst writes value to path unless a file is already there, and answers what path holds.
// Writers take the exclusive flock of path's own lock file (path + ".lock") in turn, so the first
// of two at once writes and the second reads what it wrote; the write renames a finished file into
// place, so a reader that holds no lock sees the whole value or none. It takes no hard link, so a
// file system without them keeps a value as well.
func keepFirst(path, value string) (string, error) {
	unlock, err := lockFile(path+".lock", true, true)
	if err != nil {
		return "", err
	}
	defer unlock()
	kept, err := readTrimmed(path)
	if err == nil {
		return kept, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := writeFileAtomic(path, []byte(value+"\n")); err != nil {
		return "", err
	}
	return value, nil
}

// migrateKeyDirMachineSession moves a machine session a client before #4143 kept in the key
// directory, marked machineSessionMarker, into the machine cache, under the cache's exclusive
// lock: into an empty cache by rename, or by copy and remove where the two are on different file
// systems; where the cache already holds a session, the key directory's is removed and the
// cache's kept. The marker goes last, so a move cut short is finished by the next command, and a
// second command that waited on the lock finds no marker and moves nothing. A key directory with
// no marker is a launcher's, and nothing here touches it. Both directories hold a key.pem, so a
// refusal names them by keyDirName and machineCacheName, with the system's reason alone
// (redact.File).
func migrateKeyDirMachineSession(keyDir string) error {
	marker := filepath.Join(keyDir, machineSessionMarker)
	if !exists(marker) {
		return nil
	}
	cache := machineCacheDir()
	if cache == "" {
		return fmt.Errorf("%s holds a machine session to move to the machine cache, and neither XDG_STATE_HOME nor HOME names a directory for it, nor does the password database give this user a home", keyDirName())
	}
	if err := ensureMachineCacheDir(cache); err != nil {
		return err
	}
	unlock, err := lockMachineCache(cache, true, true)
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
				return fmt.Errorf("move this machine's session from %s to %s: %w", keyDirName(), machineCacheName, redact.File(err))
			}
			continue
		}
		if err := os.Remove(src); err != nil {
			return fmt.Errorf("remove the machine session %s held, which %s replaces: %w", keyDirName(), machineCacheName, redact.File(err))
		}
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the %s marker in %s: %w", machineSessionMarker, keyDirName(), redact.File(err))
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
	if err := writeFileAtomic(dst, data); err != nil {
		return err
	}
	return os.Remove(src)
}
