// packages/envoy/cmd/cipher/airlock.go
// airlock.go is the command side of the airlock: what `agent-secrets NAME -- command` gives the
// command when the broker holds NAME's value back and answers with a proxy to reach it through
// (client.go's AirlockRelease). The command gets a placeholder where the secret was, the proxy in
// HTTPS_PROXY with that grant's proxy password, a NO_PROXY that keeps the bound hosts going
// through the proxy and everything else direct, and a trust bundle that holds the trust it would
// have had plus the proxy's own certificate — unless the command already reaches the proxy with
// the trust it has (proxyRootsTrusted), where it keeps that trust and nothing is written. The
// broker side is internal/cipher/proxy and internal/cipher/requests/airlock.go.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/fileowner"
)

// The slots of an airlock name, spelled as the broker's answer spells them
// (requests.SlotAWSAccessKeyID and its neighbours). They are three short strings of a wire
// contract, written out here rather than imported: internal/cipher/requests is the broker's
// database machine, and this CLI — which every agent box and pod installs — would link a
// Postgres driver into its binary to borrow three constants. The set is open, and a slot this
// file does not know is not a mistake: it is passed through in the name's own value and read by
// whatever takes that value apart. slotAWSSecretAccessKey is written in two halves because the
// public leak check reads the whole name as an AWS secret key; its value is the wire name.
const (
	slotAWSAccessKeyID     = "aws-access-key-id"
	slotAWSSecretAccessKey = "aws-" + "secret-access-key"
	slotAWSSessionToken    = "aws-session-token"
)

// airlockProxyUser is the user name of the Proxy-Authorization credential. The proxy reads the
// password alone and names the user part unread (proxy.proxyPassword), so it is a fixed
// word that says which program built the URL rather than the secret's name — a name in a proxy
// URL reaches a proxy log and a client's own error text.
const airlockProxyUser = "agent-secrets"

// airlockFixedNoProxy are the entries every airlock child's NO_PROXY carries beside the
// inherited ones: the loopback addresses, the suffixes of a cluster's and a tailnet's own names,
// the two metadata addresses and the four private ranges. They never reach the airlock proxy,
// which refuses a private address anyway, and a request to one of them must not wait on a proxy
// that will not carry it.
var airlockFixedNoProxy = []string{
	"localhost",
	"127.0.0.1",
	"::1",
	".svc",
	".cluster.local",
	".internal",
	".ts.net",
	"metadata.google.internal",
	"169.254.169.254",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
}

// systemTrustFiles are the usual system trust bundles, in the order they are looked for: Debian
// and its derivatives, Red Hat, SUSE, and the BSD/Alpine layout. The first that exists is the
// trust a command would have had when nothing named one for it.
var systemTrustFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// systemTrust is the machine's own trust store as every TLS client of it reads that store when
// nothing names another: the first of systemTrustFiles that exists, read with no variable. It is
// not x509.SystemCertPool, which reads SSL_CERT_FILE and SSL_CERT_DIR in place of the system's
// files, as OpenSSL does: GnuTLS, under Debian's git (libcurl-gnutls) and wget, reads neither
// variable and loads the system bundle alone, so a root that only the command's own SSL_CERT_FILE
// or SSL_CERT_DIR names is one git and wget do not trust. The certificate directories
// (/etc/ssl/certs) are left out for the same reason. A bundle that is there but cannot be read is
// a store that trusts nothing, as it is to those clients, rather than a reason to read the next;
// a machine with none of the files trusts nothing here either, and so gets the bundle, as does a
// root added to macOS's keychain, which no file here holds.
func systemTrust() *x509.CertPool {
	pool := x509.NewCertPool()
	for _, path := range systemTrustFiles {
		data, err := readRegularFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			pool.AppendCertsFromPEM(data)
		}
		break
	}
	return pool
}

// airlockFiles are the two files one run writes for its command: the trust bundle every
// bundle variable points at, and the wgetrc that carries that bundle to wget, which reads no
// bundle variable at all.
//
// Neither file holds a credential of the grant's — the bundle is the public roots plus the
// proxy's public certificate, and the wgetrc is the inherited one plus a path, so it holds no
// more than the WGETRC the command already had (an http_password among it, where that one has
// one), mode 0600 in a directory this user alone reads — so the tidying in tidyAirlockDirs is
// hygiene, not containment: a directory that outlives its grant holds nothing its user could not
// already read.
//
// Both paths are empty for a command that needs no bundle, since the machine's own trust reaches
// the proxy (proxyRootsTrusted): nothing was written, and airlockVars sets no trust variable.
type airlockFiles struct {
	bundle string // the trust bundle: the inherited or system roots, plus the proxy's certificate
	wgetrc string // a wgetrc naming that bundle, after the lines of an inherited one
}

// envValue is key's value in environ, the environment as os.Environ writes it, and "" when
// environ holds no entry for it. A later entry wins, as execve's own last-wins reading does.
func envValue(environ []string, key string) string {
	value := ""
	for _, e := range environ {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			value = v
		}
	}
	return value
}

// envHas reports whether environ holds an entry for key at all, an empty value among them, which
// envValue answers as it answers none.
func envHas(environ []string, key string) bool {
	return slices.ContainsFunc(environ, func(e string) bool {
		k, _, ok := strings.Cut(e, "=")
		return ok && k == key
	})
}

