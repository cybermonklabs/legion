package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
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
// when no directory holds the session: a session that is not in what the search read is absent.
// err is a failure to look, never the session's absence: a place the search cannot read, and so
// may hold the session — the recorded path, a searched sessions directory, a folder of one or a
// session in one — is runtime.ErrSessionStore, a store that did not answer, which the claim's
// machine waits out a minute at a time with the session kept, and a pane sessions directory that
// cannot be resolved is the launch's own failure. So is a recorded path one of whose folders is a
// file (ENOTDIR): the path names no folder there, which no wait mends, so the refusal names it and
// the machine charges it. A folder another user owns and denies this daemon's user (othersFolder)
// is passed over at every level: the recorded path's own, a sessions directory, a folder of one
// and a session in it. It is no part of this daemon's session store, so a session under it is
// searched for elsewhere, as a session that is not there is: a takeover whose pod recorded its
// session under the image's home, which is another user's folder on this host, finds the copy
// carried into the pane's sessions directory. skipped is each place passed over so, with why, in
// the order met (skippedFor), so the launch can name them; with err, it holds what the search
// passed over before it failed.
func sessionFileOf(recorded string, paneSessions func() (string, error)) (found string, searched, skipped []string, err error) {
	if _, err := os.Stat(recorded); err == nil {
		return recorded, nil, nil, nil
	} else if errors.Is(err, syscall.ENOTDIR) {
		return "", nil, nil, fmt.Errorf("the recorded session %s is under a file where a folder should be: %w", recorded, err)
	} else if othersFolder(recorded, err) {
		skipped = append(skipped, skippedFor(recorded, err))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, nil, fmt.Errorf("%w: %v", runtime.ErrSessionStore, err)
	}
	searched = []string{filepath.Dir(filepath.Dir(recorded))}
	if paneSessions != nil {
		pane, err := paneSessions()
		if err != nil {
			return "", nil, skipped, err
		}
		if pane != "" && pane != searched[0] {
			searched = append(searched, pane)
		}
	}
	name := filepath.Base(recorded)
	for _, sessions := range searched {
		var passed []string
		found, passed, err = newestSession(sessions, name)
		skipped = append(skipped, passed...)
		if err != nil || found != "" {
			return found, searched, skipped, err
		}
	}
	return "", searched, skipped, nil
}

// newestSession is the regular file written last among those one folder below sessions named
// exactly name or, for Oh My Pi's `<timestamp>_<session id>.jsonl`, of any name ending in
// `_<session id>.jsonl`, the session id name gives under any timestamp; "" for none. On equal
// times a file of the recorded name is taken before one of the session id alone. skipped is each
// place under sessions another user's folder kept it from, with why, and err a place it could not
// read (newestOneFolderDown). No character of sessions or name is a pattern, and the match by the
// id is on the end of a file's name: that picks out one session because Oh My Pi's session ids,
// UUIDs, hold no `_`.
func newestSession(sessions, name string) (found string, skipped []string, err error) {
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
// directory; "" for none. skipped is each place under sessions another user's folder kept it from,
// with why, and err a place it could not read (newestOneFolderDown). No character of sessions or
// id is a pattern, and the match is on the end of a file's name: that picks out one session
// because Oh My Pi's session ids, UUIDs, hold no `_`. An empty id names no session, so it is
// searched for nowhere.
func sessionFileByID(sessions, id string) (found string, skipped []string, err error) {
	if id == "" {
		return "", nil, nil
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
// exist, or is no directory, holds none. An entry of it that is neither a folder nor a symlink — a
// file, a socket, a named pipe — is skipped by its type and never opened, so whether the daemon
// may open it never matters; a symlink to a file or to nothing, and an entry gone by the time it
// is read, are skipped too. A matching name that leads to no file by the time it is checked (gone,
// or a symlink through a file), or to one that is not a regular file, is skipped. None of those is
// a failure to read: each is a place the search read and found no session in. A folder another
// user owns that denies this daemon's user (othersFolder) — sessions itself, a folder of it, or
// the way to a matching file — is no part of this daemon's session store and holds none of its
// sessions, as a sessions directory such as /tmp holds folders of other users: it is passed over,
// and listed in skipped, in the order met, as `<path>: <why>` (skippedFor), so the launch can name
// what its search passed over. Any other failure to read sessions, a folder of it, or a matching
// file — a folder of this daemon's own user it may not read, one that can be listed but not
// entered, an I/O error — is err, runtime.ErrSessionStore: the session may be there, and calling
// it missing would start a fresh session over the saved one. Of several written at the same time,
// the first matched is taken: the files the first of names matches, then those only the next
// matches, and so on, each name's files in the order of their folders' names and then of their own.
func newestOneFolderDown(sessions string, names ...wantedName) (newest string, skipped []string, err error) {
	folders, err := os.ReadDir(sessions)
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return "", nil, nil
	case othersFolder(sessions, err):
		return "", []string{skippedFor(sessions, err)}, nil
	case err != nil:
		return "", nil, fmt.Errorf("%w: %v", runtime.ErrSessionStore, err)
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
		case othersFolder(dir, err):
			skipped = append(skipped, skippedFor(dir, err))
			continue
		case err != nil:
			return "", skipped, fmt.Errorf("%w: %v", runtime.ErrSessionStore, err)
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
	var newestAt time.Time
	for _, candidates := range matched {
		for _, candidate := range candidates {
			info, err := os.Stat(candidate)
			switch {
			case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
				continue
			case othersFolder(candidate, err):
				skipped = append(skipped, skippedFor(candidate, err))
				continue
			case err != nil:
				return "", skipped, fmt.Errorf("%w: %v", runtime.ErrSessionStore, err)
			case !info.Mode().IsRegular():
				continue
			}
			if newest == "" || info.ModTime().After(newestAt) {
				newest, newestAt = candidate, info.ModTime()
			}
		}
	}
	return newest, skipped, nil
}

// skippedFor is how a search's skipped list (sessionFileOf, newestOneFolderDown) names path, a
// place another user's folder kept it from, and err, why: `<path>: <why>`, without the path that
// os.ReadDir's and os.Stat's errors (os.PathError) repeat.
func skippedFor(path string, err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Path == path {
		err = pathErr.Err
	}
	return path + ": " + err.Error()
}

// othersFolder is whether err, the refusal to read or look at path, is this daemon's user denied
// by a folder that another user owns: path itself, or the nearest folder above it that can be
// looked at, when the denial is of a folder on the way to it. A folder whose owner cannot be read
// is not taken for another's.
func othersFolder(path string, err error) bool {
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
		return false
	}
	for dir := path; ; dir = filepath.Dir(dir) {
		info, statErr := os.Stat(dir)
		if statErr == nil {
			owner, ok := info.Sys().(*syscall.Stat_t)
			return ok && int(owner.Uid) != os.Geteuid()
		}
		if !errors.Is(statErr, syscall.EACCES) && !errors.Is(statErr, syscall.EPERM) || dir == filepath.Dir(dir) {
			return false
		}
	}
}

// searchedIn is the sessions directories a search for a session found nothing in (sessionFileOf),
// as a refusal names them.
func searchedIn(searched []string) string {
	if len(searched) == 2 {
		return searched[0] + " or of the pane's sessions directory " + searched[1]
	}
	return strings.Join(searched, ", ")
}
