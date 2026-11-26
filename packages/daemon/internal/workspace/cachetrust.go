package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// OwnCacheRepository takes the cache's bare repository before any cache git runs. It answers
// whether a cache clone Fetch may fetch into is there. A repository the cache did not write is
// removed without following its links, and a cache clone it made has interrupted locks and partial
// packs cleared before it is reused.
func OwnCacheRepository(gitDir string, repo ghrepo.Repository, now time.Time, log func(string)) (bool, error) {
	owner := filepath.Dir(gitDir)
	if info, err := os.Lstat(owner); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return false, removeForeignCache(owner, owner, "it is a symlink", log)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(gitDir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	why, err := foreignCache(gitDir)
	if err != nil {
		return false, err
	}
	if why != "" {
		return false, removeForeignCache(gitDir, repo.String(), why, log)
	}
	if err := RemoveStaleCacheLocks(gitDir, now, log); err != nil {
		return false, err
	}
	return true, nil
}

// removeForeignCache removes path without following a link and records why it was not a cache
// clone this cache wrote, so Fetch can build a whole cache clone in its place.
func removeForeignCache(path, what, why string, log func(string)) error {
	if err := rolefile.RemoveTree(path); err != nil {
		return fmt.Errorf("remove the repository cache's %s (%s): %w", path, why, err)
	}
	log(fmt.Sprintf("the repository cache's %s is not one this fetch wrote (%s): removed; the cache is filled afresh", what, why))
	return nil
}

// foreignCache is why the bare repository gitDir is not a cache clone Fetch wrote, or "" when it
// is: a directory, not a gitfile, holding no symlink and nothing but regular files and directories,
// whose top level holds only what git's own bare init, clone and fetch write, whose configuration
// holds only the keys those and cacheConfiguration write (cacheConfigKeys), whose hooks are git's
// samples alone (none of which git runs), whose info holds only git's exclude, and whose objects
// name no alternates. A regular file named `<file>.lock` anywhere in it is a lock a killed git left
// beside a file it was replacing: git reads nothing of one, so it does not make the cache foreign,
// and RemoveStaleCacheLocks removes it once the cache is taken. Fetch runs git in this repository
// holding the provisioning token, so a repository in another shape could carry a hook,
// configuration or link the fetch would run or follow. It reads the repository's files and runs
// nothing in it.
func foreignCache(gitDir string) (string, error) {
	info, err := os.Lstat(gitDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return fmt.Sprintf("it is a %s, not a directory", rolefile.Kind(info.Mode())), nil
	}
	why := ""
	err = filepath.WalkDir(gitDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == gitDir {
			return nil
		}
		relative, err := filepath.Rel(gitDir, path)
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() && !entry.IsDir() {
			why = fmt.Sprintf("it holds %s, a %s", relative, rolefile.Kind(entry.Type()))
			return filepath.SkipAll
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".lock") {
			return nil
		}
		if refusal := cacheEntryRefusal(filepath.ToSlash(relative), entry.IsDir()); refusal != "" {
			why = refusal
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil || why != "" {
		return why, err
	}
	config, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return "", err
	}
	return cacheConfigRefusal(string(config)), nil
}

// cacheEntryRefusal is why name, a path under a repository cache clone, is not one Fetch or Git's
// own maintenance writes there, or "" when it is. Git gc packs refs, expires reflogs, records
// failures in gc.log, leaves gc.pid if it is killed, and updates commit-graph and server-info files.
// The cache still rejects hooks other than Git's samples, configuration beyond cacheConfigKeys, and
// object alternates, because Fetch would execute or follow those under its provisioning credential.
func cacheEntryRefusal(name string, dir bool) string {
	if !dir && serverInfoLeftover.MatchString(name) {
		return ""
	}
	top, rest, nested := strings.Cut(name, "/")
	switch top {
	case "HEAD", "config", "description", "packed-refs", "FETCH_HEAD", "gc.log", "gc.pid":
		if !nested && !dir {
			return ""
		}
	case "branches", "logs", "refs":
		return ""
	case "hooks":
		if !nested || (!dir && strings.HasSuffix(rest, ".sample") && !strings.Contains(rest, "/")) {
			return ""
		}
		return "it holds the hook " + name + ", which git would run"
	case "info":
		if !nested || ((rest == "exclude" || rest == "refs") && !dir) {
			return ""
		}
	case "objects":
		inner, ok := strings.CutPrefix(rest, "info/")
		if !ok || inner == "packs" || inner == "commit-graph" || inner == "commit-graphs" || strings.HasPrefix(inner, "commit-graphs/") {
			return ""
		}
	}
	return "it holds " + name + ", which no fetch writes"
}

// serverInfoLeftover is the temporary file git's update-server-info writes info/refs or
// objects/info/packs through, `<file>_` and six random characters (git's mks_tempfile), before it
// renames the file into place.
var serverInfoLeftover = regexp.MustCompile(`^(?:info/refs|objects/info/packs)_[A-Za-z0-9]{6}$`)

// cacheConfigKeys are the configuration a repository cache may hold. Git's bare,
// single-branch no-tags clone writes remote.origin.url and remote.origin.tagopt. Cache refreshes
// name their URL and refspec on the command line, so no persistent fetch refspec belongs here.
var cacheConfigKeys = map[string][]string{
	"core.repositoryformatversion": {"0", "1"},
	"core.filemode":                {"true", "false"},
	"core.bare":                    {"true"},
	"extensions.objectformat":      {"sha1"},
	"gc.auto":                      {"0"},
	"maintenance.auto":             {"false"},
	"fetch.unpacklimit":            {"1"},
	"remote.origin.url":            nil,
	"remote.origin.tagopt":         {"--no-tags"},
}

// cacheConfigHeader and cacheConfigPair are the only lines git writes in a configuration file: a
// section header alone on its line, `[section]` or `[section "subsection"]`, and `\tkey = value`
// with a value git reads as written (no quote, escape or comment). git also reads a key on its
// header's line (`[section]key=value`) and ends an unquoted value at `;` or `#`, so a line in any
// other form could set what no line of these two forms shows.
var (
	cacheConfigHeader = regexp.MustCompile(`^\[([A-Za-z0-9-]+)(?: "([^"\\]*)")?\]$`)
	cacheConfigPair   = regexp.MustCompile(`^\t([A-Za-z][A-Za-z0-9-]*) = ([^"\\;#]*)$`)
)

// cacheConfigRefusal is why config, a repository cache clone's configuration file, is not one git
// and cacheConfiguration wrote, or "" when it is: only whole lines of git's own two forms
// (cacheConfigHeader, cacheConfigPair), each key one cacheConfigKeys allows with a value it allows.
// Anything else (a key on its header's line, an include, a comment, a quoted or escaped value, a
// key the list does not name) is refused, since git would read it and the fetch would act on it.
func cacheConfigRefusal(config string) string {
	section := ""
	for number, line := range strings.Split(strings.TrimSuffix(config, "\n"), "\n") {
		if line == "" {
			continue
		}
		if header := cacheConfigHeader.FindStringSubmatchIndex(line); header != nil {
			section = strings.ToLower(line[header[2]:header[3]])
			if header[4] >= 0 {
				section += "." + line[header[4]:header[5]]
			}
			continue
		}
		pair := cacheConfigPair.FindStringSubmatch(line)
		if pair == nil || section == "" {
			return fmt.Sprintf("its configuration's line %d, %q, is not one git writes", number+1, line)
		}
		name, value := section+"."+strings.ToLower(pair[1]), pair[2]
		allowed, known := cacheConfigKeys[name]
		if !known {
			return fmt.Sprintf("its configuration sets %s, which no fetch writes", name)
		}
		if allowed != nil && !slices.Contains(allowed, value) {
			return fmt.Sprintf("its configuration sets %s to %q, which no fetch writes", name, value)
		}
	}
	return ""
}
