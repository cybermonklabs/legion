package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
// last is taken. Every directory and name is matched as written (newestOneFolderDown). found is ""
// when no directory holds the session; err is a failure to look, never the session's absence: a
// recorded path that cannot be checked, a pane sessions directory that cannot be resolved, a
// sessions directory or a folder of it that cannot be read, or a matching file in it that cannot
// be checked (newestOneFolderDown).
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

// newestSession is the regular file written last among those one folder below sessions named
// exactly name or, for Oh My Pi's `<timestamp>_<session id>.jsonl`, of any name ending in
// `_<session id>.jsonl`, the session id name gives under any timestamp; "" for none, and err a
// failure to read sessions or a folder of it, or to check a matching file (newestOneFolderDown).
// No character of sessions or name is a pattern, and the match by the id is on the end of a
// file's name: that picks out one session because Oh My Pi's session ids, UUIDs, hold no `_`.
func newestSession(sessions, name string) (string, error) {
	names := []wantedName{exactly(name)}
	if stem, ok := strings.CutSuffix(name, ".jsonl"); ok {
		if _, id, ok := strings.Cut(stem, "_"); ok && id != "" {
			names = append(names, endingIn("_"+id+".jsonl"))
		}
	}
	return newestOneFolderDown(sessions, names...)
}

// sessionFileByID is the file a resume of a session named by its id alone continues (launch): the
// regular file written last among Oh My Pi's `<timestamp>_<id>.jsonl`, any name ending in
// `_<id>.jsonl`, one folder below sessions, where Oh My Pi keeps the sessions of every working
// directory; "" for none, and err a failure to read sessions or a folder of it, or to check a
// matching file (newestOneFolderDown). No character of sessions or id is a pattern, and the match
// is on the end of a file's name: that picks out one session because Oh My Pi's session ids,
// UUIDs, hold no `_`. An empty id names no session, so it is searched for nowhere.
func sessionFileByID(sessions, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	return newestOneFolderDown(sessions, endingIn("_"+id+".jsonl"))
}

// wantedName is one file name a search one folder down asks for (newestOneFolderDown), matched
// as written: a file named exactly name (exactly), or, with suffix set, any file whose name ends
// in name (endingIn).
type wantedName struct {
	name   string
	suffix bool
}

// exactly is the file named name and no other.
func exactly(name string) wantedName { return wantedName{name: name} }

// endingIn is every file whose name ends in suffix.
func endingIn(suffix string) wantedName { return wantedName{name: suffix, suffix: true} }

func (w wantedName) matches(file string) bool {
	if w.suffix {
		return strings.HasSuffix(file, w.name)
	}
	return file == w.name
}

// newestOneFolderDown is the regular file written last among those one folder below sessions
// whose name one of names matches, newestSession's and sessionFileByID's one search; "" for none.
// sessions is read as the directory it names, each of its entries as the folder it names, a
// symlinked folder followed, and each file name is matched as written (wantedName), so no
// character of a directory, a name or an id is a pattern. A sessions directory that does not
// exist holds none. An entry of it that is neither a folder nor a symlink — a file, a socket, a
// named pipe — is skipped by its type and never opened, so whether the daemon may open it never
// matters; a symlink to a file or to nothing, and an entry gone by the time it is read, are
// skipped too. A matching name that leads to no file by the time it is checked (gone, or a
// symlink through a file), or to one that is not a regular file, is skipped. Any other failure to
// read sessions or a folder of it, or to check a matching file — one in a folder that can be
// listed but not entered — is err: what it could not read or check may hold the session, so it is
// a failure to look, never the session's absence. Of several written at the same time, the first
// matched is taken: the files the first of names matches, then those only the next matches, and
// so on, each name's files in the order of their folders' names and then of their own.
func newestOneFolderDown(sessions string, names ...wantedName) (string, error) {
	folders, err := os.ReadDir(sessions)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	// matched holds, for each of names, the files it is the first of names to match.
	matched := make([][]string, len(names))
	for _, folder := range folders {
		if !folder.IsDir() && folder.Type()&os.ModeSymlink == 0 {
			continue
		}
		dir := filepath.Join(sessions, folder.Name())
		files, err := os.ReadDir(dir)
		switch {
		case errors.Is(err, syscall.ENOTDIR), errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return "", err
		}
		for _, file := range files {
			for i, name := range names {
				if name.matches(file.Name()) {
					matched[i] = append(matched[i], filepath.Join(dir, file.Name()))
					break
				}
			}
		}
	}
	newest, newestAt := "", time.Time{}
	for _, candidates := range matched {
		for _, candidate := range candidates {
			info, err := os.Stat(candidate)
			switch {
			case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
				continue
			case err != nil:
				return "", err
			case !info.Mode().IsRegular():
				continue
			}
			if newest == "" || info.ModTime().After(newestAt) {
				newest, newestAt = candidate, info.ModTime()
			}
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