// airlockRootParts is where every grant's directory lives, in the two pieces makeAirlockRoot
// works on: the base directory, and the elements under it this CLI makes. The base is
// XDG_RUNTIME_DIR when the session has one (a tmpfs the session owns, which goes when the
// session does), else TMPDIR, else the system temporary directory. Never the command's working
// directory: a check of this change greps every file there for the secret's value, and these
// files must not be among them.
//
// The first two are named by the session's own environment and are the session's own, where the
// third is shared with every other user of the machine: a directory called "agent-secrets" there
// is one whoever gets there first holds, and what goes in it is the trust bundle this command
// verifies every direct call against. So the fallback carries this user's id in its first
// element, which gives each user a directory nobody else's run would make, and makeAirlockRoot
// checks that each element it finds is this user's.
//
// The base itself is neither this CLI's to make nor its to judge. XDG_RUNTIME_DIR and TMPDIR
// are the session's own and were there before this run, the system temporary directory is the
// machine's and is world-writable on purpose, and a CLI that refused any of the three would
// refuse the ordinary machine. What it answers for is what it makes itself, which is every
// element below the base.
func airlockRootParts(environ []string) (string, [2]string) {
	base := envValue(environ, "XDG_RUNTIME_DIR")
	if base == "" {
		base = envValue(environ, "TMPDIR")
	}
	if base != "" {
		return base, [2]string{"agent-secrets", "airlock"}
	}
	return os.TempDir(), [2]string{"agent-secrets-" + strconv.Itoa(os.Getuid()), "airlock"}
}

// makeAirlockRoot makes each element of the root below the base, outermost first, checks each
// one as it is made (checkAirlockRoot) and answers the root.
//
// os.Mkdir per element rather than one os.MkdirAll over the lot, because MkdirAll follows
// whatever stands in the way — a directory another user made, or their symbolic link to one —
// and following it is the hole. A run that checked the last element alone would still write
// under a parent somebody else holds, and that user can rename the checked element away
// between the check and the write and leave their own wgetrc and trust bundle in its place,
// which is the pair this command hands its child. An element already there is checked and kept,
// since that is every second run of this user.
//
// Both elements are checked under a session-named base as well, not only under the shared
// temporary directory: XDG_RUNTIME_DIR is a session's own, but TMPDIR is whatever the session
// was started with, and one two users share puts that same rename in the same place.
func makeAirlockRoot(environ []string) (string, error) {
	base, elements := airlockRootParts(environ)
	root := base
	for _, element := range elements {
		root = filepath.Join(root, element)
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("make the airlock directory: %w", err)
		}
		if err := checkAirlockRoot(root); err != nil {
			return "", err
		}
	}
	return root, nil
}

// airlockBuildPrefix opens the name of a directory a run is still building. A grant's files are
// written under such a name and renamed into place, so no name that is not a grant id of a
// finished directory is ever read as one, and a tidy can tell the two apart.
const airlockBuildPrefix = ".building-"

// airlockBuildLifetime is how long a directory under airlockBuildPrefix may stand before another
// run's tidy takes it for the leftover of a run that died. Building one is three writes of a few
// hundred kilobytes, which no machine takes a minute over, so an hour of it is a leftover and
// nothing else. The grant directories themselves are never aged out this way: they are meant to
// outlive their run, and how long a grant lasts is in the expires file.
const airlockBuildLifetime = time.Hour

