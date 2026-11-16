// Package sockettest reads which descriptors of a process are a given socket, from Linux's /proc,
// for a test that checks a socket by its identity rather than by its port: once a socket is closed,
// any other process on the machine may bind its port, so a bind of the port says nothing about who
// held the socket. It is an ordinary package rather than a _test.go file so that the tests of more
// than one package, the infra module's among them, import the one copy. Each call returns what it
// could not read as an error, and its caller decides what that means for its test.
package sockettest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// Descriptors returns, in ascending order, the descriptors of process pid that are the socket of
// inode: those whose /proc/<pid>/fd link reads socket:[<inode>]. A process with no /proc/<pid>/fd,
// one that has exited and been reaped, holds none, and a descriptor closed while the listing is
// read, the listing's own among them, is passed over. Any other failure to read is the error, such
// as a process of another user's, whose descriptors this process may not read (fs.ErrPermission).
func Descriptors(pid int, inode uint64) ([]int, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	link := fmt.Sprintf("socket:[%d]", inode)
	var descriptors []int
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if target != link {
			continue
		}
		descriptor, err := strconv.Atoi(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("%s lists %q, which is no descriptor", dir, entry.Name())
		}
		descriptors = append(descriptors, descriptor)
	}
	slices.Sort(descriptors)
	return descriptors, nil
}
