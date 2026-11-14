package memoryguard

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Process is one process as the guard's scan read it: its parent and start time from
// `/proc/<pid>/stat`, its executable from the `exe` link, its arguments from `cmdline` and its PID
// namespace from the `ns/pid` link. A classifier knows a process by these alone: its place in the
// tree first, then its executable and its arguments, so a program that copies a name is not taken
// for the process it copies.
type Process struct {
	PID  int
	PPID int
	// StartTime is the process's start in clock ticks since boot, the field the kill reads again
	// to know the pid still names this process and not one that took the pid after it.
	StartTime uint64
	// Name is the command name between the parentheses of stat, which can hold spaces and `)`.
	Name string
	// Exe is the target of the `exe` link, empty when the scan could not read it.
	Exe string
	// Args are the NUL-separated arguments of `cmdline`, nil for a process that shows none: of its
	// first rawCommandBytes+1 bytes alone, which is all the scan reads of it (argsCut), enough for
	// a classifier, which reads a process's first arguments.
	Args []string
	// PIDNamespace is the target of the `ns/pid` link (`pid:[4026531836]`), empty when the scan
	// could not read it.
	PIDNamespace string
	// state is stat's state field, the first after the command name: `Z` for a zombie and `X`
	// for a dead process, both of which the scan leaves out unless other threads of the process
	// still run (readProcess).
	state byte
	// argsCut is whether `cmdline` ran past rawCommandBytes, which the scan tells by reading one
	// byte more and finding that byte there: then Args are what it read, the last of them perhaps
	// part of an argument, and the command line masks no more than rawCommandBytes of them
	// (commandLine).
	argsCut bool
}

// Table is every process one scan read, keyed by pid. A process whose stat the scan read is in it
// even when its `exe` or `ns/pid` could not be read, so the tree a classifier walks has no holes;
// such a process is never ended (Guard's unread), whatever the classifier says of it. A zombie or
// a dead process with no thread left is not in it at all (readProcess): it has exited, and its
// children have passed to a reaper.
type Table struct {
	proc     string
	byPID    map[int]*Process
	children map[int][]*Process
	// unread is each pid whose stat, exe or ns/pid the scan could not read, with why. A pid
	// whose stat could not be read is here and not in byPID.
	unread map[int]error
}

// Get is the process the scan read under pid.
func (t *Table) Get(pid int) (*Process, bool) {
	p, ok := t.byPID[pid]
	return p, ok
}

// Children are the processes whose parent is pid, oldest first by start time, so "the oldest
// child that is a shim" is the first that matches.
func (t *Table) Children(pid int) []*Process { return t.children[pid] }

// Processes are every process of the table, by pid.
func (t *Table) Processes() []*Process {
	out := make([]*Process, 0, len(t.byPID))
	for _, p := range t.byPID {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b *Process) int { return a.PID - b.PID })
	return out
}

// Cwd is the working directory of pid, read when asked rather than by the scan: one rule of one
// classifier needs it (the test database's), and the scan reads every process of the pod.
func (t *Table) Cwd(pid int) (string, error) {
	return os.Readlink(filepath.Join(t.proc, strconv.Itoa(pid), "cwd"))
}

// scan reads every process under proc. A process that ends while the scan reads it is left out:
// it holds nothing any longer, and a missing file is how the kernel says so. A zombie and a dead
// process are left out the same way (errExited), and so is a process that a read after its stat's
// finds ended (endedSince). Any other failure to read its stat, exe or ns/pid counts it in unread.
func scan(proc string) (*Table, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	t := &Table{proc: proc, byPID: map[int]*Process{}, children: map[int][]*Process{}, unread: map[int]error{}}
	cmdline := make([]byte, rawCommandBytes+1)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := readProcess(proc, pid, cmdline)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errExited) {
				continue
			}
			t.unread[pid] = err
		}
		if p != nil {
			t.byPID[pid] = p
		}
	}
	for _, p := range t.byPID {
		t.children[p.PPID] = append(t.children[p.PPID], p)
	}
	for _, kids := range t.children {
		slices.SortFunc(kids, func(a, b *Process) int {
			return cmp.Or(cmp.Compare(a.StartTime, b.StartTime), cmp.Compare(a.PID, b.PID))
		})
	}
	return t, nil
}

// errExited is a process whose stat reads it exited (exited), at the scan's first read of it or
// at a read after one of its other files failed (endedSince): it holds no memory.
var errExited = errors.New("the process has exited")

