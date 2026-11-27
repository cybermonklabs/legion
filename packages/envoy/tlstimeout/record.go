package tlstimeout

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/redact"
)

// Recorder writes the record of a TLS handshake timeout (Record). Client and Stderr are a caller's
// own; every other field is a seam a test fills and the binaries leave zero.
type Recorder struct {
	// Client names what met the timeout, first on the line: `legion-gh` (`legion gh` around the
	// real gh, and the leak guard's own gh read), `agent-secrets` or `legion` (legion's own HTTP
	// clients). It also names the stamp the rate limit keeps (Record).
	Client string
	// Stderr is where the line goes: the process's standard error.
	Stderr io.Writer
	// Curl is the curl the record runs: /usr/bin/curl where it is empty, a fixed path, never the
	// first curl on the caller's PATH.
	Curl string
	// StampDir is the directory of the rate limit's stamps: <os.TempDir()>/legion-tls-timeout where
	// it is empty, so every process of a user on one machine shares them.
	StampDir string
	// Bound is how long each probe runs, 5 s where it is zero.
	Bound time.Duration
	// Every is the rate limit: one record per Client in each, a minute where it is zero.
	Every time.Duration
	// Now is the time a record is made at, for its rate limit and its `at`: time.Now where nil.
	Now func() time.Time
	// CgroupRoot is where the cgroup files are read, /sys/fs/cgroup where it is empty.
	CgroupRoot string
	// ChildCPU, where it is not nil, answers the user and system CPU of the child that met the
	// timeout, for a caller that ran the failing client as a child (`legion gh`'s real gh), and
	// whether it could read them. The line holds child_user_s and child_sys_s only where it is set.
	ChildCPU func() (user, sys time.Duration, ok bool)
}

const (
	defaultCurl   = "/usr/bin/curl"
	defaultBound  = 5 * time.Second
	defaultEvery  = time.Minute
	defaultCgroup = "/sys/fs/cgroup"
	// lineBytes is the longest a record line is, its line break included.
	lineBytes = 2048
)

// probeTemplate is the transport the Go probe clones (probeGo): http.DefaultTransport's
// settings, the proxy from the environment and the 10 s handshake timeout among them, so the probe
// offers the handshake the failing client offered, but no connection kept, since a probe makes
// one request. Each record clones it, so no record shares a connection or a dial with another, or
// with any client of the process: a shared transport could hand the probe a connection another
// request had already made, and it would measure nothing.
var probeTemplate = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ForceAttemptHTTP2:     true,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
	DisableKeepAlives:     true,
}

// probeDialer is the dialer of probeTemplate's settings, http.DefaultTransport's own.
var probeDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// Record writes one line to Stderr, at most 2048 bytes, that says what stood around a TLS handshake
// timeout to host (a host and port, the request URL's Host), where Every allows it:
//
//	<Client>: tls handshake timeout recorded: host=<host> go_dns_s=… go_connect_s=…
//	go_tls_start_s=… go_tls_done_s=… go_tls_version=… go_tls_curve=… go_error="…"
//	curl_namelookup_s=… curl_connect_s=… curl_appconnect_s=… curl_total_s=… curl_exit=…
//	proc_user_s=… proc_sys_s=… [child_user_s=… child_sys_s=…] cpu_quota_us=… cpu_period_us=…
//	cpu_usage_s=… pids=… bound_s=… at=<RFC 3339 UTC>
//
// on one line. Seconds have three decimals; a figure that could not be read, and a phase that did
// not happen, is `-`. In order, it:
//
//   - keeps the rate limit: a stamp, the file <StampDir>/<Client> of mode 0600 in a directory of
//     mode 0700 that this user owns, neither a link. Where the stamp's time is within Every of
//     Now, another record of Client is that recent and this one writes nothing and runs nothing.
//     Otherwise its time becomes Now, under a lock, so of records made at once one goes on. A
//     stamp it cannot keep so, a directory another user made among them, writes nothing either: a
//     record that could not be limited could flood a log in the burst it measures;
//   - reads the process's own user and system CPU (getrusage RUSAGE_SELF), the child's where
//     ChildCPU is set, and the cgroup's CPU quota, period and usage and its process count, before
//     either probe runs, so what they read is the moment of the timeout and not the record's own
//     work (cgroupFigures; /proc/loadavg reads zeros under gVisor, so the cgroup is what says how
//     loaded the pod is);
//   - runs two probes of https://<host>/ at once, each under Bound: a Go GET on a transport of its
//     own (probeTemplate), traced from the probes' start, and one curl, with no credential of the
//     call, its headers, its path or its query in either (probeGo, probeCurl);
//   - and writes the line, cutting the Go probe's error, never a key, to keep it at 2048 bytes.
//
// It never panics and returns nothing: a part that fails writes `-` for its figures, and a panic
// of any part is dropped, since the record runs in the path of a call that already failed and must
// not fail it again. It runs while its caller waits, at most about Bound and a second at a call
// that failed, and at most once in Every for each Client. A Client that is not a word of letters,
// digits and `-` alone records nothing, since it names a file in StampDir.
func (r *Recorder) Record(host string) {
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
			ctx, cancel := context.WithTimeout(context.Background(), seen.bound)
			defer cancel()
			seen.goProbe = probeGo(ctx, host, started)
		})
		wg.Go(func() { seen.curl = probeCurl(r.curl(), host, seen.bound) })
		wg.Wait()
	} else {
		seen.goProbe.said = "the host is no plain host, so nothing probed it"
	}
	_, _ = io.WriteString(r.Stderr, recordLine(r.Client, printedHost(host), seen))
}

