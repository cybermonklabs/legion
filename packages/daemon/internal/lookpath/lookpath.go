// Package lookpath resolves an executable on a PATH string this process did not necessarily
// start with: a pod's or a daemon child's, not this binary's own. It is exec.LookPath's search
// alone, without exec.LookPath's refusal to run what it found (the daemon never execs the result
// itself in the same step that resolved it) and without its own-process PATH default, since every
// caller here already has the PATH it means to search. It also lists every executable of a name
// on a PATH outside one directory, a wrapper's own (Others), and reads what kind of program a
// file is (Kind).
package lookpath

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// On resolves name on searchPath, filepath.SplitList's elements in order: the first regular file
// with an execute bit wins. An empty element is skipped, as it is for exec.LookPath, and so is a
// relative one, as exec.LookPath refuses the result it would give. The config and tmux resolvers
// this package replaced accepted a relative element; that is the one change from their behaviour.
func On(searchPath, name string) (string, bool) {
	for _, dir := range filepath.SplitList(searchPath) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}

// Found is one executable that Others found: the index of its entry in the PATH string, the path to
// it (that entry, a slash and the name), and what os.Stat says of it.
type Found struct {
	Index int
	Path  string
	Info  os.FileInfo
}

// Others lists name on each entry of searchPath where it is a regular file with an execute bit, in
// PATH order, leaving out every entry that is the directory own (os.SameFile, so another spelling
// of own is left out too). It also answers the index of own's first entry, or -1 when own is on no
// entry. An empty entry is skipped. A relative entry is kept, as a shell keeps it, unlike On: the
// git wrappers that call this run the git they find with the PATH they were given. A wrapper's own
// directory is own, so the list is every git that wrapper could hand a command to.
func Others(searchPath, name, own string) (found []Found, ownAt int) {
	ownInfo, ownErr := os.Stat(own)
	ownAt = -1
	for i, entry := range filepath.SplitList(searchPath) {
		if entry == "" {
			continue
		}
		if ownErr == nil {
			if info, err := os.Stat(entry); err == nil && os.SameFile(info, ownInfo) {
				if ownAt < 0 {
					ownAt = i
				}
				continue
			}
		}
		candidate := entry + string(filepath.Separator) + name
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			found = append(found, Found{Index: i, Path: candidate, Info: info})
		}
	}
	return found, ownAt
}

// Kind reads the first bytes of the file at path. script is whether they are `#!`, the mark of a
// script that the kernel runs under the interpreter that line names. compiled is whether they are
// the magic number of a compiled program that the kernel runs itself: ELF, or Mach-O on macOS,
// thin or fat. A file that is neither is one the kernel refuses to run (ENOEXEC), and a shell or
// execvp then runs it with /bin/sh. A file shorter than a mark is not that kind.
func Kind(path string) (script, compiled bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return false, false, err
	}
	defer file.Close()
	head := make([]byte, 4)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, false, err
	}
	head = head[:n]
	switch string(head) {
	case "\x7fELF", "\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe", "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe":
		compiled = true
	}
	return len(head) >= 2 && string(head[:2]) == "#!", compiled, nil
}
