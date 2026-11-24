package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"golang.org/x/sys/unix"
)

// The workspace's records — its creation moment, its in-use mark and the record of the restore the
// launcher ran on it — live in its own .jj directory, on the issue's volume, which every agent of
// the issue can write. So each record is read and written through the one descriptor openJJ
// answers, never by its path: a .jj swapped for a link once it is open reaches no other directory.

// errLinkedJJ is the refusal of a workspace whose .jj is a symbolic link (openJJ).
var errLinkedJJ = errors.New("the workspace's .jj is a symbolic link")

// openJJ opens the .jj directory of the workspace at dir, refusing a .jj that is a symbolic link
// with errLinkedJJ and one that is no other directory with ENOTDIR. A link there would carry the
// records into a directory off the workspace's disk: a record read there says nothing of this
// workspace, and one written there can be lost at a container restart while the workspace keeps
// its work. The open is rolefile.OpenDirectory's, which follows no link on any kernel, gVisor's
// included, where issue pods run, and waits on no special file planted at .jj; only its refusal
// of a link is worded here. Every read and write of a record goes through the open directory
// (openat, fstatat), so nothing after this open reads .jj by its path again. held, when set, runs
// once the directory is open and before the record is read or written: a test's seam for a .jj
// swapped in that window.
func openJJ(dir string, held func()) (*os.File, error) {
	path := filepath.Join(dir, ".jj")
	fd, err := rolefile.OpenDirectory(unix.AT_FDCWD, path, path)
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%w: %s", errLinkedJJ, path)
	}
	if err != nil {
		return nil, err
	}
	jj := os.NewFile(uintptr(fd), path)
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

// createdRecordLimit bounds the record Created reads: one RFC 3339 instant and a newline. A longer
// one is refused (readRecord).
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
	body, path, ok, err := readRecord(dir, held, createdFile, "the creation record", createdRecordLimit)
	if !ok || err != nil {
		return time.Time{}, false, err
	}
	at, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(body)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("the creation record %s holds %q, not an RFC 3339 instant", path, body)
	}
	return at, true, nil
}

// readRecord is the body of the record name in the .jj directory of the workspace at dir, and the
// path it was read at, which a refusal of what it holds names. ok is false when the workspace
// holds none: no .jj is there, or nothing is at name. Each record is on the issue's volume, which
// every agent of the issue can write, so it is read through .jj's descriptor (openJJ, which runs
// held), opened without following a link or blocking on a special file, and anything but a
// regular file of at most limit bytes is refused, naming the file. what names the record in each
// error: Created's and ReadRestoreRecord's are its two readers.
func readRecord(dir string, held func(), name, what string, limit int) (body []byte, path string, ok bool, err error) {
	jj, err := openJJ(dir, held)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("read %s of workspace %s: %w", what, dir, err)
	}
	defer jj.Close()
	path = filepath.Join(jj.Name(), name)
	file, err := openIn(jj, name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, path, false, nil
	}
	if err != nil {
		return nil, path, false, fmt.Errorf("read %s %s: %w", what, path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, path, false, fmt.Errorf("read %s %s: %w", what, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, path, false, fmt.Errorf("%s %s is not a regular file (%s)", what, path, info.Mode().Type())
	}
	body, err = io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, path, false, fmt.Errorf("read %s %s: %w", what, path, err)
	}
	if len(body) > limit {
		return nil, path, false, fmt.Errorf("%s %s is over its %d bytes", what, path, limit)
	}
	return body, path, true, nil
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

// restoreFile is where the launcher records the one restore of a saved workspace copy it runs on
// the workspace (RecordRestore), beside inUseFile in the workspace's own .jj directory: jj never
// snapshots it, so the record is never committed, and it goes with the workspace, so a launcher
// restarted over the same disk reads it and a workspace created again holds none.
const restoreFile = "legion-restore"

// RestoreRecordPath is where the record of the restore of the workspace at dir is
// (restoreFile): the path a refusal that names the record as its remedy names, and a test reads.
func RestoreRecordPath(dir string) string { return filepath.Join(dir, ".jj", restoreFile) }

// restoreTemp is the name a write of the record is made under before it is renamed over
// restoreFile, so a reader finds the record before the write or the one after it, whole.
const restoreTemp = restoreFile + ".tmp"

// restoreRecordLimit bounds how much of the record ReadRestoreRecord reads, and what
// RecordRestore writes: the report an ended record holds goes to the daemon as one frame, so a
// record over the frame limit holds no report the launcher could send.
const restoreRecordLimit = shimwire.MaxFrameBytes

// RestoreRecord is the launcher's record of a restore of a saved workspace copy into the
// workspace: Report, the report the daemon is owed of it, whose ID is the slice sequence the copy
// arrived under and whose Release is the release whose save it is, and whether the restore Ended.
// One not Ended is a restore begun. The launcher writes it before the in-use record and before the
// restore, only where InUse read false or over another begun record, and replaces it with the
// ended one, the report's OK and Error its outcome, before any role starts. So a launcher that
// reads a begun record it did not write knows that restore was cut off and that no role has run on
// the workspace since the first such record was written (internal/launcher's bundleRestore). An
// ended record is the report the daemon is owed until a connection carries it, when the record is
// removed (RemoveRestoreRecord). A record that cannot be read proves neither, so the launcher
// reads the in-use record beside it.
type RestoreRecord struct {
	Report shimwire.LauncherRestored `json:"report"`
	Ended  bool                      `json:"ended"`
}

// RecordRestore records record on the disk of the workspace at dir, replacing the record there
// before, until RemoveRestoreRecord takes it off. It opens .jj (openJJ), refusing a .jj that is a
// symbolic link, writes the record as JSON to a file of its own in that directory, created with
// O_CREAT|O_EXCL|O_NOFOLLOW after whatever was at its name is unlinked, syncs the file and renames
// it over the record, so a reader finds the old record or the new one whole, never a part, and
// nothing planted at either name is written through. Then it syncs the directory, so the record is
// on the disk before RecordRestore returns. A directory at the record's name is refused by the
// rename.
func RecordRestore(dir string, record RestoreRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("write the restore's record: %w", err)
	}
	body = append(body, '\n')
	if len(body) > restoreRecordLimit {
		return fmt.Errorf("write the restore's record: its %d bytes are over the record's %d", len(body), restoreRecordLimit)
	}
	jj, err := openJJ(dir, nil)
	if err != nil {
		return fmt.Errorf("write the restore's record: %w", err)
	}
	defer jj.Close()
	if err := unlinkIn(jj, restoreTemp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("write the restore's record: %w", err)
	}
	file, err := openIn(jj, restoreTemp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("write the restore's record: %w", err)
	}
	_, err = file.Write(body)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		if renameErr := unix.Renameat(int(jj.Fd()), restoreTemp, int(jj.Fd()), restoreFile); renameErr != nil {
			err = &fs.PathError{Op: "renameat", Path: filepath.Join(jj.Name(), restoreFile), Err: renameErr}
		}
	}
	if err != nil {
		_ = unlinkIn(jj, restoreTemp)
		return fmt.Errorf("write the restore's record: %w", err)
	}
	if err := jj.Sync(); err != nil {
		return fmt.Errorf("sync the restore record's directory: %w", err)
	}
	return nil
}

