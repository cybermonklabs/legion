package notes

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Set is what ReadSet read of one directory: each note by its name inside the directory, with its
// content, and each entry that is not a note, in name order.
type Set struct {
	Files    map[string][]byte
	Rejected []Rejected
}

// Rejected is an entry of the directory that FileKind and IsNote reject: its path, the
// directory's path joined with its name, and its kind. A command warns of it, since the importer
// passes over such an entry in silence and its author expects it to count.
type Rejected struct {
	Path string
	Kind Kind
}

// ErrNoDirectory is ReadSet's answer for a directory that does not exist.
var ErrNoDirectory = errors.New("does not exist")

// SizeRefusal is the bound's refusal of a set: one CheckSize error for each note above
// MaxFileSize, each naming the note's path. A command reads it as a refused set, as it reads a
// Parse problem; Held is the number of notes the set holds, and no note of it was parsed.
type SizeRefusal struct {
	Held     int
	Problems []error
}

func (r *SizeRefusal) Error() string   { return errors.Join(r.Problems...).Error() }
func (r *SizeRefusal) Unwrap() []error { return r.Problems }

// ReadSet reads the note set of the directory fsys holds, whose path is dir: each entry FileKind
// and IsNote accept, by name, with its content, and each entry they reject, in name order. It
// checks the size of every note with CheckSize before it reads any, as the server's GitHub reader
// refuses a listing before any blob read, and the length of each note it read again, as that
// reader checks each blob; either refusal is a *SizeRefusal and answers no files. A directory
// that does not exist is ErrNoDirectory, and any other failure to read it or a note is
// cannotRead's "cannot be read" error. The Set's Rejected holds every entry rejected before the
// failure, so a caller can warn of them whatever the error.
func ReadSet(fsys fs.FS, dir string) (Set, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return Set{}, ErrNoDirectory
	}
	if err != nil {
		return Set{}, cannotRead(dir, err)
	}
	var set Set
	var names []string
	var refusals []error
	for _, entry := range entries {
		kind := FileKind(entry.Type())
		if !IsNote(entry.Name(), kind) {
			set.Rejected = append(set.Rejected, Rejected{Path: filepath.Join(dir, entry.Name()), Kind: kind})
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return set, cannotRead(dir, err)
		}
		if err := CheckSize(filepath.Join(dir, entry.Name()), info.Size()); err != nil {
			refusals = append(refusals, err)
		}
		names = append(names, entry.Name())
	}
	if len(refusals) > 0 {
		return set, &SizeRefusal{Held: len(names), Problems: refusals}
	}
	files := make(map[string][]byte, len(names))
	for _, name := range names {
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return set, cannotRead(dir, err)
		}
		if err := CheckSize(filepath.Join(dir, name), int64(len(content))); err != nil {
			refusals = append(refusals, err)
		}
		files[name] = content
	}
	if len(refusals) > 0 {
		return set, &SizeRefusal{Held: len(names), Problems: refusals}
	}
	set.Files = files
	return set, nil
}

// cannotRead is ReadSet's answer for a directory or a note it could not read: err wrapped as
// "cannot be read". os.DirFS names a path in its error relative to the directory, so cannotRead
// first joins it to dir, as every path ReadSet names is.
func cannotRead(dir string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && !filepath.IsAbs(pathErr.Path) {
		pathErr.Path = filepath.Join(dir, filepath.FromSlash(pathErr.Path))
	}
	return fmt.Errorf("cannot be read: %w", err)
}
