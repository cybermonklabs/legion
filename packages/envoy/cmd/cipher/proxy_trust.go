// packages/envoy/cmd/cipher/proxy_trust.go
//
// The skip decision: whether a command reaches the broker's proxy with the trust it already has,
// so that agent-secrets writes no bundle and sets no trust variable for it (proxyRootsTrusted),
// and the bounded reads it makes to answer that (readRegularFile, certDirTrustsEvery).
// prepareProxyFilesFor in airlock.go asks it first.
package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// proxyRootsTrusted reports whether the command reaches the proxy with the trust it already has,
// so that it needs no bundle: the command's environment shows that every client family reads the
// system store (proxyClientsReadTheStore) and has no SSL_CERT_DIR; that store, which system
// answers, trusts every root the release carries; each file a trust variable of the command names
// holds every root too, as does wget's ca_directory (wgetTrusts); and every path this reads is
// absolute.
//
// Every root, not one of them: during a change of the proxy's root a release carries two, the
// old and the new, and a store that trusts one of them is not yet one the proxy can rely on. A
// release that carries no root this can parse is no root the store trusts, so it gets the bundle.
//
// Node, Python requests and httpx, uv, git, the AWS CLI, gcloud and gRPC's C core each read roots
// of their own, not the system store, unless a setting points them at it. A command without those
// settings, from a shell started before a launcher set them, a runtime that passes on only some
// variables or a systemd-run unit, would lose the proxy for those clients with no bundle, so it
// keeps the bundle, which reaches each of them. The answer errs toward writing the bundle, which
// is never wrong, only unneeded.
//
// The command keeps each trust variable it has when no bundle is written, and each one sets its
// client's roots: a file that lacks a root takes the proxy away from that client, as a file this
// process cannot read may, and either keeps the bundle, which holds that file's trust as well
// (trustBundle). NODE_EXTRA_CA_CERTS too: Node adds its file to the roots it has, and it is the
// one way the root reaches a Node that does not read the system store, which every Node before
// v24.6.0 and v22.19.0 does not (NODE_USE_SYSTEM_CA is added in those). git reads GIT_SSL_CAINFO
// after every level of its configuration, so an http.sslCAInfo in a gitconfig is no part of the
// answer once GIT_SSL_CAINFO names a file that holds every root (measured with git 2.47.3).
//
// A relative path is resolved from the directory of whoever reads it, and the command may read it
// from a directory other than the one it starts in (`git -C ../x`, `sh -c 'cd sub && curl'`), so a
// relative trust variable, WGETRC, SYSTEM_WGETRC, HOME or ca_directory never counts toward a skip.
//
// The command keeps its SSL_CERT_DIR too, which Node's system store (NODE_USE_SYSTEM_CA), uv
// (UV_SYSTEM_CERTS) and httpx read in place of the system's directory, Node with no system bundle
// file beside it. They do not read it alike, so no reading of the directories answers for all of
// them: Node v24 takes the whole value as one path (a ':' list is one directory that is not
// there) and an empty value as set, and httpx, through OpenSSL's capath, finds a root only under
// its subject-hash name. So any SSL_CERT_DIR entry, even an empty one, gets the bundle: its
// SSL_CERT_FILE reaches httpx, uv and OpenSSL beside the directory, and its NODE_EXTRA_CA_CERTS
// reaches Node.
//
// Nothing here opens a named pipe, a device or a socket that is one when it looks
// (readRegularFile): a pipe's bytes can be read once, and the bundle writer reads them afterwards.
func proxyRootsTrusted(environ []string, caPEM string, system func() *x509.CertPool) bool {
	// The command's environment first: it alone answers most commands, and then the system
	// bundle is not read.
	if !proxyClientsReadTheStore(environ) || envHas(environ, "SSL_CERT_DIR") {
		return false
	}
	roots := proxyRoots(caPEM)
	if len(roots) == 0 || !poolTrustsEvery(system(), roots) || !wgetTrusts(environ, roots) {
		return false
	}
	// One file is often named by several of these, as the system bundle is, and is read once.
	named := make([]string, 0, len(airlockBundleVars))
	for _, key := range airlockBundleVars {
		named = append(named, envValue(environ, key))
	}
	slices.Sort(named)
	for _, path := range slices.Compact(named) {
		data, err := readRegularFile(path)
		if err != nil {
			return false
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(data)
		if !poolTrustsEvery(pool, roots) {
			return false
		}
	}
	return true
}

// proxyClientsReadTheStore reports whether the command's environment shows that every client
// family which reads roots of its own by default reads the system store: each of
// airlockBundleVars is an absolute path (proxyRootsTrusted then checks the file it names), and
// uv's switch is on (UV_SYSTEM_CERTS as uv 0.13.5 reads a true value: 1, t, true, y, yes or on, in
// any case). An unset or empty value is no absolute path: an empty GIT_SSL_CAINFO is a setting git
// reads, as an empty CA file that fails every HTTPS request (measured with git 2.47.3), and an
// empty NODE_EXTRA_CA_CERTS leaves a Node that does not read the system store without the root.
// A relative path is resolved from the directory of whoever reads it, which need not be this one.
func proxyClientsReadTheStore(environ []string) bool {
	for _, key := range airlockBundleVars {
		if !filepath.IsAbs(envValue(environ, key)) {
			return false
		}
	}
	switch strings.ToLower(envValue(environ, "UV_SYSTEM_CERTS")) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	}
	return false
}