// clientWord reports whether client is a client's name a record may write and keep a stamp
// under: letters, digits and `-`, not empty.
func clientWord(client string) bool {
	if client == "" {
		return false
	}
	for _, r := range client {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (r *Recorder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Recorder) bound() time.Duration {
	if r.Bound > 0 {
		return r.Bound
	}
	return defaultBound
}

func (r *Recorder) every() time.Duration {
	if r.Every > 0 {
		return r.Every
	}
	return defaultEvery
}

func (r *Recorder) curl() string {
	if r.Curl != "" {
		return r.Curl
	}
	return defaultCurl
}

func (r *Recorder) stampDir() string {
	if r.StampDir != "" {
		return r.StampDir
	}
	return filepath.Join(os.TempDir(), "legion-tls-timeout")
}

func (r *Recorder) cgroupRoot() string {
	if r.CgroupRoot != "" {
		return r.CgroupRoot
	}
	return defaultCgroup
}

// claim is the rate limit (Record): whether a record of r.Client may be made at now, having set its
// stamp's time to now where it may. A stamp made now is one; an existing stamp is read and set
// under an exclusive flock, so of records made at once, in one process or in several, the first
// sets the time and the rest read it. The directory is checked by its path (a directory, no link,
// this user's, made mode 0700), and the stamp by what was opened and by its path, as cmd/legion's
// ghOpenedAs checks its keeps: under gVisor an O_NOFOLLOW open is not trusted alone to refuse a
// link. In a shared temporary directory, the sticky bit keeps another user from putting anything
// in place of a directory this user made.
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
	stamp, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	made := err == nil
	if errors.Is(err, fs.ErrExist) {
		stamp, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
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
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return false
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()
	if !made {
		held, err := stamp.Stat()
		if err != nil {
			return false
		}
		if since := now.Sub(held.ModTime()); since < r.every() && since > -r.every() {
			return false
		}
	}
	stampAt := syscall.NsecToTimeval(now.UnixNano())
	return syscall.Futimes(fd, []syscall.Timeval{stampAt, stampAt}) == nil
}

// ownedBy says whether uid owns info.
func ownedBy(info fs.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

// observed is everything a record line says beside its client and host.
type observed struct {
	goProbe goPhases
	curl    curlPhases
	self    cpuFigures
	// child is the child's CPU, nil where the recorder has no ChildCPU.
	child  *cpuFigures
	cgroup cgroupFigures
	bound  time.Duration
	at     time.Time
}

// moment is a phase's end from the probes' start, where seen.
type moment struct {
	at   time.Duration
	seen bool
}

func (m moment) word() string {
	if !m.seen {
		return "-"
	}
	return secondsWord(m.at)
}

// goPhases is what the Go probe saw: when name resolution, the connection and the TLS handshake
// ended, success or failure, and when the handshake started, each from the probes' start; the
// TLS version and key exchange of a handshake that ended in success; and said, the words of the
// error the request ended with, "" for none.
type goPhases struct {
	dns, connect, tlsStart, tlsDone moment
	version, curve                  string
	said                            string
}

// goProbe is the Go probe's trace while its request runs (trace) and what it holds the moment the
// request ends (end). The transport's dial outlives a request its context ended (Go 1.27 detaches
// a dial from the request that asked for it), so its callbacks can come after: once ended, none
// changes what was held, and a callback for one of two dials racing to connect counts the first
// that connected.
type goProbe struct {
	started time.Time
	mu      sync.Mutex
	ended   bool
	// connected is whether connect is a dial that connected, which a later failed one leaves.
	connected bool
	phases    goPhases
}

func newGoProbe(started time.Time) *goProbe { return &goProbe{started: started} }

// at sets *m to now, from the probe's start, unless the request has ended.
func (p *goProbe) at(m *moment) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ended {
		*m = moment{at: time.Since(p.started), seen: true}
	}
}

func (p *goProbe) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSDone: func(httptrace.DNSDoneInfo) { p.at(&p.phases.dns) },
		ConnectDone: func(_, _ string, err error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.ended || p.connected {
				return
			}
			p.phases.connect = moment{at: time.Since(p.started), seen: true}
			p.connected = err == nil
		},
		TLSHandshakeStart: func() { p.at(&p.phases.tlsStart) },
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.ended {
				return
			}
			p.phases.tlsDone = moment{at: time.Since(p.started), seen: true}
			if err == nil {
				p.phases.version = strings.ReplaceAll(tls.VersionName(state.Version), " ", "")
				p.phases.curve = state.CurveID.String()
			}
		},
	}
}