// ReadRestoreRecord is the record RecordRestore wrote on the disk of the workspace at dir. ok is
// false when it holds none: no workspace is there, no restore ran on it, or the report of the one
// that ran has reached the daemon. The record is read as readRecord reads one, up to
// restoreRecordLimit, and anything but one record that names its slice sequence is refused,
// naming the file. The launcher does not count a refused record as none: it reads the in-use
// record beside it (internal/launcher's bundleRestore.read).
func ReadRestoreRecord(dir string) (record RestoreRecord, ok bool, err error) {
	body, path, ok, err := readRecord(dir, nil, restoreFile, "the restore's record", restoreRecordLimit)
	if !ok || err != nil {
		return RestoreRecord{}, false, err
	}
	if err := json.Unmarshal(body, &record); err != nil {
		return RestoreRecord{}, false, fmt.Errorf("the restore's record %s holds no record: %w", path, err)
	}
	if record.Report.ID == "" {
		return RestoreRecord{}, false, fmt.Errorf("the restore's record %s names no slice sequence", path)
	}
	return record, true, nil
}

// RemoveRestoreRecord takes the record RecordRestore wrote off the disk of the workspace at dir,
// and syncs .jj so the removal is on the disk before it returns. It unlinks the record through
// .jj's descriptor (openJJ), so a link at the record's name is removed and never followed, and a
// directory there is refused. A workspace or a record that is not there is nothing to remove.
func RemoveRestoreRecord(dir string) error {
	jj, err := openJJ(dir, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove the restore's record: %w", err)
	}
	defer jj.Close()
	if err := unlinkIn(jj, restoreFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove the restore's record: %w", err)
	}
	if err := jj.Sync(); err != nil {
		return fmt.Errorf("sync the restore record's directory: %w", err)
	}
	return nil
}

// unlinkIn unlinks name in the open directory jj, the entry itself and never what a link there
// names. A directory at name is refused (EISDIR). The file's name is only used for diagnostics.
func unlinkIn(jj *os.File, name string) error {
	if err := unix.Unlinkat(int(jj.Fd()), name, 0); err != nil {
		return &fs.PathError{Op: "unlinkat", Path: filepath.Join(jj.Name(), name), Err: err}
	}
	return nil
}