// systemWgetrcFile is the system wgetrc built into wget, Debian's, which wget reads when
// SYSTEM_WGETRC names no file that is there. A test replaces it.
var systemWgetrcFile = "/etc/wgetrc"

// wgetTrusts reports whether wget's startup files leave it trusting every root. wget reads no
// trust variable, only those files, and the command keeps them when no bundle is written: first
// the system wgetrc (the file SYSTEM_WGETRC names when it is there, else systemWgetrcFile when
// that is there), then the user's (the file WGETRC names, or homeWgetrc when it is there and
// WGETRC is unset or empty); "there" is wget's own test, a stat; measured with wget 1.25.0 and
// 1.21.4. wget built on GnuTLS, as Debian's is, reads its ca_directory in place of the system
// store and adds a ca_certificate to that trust rather than replacing it (measured with wget
// 1.25.0). So the last ca_directory across those files, the one wget reads, must hold every root,
// an empty one loads no directory and no system store at all, and a ca_certificate takes nothing
// away. A startup file this process cannot read, a WGETRC naming none, or a relative path to any
// of them or to the ca_directory, which wget resolves from its own directory, is one it cannot
// vouch for. wget ignores case, '_' and '-' in a command's name, and skips a line that opens with
// '#'.
func wgetTrusts(environ []string, roots []*x509.Certificate) bool {
	var files []string
	if named := envValue(environ, "SYSTEM_WGETRC"); named != "" && !filepath.IsAbs(named) {
		return false
	} else if named != "" && exists(named) {
		files = append(files, named)
	} else if exists(systemWgetrcFile) {
		files = append(files, systemWgetrcFile)
	}
	if named := envValue(environ, "WGETRC"); named != "" {
		files = append(files, named)
	} else if user := homeWgetrc(environ); user != "" && !filepath.IsAbs(user) {
		return false
	} else if user != "" && exists(user) {
		files = append(files, user)
	}
	directory, set := "", false
	for _, file := range files {
		if !filepath.IsAbs(file) {
			return false
		}
		data, err := readRegularFile(file)
		if err != nil {
			return false
		}
		for line := range strings.Lines(string(data)) {
			name, value, found := strings.Cut(line, "=")
			if found && wgetrcCommand.Replace(strings.ToLower(strings.TrimSpace(name))) == "cadirectory" {
				directory, set = strings.TrimSpace(value), true
			}
		}
	}
	if !set {
		return true
	}
	// An empty value is still a ca_directory: wget keeps it, loads no directory from it, and so
	// loads no system store either.
	return filepath.IsAbs(directory) && certDirTrustsEvery(directory, roots)
}

// homeWgetrc is the user wgetrc wget reads where WGETRC names none: $HOME/.wgetrc, HOME being the
// command's own when it is set, even empty (wget joins the two with '/', so an empty HOME reads
// /.wgetrc), and the password database's home only when HOME is unset; "" where it has neither.
func homeWgetrc(environ []string) string {
	home := envValue(environ, "HOME")
	if !envHas(environ, "HOME") {
		if home = passwdHome(); home == "" {
			return ""
		}
	}
	return home + "/.wgetrc"
}

// wgetrcCommand folds a wgetrc command's name to the one wget reads it as.
var wgetrcCommand = strings.NewReplacer("_", "", "-", "")