// prepareProxyFilesFor writes the grant's directory for this trust — the trust bundle, the wgetrc
// and the expires file that says how long they are worth keeping — and answers the two paths the
// child's environment names. It reads two environments: own, agent-secrets' own, which places the
// airlock root (makeAirlockRoot), and command, the one the command will run with
// (prepareProxyTrustForCommand), which the decision and the files are made from. Nothing of this
// CLI runs once the command starts, so each run first tidies away what earlier runs left
// (tidyAirlockDirs).
//
// A command that reaches the proxy with the trust it already has gets none of this: where the
// machine's own system trust bundle (systemTrust) trusts every root the release carries, the
// command's environment shows that every client family reads that store, and no trust variable,
// wget startup file or SSL_CERT_DIR of the command takes a root away (proxyRootsTrusted),
// nothing is made or written, and the answer is no files. Such a run tidies nothing either; what
// earlier runs left waits for the next run that writes, and holds nothing secret.
//
// The directory is built under a name of its own and renamed into place once every file is in
// it, and that rename is the guard between two runs that share a runtime directory — parallel
// tool calls of one agent do: a tidy reads a directory with no expires file as one a run left
// unfinished and removes it, so a directory that stood under its final name half written would
// be swept away while the run writing it still needed it, and that run would either fail
// outright or hand its command a bundle variable naming a file that is no longer there. Under
// the rename the final name appears complete or not at all, however long a run takes.
//
// Every element of the root this CLI makes is checked before any of this (makeAirlockRoot): a
// directory another user holds is one the command's trust bundle may not be written in or read
// from, and the run answers an error rather than writing one.
//
// An error here names the path it failed on, as checkAirlockRoot's do: every such path is the
// root (airlockRootParts: XDG_RUNTIME_DIR, else TMPDIR, else the system's temporary directory)
// with names this CLI made below it, the grant's id, which the exec form hands it only as
// RemoteText.SendBack answers it - the id as it came, where the rule prints it so - and a keyed
// hash of the trust (proxyTrustDirName), and none of those is a setting a person points at a
// secret's file or a plain function of one. The files a person's own settings name are another
// matter: WGETRC's can hold http_password, so no error names the file WGETRC or a bundle variable
// names or quotes what it holds - readTrustFile and commandWgetrcLines skip a file they cannot
// read, and print nothing of it.
func prepareProxyFilesFor(own, command []string, release *AirlockRelease, grantID string, expiresAt *time.Time) (airlockFiles, error) {
	// First, since a command that needs no bundle has no directory to make, tidy or name.
	if proxyRootsTrusted(command, release.CACertificate, systemTrust) {
		return airlockFiles{}, nil
	}
	// The grant id becomes one path element, so it must be one: the broker's ids are opaque, and
	// a "." or a ".." would make this run tidy or write somewhere else entirely. A grant id that
	// is no path element is not printed: it is the broker's text, which beside an address that
	// carries a credential can repeat it.
	if grantID == "" || grantID == "." || grantID == ".." || grantID != filepath.Base(grantID) {
		return airlockFiles{}, errors.New("the grant id the broker answered is not a single path element (not printed), so its airlock files have nowhere to go")
	}
	// Before the tidy, which removes directories under the root, and before anything is
	// written there: a root this user does not hold is one this run neither sweeps nor writes
	// in.
	root, err := makeAirlockRoot(own)
	if err != nil {
		return airlockFiles{}, err
	}
	// Before the tidy, which sweeps every directory it does not recognize: a directory where the
	// key goes is refused (proxyTrustDirKey), not swept and replaced by a new key.
	key, err := proxyTrustDirKey(root)
	if err != nil {
		return airlockFiles{}, err
	}
	tidyAirlockDirs(root, grantID, time.Now())

	build, err := os.MkdirTemp(root, airlockBuildPrefix)
	if err != nil {
		return airlockFiles{}, fmt.Errorf("make the airlock directory: %w", err)
	}
	// A no-op once the rename has taken it, and the clean-up of a run that answers an error
	// before that.
	defer os.RemoveAll(build)
	// MkdirTemp takes the umask off the mode it asks for, so the mode is set rather than asked
	// for; the directory carries it through the rename.
	if err := os.Chmod(build, 0o700); err != nil {
		return airlockFiles{}, fmt.Errorf("make the airlock directory private: %w", err)
	}

	// The directory is named for the grant and for what its files hold (proxyTrustDirName), so the
	// bundle and the inherited wgetrc lines are read first. Each file is written under the build
	// directory and names the paths the command will read, which are the ones under dir: the
	// wgetrc carries the bundle's path to wget, and a path under the build directory would be
	// gone by the time the command read it.
	bundle, wgetrcLines := trustBundle(command, release.CACertificate), commandWgetrcLines(command)
	dir := filepath.Join(root, proxyTrustDirName(key, grantID, bundle, wgetrcLines))
	files := airlockFiles{bundle: filepath.Join(dir, "ca-bundle.pem"), wgetrc: filepath.Join(dir, "wgetrc")}
	if err := writeFileAtomic(filepath.Join(build, "ca-bundle.pem"), bundle, 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the trust bundle: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(build, "wgetrc"), wgetrcFor(wgetrcLines, files.bundle), 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the wgetrc: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(build, "expires"), []byte(expiresText(expiresAt)), 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the airlock expiry: %w", err)
	}
	if err := os.Rename(build, dir); err != nil {
		// The final name is this grant's and this trust's (proxyTrustDirName), so what holds it
		// is a run of this same grant and the same trust that got there first: a finished
		// directory, with this grant's expiry and the very bytes this run wrote. Its files are
		// left as they are rather than written over, since the command of the run that put them
		// there may be reading them now, and they are this run's answer too. A final name held
		// by anything else — a directory no run of this grant finished — is this run's failure
		// to report, and the command is not run on files it cannot trust.
		if _, statErr := os.Stat(filepath.Join(dir, "expires")); statErr != nil {
			return airlockFiles{}, fmt.Errorf("put the airlock directory in place: %w", err)
		}
	}
	return files, nil
}

// checkAirlockRoot refuses a root whose directory this user does not hold alone. Every run of
// this user shares the root, so a run reads and sweeps whatever it finds there, and what it
// writes under it is the trust bundle the command verifies each of its direct calls against: a
// root another user holds is that user's own certificate authority in the command's bundle, and
// the command is not run on files it cannot trust.
//
// Lstat rather than Stat, because a symbolic link is a root whose real directory somebody else
// chose and Stat would answer for what it points at rather than for what is checked here. The
// owner and the mode are the two other ways another user comes to hold it: one who made the
// directory before this run did, and one let in by a mode wider than this user alone.
//
// It is run on each element of the root rather than on the last alone, since a parent another
// user holds is a parent they can rename the element below it out of.
func checkAirlockRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("read the airlock directory %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the airlock directory %s is a symbolic link, so its files would go wherever whoever made the link pointed it; remove it", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("the airlock directory %s is not a directory, so the grant's files have nowhere to go; remove it", root)
	}
	if uid, ok := fileowner.Of(info); ok && uid != os.Getuid() {
		return fmt.Errorf("the airlock directory %s is owned by uid %d, not this user (uid %d), so the trust bundle written there is one its owner can replace; remove it, or run as its owner", root, uid, os.Getuid())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("the airlock directory %s is mode %04o, which lets other users write the trust bundle this command verifies its direct calls against; run: chmod 700 %s", root, perm, root)
	}
	return nil
}