// readProcess reads one process's stat, exe, ns/pid and cmdline, the last into the start of buf
// and no further (argsCut). A process whose stat reads it exited (exited) answers errExited, and
// nothing more of it is read: what its other files answer differs by kernel, and a zombie's exe
// link answers ENOENT on Linux and EACCES under gVisor, measured in a gVisor pod on 2026-11-14,
// where the scan counted unread a pool worker the guard had just ended and its parent had not
// reaped. Otherwise it answers the process with an error when stat was read and exe or ns/pid was
// not (unreadOrEnded), so the caller keeps the process in the tree and counts it unread, unless
// the process has ended since its stat was read. The arguments are best read: a process that
// shows none is still a process.
func readProcess(proc string, pid int, buf []byte) (*Process, error) {
	dir := filepath.Join(proc, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return nil, fmt.Errorf("read the stat: %w", err)
	}
	p, err := parseStat(stat)
	if err != nil {
		return nil, err
	}
	if exited(dir, p.state) {
		return nil, errExited
	}
	p.PID = pid
	if cmdline, err := readHead(filepath.Join(dir, "cmdline"), buf); err == nil {
		p.Args, p.argsCut = splitCmdline(cmdline), len(cmdline) == len(buf)
	}
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		return unreadOrEnded(dir, p, "exe", err)
	}
	p.Exe = exe
	ns, err := os.Readlink(filepath.Join(dir, "ns", "pid"))
	if err != nil {
		return unreadOrEnded(dir, p, "ns/pid", err)
	}
	p.PIDNamespace = ns
	return p, nil
}

// unreadOrEnded is what readProcess answers for p, at dir, whose file what could not be read
// after its stat was, with err: fs.ErrNotExist, or errExited where the process has ended since
// its stat was read (endedSince), either of which leaves it out of the scan, and otherwise p with
// the error that counts it unread.
func unreadOrEnded(dir string, p *Process, what string, err error) (*Process, error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, err
	case endedSince(dir, p.StartTime):
		return nil, errExited
	}
	return p, fmt.Errorf("read the %s: %w", what, err)
}

// endedSince is whether the process at dir, whose stat read the start time start, has ended
// since: its stat is gone, reads another start time (a process that took its pid), or reads it
// exited (exited). A read of a process's exe, ns/pid or smaps that fails with anything but
// fs.ErrNotExist asks it before the process counts unread, since a process can exit between the
// read of its stat and that later read, and under gVisor a zombie answers that read with EACCES
// (readProcess), which would count unread a process that has simply ended. A stat that cannot be
// read for another reason, or does not parse, shows nothing ended, and the process counts unread.
func endedSince(dir string, start uint64) bool {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	p, err := parseStat(stat)
	if err != nil {
		return false
	}
	return p.StartTime != start || exited(dir, p.state)
}

// exited is whether the process at dir, whose stat reads state, has exited: state `Z` (a zombie,
// which its parent has not reaped yet) or `X` (dead), with no thread left (threadsLeft).
func exited(dir string, state byte) bool {
	return (state == 'Z' || state == 'X') && !threadsLeft(dir)
}

// threadsLeft is whether the process at dir runs more than one thread, by the `Threads:` line of
// its status. Its stat's state is its main thread's: a process whose main thread alone has exited
// (pthread_exit) reads `Z` while its other threads run on and hold all of its memory, so a `Z`
// with threads left is a live process and the scan reads it as one. A status that cannot be read,
// or has no `Threads:` line, is taken for a zombie's, which holds no memory. meminfoBytes reads
// the line as it reads a meminfo line, and a number with no unit as the number.
func threadsLeft(dir string) bool {
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return false
	}
	threads, ok := meminfoBytes(status, "Threads")
	return ok && threads > 1
}

// statStartField is the index of starttime (stat's field 22) among the fields after the command
// name's closing parenthesis, which begin at field 3 (state).
const statStartField = 22 - 3

// parseStat reads a `/proc/<pid>/stat` line: the command name between the first `(` and the last
// `)`, and the state, the parent and the start time among the fields after that last `)`. The name
// is the one field a process chooses, and it can hold spaces and parentheses of its own (`(omp
// daemon brok)`), so nothing after the first `(` is split until the last `)`.
func parseStat(stat []byte) (*Process, error) {
	open, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return nil, fmt.Errorf("a stat with no command name: %.80q", stat)
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= statStartField {
		return nil, fmt.Errorf("a stat of %d fields after its command name: %.80q", len(fields), stat)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("a stat whose parent is %q", fields[1])
	}
	start, err := strconv.ParseUint(fields[statStartField], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("a stat whose start time is %q", fields[statStartField])
	}
	return &Process{PPID: ppid, StartTime: start, Name: string(stat[open+1 : end]), state: fields[0][0]}, nil
}

// readStartTime is pid's start time as its stat reads now.
func readStartTime(proc string, pid int) (uint64, error) {
	stat, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	p, err := parseStat(stat)
	if err != nil {
		return 0, err
	}
	return p.StartTime, nil
}

