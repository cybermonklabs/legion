// Package ghconfig renders the two files a plain `gh` reads its GitHub credential from, so a
// Legion role's GitHub App tokens reach `gh` and `git` as gh's own configuration.
//
// The files are gh's `hosts.yml`, which holds the token for github.com, and its `config.yml`, in
// one directory per entry of the role's bundle under its bundle root (Bundle, WriteBundle): one
// per owner account the role's GitHub App is installed on, each holding that account's
// installation lease at the role's own permissions, with the issue's own owner's directory the
// one GH_CONFIG_DIR names (INFRA-2543). gh reads both files
// on every invocation and holds nothing between invocations, so the daemon rewriting hosts.yml
// from a fresh lease is what the next `gh` runs with. `legion gh`, the `gh` first on a pane's
// PATH, and `legion git-credential`, the helper every shared clone's `credential.helper` names
// (workspace.GitHubCredentialHelper), both pick the directory of the owner of the repository the
// command or git's request names, the one directory the bundle holds for that account — except
// for a repository a configured rename moved to another owner, which takes the directory of the
// owner it moved to, since that account's installation is the one holding it now. The pairs that
// route are the bundle's own file in the root, beside the owners' directories (Bundle, PairsFile),
// and the two wrappers read them from there and from nowhere else; a pod launched before that
// ruling may still hold an `<owner>/<repo>` directory of its own, which no token is read out of,
// and the Sandbox drift check reports both that and a root with no pairs file. The helper answers
// git from the chosen directory's hosts.yml and treats git's `store` and `erase` as no-ops. Where
// the bundle holds no directory for the owner git's request names, the helper reads that
// repository from GitHub, and a fork takes the directory its parent, or else its source, routes
// to (cmd/legion's forkUpstreamDir). In a pod the
// root is a read-only Secret volume, which gh never needs to write: it writes under
// GH_CONFIG_DIR only when told to
// (`gh auth login`, `gh config set`) or when migrating a pre-2.40 hosts.yml into its
// multi-account shape. A `config.yml` carrying `version: "1"` is what tells gh that migration has
// already happened, and Hosts renders the migrated shape it then expects.
package ghconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/yamlerr"
)

// HostsFile is the name of gh's per-host credential file under GH_CONFIG_DIR.
const HostsFile = "hosts.yml"

// ConfigFile is the name of gh's settings file under GH_CONFIG_DIR.
const ConfigFile = "config.yml"

// GitUser is the username GitHub documents for an installation token over HTTPS, and the user
// hosts.yml records for it, so `gh auth git-credential` answers git with it.
const GitUser = "x-access-token"

// Config is config.yml as gh >= 2.40 writes it after its multi-account migration: the version
// alone, which is what stops gh from attempting that migration write of its own.
const Config = "version: \"1\"\n"

// host is the github.com entry of hosts.yml in gh's multi-account shape: the active user's token
// and name at the top, the protocol git uses, and the same token again under `users`, where gh
// keeps every account logged in to the host. Decoding a pre-2.40 hosts.yml, which has no `users`
// block, fills the top-level fields alone, which is where TokenFromHosts reads.
type host struct {
	OAuthToken  string          `yaml:"oauth_token"`
	User        string          `yaml:"user"`
	GitProtocol string          `yaml:"git_protocol"`
	Users       map[string]user `yaml:"users,omitempty"`
}

// user is one account under a host's `users` block.
type user struct {
	OAuthToken string `yaml:"oauth_token"`
}

// hostsFile is hosts.yml: one entry, github.com, since Legion's Apps live on github.com alone.
type hostsFile struct {
	GitHub host `yaml:"github.com"`
}

// Hosts renders hosts.yml for token in the shape gh >= 2.40 writes after its multi-account
// migration, with GitUser as the one account:
//
//	github.com:
//	    oauth_token: <token>
//	    user: x-access-token
//	    git_protocol: https
//	    users:
//	        x-access-token:
//	            oauth_token: <token>
//
// yaml.v3 renders it, so a token the encoder would quote is quoted; an installation token is a
// `ghs_` string with no YAML-special character, so it never is.
func Hosts(token string) string {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(4)
	if err := encoder.Encode(hostsFile{GitHub: host{
		OAuthToken:  token,
		User:        GitUser,
		GitProtocol: "https",
		Users:       map[string]user{GitUser: {OAuthToken: token}},
	}}); err != nil {
		// Encoding a struct of strings cannot fail; a failure here is a bug, never a condition a
		// caller handles.
		panic(fmt.Sprintf("render %s: %v", HostsFile, err))
	}
	return buf.String()
}

// TokenFromHosts reads the github.com token out of a hosts.yml, the one Hosts renders or gh's own
// pre-2.40 shape without a `users` block. A file that does not parse, or that names no
// github.com oauth_token (an empty file included), is an error naming the key. A file yaml.v3
// cannot decode is refused naming the file and the line alone (yamlerr): its own text quotes the
// document, and the token is the document's one value of note.
func TokenFromHosts(data []byte) (string, error) {
	var hosts hostsFile
	if err := yaml.Unmarshal(data, &hosts); err != nil {
		return "", fmt.Errorf("parse %s: %s", HostsFile, yamlerr.Reason(err))
	}
	if hosts.GitHub.OAuthToken == "" {
		return "", fmt.Errorf("%s names no github.com oauth_token", HostsFile)
	}
	return hosts.GitHub.OAuthToken, nil
}

// Rendered is one role's two gh files, ready to write under its GH_CONFIG_DIR, with the label of
// the App the token belongs to ("implement" or "review") and the lease's expiry, so the daemon's
// log line can say whose token it wrote and how long it lasts.
type Rendered struct {
	Hosts     string
	Config    string
	App       string
	ExpiresAt time.Time
}

