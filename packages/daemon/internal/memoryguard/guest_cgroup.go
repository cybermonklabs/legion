package memoryguard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The two versions of a memory cgroup the guest watch reads, as its start line names them.
const (
	cgroupV1 = "v1"
	cgroupV2 = "v2"
)

// cgroupFiles are the files of the container's memory cgroup the guest watch reads, in the
// version the cgroup is: the kill counter, the `oom_kill` line of v2's memory.events or of v1's
// memory.oom_control; the usage, memory.current or memory.usage_in_bytes; the limit, memory.max or
// memory.limit_in_bytes; and memory.stat, whose figures each version names its own way (statKeys).
// A cgroup of no version has none of them, and each of its reads answers nothing. Each file is
// held open and read from its start (heldFile), since the watch reads the counter and the usage at
// every scan.
type cgroupFiles struct {
	version string
	dir     string
	kills   heldFile
	usage   heldFile
	limit   heldFile
	stat    heldFile
	keys    statKeys
}

// statKeys are the keys of a memory.stat the guest watch reads: v2's anon, file, inactive_file and
// shmem, and v1's totals of the same, which count the cgroup's descendants as v2's figures do.
type statKeys struct{ anon, file, inactiveFile, shmem string }

var (
	v2StatKeys = statKeys{anon: "anon", file: "file", inactiveFile: "inactive_file", shmem: "shmem"}
	v1StatKeys = statKeys{anon: "total_rss", file: "total_cache", inactiveFile: "total_inactive_file", shmem: "total_shmem"}
)

// newCgroupFiles is the files of the cgroup of version at dir, none for a version that is neither.
func newCgroupFiles(version, dir string) cgroupFiles {
	held := func(name string) heldFile { return heldFile{path: filepath.Join(dir, name)} }
	switch version {
	case cgroupV2:
		return cgroupFiles{version: version, dir: dir, kills: held("memory.events"), usage: held("memory.current"),
			limit: held("memory.max"), stat: held("memory.stat"), keys: v2StatKeys}
	case cgroupV1:
		return cgroupFiles{version: version, dir: dir, kills: held("memory.oom_control"), usage: held("memory.usage_in_bytes"),
			limit: held("memory.limit_in_bytes"), stat: held("memory.stat"), keys: v1StatKeys}
	}
	return cgroupFiles{}
}

// figure is a size or a count the guest watch read, or none where it could not read one, which
// its lines write `-`.
type figure struct {
	n  uint64
	ok bool
}

func (f figure) String() string {
	if !f.ok {
		return "-"
	}
	return strconv.FormatUint(f.n, 10)
}

// cgroupStat is a memory.stat's figures the guest watch reads, each none where the file or its key
// cannot be read.
type cgroupStat struct {
	anon, file, inactiveFile, shmem figure
}

// readKills is the cgroup's kill counter: how many of its processes any out-of-memory killer has
// killed, the guest kernel's own when the pod is a microVM.
func (c *cgroupFiles) readKills() (uint64, error) {
	if c.version == "" {
		return 0, errors.New("no memory cgroup")
	}
	data, err := c.kills.read()
	if err != nil {
		return 0, err
	}
	n, ok := keyedCount(data, "oom_kill")
	if !ok {
		return 0, fmt.Errorf("%s has no oom_kill line", c.kills.path)
	}
	return n, nil
}

// readUsage is the cgroup's usage.
func (c *cgroupFiles) readUsage() figure {
	if c.version == "" {
		return figure{}
	}
	data, err := c.usage.read()
	if err != nil {
		return figure{}
	}
	n, ok := parseCount(data)
	return figure{n: n, ok: ok}
}

// readLimit is the cgroup's limit, none where it sets none: v2 writes `max`, and v1 the largest
// page-aligned int64 (noLimit).
func (c *cgroupFiles) readLimit() figure {
	if c.version == "" {
		return figure{}
	}
	data, err := c.limit.read()
	if err != nil {
		return figure{}
	}
	n, ok := parseCount(data)
	return figure{n: n, ok: ok && n > 0 && n < noLimit}
}

// readStat is the cgroup's memory.stat figures.
func (c *cgroupFiles) readStat() cgroupStat {
	if c.version == "" {
		return cgroupStat{}
	}
	data, err := c.stat.read()
	if err != nil {
		return cgroupStat{}
	}
	read := func(key string) figure {
		n, ok := keyedCount(data, key)
		return figure{n: n, ok: ok}
	}
	return cgroupStat{anon: read(c.keys.anon), file: read(c.keys.file), inactiveFile: read(c.keys.inactiveFile), shmem: read(c.keys.shmem)}
}