// end is what the probe held when its request ended; nothing after changes it.
func (p *goProbe) end() goPhases {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended = true
	return p.phases
}

// probeGo is the Go probe: one GET of https://<host>/ under ctx, on a clone of probeTemplate. The
// transport's dial outlives a request its context ended, so the probe ends its dials itself: each
// dial also ends with ctx, keeping the context the transport hands it, whose trace the DNS and
// connect phases are read through, and each connection made is closed once the probe ends, so a
// handshake its bound left running does not run on beside the caller's retry. It sends no header
// but Go's own and reads nothing of the answer. Its error is said as the words redact.Cause gives
// an error of that URL: a transport failure's own text, a context's end, or the failure's class,
// never a URL or a value an error quoted, a proxy variable that does not parse among them.
func probeGo(ctx context.Context, host string, started time.Time) goPhases {
	transport := probeTemplate.Clone()
	var mu sync.Mutex
	var dialed []net.Conn
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		dialCtx, cancel := context.WithCancel(dialCtx)
		defer cancel()
		defer context.AfterFunc(ctx, cancel)()
		conn, err := probeDialer.DialContext(dialCtx, network, address)
		if conn != nil {
			mu.Lock()
			dialed = append(dialed, conn)
			mu.Unlock()
		}
		return conn, err
	}
	probe := newGoProbe(started)
	address := "https://" + host + "/"
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, probe.trace()), http.MethodGet, address, nil)
	if err == nil {
		var response *http.Response
		if response, err = transport.RoundTrip(request); response != nil {
			_ = response.Body.Close()
		}
	}
	phases := probe.end()
	mu.Lock()
	for _, conn := range dialed {
		_ = conn.Close()
	}
	mu.Unlock()
	transport.CloseIdleConnections()
	if err != nil {
		phases.said = redact.Cause(err, address).Error()
	}
	return phases
}

// curlPhases is what the curl probe saw: time_namelookup, time_connect, time_appconnect and
// time_total, each where curl printed it and it was not 0 (curl prints 0 for a phase that never
// ended), and the status curl exited with, where it exited of its own.
type curlPhases struct {
	timings [4]moment
	exit    int
	exited  bool
}

// curlWrites is what the curl probe asks curl to print (-w): its four timings, from curl's start.
const curlWrites = "%{time_namelookup} %{time_connect} %{time_appconnect} %{time_total}"

// curlOutBytes is the most of curl's standard output the probe reads: the four timings take about
// forty.
const curlOutBytes = 256

// curlMargin is how long after its --max-time the curl probe is stopped: curl stops itself at the
// bound and prints the timings it has, which a stop at the same moment would race and lose
// (measured with curl 8 against a peer that never answered: a stop at the bound left no timings);
// the stop is for a curl that does not keep its own --max-time.
const curlMargin = 500 * time.Millisecond

