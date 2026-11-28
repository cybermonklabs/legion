package memoryguard

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// GroupReader reads the size of one process group the way the guard reads a process it may end:
// each member's smaps size (smapsSize: its anonymous memory and the resident pages of its shared
// mappings of memory-backed files), and the memfds its descriptors hold and no member maps
// (heldMemfdBytes), each memfd counted once across the group and at most the pod's Shmem. The
// CodeGraph warm-up's memory cap reads its build's group with it every 250 ms
// (internal/workspace's runCodegraph), so the cap and the guard behind it measure one thing.
//
// It keeps its buffers from one read to the next, a smaps the size of a large Node process's
// included, since it reads the same processes four times a second for as long as one build runs;
// the caller drops the reader once the build has ended, and Close lets go of its meminfo.
type GroupReader struct {
	proc    string
	stat    []byte
	smaps   []byte
	mapped  map[memfdID]bool
	members []int
	meminfo heldFile
}

// NewGroupReader is a reader of the process groups under proc, "/proc" on a real host.
func NewGroupReader(proc string) *GroupReader {
	return &GroupReader{
		proc:    proc,
		stat:    make([]byte, 0, 512),
		mapped:  map[memfdID]bool{},
		meminfo: heldFile{path: filepath.Join(proc, "meminfo")},
	}
}

// Read is the size in bytes of the processes whose process group is pgid, and how many of them it
// read. A process that ends under the read, or whose stat or smaps cannot be read, adds nothing:
// the reader judges a group that grows, and one read in four of a second is followed by the next.
// A zombie, or a dead process with no thread left (exited), holds no memory and is no member. It
// fails only where proc itself cannot be listed.
func (r *GroupReader) Read(pgid int) (size uint64, processes int, err error) {
	dir, err := os.Open(r.proc)
	if err != nil {
		return 0, 0, err
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		return 0, 0, err
	}
	clear(r.mapped)
	r.members = r.members[:0]
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		at := filepath.Join(r.proc, name)
		stat, err := readAll(filepath.Join(at, "stat"), r.stat)
		r.stat = stat
		if err != nil {
			continue
		}
		state, group, ok := statGroup(stat)
		if !ok || group != pgid || exited(at, state) {
			continue
		}
		smaps, err := readAll(filepath.Join(at, "smaps"), r.smaps)
		r.smaps = smaps
		if err != nil {
			continue
		}
		size += smapsSize(smaps, r.mapped)
		r.members = append(r.members, pid)
	}
	var shmem uint64
	if data, err := r.meminfo.read(); err == nil {
		shmem, _ = meminfoBytes(data, "Shmem")
	}
	for _, pid := range r.members {
		size += heldMemfdBytes(filepath.Join(r.proc, strconv.Itoa(pid)), memfdPrefix, r.mapped, shmem)
	}
	return size, len(r.members), nil
}

// Close lets go of the meminfo the reader holds open.
func (r *GroupReader) Close() { r.meminfo.close() }

// statGroup reads a `/proc/<pid>/stat` line's state and process group, the first and the third
// field after the command name's last `)` (parseStat), without the allocations parseStat makes,
// since the reader reads every process's stat four times a second.
func statGroup(stat []byte) (state byte, pgrp int, ok bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, 0, false
	}
	rest := stat[end+1:]
	var fields [3][]byte
	for i := range fields {
		rest = bytes.TrimLeft(rest, " ")
		fields[i], rest, _ = bytes.Cut(rest, []byte{' '})
	}
	n, after, ok := parseUint(fields[2])
	if !ok || len(after) != 0 || len(fields[0]) != 1 || n > uint64(^uint32(0)>>1) {
		return 0, 0, false
	}
	return fields[0][0], int(n), true
}