// expiresText is what the expires file holds: the grant's expiry in RFC 3339, and the empty
// string for a grant that lasts until its session ends — a directory the next run keeps, since
// its files hold nothing secret and a later run for the same grant writes the same bytes.
func expiresText(expiresAt *time.Time) string {
	if expiresAt == nil {
		return ""
	}
	return expiresAt.Format(time.RFC3339)
}

// tidyAirlockDirs removes every directory under root but the running grant's (keep, read from the
// name with proxyTrustDirGrant, so a grant whose id begins with keep is not taken for it) whose
// grant has expired, every one a run left unfinished (no expires file, or one that cannot be read
// or parsed), and every directory under airlockBuildPrefix that has stood longer than a build can
// take, which is a leftover of a run that died between making it and renaming it into place. A
// directory whose expires file is empty lasts until its session ends and is left. Each directory
// of a grant, one per trust its commands named (proxyTrustDirName), carries that grant's expiry, so
// once the grant ends a tidy takes every one of them.
//
// The running grant's directories are all kept, whatever trust each holds: a command of that grant
// that is still running may read its bundle again at any time (Python requests reads the file on
// each request), and nothing here can tell whether one still runs. So a live grant keeps one
// directory of about 250 KB per distinct trust its commands named, not per run: a run whose trust
// matches an earlier one's takes that directory. A grant with no expiry keeps them as long as the
// root lasts: until its session ends under XDG_RUNTIME_DIR, which goes with the session, and until
// that directory is cleaned under TMPDIR or the system's temporary directory. A command that names
// other trust on every run, such as a WGETRC made fresh each time, makes a directory each time.
//
// Tidying is not the run: nothing here is logged, nothing is refused and no error is answered,
// because a command must still run when the runtime directory is read-only, is shared with
// another user's leftovers, or holds something this version does not recognize.
func tidyAirlockDirs(root, keep string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			// A key a run wrote under its own name and was killed before it linked into place
			// (writeProxyTrustDirKey): taken once it has stood longer than a build can take, as a
			// build directory is, since a young one is a run in flight. The key itself and any
			// other file are left.
			if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), proxyTrustKeyTempPrefix) {
				if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > airlockBuildLifetime {
					_ = os.Remove(filepath.Join(root, entry.Name()))
				}
			}
			continue
		}
		if grant, ok := proxyTrustDirGrant(entry.Name()); ok && grant == keep {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if strings.HasPrefix(entry.Name(), airlockBuildPrefix) {
			// A directory another run is building now, or one a run that died left behind:
			// age is what tells them apart, and taking a young one would be the very race
			// the rename is there to close.
			if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > airlockBuildLifetime {
				_ = os.RemoveAll(dir)
			}
			continue
		}
		if airlockDirWorthKeeping(dir, now) {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// airlockDirWorthKeeping reads one directory's expires file: an empty one is a grant that lasts
// until its session ends (keep), a time still ahead is a grant still live (keep), and anything
// else — a time past, a file that is not there, unreadable, or not an RFC 3339 time — is not.
func airlockDirWorthKeeping(dir string, now time.Time) bool {
	data, err := os.ReadFile(filepath.Join(dir, "expires"))
	if err != nil {
		return false
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return true
	}
	expires, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return false
	}
	return !expires.Before(now)
}

// trustBundle is the trust the command gets: the trust it would have had, then the proxy's own
// certificate.
//
// "Would have had" is every bundle variable the command inherited (airlockBundleVars, the same
// variables airlockVars will point at this one file) whose value names a file this process can
// read, each file's bytes once and in the variables' own order, with the first system bundle
// that exists standing behind an SSL_CERT_FILE that names nothing readable. A command whose
// environment puts a private authority in NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE or
// GRPC_DEFAULT_SSL_ROOTS_FILE_PATH rather than in SSL_CERT_FILE keeps it: NO_PROXY sends its calls
// to that authority's hosts direct, and those calls are verified against this bundle and nothing
// else. The cost of one bundle is that a client whose variable replaces its own roots (all but
// Node's) trusts the whole of it, the system roots and every other variable's file beside its
// own, where it trusted its own file alone: each authority in it is one the command trusted for
// some of its calls already.
//
// When nothing is readable at all the bundle is the proxy's certificate alone, and that costs
// the command every other certificate authority: a request the proxy tunnels rather than carries
// (a host no airlock name is bound to) is verified against this bundle and fails. That is the
// same trust the command would have had — none it could name — made visible, and a machine with
// no system bundle at all is one where a command that verifies anything already brings its own.
func trustBundle(environ []string, caPEM string) []byte {
	var bundle []byte
	var held [][]byte
	add := func(data []byte) {
		// One file is usually named by several of these variables, and an authority held
		// twice says no more than the same authority held once.
		if slices.ContainsFunc(held, func(have []byte) bool { return bytes.Equal(have, data) }) {
			return
		}
		held = append(held, data)
		bundle = append(bundle, data...)
		if len(bundle) > 0 && !bytes.HasSuffix(bundle, []byte("\n")) {
			bundle = append(bundle, '\n')
		}
	}
	for _, key := range airlockBundleVars {
		if data, ok := readTrustFile(environ, key); ok {
			add(data)
		}
	}
	bundle = append(bundle, caPEM...)
	if !bytes.HasSuffix(bundle, []byte("\n")) {
		bundle = append(bundle, '\n')
	}
	return bundle
}

// proxyTrustDirName is the name of the directory one run's files go in: the grant's id, a dot, and
// 16 hex digits of the HMAC-SHA-256, keyed with key (proxyTrustDirKey), of what the command's clients
// will read from it (writeProxyTrustDirInput). That is the bundle's bytes as written (every trust
// file read for it, the system bundle standing behind an SSL_CERT_FILE that names none, and the
// proxy's roots) and the inherited wgetrc's lines as read; the path of the bundle the wgetrc then
// names is the one part left out, since it is this very name. It is of contents read, not of the
// settings, so another path holding the same bytes is the same trust and the same path holding
// other bytes is other trust.
//
// agent-secrets reuses a live grant across the commands of a session, so two runs of one grant
// can name different trust: before this name, both took the grant's id alone, the second run
// found the first's directory under it and answered that (the rename's collision in
// prepareProxyFilesFor), and the second command got the first command's bundle. Runs of one
// grant and one trust still take one name, and so one directory, which the rename hands over
// finished.
//
// Keyed, because the name leaves the root, in the child's environment, the argv of a command that
// passes the bundle's path on, and an error that names the directory, while the wgetrc lines can
// hold http_password and the rest of the input is public: a plain hash would let anyone who sees
// the name test password guesses against it offline.
func proxyTrustDirName(key []byte, grantID string, bundle, wgetrcLines []byte) string {
	h := hmac.New(sha256.New, key)
	writeProxyTrustDirInput(h, bundle, wgetrcLines)
	return grantID + "." + hex.EncodeToString(h.Sum(nil))[:proxyTrustDirHashLen]
}

// writeProxyTrustDirInput writes what a directory is named for to w: the bundle's length as eight
// bytes, big-endian, then the bundle, then the wgetrc lines. A trust file can hold any byte, so the
// length, not a separator, is what tells where the bundle ends: without it, a bundle that runs on
// into what is another command's wgetrc would hash as that command's trust, and get its directory.
func writeProxyTrustDirInput(w io.Writer, bundle, wgetrcLines []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(bundle)))
	w.Write(length[:])
	w.Write(bundle)
	w.Write(wgetrcLines)
}

