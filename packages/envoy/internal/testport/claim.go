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
// run sets TMPDIR to a directory of its own) still see each other's claims. It is in the shared
// /tmp, so Claim uses it only as a directory of this user's, which no other user may enter.
func ClaimDir() string {
	return filepath.Join("/tmp", "legion-loopback-hosts-"+strconv.Itoa(os.Getuid()))
}

// claimDraws is how many addresses Claim draws before it gives up: an address a live process
// holds is drawn again, and with 16,516,096 to draw from, this many held in a row means the draw
// is broken (one that gives the same address every time), not that the machine is full.
const claimDraws = 64

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
	return ClaimFrom(dir, owner, LoopbackIP)
}

// ClaimFrom is Claim with each address drawn from draw rather than LoopbackIP, so a test can
// choose the address a claim meets: one a live process holds, or one that is not up.
func ClaimFrom(dir string, owner int, draw func() net.IP) (string, error) {
	if runtime.GOOS != "linux" {
		return "127.0.0.1", nil
	}
	if err := ownClaimDir(dir); err != nil {
		return "", err
	}
	sweep(dir)
	held := ""
	for range claimDraws {
		host := draw().String()
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
		held = host
	}
	return "", fmt.Errorf("no loopback address claimed in %d draws: a live process held each one drawn, the last %s", claimDraws, held)
}

// ownClaimDir makes dir, a directory only this user may enter, when it is missing, and refuses it
// unless it is one: a directory, not a link to one, owned by this user, with mode 0700. ClaimDir
// is a fixed path in the shared /tmp, so another user can make it first: with mode 0755 to refuse
// every claim, or 0777 to remove claims or plant them.
func ownClaimDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make the loopback claims directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("read the loopback claims directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm() != 0o700 {
		owner := "an unknown user"
		if ok {
			owner = "uid " + strconv.FormatUint(uint64(stat.Uid), 10)
		}
		return fmt.Errorf("the loopback claims directory %s is %s with mode %04o, owned by %s; a claim needs a directory of uid %d with mode 0700: remove %s",
			dir, kindOf(info), uint32(info.Mode().Perm()), owner, os.Getuid(), dir)
	}
	return nil
}

// kindOf names what a file is, for ownClaimDir's refusal.
func kindOf(info fs.FileInfo) string {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return "a symbolic link"
	case info.IsDir():
		return "a directory"
	default:
		return "a file"
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