// proxyCertFileMaxBytes, proxyCertDirMaxFiles and proxyCertDirMaxBytes bound what the skip decision
// reads: each file (a trust file, the system bundle, a wget startup file, a ca_directory's entry),
// a ca_directory's entries, and a ca_directory's bytes in all. A certificate is a few kilobytes,
// Debian's whole bundle is under 250 KB, and Debian's /etc/ssl/certs holds about 300 entries, each
// root under two names and the bundle, under 2 MiB in all. Past a bound the answer is that the
// command needs the bundle, which is never wrong. The bundle writer reads a named pipe or a device
// that a bundle variable or WGETRC names up to proxyCertFileMaxBytes too, and a regular one whole
// (readTrustSource).
const (
	proxyCertFileMaxBytes = 1 << 20
	proxyCertDirMaxFiles  = 4096
	proxyCertDirMaxBytes  = 8 << 20
)

// certDirTrustsEvery reports whether the certificate directory dir holds every root. It reads the
// directory's regular files alone (readRegularFile), so a device, a named pipe or a socket there
// cannot stall or flood agent-secrets before the command runs, and opens the directory itself so
// that no open waits (openDirNoWait). A file it skips adds nothing, and a directory of more than
// proxyCertDirMaxFiles entries or more than proxyCertDirMaxBytes in all, or one this process
// cannot read, is one it cannot vouch for.
func certDirTrustsEvery(dir string, roots []*x509.Certificate) bool {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return false
	}
	f, err := openDirNoWait(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	entries, err := f.ReadDir(proxyCertDirMaxFiles + 1)
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > proxyCertDirMaxFiles {
		return false
	}
	pool := x509.NewCertPool()
	total := 0
	for _, entry := range entries {
		data, err := readRegularFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if total += len(data); total > proxyCertDirMaxBytes {
			return false
		}
		pool.AppendCertsFromPEM(data)
	}
	return poolTrustsEvery(pool, roots)
}

// errNotRegular and errFileTooLarge are readRegularFile's answers for a path that names something
// other than a regular file, and for one of more than proxyCertFileMaxBytes.
var (
	errNotRegular   = errors.New("not a regular file")
	errFileTooLarge = errors.New("larger than the bound")
)

// readRegularFile is the regular file at path, up to proxyCertFileMaxBytes (a symbolic link is
// followed to what it names), for the files the skip decision reads, where a file it will not read
// only means the bundle is written, and for the airlock key (proxyTrustDirKey). It looks before it
// opens, and opens nothing that is not a regular file when it looks: opening a named pipe for
// reading waits for a writer that may never come, and opening one at all lets a writer waiting in
// its own open go on and hand its bytes to a reader that closes them unread. A pipe swapped in
// between the look and the open is opened without waiting (openNoWait) and refused by what the
// open file is. Its error is the look's where that fails, so a path that is not there answers
// os.ErrNotExist; errNotRegular where the path names something else, and errFileTooLarge past the
// bound.
func readRegularFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	if info.Size() > proxyCertFileMaxBytes {
		return nil, errFileTooLarge
	}
	f, regular, size, err := openNoWait(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if !regular {
		return nil, errNotRegular
	}
	if size > proxyCertFileMaxBytes {
		return nil, errFileTooLarge
	}
	var data bytes.Buffer
	data.Grow(int(size) + bytes.MinRead)
	if _, err := data.ReadFrom(io.LimitReader(f, proxyCertFileMaxBytes+1)); err != nil {
		return nil, err
	}
	if data.Len() > proxyCertFileMaxBytes {
		return nil, errFileTooLarge
	}
	return data.Bytes(), nil
}

// proxyRoots is every certificate of the release's PEM, and nil when any block of it is not a
// certificate this can parse: a release this cannot read whole is one it cannot vouch for.
// pem.Decode passes over a block whose body is not base64 and answers the next one, so each
// block's opening line is counted against the certificates parsed.
func proxyRoots(caPEM string) []*x509.Certificate {
	var roots []*x509.Certificate
	rest := []byte(caPEM)
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = next
		if block.Type != "CERTIFICATE" {
			return nil
		}
		root, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil
		}
		roots = append(roots, root)
	}
	if strings.Count(caPEM, "-----BEGIN ") != len(roots) {
		return nil
	}
	return roots
}

// poolTrustsEvery reports whether each of roots verifies against pool as its roots
// (x509.Certificate.Verify): it chains to a certificate the pool holds, and is within its validity
// period. A nil pool trusts nothing here, where Verify would read it as the system's own.
func poolTrustsEvery(pool *x509.CertPool, roots []*x509.Certificate) bool {
	if pool == nil {
		return false
	}
	for _, root := range roots {
		if _, err := root.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
			return false
		}
	}
	return true
}