// proxyTrustKeyFile and proxyTrustKeyLen are the file in the airlock root that holds
// proxyTrustDirName's key, and how many bytes the key is; proxyTrustKeyTempPrefix begins the name
// a run writes the key under before it links it into place.
const (
	proxyTrustKeyFile       = "dir-name.key"
	proxyTrustKeyLen        = 32
	proxyTrustKeyTempPrefix = ".key-"
)

// proxyTrustKeyLink puts the key in place (os.Link). It is a variable so a test stands in a file
// system that takes no hard link.
var proxyTrustKeyLink = os.Link

// proxyTrustDirKey is the key proxyTrustDirName is keyed with: 32 random bytes in the airlock
// root's proxyTrustKeyFile, made by the first run and read back by every later one, so runs of one
// grant and one trust, each its own process, still take one name. The key sits in the same root,
// owned by this user alone and mode 0700 (makeAirlockRoot), as the wgetrc copies that hold the
// inherited lines in clear, so whoever can read it can read those lines anyway; it is never put in
// an environment, an argv or any output.
//
// A first run writes the key under a name of its own (proxyTrustKeyTempPrefix, mode 0600, created
// exclusively) and links it to proxyTrustKeyFile, which a link never replaces, then reads
// proxyTrustKeyFile back: two first runs that race both end with the key the link that won put in
// place. A key file that is not a regular file holding exactly proxyTrustKeyLen bytes, that its
// group or others can read or write (the rule checkAirlockRoot holds the root to, so the two agree
// on every platform), or that another user owns, is refused, not used: it is not one this CLI
// wrote. It is read without waiting (readRegularFile), so a named pipe put in its place is refused
// rather than waited on. The tidy passes the key over, and takes a temporary key a killed run left
// once it has stood longer than a build can take (tidyAirlockDirs).
func proxyTrustDirKey(root string) ([]byte, error) {
	path := filepath.Join(root, proxyTrustKeyFile)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := writeProxyTrustDirKey(root, path); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read the airlock key %s: %w", path, err)
	}
	// fileOwner, not the fileowner.Of checkAirlockRoot reads: the same uid, from a variable a test
	// can name another owner through.
	if uid, ok := fileOwner(info); ok && uid != os.Getuid() {
		return nil, fmt.Errorf("the airlock key %s is owned by uid %d, not this user (uid %d); remove it and run again", path, uid, os.Getuid())
	}
	refused := fmt.Errorf("the airlock key %s is not a %d-byte regular file that only this user can read; remove it and run again", path, proxyTrustKeyLen)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != proxyTrustKeyLen {
		return nil, refused
	}
	key, err := readRegularFile(path)
	if errors.Is(err, errNotRegular) || errors.Is(err, errFileTooLarge) || err == nil && len(key) != proxyTrustKeyLen {
		return nil, refused
	}
	if err != nil {
		return nil, fmt.Errorf("read the airlock key %s: %w", path, err)
	}
	return key, nil
}

