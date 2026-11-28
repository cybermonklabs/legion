package notes

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Kind is what one directory entry is, read from a GitHub tree listing
// (GitKind) or from a checkout (FileKind); IsNote takes it.
type Kind int

const (
	// KindOther is an entry none of the other kinds name: a blob of another
	// mode, a named pipe, a device. It is the zero Kind, so an entry nobody
	// classified is never a note.
	KindOther Kind = iota
	// KindFile is a regular file: a GitHub blob of mode 100644 or 100755, or
	// a checkout's regular file of any permission bits. Only a file can be a
	// note.
	KindFile
	// KindSymlink is a symbolic link: a GitHub blob of mode 120000, or a
	// checkout's symbolic link. It is never followed.
	KindSymlink
	// KindDirectory is a nested directory: a GitHub tree, or a checkout's
	// directory. Its contents are not part of the note set.
	KindDirectory
	// KindSubmodule is a GitHub commit entry, a submodule.
	KindSubmodule
)

// String names the kind as a warning or an error message reads it.
func (k Kind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindSymlink:
		return "symbolic link"
	case KindDirectory:
		return "directory"
	case KindSubmodule:
		return "submodule"
	default:
		return "other entry"
	}
}

// GitKind decides the kind of one GitHub tree entry from its object type and
// mode: a blob of mode 100644 or 100755 is a file, a blob of mode 120000 a
// symbolic link, a tree a directory, a commit a submodule, and anything else,
// a blob of any other mode included, other.
func GitKind(objectType, mode string) Kind {
	switch objectType {
	case "blob":
		switch mode {
		case "100644", "100755":
			return KindFile
		case "120000":
			return KindSymlink
		}
		return KindOther
	case "tree":
		return KindDirectory
	case "commit":
		return KindSubmodule
	}
	return KindOther
}

// FileKind decides the kind of one checkout entry from its mode, as
// fs.DirEntry.Type or fs.FileInfo.Mode gives it: a regular file of any
// permission bits is a file, then a symbolic link, a directory, and anything
// else other.
func FileKind(mode fs.FileMode) Kind {
	switch {
	case mode.IsRegular():
		return KindFile
	case mode&fs.ModeSymlink != 0:
		return KindSymlink
	case mode.IsDir():
		return KindDirectory
	}
	return KindOther
}

// IsNote decides whether one entry of the architecture directory is a note:
// a file (kind KindFile) whose name has no "/" and ends in ".md". name is the
// entry's name inside the directory; every other entry is passed over.
func IsNote(name string, kind Kind) bool {
	return kind == KindFile && !strings.Contains(name, "/") && strings.HasSuffix(name, ".md")
}

// MaxFileSize is the largest note, in bytes, that a note set may hold.
const MaxFileSize = 1 << 20

// ErrFileTooLarge is CheckSize's refusal of a note larger than MaxFileSize,
// which a caller tells from another failure with errors.Is.
var ErrFileTooLarge = errors.New("file too large")

// CheckSize decides whether a note of size bytes at path is within
// MaxFileSize: nil when it is, else ErrFileTooLarge wrapped with the path, the
// size and the limit.
func CheckSize(path string, size int64) error {
	if size > MaxFileSize {
		return fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrFileTooLarge, path, size, MaxFileSize)
	}
	return nil
}

// MaxFiles is the most notes a note set may hold, and MaxSetSize the most bytes its notes may
// hold in sum. MaxFileSize bounds each note and these bound the set, so a reader that checks them
// from a directory's listing reads at most MaxSetSize bytes of notes. Each reader checks them from
// its listing before it reads a note: the Dispatch server's GitHub reader for a CI job's result of
// a check of the spec files and for the architecture import, spec-check, architecture-check and
// the CI reporter.
//
// MaxFiles is what the server can list: it reads a directory's listing as one GitHub response of
// at most 1 MiB (githubapp's responseLimit), GitHub writes about 200 bytes for each entry beside
// its name and the repository's (13,351 bytes for the 56 entries of this repository's
// .github/workflows), and so 1,000 entries fit with a name of 255 bytes each and the longest
// repository name. MaxSetSize is what the server keeps of one commit: it keeps the checks it
// judged of a commit's spec files in a cache of 16 MiB that keeps no entry larger than itself,
// and those checks take about the bytes of the files they are written in, so a set of 16 MiB is
// read once for each commit, and not once for each result.
const (
	MaxFiles   = 1000
	MaxSetSize = 16 << 20
)

// ErrSetTooLarge is CheckSet's refusal of a set over MaxFiles or MaxSetSize, which a caller tells
// from another failure with errors.Is.
var ErrSetTooLarge = errors.New("set too large")

// CheckSet decides whether a set of files notes holding size bytes in sum, in the directory dir,
// is within MaxFiles and MaxSetSize: nil when it is, else ErrSetTooLarge wrapped with the
// directory, and the count and its limit, or, where the count is within its bound, the sum and its
// limit.
func CheckSet(dir string, files int, size int64) error {
	if files > MaxFiles {
		return fmt.Errorf("%w: %s holds %d files (limit %d)", ErrSetTooLarge, dir, files, MaxFiles)
	}
	if size > MaxSetSize {
		return fmt.Errorf("%w: %s holds %d bytes in its files (limit %d)", ErrSetTooLarge, dir, size, MaxSetSize)
	}
	return nil
}
