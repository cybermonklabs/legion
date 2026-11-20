package workspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The workspace's records — its creation moment and its in-use mark — live in its own .jj
// directory, on the issue's volume, which every agent of the issue can write. So each record is
// read and written through the one descriptor openJJ answers, never by its path: a .jj swapped for
// a link once it is open reaches no other directory.

// errLinkedJJ is the refusal of a workspace whose .jj is a symbolic link (openJJ).
var errLinkedJJ = errors.New("the workspace's .jj is a symbolic link")

// openJJ opens the .jj directory of the workspace at dir with O_DIRECTORY|O_NOFOLLOW, so a .jj
// that is a symbolic link is refused with errLinkedJJ and one that is no other directory with
// ENOTDIR. A link there would carry the records into a directory off the workspace's disk: a
// record read there says nothing of this workspace, and one written there can be lost at a
// container restart while the workspace keeps its work. Every read and write of a record goes
// through the open directory (openat, fstatat), so nothing after this open reads .jj by its path
// again. held, when set, runs once the directory is open and before the record is read or
// written: a test's seam for a .jj swapped in that window.
func openJJ(dir string, held func()) (*os.File, error) {
	path := filepath.Join(dir, ".jj")
	jj, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		// The open refused it: Linux answers a link opened with O_DIRECTORY|O_NOFOLLOW ENOTDIR, as
		// it does a file, and other kernels ELOOP. The Lstat only says which the refusal was.
		if info, statErr := os.Lstat(path); errors.Is(err, unix.ELOOP) || (statErr == nil && info.Mode()&fs.ModeSymlink != 0) {
			return nil, fmt.Errorf("%w: %s: %w", errLinkedJJ, path, err)
		}
	}
	if err != nil {
		return nil, err
	}
	if held != nil {
		held()
	}
	return jj, nil
}

// openIn opens name in the open directory jj with flags, adding O_NOFOLLOW and O_CLOEXEC, so a
// record a role planted as a link is not followed. The file's name is only used for diagnostics.
func openIn(jj *os.File, name string, flags int, perm uint32) (*os.File, error) {
	path := filepath.Join(jj.Name(), name)
	fd, err := unix.Openat(int(jj.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		return nil, &fs.PathError{Op: "openat", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// createdFile is where provisioning records the moment it created a workspace: in the workspace's
// own .jj directory, which jj never snapshots, so the record is never committed, and which goes with
// the workspace, so a workspace created again records its own creation.
const createdFile = "legion-created"

// createdRecordLimit bounds how much of the record Created reads: one RFC 3339 instant and a newline.
const createdRecordLimit = 128

// RecordCreated records at, in UTC, as the moment the workspace at dir was created. It writes the
// record through .jj's descriptor (openJJ), and refuses a .jj that is a link or a record that is.
func RecordCreated(dir string, at time.Time) error {
	jj, err := openJJ(dir, nil)
	if err != nil {
		return fmt.Errorf("record the creation of workspace %s: %w", dir, err)
	}
	defer jj.Close()
	file, err := openIn(jj, createdFile, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("record the creation of workspace %s: %w", dir, err)
	}
	_, err = file.WriteString(at.UTC().Format(time.RFC3339Nano) + "\n")
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("record the creation of workspace %s: %w", dir, err)
	}
	return nil
}

// Created is the moment RecordCreated recorded for the workspace at dir. ok is false when it holds
// none: no workspace is there, or provisioning created it before it kept the record. The record is
// on the issue's volume, which every agent of the issue can write, so it is read through .jj's
// descriptor (openJJ), opened without following a link or blocking on a special file and read only
// up to createdRecordLimit, and anything but one RFC 3339 instant is refused, naming the file.
func Created(dir string) (at time.Time, ok bool, err error) { return created(dir, nil) }

// created is Created, with held run as openJJ runs it.
func created(dir string, held func()) (at time.Time, ok bool, err error) {
	jj, err := openJJ(dir, held)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record of workspace %s: %w", dir, err)
	}
	defer jj.Close()
	record := filepath.Join(jj.Name(), createdFile)
	file, err := openIn(jj, createdFile, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", record, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", record, err)
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, false, fmt.Errorf("the creation record %s is not a regular file (%s)", record, info.Mode().Type())
	}
	body, err := io.ReadAll(io.LimitReader(file, createdRecordLimit))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", record, err)
	}
	at, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(body)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("the creation record %s holds %q, not an RFC 3339 instant", record, body)
	}
	return at, true, nil
}

// inUseFile is where MarkInUse records that the workspace is in use, beside createdFile in the
// workspace's own .jj directory: jj never snapshots it, so the record is never committed, and it
// goes with the workspace, so a workspace created again holds no record.
const inUseFile = "legion-in-use"

// MarkInUse records that the workspace at dir is in use: a restore or a role has started on it.
// It opens .jj (openJJ), refusing a .jj that is a symbolic link, and creates the record in that
// directory with O_CREAT|O_EXCL|O_NOFOLLOW. A create that fails with EEXIST means something is at
// its path already, which InUse reads as in use, and MarkInUse returns nil for it. Then it syncs
// the same directory, so the record's entry is on the disk before MarkInUse returns. It returns
// every other error it meets: an ENOTDIR from a .jj that is no directory, an EACCES, an ENOSPC.
// The caller must start no role on a workspace it could not mark.
func MarkInUse(dir string) error { return markInUse(dir, nil) }

// markInUse is MarkInUse, with held run as openJJ runs it.
func markInUse(dir string, held func()) error {
	jj, err := openJJ(dir, held)
	if err != nil {
		return fmt.Errorf("create the in-use record: %w", err)
	}
	defer jj.Close()
	file, err := openIn(jj, inUseFile, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o644)
	switch {
	case errors.Is(err, fs.ErrExist):
	case err != nil:
		return fmt.Errorf("create the in-use record: %w", err)
	default:
		if err := file.Close(); err != nil {
			return fmt.Errorf("create the in-use record: %w", err)
		}
	}
	if err := jj.Sync(); err != nil {
		return fmt.Errorf("sync the in-use record's directory: %w", err)
	}
	return nil
}

// InUse says whether MarkInUse recorded the workspace at dir in use. The record is on the issue's
// volume, which every agent of the issue can write, so .jj is opened refusing a symbolic link
// (openJJ), and the record is read in that directory with fstatat and AT_SYMLINK_NOFOLLOW, which
// follows no link and opens nothing, so no special file can block it. Anything at the path counts
// as in use. A workspace with no .jj holds no record.
func InUse(dir string) (bool, error) { return inUse(dir, nil) }

// inUse is InUse, with held run as openJJ runs it.
func inUse(dir string, held func()) (bool, error) {
	jj, err := openJJ(dir, held)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the in-use record: %w", err)
	}
	defer jj.Close()
	var stat unix.Stat_t
	err = unix.Fstatat(int(jj.Fd()), inUseFile, &stat, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("read the in-use record: %w", &fs.PathError{Op: "fstatat", Path: filepath.Join(jj.Name(), inUseFile), Err: err})
	}
}