// splitCmdline is cmdline's NUL-separated arguments, the last one's terminator dropped.
func splitCmdline(cmdline []byte) []string {
	cmdline = bytes.TrimSuffix(cmdline, []byte{0})
	if len(cmdline) == 0 {
		return nil
	}
	return strings.Split(string(cmdline), "\x00")
}

// memfdPrefix is how procfs names a memfd, in a smaps mapping line and in the link of a
// descriptor that holds one: `/memfd:<name> (deleted)`, on Linux and under gVisor alike, where a
// pod measured on 2026-11-14 read that link for a memfd its descriptor held and that path in the
// smaps line of one it mapped.
const memfdPrefix = "/memfd:"

// memfdID is a memfd by its device and inode, the numbers by which a smaps mapping line and the
// stat of a descriptor's link name the same file. The same pod read a memfd's device as 5 through
// its descriptor's stat, and a mapped memfd's as `00:05` (major and minor, in hex) in its smaps
// line, each with the memfd's own inode.
type memfdID struct{ dev, ino uint64 }

// smapsSize is the part of a process's size its smaps shows: the sum of its `Anonymous:` lines,
// plus the `Rss:` of each of its shared mappings of memory-backed files. A shared anonymous map
// (`/dev/zero (deleted)`) and a memfd (`/memfd:`) were measured under gVisor to raise the pod's
// AnonPages while their pages read 0 `Anonymous`, and a POSIX shared memory object (`/dev/shm/`,
// tmpfs) and a System V segment (`/SYSV`) are memory of the same kind, so without them a grower
// that uses one would be invisible. A mapping of a file on disk adds nothing: its pages are the
// file's, which AnonPages does not count, and a mapped database is the case the guard must not
// end. A private mapping adds nothing beyond its `Anonymous` either, since the pages it wrote are
// counted there. The rest of a process's size is each memfd it holds open by a descriptor and has
// not mapped (heldMemfdBytes).
//
// Each memfd the process maps shared is added to mapped, when mapped is not nil: its pages count
// in this size through the mapping's `Rss`, so no descriptor counts it again. A memfd mapped
// private is not added, since its `Rss` counts in no size.
func smapsSize(data []byte, mapped map[memfdID]bool) uint64 {
	var size uint64
	counted := false
	for len(data) > 0 {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		key, rest, _ := bytes.Cut(line, []byte{' '})
		if len(key) == 0 {
			continue
		}
		if key[len(key)-1] != ':' {
			var memfd memfdID
			var isMemfd bool
			counted, memfd, isMemfd = parseMapping(line)
			if isMemfd && mapped != nil {
				mapped[memfd] = true
			}
			continue
		}
		switch string(key) {
		case "Anonymous:":
			if n, ok := parseKB(rest); ok {
				size += n
			}
		case "Rss:":
			if counted {
				if n, ok := parseKB(rest); ok {
					size += n
				}
			}
		}
	}
	return size
}

// parseMapping reads a smaps mapping line (`start-end perms offset dev inode path`): whether it
// is a shared mapping of a memory-backed file, whose resident pages count in a process's size,
// and, where that file is a memfd, the memfd by the device and inode the line carries.
func parseMapping(line []byte) (counted bool, memfd memfdID, isMemfd bool) {
	var fields [5][]byte
	rest := line
	for i := range fields {
		rest = bytes.TrimLeft(rest, " ")
		fields[i], rest, _ = bytes.Cut(rest, []byte{' '})
	}
	if perms := fields[1]; len(perms) < 4 || perms[3] != 's' {
		return false, memfdID{}, false
	}
	path := string(bytes.TrimSpace(rest))
	if !strings.HasPrefix(path, memfdPrefix) {
		return path == "/dev/zero (deleted)" || strings.HasPrefix(path, "/dev/shm/") || strings.HasPrefix(path, "/SYSV"),
			memfdID{}, false
	}
	memfd, isMemfd = parseMemfdID(fields[3], fields[4])
	return true, memfd, isMemfd
}

// parseMemfdID reads a mapping line's device, `major:minor` in hex, and its inode, in decimal, as
// the device number and inode a stat gives.
func parseMemfdID(dev, inode []byte) (memfdID, bool) {
	majorText, minorText, found := bytes.Cut(dev, []byte{':'})
	major, majorErr := strconv.ParseUint(string(majorText), 16, 32)
	minor, minorErr := strconv.ParseUint(string(minorText), 16, 32)
	ino, inodeErr := strconv.ParseUint(string(inode), 10, 64)
	if !found || majorErr != nil || minorErr != nil || inodeErr != nil {
		return memfdID{}, false
	}
	return memfdID{dev: unix.Mkdev(uint32(major), uint32(minor)), ino: ino}, true
}