// probeCurl is the curl probe: curl, a fixed path, asking https://<host>/ with no .curlrc and no
// netrc (-q first, --no-netrc), no globbing of the host (--globoff, which an IPv6 address's
// brackets would be), silent but for its errors, its body dropped, under the bound (--max-time),
// and printing its four timings, stopped curlMargin after the bound where it runs on. Its standard
// input is empty and its standard error dropped. It is given no credential: its argv is the host's
// root and these options, and its environment curlEnviron's.
func probeCurl(curl, host string, bound time.Duration) curlPhases {
	ctx, cancel := context.WithTimeout(context.Background(), bound+curlMargin)
	defer cancel()
	command := exec.CommandContext(ctx, curl, "-q", "--no-netrc", "--globoff", "-sS", "-o", os.DevNull,
		"--max-time", strconv.FormatFloat(bound.Seconds(), 'f', -1, 64), "-w", curlWrites, "https://"+host+"/")
	command.Env = curlEnviron(os.Environ())
	out := &cappedBuffer{max: curlOutBytes}
	command.Stdout = out
	// A curl that left a child holding its output is waited for no longer than this once it
	// exits or is stopped.
	command.WaitDelay = 250 * time.Millisecond
	// A curl that is not there starts no process, and every figure of it stays `-`.
	_ = command.Run()
	var seen curlPhases
	if state := command.ProcessState; state != nil && state.Exited() {
		seen.exit, seen.exited = state.ExitCode(), true
	}
	if fields := strings.Fields(string(out.data)); len(fields) == len(seen.timings) {
		for i, field := range fields {
			value, err := strconv.ParseFloat(field, 64)
			if err == nil && value > 0 {
				seen.timings[i] = moment{at: time.Duration(value * float64(time.Second)), seen: true}
			}
		}
	}
	return seen
}

// curlProxyNames are the proxy variables curl reads, which the curl probe keeps, so a pod that
// reaches the internet through a proxy probes through it as its failing client did.
var curlProxyNames = map[string]bool{
	"http_proxy": true, "HTTP_PROXY": true, "https_proxy": true, "HTTPS_PROXY": true,
	"all_proxy": true, "ALL_PROXY": true, "no_proxy": true, "NO_PROXY": true,
}

// curlEnviron is the curl probe's environment out of environ: PATH, HOME and TMPDIR, and each
// proxy variable curl reads that is set, is not empty and holds no `@`, so no proxy that carries a
// user and password (the secrets broker's airlock proxy is one) reaches curl. No token variable,
// trust store, TLS key log or curl configuration home is in it.
func curlEnviron(environ []string) []string {
	var kept []string
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		switch {
		case name == "PATH" || name == "HOME" || name == "TMPDIR":
			kept = append(kept, entry)
		case curlProxyNames[name] && value != "" && !strings.Contains(value, "@"):
			kept = append(kept, entry)
		}
	}
	return kept
}

// cappedBuffer keeps the first max bytes written to it and drops the rest, so a curl that prints
// more than its timings costs the record nothing.
type cappedBuffer struct {
	max  int
	data []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.data); room > 0 {
		b.data = append(b.data, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// cpuFigures is a user and system CPU, where it could be read.
type cpuFigures struct {
	user, sys time.Duration
	ok        bool
}

// selfCPU is this process's own user and system CPU so far (getrusage RUSAGE_SELF).
func selfCPU() cpuFigures {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return cpuFigures{}
	}
	return cpuFigures{user: time.Duration(usage.Utime.Nano()), sys: time.Duration(usage.Stime.Nano()), ok: true}
}

// cgroupFigures is the cgroup's CPU quota and period in microseconds, its CPU usage in seconds and
// its process count, each as the line writes it, "" for one that could not be read.
type cgroupFigures struct {
	quota, period, usage, pids string
}

// readCgroup reads the cgroup figures under root in either form: cgroup v2's cpu.max (`<quota>
// <period>`, `max` for no quota), cpu.stat's usage_usec and pids.current at root; else cgroup v1's
// cpu/cpu.cfs_quota_us (-1 for no quota, written `max` as v2 writes it), cpu/cpu.cfs_period_us,
// cpuacct/cpuacct.usage in nanoseconds and pids/pids.current. Each figure is read on its own, the
// v2 file first, so a tree that holds one form's file for one figure and the other's for another
// still gives both.
func readCgroup(root string) cgroupFigures {
	var c cgroupFigures
	if limit, ok := readText(filepath.Join(root, "cpu.max")); ok {
		if fields := strings.Fields(limit); len(fields) == 2 {
			c.quota, c.period = quotaWord(fields[0]), countWord(fields[1])
		}
	} else {
		quota, _ := readText(filepath.Join(root, "cpu", "cpu.cfs_quota_us"))
		period, _ := readText(filepath.Join(root, "cpu", "cpu.cfs_period_us"))
		c.quota, c.period = quotaWord(quota), countWord(period)
	}
	if stat, ok := readText(filepath.Join(root, "cpu.stat")); ok {
		for line := range strings.Lines(stat) {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "usage_usec "); ok {
				c.usage = usageWord(value, time.Microsecond)
			}
		}
	} else if usage, ok := readText(filepath.Join(root, "cpuacct", "cpuacct.usage")); ok {
		c.usage = usageWord(usage, time.Nanosecond)
	}
	pids, ok := readText(filepath.Join(root, "pids.current"))
	if !ok {
		pids, _ = readText(filepath.Join(root, "pids", "pids.current"))
	}
	c.pids = countWord(pids)
	return c
}

