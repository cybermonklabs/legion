//go:build unix

// A record is made on a unix system alone: its rate limit keeps its stamp under flock, opened with
// O_NOFOLLOW and held to its owner's uid (claim), and the process's CPU is getrusage's (selfCPU),
// none of which windows has. On any other system RecordContext writes nothing (record_other.go).
// The calls are golang.org/x/sys/unix's, as cmd/cipher's unix files make theirs, since the
// standard library's syscall has no flock for solaris and illumos.

package tlstimeout

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RecordContext writes one line to Stderr, at most 2048 bytes, that says what stood around event
// at host (a host and port, the request URL's Host): a TLS handshake timeout to it, or the leak
// guard's own bound stopping a read of it before any answer (Event). It writes it where its rate
// limit of one a minute allows it:
//
//	<Client>: tls handshake timeout recorded: host=<host> event=<handshake_timeout|guard_bound>
//	go_dns_s=… go_connect_s=… go_tls_start_s=… go_tls_done_s=… go_tls_version=… go_tls_curve=…
//	go_error="…" curl_namelookup_s=… curl_connect_s=… curl_appconnect_s=… curl_total_s=…
//	curl_exit=… proc_user_s=… proc_sys_s=… [child_user_s=… child_sys_s=…] cpu_quota_us=…
//	cpu_period_us=… cpu_usage_s=… pids=… bound_s=… at=<RFC 3339 UTC>
//
// on one line. Seconds have three decimals; a figure that could not be read, and a phase that did
// not happen, is `-`. Both events share the rate limit, since both are the one fault. In order,
// it:
//
//   - keeps the rate limit: a stamp, the file <StampDir>/<Client> of mode 0600 in a directory of
//     mode 0700 that this user owns, neither a link. Where the stamp's time is within a minute of
//     Now, another record of Client is that recent and this one writes nothing and runs nothing.
//     Otherwise its time becomes Now, under a lock that waits for nothing: a record that finds
//     another holding it writes nothing and runs nothing too, so of records made at once one goes
//     on, and no record waits on another (claim). A stamp it cannot keep so, a directory another
//     user made among them, writes nothing either: a record that could not be limited could flood
//     a log in the burst it measures;
//   - reads the process's own user and system CPU (getrusage RUSAGE_SELF), the child's where
//     ChildCPU is set, and the cgroup's CPU quota, period and usage and its process count, before
//     either probe runs, so what they read is the moment of the timeout and not the record's own
//     work (cgroupFigures; /proc/loadavg reads zeros under gVisor, so the cgroup is what says how
//     loaded the pod is);
//   - runs two probes of https://<host>/ at once, each under Bound and ctx: a Go GET on a
//     transport of its own (probeTemplate), traced from the probes' start, and one curl, which
//     Start starts, with no credential of the call, its headers, its path or its query in either
//     (probeGo, probeCurl). A Recorder with no Start runs the Go probe alone;
//   - and writes the line, cutting the Go probe's error, never a key, to keep it at 2048 bytes.
//
// It never panics and returns nothing: a part that fails writes `-` for its figures, and a panic
// of any part is dropped, since the record runs in the path of a call, most often one that already
// failed, and must not fail it. It runs while its caller waits, at most about Bound and a second,
// and at most once a minute for each Client. A Client that is not a word of letters, digits and
// `-` alone records nothing, since it names a file in StampDir.
//
// ctx ends the probes where it ends before their bound: a caller whose own end comes while they
// run, such as `legion gh` on a SIGTERM, is not held to the bound. The line is written all the
// same, with what the probes saw by then: the Go probe's error is the context's end, and a curl
// stopped before it exited of its own has no exit status.
func (r *Recorder) RecordContext(ctx context.Context, host string, event Event) {
	defer func() { _ = recover() }()
	if r == nil || r.Stderr == nil || !clientWord(r.Client) {
		return
	}
	at := r.now()
	if !r.claim(at) {
		return
	}
	seen := observed{self: selfCPU(), cgroup: readCgroup(r.cgroupRoot()), bound: r.bound(), at: at}
	if r.ChildCPU != nil {
		user, sys, ok := r.ChildCPU()
		seen.child = &cpuFigures{user: user, sys: sys, ok: ok}
	}
	if plainHost(host) {
		started := time.Now()
		var wg sync.WaitGroup
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, seen.bound)
			defer cancel()
			seen.goProbe = probeGo(ctx, host, started)
		})
		if r.Start != nil {
			wg.Go(func() { seen.curl = probeCurl(ctx, r.Start, r.curl(), host, seen.bound) })
		}
		wg.Wait()
	} else {
		seen.goProbe.said = "the host is no plain host, so nothing probed it"
	}
	_, _ = io.WriteString(r.Stderr, recordLine(r.Client, printedHost(host), event, seen))
}

// claim is the rate limit (RecordContext): whether a record of r.Client may be made at now, having
// set its stamp's time to now where it may. A stamp made now is this record's: of records made at
// once the O_EXCL create lets one alone make it, and every other reads its time as recent. An
// existing stamp is read and set under an exclusive flock that waits for nothing: where another
// holder has it, another record is in its claim at that moment, in this process or in another, and
// of records made at once only one goes on, so this one writes nothing, as for a stamp within the
// minute. A lock that waited would wait with no bound, and every role of an issue pod shares the
// stamp (its $TMPDIR is on the issue's volume and every role is one user), so a holder stopped or
// stuck would hold each other role's record, and the call or the push that runs it. The directory
// is checked by its path (a directory, no link, this user's, made mode 0700), and the stamp by
// what was opened and by its path, as cmd/legion's ghOpenedAs checks its keeps: under gVisor an
// O_NOFOLLOW open is not trusted alone to refuse a link. In a shared temporary directory, the
// sticky bit keeps another user from putting anything in place of a directory this user made.
func (r *Recorder) claim(now time.Time) bool {
	dir := r.stampDir()
	uid := os.Getuid()
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !ownedBy(info, uid) {
		return false
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return false
		}
	}
	path := filepath.Join(dir, r.Client)
	stamp, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	made := err == nil
	if errors.Is(err, fs.ErrExist) {
		stamp, err = os.OpenFile(path, os.O_RDWR|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return false
	}
	defer stamp.Close()
	opened, err := stamp.Stat()
	if err != nil || !opened.Mode().IsRegular() || !ownedBy(opened, uid) {
		return false
	}
	if found, err := os.Lstat(path); err != nil || !found.Mode().IsRegular() || !os.SameFile(opened, found) {
		return false
	}
	if err := stamp.Chmod(0o600); err != nil {
		return false
	}
	fd := int(stamp.Fd())
	if !made {
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return false
		}
		defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()
		held, err := stamp.Stat()
		if err != nil {
			return false
		}
		if since := now.Sub(held.ModTime()); since < defaultEvery && since > -defaultEvery {
			return false
		}
	}
	stampAt := unix.NsecToTimeval(now.UnixNano())
	return unix.Futimes(fd, []unix.Timeval{stampAt, stampAt}) == nil
}

// ownedBy says whether uid owns info: os answers a file's own facts as the standard library's
// syscall.Stat_t.
func ownedBy(info fs.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

// selfCPU is this process's own user and system CPU so far (getrusage RUSAGE_SELF).
func selfCPU() cpuFigures {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return cpuFigures{}
	}
	return cpuFigures{user: time.Duration(usage.Utime.Nano()), sys: time.Duration(usage.Stime.Nano()), ok: true}
}
