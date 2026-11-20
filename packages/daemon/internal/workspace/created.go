package workspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// createdFile is where provisioning records the moment it created a workspace: in the workspace's
// own .jj directory, which jj never snapshots, so the record is never committed, and which goes with
// the workspace, so a workspace created again records its own creation.
const createdFile = "legion-created"

// createdRecordLimit bounds how much of the record Created reads: one RFC 3339 instant and a newline.
const createdRecordLimit = 128

// RecordCreated records at, in UTC, as the moment the workspace at dir was created.
func RecordCreated(dir string, at time.Time) error {
	path := filepath.Join(dir, ".jj", createdFile)
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		return fmt.Errorf("record the creation of workspace %s: %w", dir, err)
	}
	return nil
}

// Created is the moment RecordCreated recorded for the workspace at dir. ok is false when it holds
// none: no workspace is there, or provisioning created it before it kept the record. The record is
// on the issue's volume, which every agent of the issue can write, so it is opened without following a
// link or blocking on a special file and read only up to createdRecordLimit, and anything but one
// RFC 3339 instant is refused, naming the file.
func Created(dir string) (at time.Time, ok bool, err error) {
	path := filepath.Join(dir, ".jj", createdFile)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, false, fmt.Errorf("the creation record %s is not a regular file (%s)", path, info.Mode().Type())
	}
	body, err := io.ReadAll(io.LimitReader(file, createdRecordLimit))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", path, err)
	}
	at, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(body)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("the creation record %s holds %q, not an RFC 3339 instant", path, body)
	}
	return at, true, nil
}

// inUseFile is where MarkInUse records that the workspace is in use, beside createdFile in the
// workspace's own .jj directory: jj never snapshots it, so the record is never committed, and it
// goes with the workspace, so a workspace created again holds no record.
const inUseFile = "legion-in-use"

// errLinkedJJ is the refusal of a workspace whose .jj is a symbolic link (jjDirectory).
var errLinkedJJ = errors.New("the workspace's .jj is a symbolic link")

// jjDirectory is the .jj directory of the workspace at dir, refused with errLinkedJJ when it is a
// symbolic link. .jj is on the issue's volume, which every agent of the issue can write, and a link
// there would carry the in-use record into a directory off the workspace's disk: one read there
// says nothing of this workspace, and one written there can be lost at a container restart while
// the workspace keeps its work. It is read with Lstat, which follows no link. A .jj that is not
// there, or no directory, is left to the read or the create that follows, which answers it.
func jjDirectory(dir string) (string, error) {
	jj := filepath.Join(dir, ".jj")
	if info, err := os.Lstat(jj); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s", errLinkedJJ, jj)
	}
	return jj, nil
}

// MarkInUse records that the workspace at dir is in use: a restore or a role has started on it.
// It refuses a .jj that is a symbolic link (jjDirectory), and creates the record with
// O_CREATE|O_EXCL|O_NOFOLLOW. A create that fails with EEXIST means something is at its path
// already, which InUse reads as in use, and MarkInUse returns nil for it. Then it syncs the .jj
// directory, so the record's entry is on the disk before MarkInUse returns. It returns every other
// error it meets: an ENOTDIR from a .jj that is no directory, an EACCES, an ENOSPC. The caller
// must start no role on a workspace it could not mark.
func MarkInUse(dir string) error {
	jj, err := jjDirectory(dir)
	if err != nil {
		return fmt.Errorf("create the in-use record: %w", err)
	}
	path := filepath.Join(jj, inUseFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	switch {
	case errors.Is(err, fs.ErrExist):
	case err != nil:
		return fmt.Errorf("create the in-use record: %w", err)
	default:
		if err := file.Close(); err != nil {
			return fmt.Errorf("create the in-use record: %w", err)
		}
	}
	parent, err := os.Open(jj)
	if err != nil {
		return fmt.Errorf("sync the in-use record's directory: %w", err)
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("sync the in-use record's directory: %w", err)
	}
	return nil
}

// InUse says whether MarkInUse recorded the workspace at dir in use. The record is on the issue's
// volume, which every agent of the issue can write, so a .jj that is a symbolic link is refused
// (jjDirectory), and the record is read with Lstat, which follows no link and opens nothing, so no
// special file can block it. Anything at the path counts as in use.
func InUse(dir string) (bool, error) {
	jj, err := jjDirectory(dir)
	if err != nil {
		return false, fmt.Errorf("read the in-use record: %w", err)
	}
	path := filepath.Join(jj, inUseFile)
	_, err = os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("read the in-use record: %w", err)
	}
}