// Render is Rendered for one lease: hosts.yml for token, the fixed config.yml, and the App and
// expiry carried through for the log.
func Render(token, app string, expiresAt time.Time) Rendered {
	return Rendered{Hosts: Hosts(token), Config: Config, App: app, ExpiresAt: expiresAt}
}

// Write brings dir, one directory of a gh bundle, to rendered: the directory made 0700 if it is
// gone, and config.yml and hosts.yml each replaced (replaceFile) only where its content differs, so
// the gh reading them never sees a half-written file. It reports whether hosts.yml changed.
// WriteBundle calls it for each directory of a bundle.
//
// A pane can put anything at those paths between two writes, a named pipe or a symbolic link
// among them, and swap it in between any check and any open. replaceFile reads what is there only
// through ReadRegular, which never waits on a pipe and never follows a link, and writes only by
// renaming a file of its own over the path, so no such swap stalls the refresh, and with it every
// pane's, and none redirects a write.
func Write(dir string, rendered Rendered) (changed bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return false, err
	}
	if _, err := replaceFile(filepath.Join(dir, ConfigFile), rendered.Config); err != nil {
		return false, err
	}
	return replaceFile(filepath.Join(dir, HostsFile), rendered.Hosts)
}

// MaxFileBytes is the most ReadRegular reads of one file: far beyond any hosts.yml, config.yml or
// pairs file the daemon writes, each a few hundred bytes, and small enough that a file a pane
// planted costs its reader nothing.
const MaxFileBytes = 1 << 20

// ErrNotRegular is ReadRegular's answer for a path that is no regular file: a symbolic link, a
// named pipe, a directory, a socket or a device, none of which it reads from.
var ErrNotRegular = errors.New("is no regular file")

// ErrTooLarge is ReadRegular's answer for a regular file larger than MaxFileBytes.
var ErrTooLarge = fmt.Errorf("is larger than %d bytes, which no file the daemon writes is", MaxFileBytes)

// ReadRegular reads the regular file at path whole and refuses anything else without reading from
// it. It opens path with O_NOFOLLOW, so a symbolic link is refused rather than followed, and
// O_NONBLOCK, so a named pipe opens at once rather than waiting for a writer; it then asks the
// open file what it is (fstat), not the path, so a pane that swaps the path between a check and
// the open wins nothing; and it reads through a limit, so a file that grows while it is read is
// refused too. A path that is not there is fs.ErrNotExist; one that is no regular file is
// ErrNotRegular; one larger than MaxFileBytes is ErrTooLarge.
//
// Only the last component goes unfollowed. A pod's Secret volume shows each item's top directory
// as a link into the volume's `..data`, so an owner's directory is a link there, while the
// hosts.yml and config.yml inside it are regular files, which this reads as they are.
func ReadRegular(path string) ([]byte, error) { return readRegular(path, syscall.O_NOFOLLOW) }

// readRegularFollowing is ReadRegular for a file that a Secret volume shows as a link: the bundle's
// pairs file sits at the volume's top, so the kubelet makes it a link into `..data` (ReadPairs).
// It follows that link, and reads what it reaches only as ReadRegular reads: without waiting on a
// pipe, refusing anything the open file says is no regular file, and through the same limit.
func readRegularFollowing(path string) ([]byte, error) { return readRegular(path, 0) }

func readRegular(path string, follow int) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|follow, 0)
	if err != nil {
		// ELOOP is a symbolic link under O_NOFOLLOW; ENXIO and EOPNOTSUPP a socket; ENODEV a device
		// with no driver.
		var errno syscall.Errno
		if errors.As(err, &errno) && (errno == syscall.ELOOP || errno == syscall.ENXIO || errno == syscall.EOPNOTSUPP || errno == syscall.ENODEV) {
			return nil, fmt.Errorf("%s %w", path, ErrNotRegular)
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %w", path, ErrNotRegular)
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Errorf("%s %w", path, ErrTooLarge)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%s %w", path, ErrTooLarge)
	}
	return data, nil
}

// replaceFile brings path to content and reports whether it changed: a temporary file beside it
// (0600, as os.CreateTemp makes it) renamed over it, so a reader of the file — the gh of a pane
// reading a hosts.yml or a config.yml, the `legion gh` or `legion git-credential` of one reading
// the bundle's pairs file — never sees a half-written one, and a reader that opened the old file
// keeps reading it whole. A file whose content already is content is left alone, which is what
// keeps a bundle's writer from touching the files a lease's turn did not change (WriteBundle,
// Write). What is at path is read only through ReadRegular: anything that is no regular file is
// removed first, since a rename replaces a link or a pipe but not a directory, and one too large
// to read is simply replaced. The pane's own user can write every path handed here, so what is
// removed may be a directory it filled, and it is removed as every tree a pane writes is: without
// following a link in it (rolefile.RemoveTree), under path's directory resolved once, since that
// directory may be reached through a link and RemoveTree refuses a parent that is one.
func replaceFile(path, content string) (changed bool, err error) {
	current, err := ReadRegular(path)
	switch {
	case err == nil:
		if bytes.Equal(current, []byte(content)) {
			return false, nil
		}
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, ErrTooLarge):
	case errors.Is(err, ErrNotRegular):
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return false, err
		}
		if err := rolefile.RemoveTree(filepath.Join(parent, filepath.Base(path))); err != nil {
			return false, err
		}
	default:
		return false, err
	}
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return false, err
	}
	return true, nil
}
