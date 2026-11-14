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
	// Args are the NUL-separated arguments of `cmdline`, nil for a process that shows none.
	Args []string
	// PIDNamespace is the target of the `ns/pid` link (`pid:[4026531836]`), empty when the scan
	// could not read it.
	PIDNamespace string
}

// Table is every process one scan read, keyed by pid. A process whose stat the scan read is in it
// even when its `exe` or `ns/pid` could not be read, so the tree a classifier walks has no holes;
// such a process is never ended (Guard's unread), whatever the classifier says of it.
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
// it holds nothing any longer, and a missing file is how the kernel says so. Any other failure to
// read its stat, exe or ns/pid counts it in unread.
func scan(proc string) (*Table, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	t := &Table{proc: proc, byPID: map[int]*Process{}, children: map[int][]*Process{}, unread: map[int]error{}}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := readProcess(proc, pid)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
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

// readProcess reads one process's stat, exe, ns/pid and cmdline. It answers the process with an
// error when stat was read and exe or ns/pid was not, so the caller keeps the process in the tree
// and counts it unread; a missing exe is a process that has ended or a zombie, neither of which
// holds memory, and answers fs.ErrNotExist like a missing stat. The arguments are best read: a
// process that shows none is still a process.
func readProcess(proc string, pid int) (*Process, error) {
	dir := filepath.Join(proc, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return nil, fmt.Errorf("read the stat: %w", err)
	}
	p, err := parseStat(stat)
	if err != nil {
		return nil, err
	}
	p.PID = pid
	if cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		p.Args = splitCmdline(cmdline)
	}
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return p, fmt.Errorf("read the exe: %w", err)
	}
	p.Exe = exe
	ns, err := os.Readlink(filepath.Join(dir, "ns", "pid"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return p, fmt.Errorf("read the ns/pid: %w", err)
	}
	p.PIDNamespace = ns
	return p, nil
}

// statStartField is the index of starttime (stat's field 22) among the fields after the command
// name's closing parenthesis, which begin at field 3 (state).
const statStartField = 22 - 3

// parseStat reads a `/proc/<pid>/stat` line: the command name between the first `(` and the last
// `)`, and the parent and start time among the fields after that last `)`. The name is the one
// field a process chooses, and it can hold spaces and parentheses of its own (`(omp daemon
// brok)`), so nothing after the first `(` is split until the last `)`.
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
	return &Process{PPID: ppid, StartTime: start, Name: string(stat[open+1 : end])}, nil
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

// smapsSize is a process's size as the guard ranks it, read from its smaps: the sum of its
// `Anonymous:` lines, plus the `Rss:` of each of its shared mappings of memory-backed files. A
// shared anonymous map (`/dev/zero (deleted)`) and a memfd (`/memfd:`) were measured under gVisor
// to raise the pod's AnonPages while their pages read 0 `Anonymous`, and a POSIX shared memory
// object (`/dev/shm/`, tmpfs) and a System V segment (`/SYSV`) are memory of the same kind, so
// without them a grower that uses one would be invisible. A mapping of a file on disk adds
// nothing: its pages are the file's, which AnonPages does not count, and a mapped database is the
// case the guard must not end. A private mapping adds nothing beyond its `Anonymous` either,
// since the pages it wrote are counted there.
func smapsSize(data []byte) uint64 {
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
			counted = sharedMemoryMapping(line)
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

// sharedMemoryMapping is whether a smaps mapping line (`start-end perms offset dev inode path`)
// is a shared mapping of a memory-backed file whose resident pages count in a process's size.
func sharedMemoryMapping(line []byte) bool {
	fields := line
	var perms []byte
	for i := range 5 {
		fields = bytes.TrimLeft(fields, " ")
		field, rest, _ := bytes.Cut(fields, []byte{' '})
		if i == 1 {
			perms = field
		}
		fields = rest
	}
	if len(perms) < 4 || perms[3] != 's' {
		return false
	}
	path := string(bytes.TrimSpace(fields))
	return path == "/dev/zero (deleted)" || strings.HasPrefix(path, "/memfd:") ||
		strings.HasPrefix(path, "/dev/shm/") || strings.HasPrefix(path, "/SYSV")
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