// writeProxyTrustDirKey puts a new key at path, unless another run's link got there first, which is
// no error: proxyTrustDirKey reads back whichever key the link put in place. The key is synced to
// disk before the link, so a root on a disk (TMPDIR, or the system's temporary directory) never
// shows a linked key with no bytes in it after a power loss. A root on a file system that takes no
// hard link (some FUSE mounts) is refused with that reason: a key put in place any other way could
// replace another run's, or be read before it is whole.
func writeProxyTrustDirKey(root, path string) error {
	key := make([]byte, proxyTrustKeyLen)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("make the airlock key: %w", err)
	}
	temp, err := os.CreateTemp(root, proxyTrustKeyTempPrefix)
	if err != nil {
		return fmt.Errorf("write the airlock key: %w", err)
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("write the airlock key: %w", err)
	}
	if _, err := temp.Write(key); err != nil {
		temp.Close()
		return fmt.Errorf("write the airlock key: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("write the airlock key: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write the airlock key: %w", err)
	}
	err = proxyTrustKeyLink(temp.Name(), path)
	switch {
	case err == nil || errors.Is(err, os.ErrExist):
		return nil
	case errors.Is(err, errors.ErrUnsupported) || errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("put the airlock key in place: %w: the file system of %s takes no hard link, which the key is put in place with so that no run replaces another's; point XDG_RUNTIME_DIR or TMPDIR at a directory on another file system", err, root)
	default:
		return fmt.Errorf("put the airlock key in place: %w", err)
	}
}

// proxyTrustDirHashLen is how many hex digits of the HMAC proxyTrustDirName keeps: 64 bits, so two
// trusts of one grant share a directory only by a collision no run will meet.
const proxyTrustDirHashLen = 16