// oomGroup is v2's memory.oom.group as the start line writes it: 1 where the kernel kills the
// cgroup's processes together rather than one, which no kill line could then name one by one;
// none for v1, which has no such file, or where it cannot be read.
func (c *cgroupFiles) oomGroup() figure {
	if c.version != cgroupV2 {
		return figure{}
	}
	data, err := os.ReadFile(filepath.Join(c.dir, "memory.oom.group"))
	if err != nil {
		return figure{}
	}
	n, ok := parseCount(data)
	return figure{n: n, ok: ok}
}

func (c *cgroupFiles) close() {
	c.kills.close()
	c.usage.close()
	c.limit.close()
	c.stat.close()
}

// keyedCount is the value of key's line in a file of `key value` lines (memory.stat,
// memory.events, memory.oom_control), read in place.
func keyedCount(data []byte, key string) (uint64, bool) {
	for len(data) > 0 {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		if len(line) > len(key) && line[len(key)] == ' ' && string(line[:len(key)]) == key {
			return parseCount(line[len(key)+1:])
		}
	}
	return 0, false
}

// cgroupVersionOf is the version of the memory cgroup at dir by the files it holds: v2's
// memory.current or memory.events, else v1's memory.usage_in_bytes or memory.oom_control, else
// none.
func cgroupVersionOf(dir string) string {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	switch {
	case has("memory.current") || has("memory.events"):
		return cgroupV2
	case has("memory.usage_in_bytes") || has("memory.oom_control"):
		return cgroupV1
	}
	return ""
}

// findCgroup is the memory cgroup of the process that reads proc: its version and directory, by
// its own cgroup membership (self/cgroup) and its mount table (self/mountinfo). The memory
// controller of v1 comes first where both versions are mounted, since a hybrid host mounts v2
// with no controller in it; v2's single hierarchy otherwise.
func findCgroup(proc string) (version, dir string, err error) {
	membership, err := os.ReadFile(filepath.Join(proc, "self", "cgroup"))
	if err != nil {
		return "", "", err
	}
	table, err := os.ReadFile(filepath.Join(proc, "self", "mountinfo"))
	if err != nil {
		return "", "", err
	}
	var v1Path, v2Path string
	var inV1, inV2 bool
	for line := range strings.SplitSeq(string(membership), "\n") {
		id, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		controllers, path, ok := strings.Cut(rest, ":")
		switch {
		case !ok:
		case id == "0" && controllers == "":
			v2Path, inV2 = path, true
		case slices.Contains(strings.Split(controllers, ","), "memory"):
			v1Path, inV1 = path, true
		}
	}
	mounts := parseMountinfo(table)
	if inV1 {
		for _, m := range mounts {
			if m.fstype == "cgroup" && slices.Contains(strings.Split(m.superOptions, ","), "memory") {
				return cgroupV1, m.dirOf(v1Path, "memory.usage_in_bytes"), nil
			}
		}
	}
	if inV2 {
		for _, m := range mounts {
			if m.fstype == "cgroup2" {
				return cgroupV2, m.dirOf(v2Path, "memory.current"), nil
			}
		}
	}
	return "", "", fmt.Errorf("no mounted memory cgroup holds this process by %s and %s",
		filepath.Join(proc, "self", "cgroup"), filepath.Join(proc, "self", "mountinfo"))
}

// mount is one line of a mountinfo: the directory of the mounted file system that is mounted
// (root), where (point), the file system's type and its own options.
type mount struct {
	root, point, fstype, superOptions string
}

// parseMountinfo reads a mountinfo's lines (`id parent major:minor root point options [optional
// fields] - type source super-options`), each path with procfs's octal escapes undone.
func parseMountinfo(table []byte) []mount {
	var mounts []mount
	for line := range strings.SplitSeq(string(table), "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields, rest := strings.Fields(before), strings.Fields(after)
		if len(fields) < 5 || len(rest) < 3 {
			continue
		}
		mounts = append(mounts, mount{root: unescapeMount(fields[3]), point: unescapeMount(fields[4]), fstype: rest[0], superOptions: rest[2]})
	}
	return mounts
}

// dirOf is the directory of the cgroup at path, a cgroup's path as /proc/<pid>/cgroup writes it,
// under this mount: the mount point with path's part under the mount's root, or the mount point
// itself when path is outside that root (a container's cgroup mounted as its own root, which a
// cgroup namespace or the runtime's bind gives) or the first directory holds no probe file.
func (m mount) dirOf(path, probe string) string {
	dir := m.point
	if rel, ok := strings.CutPrefix(path, m.root); ok && (m.root == "/" || rel == "" || strings.HasPrefix(rel, "/")) {
		dir = filepath.Join(m.point, rel)
	}
	if _, err := os.Stat(filepath.Join(dir, probe)); err != nil {
		if _, err := os.Stat(filepath.Join(m.point, probe)); err == nil {
			return m.point
		}
	}
	return dir
}

// unescapeMount undoes the octal escapes (`\040` for a space) by which a mountinfo writes the
// characters of a path that would split its fields.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