// heldMemfdBytes is what the memfds the process at dir holds open by a descriptor add to its
// size: for each such memfd that counted does not hold, which it then adds to counted, the size
// stat finds through the descriptor or shmem, the pod's Shmem reading in bytes, whichever is less.
// So a memfd that a process of the same pass maps (smapsSize), that a process before this one
// holds, or that two descriptors of this one hold, counts once. A descriptor holds a memfd where
// its link starts with link (memfdPrefix on a real /proc). Ending the process frees only the
// memfd's resident pages, which no line of its smaps shows, and Shmem bounds them, since it counts
// the pages of every memfd. Under gVisor neither the file's size nor its block count shows those
// pages: a memfd made large with ftruncate and never written has its whole size and no pages.
// An fd directory or a link that cannot be read adds nothing and is no error: a process opens and
// closes descriptors as it runs, and one may be gone under the read.
func heldMemfdBytes(dir, link string, counted map[memfdID]bool, shmem uint64) uint64 {
	fds := filepath.Join(dir, "fd")
	f, err := os.Open(fds)
	if err != nil {
		return 0
	}
	names, _ := f.Readdirnames(-1)
	_ = f.Close()
	var size uint64
	for _, name := range names {
		path := filepath.Join(fds, name)
		if target, err := os.Readlink(path); err != nil || !strings.HasPrefix(target, link) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		memfd := memfdID{dev: uint64(st.Dev), ino: uint64(st.Ino)}
		if counted[memfd] {
			continue
		}
		counted[memfd] = true
		size += min(uint64(info.Size()), shmem)
	}
	return size
}

// meminfoBytes is the value of key's line in a /proc/meminfo, in bytes, read in place.
func meminfoBytes(data []byte, key string) (uint64, bool) {
	for len(data) > 0 {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		if len(line) > len(key) && line[len(key)] == ':' && string(line[:len(key)]) == key {
			return parseKB(line[len(key)+1:])
		}
	}
	return 0, false
}

// parseKB reads `  123 kB` as bytes, and a number with no unit as bytes already.
func parseKB(field []byte) (uint64, bool) {
	field = bytes.TrimLeft(field, " \t")
	n, rest, ok := parseUint(field)
	if !ok {
		return 0, false
	}
	switch unit := bytes.TrimSpace(rest); {
	case len(unit) == 0:
		return n, true
	case string(unit) == "kB":
		if n > maxCount/1024 {
			return 0, false
		}
		return n * 1024, true
	}
	return 0, false
}

// parseCount reads a file that holds one number, such as memory.limit_in_bytes.
func parseCount(data []byte) (uint64, bool) {
	n, rest, ok := parseUint(bytes.TrimSpace(data))
	return n, ok && len(rest) == 0
}

// maxCount is the largest value parseUint reads.
const maxCount = math.MaxUint64

// parseUint reads the decimal digits at the start of b, and what follows them.
func parseUint(b []byte) (n uint64, rest []byte, ok bool) {
	i := 0
	for ; i < len(b) && b[i] >= '0' && b[i] <= '9'; i++ {
		digit := uint64(b[i] - '0')
		if n > (maxCount-digit)/10 {
			return 0, nil, false
		}
		n = n*10 + digit
	}
	return n, b[i:], i > 0
}

// heldFile is a file the guard reads at every interval: held open and read from its start into a
// buffer it keeps, so a read allocates nothing once the buffer has grown to the file's size. A
// read from offset 0 makes procfs and cgroupfs generate the file again, on Linux and under gVisor
// alike.
type heldFile struct {
	path string
	file *os.File
	buf  []byte
}

// maxHeldFile bounds what a heldFile grows its buffer to: the files it holds are a few kilobytes.
const maxHeldFile = 1 << 20

// read is the file's whole content, valid until the next read. A failed read closes the file, so
// the next one opens it again.
func (h *heldFile) read() ([]byte, error) {
	if h.file == nil {
		f, err := os.Open(h.path)
		if err != nil {
			return nil, err
		}
		h.file = f
		if h.buf == nil {
			h.buf = make([]byte, 4096)
		}
	}
	for {
		n, err := h.file.ReadAt(h.buf, 0)
		if err != nil && err != io.EOF {
			h.close()
			return nil, err
		}
		if n < len(h.buf) {
			return h.buf[:n], nil
		}
		if len(h.buf) >= maxHeldFile {
			h.close()
			return nil, fmt.Errorf("%s is larger than %d bytes", h.path, maxHeldFile)
		}
		h.buf = make([]byte, 2*len(h.buf))
	}
}

func (h *heldFile) close() {
	if h.file != nil {
		_ = h.file.Close()
		h.file = nil
	}
}

// readAll is the whole file at path, read into buf's storage, grown as it needs.
func readAll(path string, buf []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return buf[:0], err
	}
	defer f.Close()
	buf = buf[:0]
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}

// readHead is the start of the file at path, read into buf and no further: the whole file when it
// is shorter than buf, and buf full when it is not.
func readHead(path string, buf []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := io.ReadFull(f, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return buf[:n], err
}
