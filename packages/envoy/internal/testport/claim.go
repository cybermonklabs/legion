package testport

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ClaimDir is the directory every claim of this user is a file in: one fixed path, read from no
// variable, so two stacks whose environments differ (a script runs natsd under `env -i`, and a
// run sets TMPDIR to a directory of its own) still see each other's claims.
func ClaimDir() string {
	return filepath.Join("/tmp", "legion-loopback-hosts-"+strconv.Itoa(os.Getuid()))
}

// Claim is a loopback address LoopbackIP draws that no live process of this user has claimed, now
// claimed for owner, a process id: the file dir/<address> holds owner's id. A claim keeps two
// processes, two script stacks that each start a server, from both taking one address. An address
// whose claim names a process that no longer runs is free again, so a claim a killed script never
// released costs nothing, and each Claim first removes every such claim in dir (sweep), so the
// directory holds the live claims alone. Before it claims an address, Claim binds port 0 on it, so
// an address that is not up fails here and names itself rather than at the server's start.
//
// Off Linux it is 127.0.0.1, the address LoopbackIP gives there, and nothing is claimed: every
// server there shares it.
//
// Two processes that draw the same address at the same moment, both after its owner died, can
// both take it; with 16,516,096 addresses to draw from, that is no case a script meets.
func Claim(dir string, owner int) (string, error) {
	if runtime.GOOS != "linux" {
		return "127.0.0.1", nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("make the loopback claims directory: %w", err)
	}
	sweep(dir)
	for {
		host := LoopbackIP().String()
		listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return "", fmt.Errorf("loopback address %s is not up: %w", host, err)
		}
		listener.Close()
		claimed, err := claim(filepath.Join(dir, host), owner)
		if err != nil {
			return "", err
		}
		if claimed {
			return host, nil
		}
	}
}

// sweep removes every claim in dir whose process no longer runs. It is housekeeping: an entry it
// cannot read or remove is left for a later sweep, and claim takes a stale claim it meets anyway.
func sweep(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.Type().IsRegular() && !holderRuns(path) {
			_ = os.Remove(path)
		}
	}
}

// claim creates path holding owner's id, and reports whether it now holds it: false when a live
// process holds the claim. A claim naming a process that no longer runs is removed and taken.
func claim(path string, owner int) (bool, error) {
	for {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = file.WriteString(strconv.Itoa(owner) + "\n")
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return false, fmt.Errorf("write the claim %s: %w", path, err)
			}
			return true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return false, fmt.Errorf("claim %s: %w", path, err)
		}
		if holderRuns(path) {
			return false, nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("remove the stale claim %s: %w", path, err)
		}
	}
}

// holderRuns reports whether the process a claim names still runs. A claim that names no process
// (an empty or partly written file) is read as held, since its writer may still be writing it.
func holderRuns(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return true
	}
	// Signal 0 checks the process without signalling it: no error, or EPERM for another user's
	// process, means it runs.
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Release removes owner's claim on host, and leaves a claim another process holds.
func Release(dir, host string, owner int) error {
	path := filepath.Join(dir, host)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) != strconv.Itoa(owner) {
		return nil
	}
	return os.Remove(path)
}