// readText is the trimmed text of the small file at path, and whether it could be read.
func readText(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// countWord is text where it is a whole number of no sign, and "" otherwise.
func countWord(text string) string {
	if _, err := strconv.ParseUint(text, 10, 64); err != nil {
		return ""
	}
	return text
}

// quotaWord is a CPU quota as the line writes it: a whole number, or `max` for none (v2's `max`,
// v1's -1).
func quotaWord(text string) string {
	if text == "max" || text == "-1" {
		return "max"
	}
	return countWord(text)
}

// usageWord is a usage of text whole units of unit, in seconds.
func usageWord(text string, unit time.Duration) string {
	value, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatFloat(float64(value)*unit.Seconds(), 'f', 3, 64)
}

// secondsWord is d in seconds, with three decimals.
func secondsWord(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 3, 64)
}

// figure is word, or `-` where it is "".
func figure(word string) string {
	if word == "" {
		return "-"
	}
	return word
}

// cpuWords is c's user and system CPU as the line writes them.
func cpuWords(c cpuFigures) (string, string) {
	if !c.ok {
		return "-", "-"
	}
	return secondsWord(c.user), secondsWord(c.sys)
}

// printedHost is host as the line names it: as it stands where it is a plain host, which is the
// only kind the probes ask, and Go-quoted otherwise.
func printedHost(host string) string {
	if plainHost(host) {
		return host
	}
	return strconv.Quote(host)
}

// recordLine is the record's line for host, its line break included, at most lineBytes: where it
// would be longer, the Go probe's error is cut at a character's start until its quote fits.
func recordLine(client, host string, seen observed) string {
	exit := "-"
	if seen.curl.exited {
		exit = strconv.Itoa(seen.curl.exit)
	}
	selfUser, selfSys := cpuWords(seen.self)
	pairs := func(said string) string {
		var b strings.Builder
		write := func(key, value string) {
			b.WriteByte(' ')
			b.WriteString(key)
			b.WriteByte('=')
			b.WriteString(value)
		}
		b.WriteString(client + ": tls handshake timeout recorded:")
		write("host", host)
		write("go_dns_s", seen.goProbe.dns.word())
		write("go_connect_s", seen.goProbe.connect.word())
		write("go_tls_start_s", seen.goProbe.tlsStart.word())
		write("go_tls_done_s", seen.goProbe.tlsDone.word())
		write("go_tls_version", figure(seen.goProbe.version))
		write("go_tls_curve", figure(seen.goProbe.curve))
		if said == "" {
			write("go_error", "-")
		} else {
			write("go_error", strconv.Quote(said))
		}
		for i, key := range []string{"curl_namelookup_s", "curl_connect_s", "curl_appconnect_s", "curl_total_s"} {
			write(key, seen.curl.timings[i].word())
		}
		write("curl_exit", exit)
		write("proc_user_s", selfUser)
		write("proc_sys_s", selfSys)
		if seen.child != nil {
			user, sys := cpuWords(*seen.child)
			write("child_user_s", user)
			write("child_sys_s", sys)
		}
		write("cpu_quota_us", figure(seen.cgroup.quota))
		write("cpu_period_us", figure(seen.cgroup.period))
		write("cpu_usage_s", figure(seen.cgroup.usage))
		write("pids", figure(seen.cgroup.pids))
		write("bound_s", secondsWord(seen.bound))
		write("at", seen.at.UTC().Format(time.RFC3339))
		b.WriteByte('\n')
		return b.String()
	}
	said := seen.goProbe.said
	line := pairs(said)
	if len(line) > lineBytes && said != "" {
		// Every other field is bounded, a host at 261 bytes, so the error's quote alone is cut: to
		// the room the rest of the line leaves it.
		said = cutToQuote(said, lineBytes-(len(line)-len(strconv.Quote(said))))
		line = pairs(said)
	}
	return line
}

// cutToQuote is the longest start of said, cut at a character's start, whose Go quote
// (strconv.Quote) is at most room bytes. Quote escapes each character on its own, so the quote of a
// start is the quotes of its characters.
func cutToQuote(said string, room int) string {
	used := len(`""`)
	for i := 0; i < len(said); {
		_, size := utf8.DecodeRuneInString(said[i:])
		if used += len(strconv.Quote(said[i:i+size])) - len(`""`); used > room {
			return said[:i]
		}
		i += size
	}
	return said
}
