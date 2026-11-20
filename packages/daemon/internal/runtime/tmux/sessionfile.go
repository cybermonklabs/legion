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
// when no directory holds the session. skipped is every folder of a searched sessions directory
// that the search could not read, and every matching file in one that it could not check, each
// with why, in the order met (newestOneFolderDown): the search runs as the user Oh My Pi runs as,
// so what that user cannot read holds no session Oh My Pi could resume, and is skipped as holding
// none. err is a failure to look, never the session's absence: a recorded path that cannot be
// checked, a pane sessions directory that cannot be resolved, or a searched sessions directory
// itself that cannot be read; skipped then holds what the directories searched before it skipped.
func sessionFileOf(recorded string, paneSessions func() (string, error)) (found string, searched, skipped []string, err error) {
	if _, err := os.Stat(recorded); err == nil {
		return recorded, nil, nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, nil, err
	}
	searched = []string{filepath.Dir(filepath.Dir(recorded))}
	if paneSessions != nil {
		pane, err := paneSessions()
		if err != nil {
			return "", nil, nil, err
		}
		if pane != "" && pane != searched[0] {
			searched = append(searched, pane)
		}
	}
	name := filepath.Base(recorded)
	for _, sessions := range searched {
		var unread []string
		found, unread, err = newestSession(sessions, name)
		skipped = append(skipped, unread...)
		if err != nil || found != "" {
			return found, searched, skipped, err
		}
	}
	return "", searched, skipped, nil
}

// newestSession is the regular file written last among those one folder below sessions named
// exactly name or, for Oh My Pi's `<timestamp>_<session id>.jsonl`, of any name ending in
// `_<session id>.jsonl`, the session id name gives under any timestamp; "" for none. skipped is
// each folder of sessions it could not read and each matching file it could not check, with why,
// and err a failure to read sessions itself (newestOneFolderDown). No character of sessions or
// name is a pattern, and the match by the id is on the end of a file's name: that picks out one
// session because Oh My Pi's session ids, UUIDs, hold no `_`.
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
// directory; "" for none. skipped is each folder of sessions it could not read and each matching
// file it could not check, with why, and err a failure to read sessions itself
// (newestOneFolderDown). No character of sessions or id is a pattern, and the match is on the end
// of a file's name: that picks out one session because Oh My Pi's session ids, UUIDs, hold no
// `_`. An empty id names no session, so it is searched for nowhere.
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
// exist holds none; a failure to read sessions itself for any other reason is err, the search's
// one failure to look: it read nothing, so it can say nothing of the session. An entry of it that
// is neither a folder nor a symlink — a file, a socket, a named pipe — is skipped by its type and
// never opened, so whether the daemon may open it never matters; a symlink to a file or to
// nothing, and an entry gone by the time it is read, are skipped too. A matching name that leads
// to no file by the time it is checked (gone, or a symlink through a file), or to one that is not
// a regular file, is skipped. A folder that cannot be read for any other reason, and a matching
// file that cannot be checked for any other — one in a folder that can be listed but not entered
// — are skipped as well, and each is listed in skipped, in the order met, as `<path>: <why>`
// (skippedFor), so the launch can name what its search passed over: the search runs as the user
// Oh My Pi runs as, so a folder or file that user cannot read holds no session Oh My Pi could
// resume. Nothing skipped by its type, gone, or not a regular file is listed: none of those is a
// failure to read. Of several written at the same time, the first matched is taken: the files the
// first of names matches, then those only the next matches, and so on, each name's files in the
// order of their folders' names and then of their own.
func newestOneFolderDown(sessions string, names ...wantedName) (newest string, skipped []string, err error) {
	folders, err := os.ReadDir(sessions)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	} else if err != nil {
		return "", nil, err
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
			skipped = append(skipped, skippedFor(dir, err))
			continue
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
			case err != nil:
				skipped = append(skipped, skippedFor(candidate, err))
				continue
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

// skippedFor is how a search's skipped list (newestOneFolderDown) names path, a folder it could
// not read or a matching file it could not check, and err, why: `<path>: <why>`, without the path
// that os.ReadDir's and os.Stat's errors (os.PathError) repeat.
func skippedFor(path string, err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Path == path {
		err = pathErr.Err
	}
	return path + ": " + err.Error()
}

// searchedIn is the sessions directories a search for a session found nothing in (sessionFileOf),
// as a refusal names them.
func searchedIn(searched []string) string {
	if len(searched) == 2 {
		return searched[0] + " or of the pane's sessions directory " + searched[1]
	}
	return strings.Join(searched, ", ")
}
