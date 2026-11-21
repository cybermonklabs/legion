// Package sockettest reads a socket's holders from Linux's /proc by the socket's inode: which
// descriptors of a process are the socket, and whether the TCP tables still list it. It is for a
// test that checks a socket by its identity rather than by its port: once a socket is closed, any
// other process on the machine may bind its port, so a bind of the port says nothing about who held
// the socket. It is an ordinary package rather than a _test.go file so that the tests of more than
// one package, the infra module's among them, import the one copy. Each call returns what it could
// not read as an error, and its caller decides what that means for its test.
package sockettest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// Row is one row of /proc/net/tcp or /proc/net/tcp6: its local address and state as the kernel
// prints them (127.0.0.1 port 8080 is 0100007F:1F90 on the little-endian machines the tests run on,
// and state 0A is LISTEN), and its socket's inode.
type Row struct {
	Local string
	State string
	Inode uint64
}

// TCPRows returns every row of /proc/net/tcp and /proc/net/tcp6. The two list a TCP socket while it
// listens or is connected; a socket that is only bound is in neither. A machine with IPv6 off has
// no /proc/net/tcp6, and any other failure to read either table is the error.
func TCPRows() ([]Row, error) {
	return tcpRows("/proc")
}

// tcpRows is TCPRows over the tables under root, which its test lays out itself.
func tcpRows(root string) ([]Row, error) {
	var rows []Row
	for _, table := range []string{"tcp", "tcp6"} {
		path := filepath.Join(root, "net", table)
		data, err := os.ReadFile(path)
		if table == "tcp6" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(data), "\n")
		// The first line is the table's header; the tenth field of a row is its inode.
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			inode, err := strconv.ParseUint(fields[9], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s has a row whose inode is %q: %s", path, fields[9], line)
			}
			rows = append(rows, Row{Local: fields[1], State: fields[3], Inode: inode})
		}
	}
	return rows, nil
}