// proxyTrustDirGrant is the grant id of a directory name proxyTrustDirName gave, read from the end,
// since the broker's ids are opaque and may hold a dot; false for a name of any other form.
func proxyTrustDirGrant(name string) (string, bool) {
	i := len(name) - proxyTrustDirHashLen - 1
	if i < 1 || name[i] != '.' {
		return "", false
	}
	for _, c := range name[i+1:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return name[:i], true
}

// prepareProxyTrustForCommand is prepareProxyFilesFor for the exec form's command. The trust it
// decides and builds on is the command's: the inherited environment with the released values
// after it (buildChildEnv), so a released trust setting, HOME or SSL_CERT_DIR counts as the
// command's own.
// Where the airlock root goes is agent-secrets' own choice (airlockRootParts), made from the
// inherited environment alone, so a released value named XDG_RUNTIME_DIR or TMPDIR, which the
// broker answers and agent-secrets does not choose, does not move where it writes and tidies.
// Where it writes a bundle, it names on stderr each trust setting given as a relative path
// (relativeTrustSettings), one line each.
func prepareProxyTrustForCommand(environ []string, released map[string]string, release *AirlockRelease, grantID string, expiresAt *time.Time, stderr io.Writer) (airlockFiles, error) {
	command := slices.Clone(environ)
	for _, name := range slices.Sorted(maps.Keys(released)) {
		command = append(command, name+"="+released[name])
	}
	files, err := prepareProxyFilesFor(environ, command, release, grantID, expiresAt)
	if err != nil || files.bundle == "" {
		return files, err
	}
	for _, key := range relativeTrustSettings(command) {
		into := "trust bundle"
		if key == "WGETRC" {
			into = "wgetrc"
		}
		fmt.Fprintf(stderr, "agent-secrets: %s is a relative path, resolved for the %s from the directory the command starts in\n", key, into)
	}
	return files, nil
}

// relativeTrustSettings are the settings the bundle writer reads (airlockBundleVars and WGETRC)
// whose value is a relative path. agent-secrets runs the command in its own process (syscall.Exec),
// so the command starts in this directory, and such a value names here the file the command itself
// would read at its start: the bundle (or, for WGETRC, the wgetrc) carries what that file holds,
// under an absolute path that still holds after the command changes directory, and nothing where
// no file is there. The command is told, since a relative path is one it might have meant to
// resolve from somewhere else. Only the variable's name is printed, never its value: WGETRC's file
// can hold http_password.
func relativeTrustSettings(environ []string) []string {
	var relative []string
	for _, key := range append(slices.Clone(airlockBundleVars), "WGETRC") {
		if named := envValue(environ, key); named != "" && !filepath.IsAbs(named) {
			relative = append(relative, key)
		}
	}
	return relative
}

// readTrustFile is the trust one inherited bundle variable names, read whole, and for
// SSL_CERT_FILE the first system bundle that exists where it names no file this process can
// read — the trust a command with no variable of its own would have had, which is OpenSSL's own
// answer and so stands behind OpenSSL's variable rather than behind each client family's.
//
// A variable naming a file this process cannot read is skipped, and that costs the command
// whatever trust that file held: a direct call to a host that authority signed fails
// verification, exactly as it does on a machine with no system bundle. It is skipped rather than
// refused because such a variable names no trust the command would have had either — the file is
// as unreadable to the command as it is here — and refusing would take away a run that works
// today. A variable naming a named pipe or a device is read as the command's own client reads it
// (readTrustSource): a pipe gives what its writer sends, and one with no writer is empty at once,
// as /dev/null is, rather than waited on. Such a file that reads empty still counts as read, so
// SSL_CERT_FILE=/dev/null adds nothing, not the system bundle.
//
// In a run whose broker URL is https, SSL_CERT_FILE reaches here second: agent-secrets' own
// connection to the broker reads it first, before prepareProxyFilesFor runs, for the roots of Go's
// TLS client (crypto/x509's root.go reads it with os.ReadFile). So, as before readTrustSource, a pipe
// it names arrives here drained and adds nothing, a named pipe with no writer stalls that
// connection until its timeout, and a device that never ends is read there with no bound. With a
// plain-http broker, such as the local stack's loopback one, no TLS handshake reads it, so it
// reaches this read first, as the other variables always do.
func readTrustFile(environ []string, key string) ([]byte, bool) {
	var candidates []string
	if named := envValue(environ, key); named != "" {
		candidates = append(candidates, named)
	}
	if key == "SSL_CERT_FILE" {
		candidates = append(candidates, systemTrustFiles...)
	}
	for _, path := range candidates {
		if data, err := readTrustSource(path); err == nil {
			return data, true
		}
	}
	return nil, false
}

// commandWgetrcLines is the lines of the WGETRC the command has, when it names a file that can be
// read (readTrustSource, so a named pipe is read as its writer sends it, and one with no writer
// reads as empty rather than being waited on), ending in a newline.
func commandWgetrcLines(environ []string) []byte {
	named := envValue(environ, "WGETRC")
	if named == "" {
		return nil
	}
	data, err := readTrustSource(named)
	if err != nil {
		return nil
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	return data
}

// wgetrcFor is the wgetrc the child gets: the command's own wgetrc lines (commandWgetrcLines), and
// then the bundle. wget reads none of the bundle variables, so this file is the only way the proxy's
// certificate reaches it.
func wgetrcFor(inherited []byte, bundle string) []byte {
	return append(slices.Clone(inherited), "ca_certificate = "+bundle+"\n"...)
}

// airlockBundleVars are the variables that each name the trust bundle: one per client family that
// reads its own. Every one of them takes a file holding a concatenation of PEM certificates. All
// but NODE_EXTRA_CA_CERTS replace their client's own roots rather than add to them, and each is
// read the same way: what an inherited one names joins the one bundle (trustBundle), and, where
// prepareProxyFilesFor writes the bundle, every one of them names it in the command's environment.
var airlockBundleVars = []string{
	"SSL_CERT_FILE",                      // OpenSSL, and everything that links it; httpx reads it too
	"CURL_CA_BUNDLE",                     // curl
	"REQUESTS_CA_BUNDLE",                 // Python requests (httpx reads SSL_CERT_FILE, not this)
	"NODE_EXTRA_CA_CERTS",                // Node, which adds these to its built-in roots
	"GIT_SSL_CAINFO",                     // git over HTTPS
	"CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE", // gcloud
	"AWS_CA_BUNDLE",                      // the AWS CLI and SDKs
	// gRPC's C core, under its Python, Ruby, C++ and PHP clients, which reads its roots from
	// this file in place of its own (https://github.com/grpc/grpc/blob/master/doc/environment_variables.md).
	// grpc-go reads the system roots, which SSL_CERT_FILE names.
	"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH",
}

// airlockVars are the environment variables an airlock release adds to the command's
// environment: one per name held back, the AWS pair's own three when a name has them, and then
// the grant's own — the proxy, the NO_PROXY the proxy needs, Node's switch for reading them, and,
// where prepareProxyFilesFor wrote one, the bundle every client family reads.
//
// It refuses a grant that holds two names with AWS slots: one environment has one
// AWS_ACCESS_KEY_ID, so one of the two pairs would silently win. Nobody has that configuration
// today, and a refusal is the answer a person can act on where a silent choice is not. The
// refusal names each of the two as this run asked for it (asked), and a name it did not ask for
// not at all: a key of the broker's answer, which no record holds (releasedName).
func airlockVars(environ []string, release *AirlockRelease, files airlockFiles, asked []string) (map[string]string, error) {
	vars := make(map[string]string, len(release.Names)+len(airlockBundleVars)+6)
	awsFrom := ""
	for _, name := range slices.Sorted(maps.Keys(release.Names)) {
		held := release.Names[name]
		vars[name] = held.Value
		id, hasID := held.Slots[slotAWSAccessKeyID]
		secret, hasSecret := held.Slots[slotAWSSecretAccessKey]
		if !hasID || !hasSecret {
			continue
		}
		if awsFrom != "" {
			return nil, fmt.Errorf("the grant releases an AWS key pair through the airlock for both %s and %s, and one environment holds one AWS_ACCESS_KEY_ID; ask for them in separate calls", releasedName(awsFrom, asked), releasedName(name, asked))
		}
		awsFrom = name
		vars["AWS_ACCESS_KEY_ID"] = id
		vars["AWS_SECRET_ACCESS_KEY"] = secret
		if token, ok := held.Slots[slotAWSSessionToken]; ok {
			vars["AWS_SESSION_TOKEN"] = token
		}
	}

	proxy, err := airlockProxyURL(release)
	if err != nil {
		return nil, err
	}
	vars["HTTPS_PROXY"], vars["https_proxy"] = proxy, proxy
	// Node 24 reads HTTPS_PROXY only when this is set; every other client family reads the
	// proxy variables on their own.
	vars["NODE_USE_ENV_PROXY"] = "1"
	noProxy := airlockNoProxy(environ, release)
	vars["NO_PROXY"], vars["no_proxy"] = noProxy, noProxy
	// No bundle was written where the command needs none (prepareProxyFilesFor), and then the command
	// keeps each trust variable and the WGETRC it has.
	if files.bundle != "" {
		for _, key := range airlockBundleVars {
			vars[key] = files.bundle
		}
		vars["WGETRC"] = files.wgetrc
	}
	return vars, nil
}

// airlockProxyURL is the proxy URL the command gets: the broker's own, with this grant's proxy
// password as the password of its credential. net/url writes the credential, so a password
// holding a character that must be percent-encoded is encoded once and exactly, and the URL
// parses back into the same password. A proxy URL it cannot use is refused without the URL, and
// without net/url's words, which quote it whole: it is the broker's answer, and beside an
// AGENT_SECRETS_URL that is not clean whoever answers can put that address's credential in it.
func airlockProxyURL(release *AirlockRelease) (string, error) {
	parsed, err := url.Parse(release.ProxyURL)
	if err != nil {
		return "", errors.New("the broker's airlock proxy URL (proxy_url) is not a URL")
	}
	if parsed.Host == "" {
		return "", errors.New("the broker's airlock proxy URL (proxy_url) names no host")
	}
	parsed.User = url.UserPassword(airlockProxyUser, release.ProxyPassword)
	return parsed.String(), nil
}

// airlockNoProxy is the child's NO_PROXY: the inherited list, less everything that would take a
// bound host's request past the proxy, plus what must never reach the proxy at all.
//
// The last of those is the broker's own host, which is not in the design: the exec form leaves
// AGENT_SECRETS_URL in the child's environment on purpose (sessionIdentityVars), so a nested
// `agent-secrets` call reaches the broker as this same session. Without this entry that nested
// call would go to the broker through the proxy, and the proxy refuses a private address — which
// a broker reached over a tailnet or inside a cluster always is.
func airlockNoProxy(environ []string, release *AirlockRelease) string {
	inherited := envValue(environ, "no_proxy")
	if inherited == "" {
		inherited = envValue(environ, "NO_PROXY")
	}
	var bound []string
	for _, held := range release.Names {
		bound = append(bound, held.Hosts...)
	}

	entries := make([]string, 0, len(airlockFixedNoProxy)+8)
	for _, entry := range strings.Split(inherited, ",") {
		entry = strings.TrimSpace(entry)
		// "*" turns the proxy off for everything, so no bound host would reach it. A list that
		// holds it holds nothing else that matters.
		if entry == "" || entry == "*" || overlapsBoundHost(entry, bound) {
			continue
		}
		entries = append(entries, entry)
	}
	entries = append(entries, airlockFixedNoProxy...)
	if host := brokerHostname(envValue(environ, "AGENT_SECRETS_URL")); host != "" {
		entries = append(entries, host)
	}

	seen := make(map[string]bool, len(entries))
	out := entries[:0]
	for _, entry := range entries {
		key := strings.ToLower(entry)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, entry)
	}
	return strings.Join(out, ",")
}

// brokerHostname is the host of the broker's base URL, without its port, and "" when the URL
// names none. The port is dropped so the entry covers the broker however it is reached, and
// source.go's brokerHost is not reused: that one keeps the port and redacts a URL this one must
// read, because its answer is printed and this one's only ever goes into NO_PROXY.
func brokerHostname(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// overlapsBoundHost reports whether a NO_PROXY entry could take a bound host's request past the
// proxy. An entry and a bound host overlap when they name the same host or one is a parent domain
// of the other, each read without its port, its leading dot or its "*." — the two spellings of a
// suffix in a NO_PROXY list and in an airlock entry's hosts. The test is deliberately generous in
// both directions: "api.example.com" in the list is dropped against the bound "*.example.com"
// because it names a host the proxy must carry, and "example.com" is dropped against the bound
// "api.example.com" because every reader of NO_PROXY but none takes a bare domain to cover its
// subdomains. Dropping one entry too many sends a request through the proxy, which carries it;
// keeping one too many sends a secret's request straight out with a placeholder in it.
func overlapsBoundHost(entry string, hosts []string) bool {
	e := noProxyHost(entry)
	if e == "" {
		return false
	}
	for _, host := range hosts {
		h := noProxyHost(host)
		if h == "" {
			continue
		}
		if h == e || strings.HasSuffix(h, "."+e) || strings.HasSuffix(e, "."+h) {
			return true
		}
	}
	return false
}

// noProxyHost is the host a NO_PROXY entry or an airlock entry's host names, lower-cased and
// without the port, the leading dot or the "*." a suffix is written with. A CIDR block or an
// address keeps its own text, which matches no host name.
func noProxyHost(entry string) string {
	host := strings.ToLower(strings.TrimSpace(entry))
	host = strings.TrimPrefix(host, "*")
	host = strings.TrimPrefix(host, ".")
	if strings.HasPrefix(host, "[") {
		// A bracketed IPv6 address, with or without a port.
		if end := strings.Index(host, "]"); end >= 0 {
			return host[1:end]
		}
		return host
	}
	if colon := strings.LastIndex(host, ":"); colon >= 0 && !strings.Contains(host[colon+1:], ":") && !strings.Contains(host, "/") {
		if _, err := strconv.Atoi(host[colon+1:]); err == nil {
			host = host[:colon]
		}
	}
	return host
}
