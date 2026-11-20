package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// sessionFileOf is the file a resume continues the claim's recorded session from, and the
// sessions directories it searched for it: the recorded path itself when it is there, and otherwise
// the same session found by its file name in a working directory's folder, first of the recorded
// path's own sessions directory, then of the one paneSessions resolves, where the pane's harness
// keeps its sessions (Launcher.SessionsDir), resolved only when the recorded path is not there;
// nil is none. Oh My Pi keeps a session as
// `<sessions>/<folder named for the working directory>/<timestamp>_<session id>.jsonl` (Claude Code
// as `<projects>/<folder>/<session id>.jsonl`), so a session carried onto this host from where it
// was recorded — a pod, whose path names the image's home and profile — sits in a folder of the
// pane's own sessions directory, named for the pod's working directory or the pane's: each search
// looks one level below a sessions directory for a file of the recorded name, or, for Oh My Pi's
// name, of any timestamp with the recorded session id. Of several in one directory, the one written
// last is taken. found is "" when no directory holds the session; err is a failure to look, never
// the session's absence.
func sessionFileOf(recorded string, paneSessions func() (string, error)) (found string, searched []string, err error) {
	if _, err := os.Stat(recorded); err == nil {
		return recorded, nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	searched = []string{filepath.Dir(filepath.Dir(recorded))}
	if paneSessions != nil {
		pane, err := paneSessions()
		if err != nil {
			return "", nil, err
		}
		if pane != "" && pane != searched[0] {
			searched = append(searched, pane)
		}
	}
	name := filepath.Base(recorded)
	for _, sessions := range searched {
		if found, err = newestSession(sessions, name); err != nil || found != "" {
			return found, searched, err
		}
	}
	return "", searched, nil
}

// newestSession is the regular file written last among those one folder below sessions named name
// or, for Oh My Pi's `<timestamp>_<session id>.jsonl`, of any timestamp with name's session id; ""
// for none. A directory or name holding a glob character is searched nowhere: it would match other
// sessions.
func newestSession(sessions, name string) (string, error) {
	if strings.ContainsAny(sessions+name, globCharacters) {
		return "", nil
	}
	names := []string{name}
	if stem, ok := strings.CutSuffix(name, ".jsonl"); ok {
		if _, id, ok := strings.Cut(stem, "_"); ok && id != "" {
			names = append(names, "*_"+id+".jsonl")
		}
	}
	return newestOneFolderDown(sessions, names)
}

// sessionFileByID is the file a resume of a session named by its id alone continues (launch): the
// regular file written last among Oh My Pi's `<timestamp>_<id>.jsonl`, of any timestamp, one folder
// below sessions, where Oh My Pi keeps the sessions of every working directory; "" for none. A
// directory or id holding a glob character is searched nowhere, as newestSession searches nowhere
// for one: it would match other sessions. So is an empty id, which names no session, and an id
// holding a path separator, whose file name would reach below one folder down or out of sessions.
func sessionFileByID(sessions, id string) (string, error) {
	if id == "" || strings.ContainsAny(sessions+id, globCharacters) || strings.ContainsRune(id, filepath.Separator) {
		return "", nil
	}
	return newestOneFolderDown(sessions, []string{"*_" + id + ".jsonl"})
}

// globCharacters are the characters filepath.Glob reads as part of a pattern: `*`, `?`, `[` and
// the escape `\`.
const globCharacters = `*?[\`

// newestOneFolderDown is the regular file written last among those one folder below sessions
// whose name matches one of names, each a filepath.Glob pattern of a file name, newestSession's
// and sessionFileByID's one search; "" for none. Of several written at the same time, the first
// matched is taken. Its callers search nowhere for a sessions directory, name or id holding a glob
// character before they write names, so the only wildcards are the folder's and their own.
func newestOneFolderDown(sessions string, names []string) (string, error) {
	var candidates []string
	for _, name := range names {
		matches, err := filepath.Glob(filepath.Join(sessions, "*", name))
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			if !slices.Contains(candidates, match) {
				candidates = append(candidates, match)
			}
		}
	}
	newest, newestAt := "", time.Time{}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = candidate, info.ModTime()
		}
	}
	return newest, nil
}

// searchedIn is the sessions directories a search for a session found nothing in (sessionFileOf),
// as a refusal names them.
func searchedIn(searched []string) string {
	if len(searched) == 2 {
		return searched[0] + " or of the pane's sessions directory " + searched[1]
	}
	return strings.Join(searched, ", ")
}
