package tlstimeout

import (
	"context"
	"crypto/tls"
	"errors"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/envoy/tlstimeout/tlstimeouttest"
)

// curlStandIn is a stand-in curl (the Recorder's Curl) in a directory of the test's own: each run
// appends a line `ran` to runs, writes its arguments to argv, one a line, and its environment to
// env, and then does what then says, such as print the four timings `-w` asks for and exit as a
// curl that gave up does.
func curlStandIn(t *testing.T, then string) (program, argv, env string) {
	t.Helper()
	dir := t.TempDir()
	program, argv, env = filepath.Join(dir, "curl"), filepath.Join(dir, "argv"), filepath.Join(dir, "env")
	script := "#!/bin/sh\necho ran >> '" + filepath.Join(dir, "runs") + "'\nprintf '%s\\n' \"$@\" > '" + argv + "'\nenv > '" + env + "'\n" + then
	if err := testexec.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return program, argv, env
}

// curlRuns is how many times the stand-in curl at program ran.
func curlRuns(t *testing.T, program string) int {
	t.Helper()
	written, err := os.ReadFile(filepath.Join(filepath.Dir(program), "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(written), "ran\n")
}

// curlTimesOut is a curl whose handshake did not finish within its --max-time: curl prints the
// timings it has, time_appconnect 0 for the handshake that never ended, and exits 28.
const curlTimesOut = "printf '0.012 0.034 0.000 1.000'\nexit 28\n"

// writeFiles writes each file of files, by its path under root, making its directories.
func writeFiles(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// cgroupV1 is a cgroup v1 tree as an issue pod's reads: a quota of two CPUs, its usage in
// nanoseconds and its process count.
func cgroupV1(t *testing.T) string {
	return writeFiles(t, t.TempDir(), map[string]string{
		"cpu/cpu.cfs_quota_us":         "200000\n",
		"cpu/cpu.cfs_period_us":        "100000\n",
		"cpuacct/cpuacct.usage":        "2884240000000\n",
		"pids/pids.current":            "227\n",
		"memory/memory.limit_in_bytes": "8589934592\n",
	})
}

// recordKeys are the keys of a record line, in their order, the child's CPU among them.
var recordKeys = []string{
	"host", "event", "go_dns_s", "go_connect_s", "go_tls_start_s", "go_tls_done_s", "go_tls_version", "go_tls_curve", "go_error",
	"curl_namelookup_s", "curl_connect_s", "curl_appconnect_s", "curl_total_s", "curl_exit",
	"proc_user_s", "proc_sys_s", "child_user_s", "child_sys_s",
	"cpu_quota_us", "cpu_period_us", "cpu_usage_s", "pids", "bound_s", "at",
}

// childlessKeys are recordKeys without the child's CPU, the keys of a record with no ChildCPU.
func childlessKeys() []string {
	return slices.DeleteFunc(slices.Clone(recordKeys), func(key string) bool { return strings.HasPrefix(key, "child_") })
}

// seconds is a figure of seconds as a record writes one: three decimals.
var seconds = regexp.MustCompile(`^(0|[1-9][0-9]*)\.[0-9]{3}$`)

// parseRecord reads the one record line written holds: the client it names, its keys in order and
// each key's value, a quoted value unquoted. It fails the test where written is not exactly one
// such line.
func parseRecord(t *testing.T, written string) (client string, keys []string, values map[string]string) {
	t.Helper()
	line, ok := strings.CutSuffix(written, "\n")
	if !ok || strings.Contains(line, "\n") {
		t.Fatalf("the record wrote %q, want one line", written)
	}
	client, rest, ok := strings.Cut(line, ": tls handshake timeout recorded: ")
	if !ok {
		t.Fatalf("the line %q does not say `<client>: tls handshake timeout recorded: `", line)
	}
	values = map[string]string{}
	for rest != "" {
		key, after, ok := strings.Cut(rest, "=")
		if !ok || strings.Contains(key, " ") {
			t.Fatalf("the line %q holds %q where a key=value pair goes", line, rest)
		}
		value := after
		if strings.HasPrefix(after, `"`) {
			quoted, err := strconv.QuotedPrefix(after)
			if err != nil {
				t.Fatalf("the value of %s in %q is no quoted string: %v", key, line, err)
			}
			value, _ = strconv.Unquote(quoted)
			after = after[len(quoted):]
		} else {
			value, after, _ = strings.Cut(after, " ")
			after = " " + after
		}
		keys = append(keys, key)
		values[key] = value
		rest = strings.TrimPrefix(after, " ")
		if after != "" && !strings.HasPrefix(after, " ") {
			t.Fatalf("the value of %s in %q runs on into %q", key, line, after)
		}
	}
	return client, keys, values
}

// startPlain is a test process's Start (Recorder.Start): cmd.Start, with nothing to do once the
// wait is over, since a test process reaps no child it did not start.
func startPlain(cmd *exec.Cmd) (func(), error) { return func() {}, cmd.Start() }

// recordRig is a Recorder whose every seam is the test's: a stand-in curl started by startPlain,
// stamps in a directory of the test's own, a cgroup tree of the test's, a bound of a second and a
// fixed time.
type recordRig struct {
	r               *Recorder
	stderr          *logtest.Buffer
	curl, argv, env string
}

func newRecordRig(t *testing.T, curl string) *recordRig {
	t.Helper()
	rig := &recordRig{stderr: &logtest.Buffer{}}
	rig.curl, rig.argv, rig.env = curlStandIn(t, curl)
	at := time.Date(2026, 11, 27, 21, 30, 0, 0, time.FixedZone("CET", 3600))
	rig.r = &Recorder{
		Client:     "agent-secrets",
		Stderr:     rig.stderr,
		Start:      startPlain,
		Curl:       rig.curl,
		StampDir:   filepath.Join(t.TempDir(), "stamps"),
		Bound:      time.Second,
		Now:        func() time.Time { return at },
		CgroupRoot: cgroupV1(t),
	}
	return rig
}

// Check c-tls-timeout-recorded, point 1: a record is one line, `<client>: tls handshake timeout
// recorded: ` and then every field in its order: the host; the event it records (Record's is
// net/http's own handshake timeout); the Go probe's phases from the record's start, with the TLS
// version and curve and its error; curl's four timings and its exit; the process's CPU and the
// child's; the cgroup's quota, period, usage and process count; the bound; and the time in RFC
// 3339 UTC. Against a peer that never answers, the Go probe connects, starts its handshake and is
// stopped by the bound, and a phase that never happened is `-`. curl is run with no .curlrc and no
// netrc, no globbing, silent with its errors, its body dropped, under the bound, with the four
// timings asked of it, for the host's root alone.
func TestARecordIsOneLineOfEveryFieldInOrder(t *testing.T) {
	rig := newRecordRig(t, curlTimesOut)
	rig.r.ChildCPU = func() (time.Duration, time.Duration, bool) {
		return 1500 * time.Millisecond, 250 * time.Millisecond, true
	}
	host := tlstimeouttest.Silent(t)
	rig.r.Record(host)
	client, keys, values := parseRecord(t, rig.stderr.String())
	if client != "agent-secrets" {
		t.Errorf("the line names the client %q, want agent-secrets", client)
	}
	if !slices.Equal(keys, recordKeys) {
		t.Fatalf("the line's keys are %q, want %q", keys, recordKeys)
	}
	want := map[string]string{
		"host": host, "event": "handshake_timeout", "go_dns_s": "-", "go_tls_done_s": "-", "go_tls_version": "-", "go_tls_curve": "-",
		"go_error": "context deadline exceeded", "curl_namelookup_s": "0.012", "curl_connect_s": "0.034",
		"curl_appconnect_s": "-", "curl_total_s": "1.000", "curl_exit": "28", "child_user_s": "1.500",
		"child_sys_s": "0.250", "cpu_quota_us": "200000", "cpu_period_us": "100000", "cpu_usage_s": "2884.240",
		"pids": "227", "bound_s": "1.000", "at": "2026-11-27T20:30:00Z",
	}
	for key, value := range want {
		if values[key] != value {
			t.Errorf("%s=%q, want %q", key, values[key], value)
		}
	}
	for _, key := range []string{"go_connect_s", "go_tls_start_s", "proc_user_s", "proc_sys_s"} {
		if !seconds.MatchString(values[key]) {
			t.Errorf("%s=%q, want seconds with three decimals", key, values[key])
		}
	}
	argv, err := os.ReadFile(rig.argv)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"-q", "--no-netrc", "--globoff", "-sS", "-o", "/dev/null", "--max-time", "1",
		"-w", "%{time_namelookup} %{time_connect} %{time_appconnect} %{time_total}", "https://" + host + "/"}
	if got := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"); !slices.Equal(got, wantArgv) {
		t.Errorf("curl ran with %q, want %q", got, wantArgv)
	}
}

// Each figure a record cannot read is `-`, and the line keeps its key: a cgroup tree with no file
// of its own, a curl that is not there, a curl that printed no timings, and a child whose CPU
// cannot be read. cgroup v2's files are read as v1's are, and either form's no-quota is `max`. A
// record with no ChildCPU writes no child keys. A host the Go probe is refused at ends at once.
func TestARecordWritesADashForEachFigureItCouldNotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rig changes the recorder of each row; want is the fields the row checks.
		rig  func(t *testing.T, rig *recordRig)
		want map[string]string
		// absent are keys the line does not hold.
		absent []string
	}{
		{
			name: "cgroup v2",
			rig: func(t *testing.T, rig *recordRig) {
				rig.r.CgroupRoot = writeFiles(t, t.TempDir(), map[string]string{
					"cpu.max": "max 100000\n", "cpu.stat": "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\n", "pids.current": "12\n",
				})
			},
			want:   map[string]string{"cpu_quota_us": "max", "cpu_period_us": "100000", "cpu_usage_s": "1.500", "pids": "12"},
			absent: []string{"child_user_s", "child_sys_s"},
		},
		{
			name: "cgroup v1 with no quota",
			rig: func(t *testing.T, rig *recordRig) {
				rig.r.CgroupRoot = writeFiles(t, t.TempDir(), map[string]string{"cpu/cpu.cfs_quota_us": "-1\n", "cpu/cpu.cfs_period_us": "100000\n"})
			},
			want: map[string]string{"cpu_quota_us": "max", "cpu_period_us": "100000", "cpu_usage_s": "-", "pids": "-"},
		},
		{
			name: "a cgroup tree with no file",
			rig:  func(t *testing.T, rig *recordRig) { rig.r.CgroupRoot = t.TempDir() },
			want: map[string]string{"cpu_quota_us": "-", "cpu_period_us": "-", "cpu_usage_s": "-", "pids": "-"},
		},
		{
			name: "cgroup files that hold no figure",
			rig: func(t *testing.T, rig *recordRig) {
				rig.r.CgroupRoot = writeFiles(t, t.TempDir(), map[string]string{"cpu.max": "lots\n", "cpu.stat": "user_usec 5\n", "pids.current": "some\n"})
			},
			want: map[string]string{"cpu_quota_us": "-", "cpu_period_us": "-", "cpu_usage_s": "-", "pids": "-"},
		},
		{
			name: "no curl",
			rig:  func(t *testing.T, rig *recordRig) { rig.r.Curl = filepath.Join(t.TempDir(), "no-curl") },
			want: map[string]string{"curl_namelookup_s": "-", "curl_connect_s": "-", "curl_appconnect_s": "-", "curl_total_s": "-", "curl_exit": "-"},
		},
		{
			name: "a curl that printed no timings",
			rig: func(t *testing.T, rig *recordRig) {
				rig.r.Curl, _, _ = curlStandIn(t, "echo 'curl: (7) Failed to connect' >&2\nexit 7\n")
			},
			want: map[string]string{"curl_namelookup_s": "-", "curl_connect_s": "-", "curl_appconnect_s": "-", "curl_total_s": "-", "curl_exit": "7"},
		},
		{
			name: "a child whose CPU cannot be read",
			rig: func(_ *testing.T, rig *recordRig) {
				rig.r.ChildCPU = func() (time.Duration, time.Duration, bool) { return time.Hour, time.Hour, false }
			},
			want: map[string]string{"child_user_s": "-", "child_sys_s": "-"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRecordRig(t, curlTimesOut)
			tc.rig(t, rig)
			rig.r.Record(testport.Refused(t))
			_, keys, values := parseRecord(t, rig.stderr.String())
			for key, value := range tc.want {
				if values[key] != value {
					t.Errorf("%s=%q, want %q", key, values[key], value)
				}
			}
			for _, key := range tc.absent {
				if slices.Contains(keys, key) {
					t.Errorf("the line holds %s, want none", key)
				}
			}
			if values["go_error"] == "-" || !seconds.MatchString(values["go_connect_s"]) || values["go_tls_start_s"] != "-" {
				t.Errorf("the Go probe of a refused port says go_error=%q go_connect_s=%q go_tls_start_s=%q, want its dial's error, the moment the dial ended and no handshake",
					values["go_error"], values["go_connect_s"], values["go_tls_start_s"])
			}
		})
	}
}

// A record starts its curl through Start, and calls the done Start answered once curl's wait has
// read its exit, which is how a process that reaps children it does not hold (the pod's launcher)
// keeps curl's exit for that wait. A Recorder with no Start starts no curl, and the line's curl
// figures are `-`.
func TestARecordStartsItsCurlThroughItsStart(t *testing.T) {
	t.Run("a start", func(t *testing.T) {
		rig := newRecordRig(t, curlTimesOut)
		var starts, dones int
		var waited bool
		rig.r.Start = func(cmd *exec.Cmd) (func(), error) {
			starts++
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return func() { dones, waited = dones+1, cmd.ProcessState != nil }, nil
		}
		rig.r.Record(testport.Refused(t))
		_, _, values := parseRecord(t, rig.stderr.String())
		if starts != 1 || dones != 1 || !waited || curlRuns(t, rig.curl) != 1 || values["curl_exit"] != "28" {
			t.Errorf("Start ran %d times and its done %d times (after curl's wait: %t), curl ran %d times and curl_exit=%q; want one start, one done after the wait, one curl and 28",
				starts, dones, waited, curlRuns(t, rig.curl), values["curl_exit"])
		}
	})
	t.Run("no start", func(t *testing.T) {
		rig := newRecordRig(t, curlTimesOut)
		rig.r.Start = nil
		rig.r.Record(testport.Refused(t))
		_, _, values := parseRecord(t, rig.stderr.String())
		if runs := curlRuns(t, rig.curl); runs != 0 {
			t.Errorf("a Recorder with no Start ran curl %d times, want none", runs)
		}
		for _, key := range []string{"curl_namelookup_s", "curl_connect_s", "curl_appconnect_s", "curl_total_s", "curl_exit"} {
			if values[key] != "-" {
				t.Errorf("%s=%q, want -", key, values[key])
			}
		}
	})
}

// Check c-tls-timeout-recorded, point 2: a record carries nothing of the call it records but its
// host. A call through Transport whose URL holds a path and a query of tokens, and whose headers
// carry an Authorization and a cookie, records a line that holds none of them, and runs curl with
// none of them in its argv or its environment. curl's environment is PATH, HOME, TMPDIR and the
// proxy variables that carry no credential alone: no token variable, no proxy with a password, no
// trust store, no TLS key log and no curl configuration home, whatever the process holds.
func TestARecordCarriesNothingOfTheCallButItsHost(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv).
	secrets := []string{
		"ghsEnvTokenNotASecret1", "ghsEnvTokenNotASecret2", "proxyPasswordNotASecret",
		"tok3nInPathNotASecret", "tok3nInQueryNotASecret", "tok3nInHeaderNotASecret", "tok3nInCookieNotASecret",
	}
	held := t.TempDir()
	// Every proxy variable is set, so none of the machine's own decides the row: an empty one names
	// no proxy, and one that carries a password is a credential.
	for name, value := range map[string]string{
		"GH_TOKEN": secrets[0], "GITHUB_TOKEN": secrets[1],
		"HTTPS_PROXY": "http://someone:" + secrets[2] + "@127.0.0.1:9", "https_proxy": "http://127.0.0.1:3128",
		"ALL_PROXY": "socks5://someone:" + secrets[2] + "@127.0.0.1:9", "all_proxy": "", "HTTP_PROXY": "", "http_proxy": "http://127.0.0.1:3129",
		"NO_PROXY": "localhost", "no_proxy": "", "SSL_CERT_FILE": held + "/ca.pem", "SSLKEYLOGFILE": held + "/keys",
		"CURL_HOME": held, "XDG_CONFIG_HOME": held,
	} {
		t.Setenv(name, value)
	}
	rig := newRecordRig(t, curlTimesOut)
	host := tlstimeouttest.Silent(t)
	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/v1/secret-path/"+secrets[3]+"?access_token="+secrets[4], nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secrets[5])
	req.Header.Set("Cookie", "session="+secrets[6])
	client := &http.Client{Transport: Transport(&http.Transport{TLSHandshakeTimeout: shortHandshake}, rig.r)}
	if _, err := client.Do(req); !IsTimeout(err) {
		t.Fatalf("the call answered %v, want a handshake timeout", err)
	}
	_, _, values := parseRecord(t, rig.stderr.String())
	if values["host"] != host {
		t.Errorf("the line names the host %q, want %q", values["host"], host)
	}
	argv, err := os.ReadFile(rig.argv)
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(rig.env)
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{"the line": rig.stderr.String(), "curl's argv": string(argv), "curl's environment": string(env)} {
		for _, word := range append(slices.Clone(secrets), "Authorization", "Bearer", "Cookie", "secret-path", "access_token", "/v1") {
			if strings.Contains(text, word) {
				t.Errorf("%s holds %q:\n%s", what, word, text)
			}
		}
	}
	// What the shell the stand-in is adds to its environment itself.
	shells := map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true}
	kept := map[string]string{}
	for _, entry := range strings.Split(strings.TrimSuffix(string(env), "\n"), "\n") {
		name, value, _ := strings.Cut(entry, "=")
		if !shells[name] {
			kept[name] = value
		}
	}
	want := map[string]string{"PATH": os.Getenv("PATH"), "https_proxy": "http://127.0.0.1:3128", "http_proxy": "http://127.0.0.1:3129", "NO_PROXY": "localhost"}
	for _, name := range []string{"HOME", "TMPDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			want[name] = value
		}
	}
	if !maps.Equal(kept, want) {
		t.Errorf("curl's environment is %q, want %q", kept, want)
	}
}

// recordSlack is how much longer than its bound a record may take: the moment between the bound's
// end and the stop of a curl that never ends, and the reads before the probes.
const recordSlack = 1500 * time.Millisecond

// Check c-tls-timeout-recorded, point 3: a record whose two probes never end ends at its bound,
// the Go probe's error that bound's and curl's figures `-`; a second timeout inside a minute of
// the first writes nothing and runs no curl; and one after a minute writes again.
func TestARecordEndsWithinItsBoundAndWritesOnceInEvery(t *testing.T) {
	rig := newRecordRig(t, "exec sleep 30\n")
	now := time.Date(2026, 11, 27, 21, 30, 0, 0, time.UTC)
	rig.r.Now = func() time.Time { return now }
	host := tlstimeouttest.Silent(t)
	started := time.Now()
	rig.r.Record(host)
	if took := time.Since(started); took > rig.r.Bound+recordSlack {
		t.Errorf("the record took %s, want at most its bound %s and %s more", took, rig.r.Bound, recordSlack)
	}
	_, _, values := parseRecord(t, rig.stderr.String())
	for key, value := range map[string]string{"go_error": "context deadline exceeded", "curl_total_s": "-", "curl_exit": "-"} {
		if values[key] != value {
			t.Errorf("%s=%q, want %q", key, values[key], value)
		}
	}
	written := rig.stderr.String()
	now = now.Add(59 * time.Second)
	started = time.Now()
	rig.r.Record(host)
	if took := time.Since(started); rig.stderr.String() != written || curlRuns(t, rig.curl) != 1 || took > recordSlack {
		t.Errorf("a second timeout 59 s later wrote %q, ran curl %d times in all and took %s; want nothing more, one curl, and no wait",
			strings.TrimPrefix(rig.stderr.String(), written), curlRuns(t, rig.curl), took)
	}
	now = now.Add(2 * time.Second)
	rig.r.Record(host)
	if after := strings.TrimPrefix(rig.stderr.String(), written); !strings.HasPrefix(after, "agent-secrets: tls handshake timeout recorded: host="+host+" ") || curlRuns(t, rig.curl) != 2 {
		t.Errorf("a timeout 61 s after the first wrote %q and ran curl %d times in all, want a line and two", after, curlRuns(t, rig.curl))
	}
}

// A record ends with the context it is made under (RecordContext): one whose context ends while
// both probes wait, the Go probe on a peer that never answers its handshake and a curl that never
// ends, returns at once rather than at its bound, an hour here, and still writes its one line,
// whose figures are what the probes saw by then: the Go probe's error that context's end, and
// curl's figures `-`, since curl was stopped rather than exiting of its own.
func TestARecordEndsWithItsContextAndStillWritesItsLine(t *testing.T) {
	rig := newRecordRig(t, "exec sleep 30\n")
	rig.r.Bound = time.Hour
	host := tlstimeouttest.Silent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan time.Time, 1)
	go func() {
		rig.r.RecordContext(ctx, host, HandshakeTimeout)
		ended <- time.Now()
	}()
	testwait.Eventually(t, "the record's curl to run", func() bool { return curlRuns(t, rig.curl) == 1 })
	cancelled := time.Now()
	cancel()
	if took := testwait.Receive(t, "the record to end after its context ended", ended).Sub(cancelled); took > recordSlack {
		t.Errorf("the record ended %s after its context, want at most %s: its probes did not end with the context, and its bound is %s", took, recordSlack, rig.r.Bound)
	}
	_, _, values := parseRecord(t, rig.stderr.String())
	for key, value := range map[string]string{"host": host, "go_error": "context canceled", "curl_total_s": "-", "curl_exit": "-", "bound_s": "3600.000"} {
		if values[key] != value {
			t.Errorf("%s=%q, want %q", key, values[key], value)
		}
	}
}

// A burst of timeouts writes one line: records made at once, which take the same stamp, write
// one line between them and run one curl.
func TestABurstOfTimeoutsWritesOneLine(t *testing.T) {
	rig := newRecordRig(t, curlTimesOut)
	rig.r.Now = time.Now
	host := testport.Refused(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { rig.r.Record(host) })
	}
	wg.Wait()
	if lines := strings.Count(rig.stderr.String(), "tls handshake timeout recorded"); lines != 1 || curlRuns(t, rig.curl) != 1 {
		t.Errorf("8 records at once wrote %d lines and ran curl %d times, want 1 and 1:\n%s", lines, curlRuns(t, rig.curl), rig.stderr.String())
	}
}

// R11 of dispatch://INFRA-3562 records two events, and the line names which, in the key after the
// host: Record, which the HTTP transports call for net/http's own TLS handshake timeout, and
// RecordContext for that timeout write event=handshake_timeout, and RecordContext for the leak
// guard's own bound stopping its gh read before GitHub sent any answer writes event=guard_bound.
// Every other key of the line is the same for both.
func TestARecordNamesItsEvent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		record func(r *Recorder, host string)
		event  string
	}{
		{name: "Record", record: (*Recorder).Record, event: "handshake_timeout"},
		{name: "a TLS handshake timeout", record: func(r *Recorder, host string) { r.RecordContext(context.Background(), host, HandshakeTimeout) }, event: "handshake_timeout"},
		{name: "the leak guard's own bound", record: func(r *Recorder, host string) { r.RecordContext(context.Background(), host, GuardBound) }, event: "guard_bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newRecordRig(t, curlTimesOut)
			tc.record(rig.r, testport.Refused(t))
			_, keys, values := parseRecord(t, rig.stderr.String())
			if want := childlessKeys(); !slices.Equal(keys, want) {
				t.Errorf("the line's keys are %q, want %q", keys, want)
			}
			if values["event"] != tc.event {
				t.Errorf("event=%q, want %q", values["event"], tc.event)
			}
		})
	}
}

// A record keeps its stamp in a directory of mode 0700 that the user owns, as a file of mode 0600
// named for its client, and refuses a stamp it cannot keep so: a directory that is a link and a
// stamp that is a link each write nothing, run no curl and leave the file the link names as it
// was.
func TestARecordKeepsItsStampPrivateAndRefusesALink(t *testing.T) {
	t.Run("a stamp made", func(t *testing.T) {
		rig := newRecordRig(t, curlTimesOut)
		rig.r.Record(testport.Refused(t))
		// The type and the permission bits alone: a directory made under a TMPDIR whose directory
		// is setgid, as an issue pod's scratch is, takes that bit too.
		for path, mode := range map[string]os.FileMode{rig.r.StampDir: os.ModeDir | 0o700, filepath.Join(rig.r.StampDir, "agent-secrets"): 0o600} {
			info, err := os.Lstat(path)
			if err != nil || info.Mode().Type()|info.Mode().Perm() != mode {
				t.Errorf("%s is %v, %v; want mode %v", path, info.Mode(), err, mode)
			}
		}
	})
	for _, tc := range []struct {
		name string
		// plant makes the stamp directory dir a link, or its stamp, and answers the file the link
		// names.
		plant func(t *testing.T, dir string) string
	}{
		{
			name: "a directory that is a link",
			plant: func(t *testing.T, dir string) string {
				target := t.TempDir()
				if err := os.Chmod(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
				return target
			},
		},
		{
			name: "a stamp that is a link",
			plant: func(t *testing.T, dir string) string {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "elsewhere")
				if err := testexec.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
				if err := os.Chtimes(target, old, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, "agent-secrets")); err != nil {
					t.Fatal(err)
				}
				return target
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRecordRig(t, curlTimesOut)
			target := tc.plant(t, rig.r.StampDir)
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			rig.r.Record(testport.Refused(t))
			if rig.stderr.Len() != 0 || curlRuns(t, rig.curl) != 0 {
				t.Errorf("the record wrote %q and ran curl %d times, want nothing", rig.stderr.String(), curlRuns(t, rig.curl))
			}
			after, err := os.Stat(target)
			if err != nil || !after.ModTime().Equal(before.ModTime()) {
				t.Errorf("the file the link names is %v, %v after the record, want it as it was (%v)", after, err, before.ModTime())
			}
			if entries, err := os.ReadDir(target); err == nil && len(entries) != 0 {
				t.Errorf("the directory the link names holds %v, want nothing", entries)
			}
		})
	}
}

// The line is at most 2048 bytes: an error long enough to pass it is cut, its quote kept whole,
// and every key is still there.
func TestARecordLineIsAtMost2048Bytes(t *testing.T) {
	t.Parallel()
	said := strings.Repeat("é\"", 2000)
	line := recordLine("agent-secrets", "api.github.com", HandshakeTimeout, observed{goProbe: goPhases{said: said}, bound: time.Second, at: time.Now()})
	if len(line) > 2048 {
		t.Errorf("the line is %d bytes, want at most 2048", len(line))
	}
	_, keys, values := parseRecord(t, line)
	if want := childlessKeys(); !slices.Equal(keys, want) {
		t.Errorf("the line's keys are %q, want %q", keys, want)
	}
	if !strings.HasPrefix(said, values["go_error"]) || values["go_error"] == "" {
		t.Errorf("go_error is %q, want the error's start", values["go_error"])
	}
}

// The Go probe keeps the phases of the moment its request ended: a handshake that ended keeps its
// version and curve, one that failed keeps its end and neither, and a callback after the request
// ended, the handshake a canceled request left running, changes nothing.
func TestTheGoProbeKeepsTheHandshakeItSaw(t *testing.T) {
	t.Parallel()
	probe := newGoProbe(time.Now())
	trace := probe.trace()
	trace.ConnectDone("tcp", "127.0.0.1:443", nil)
	trace.TLSHandshakeStart()
	trace.TLSHandshakeDone(tls.ConnectionState{Version: tls.VersionTLS13, CurveID: tls.X25519MLKEM768}, nil)
	phases := probe.end()
	if phases.version != "TLS1.3" || phases.curve != "X25519MLKEM768" || !phases.tlsDone.seen || !phases.connect.seen {
		t.Errorf("the probe kept %+v, want TLS1.3, X25519MLKEM768 and each phase", phases)
	}
	trace.TLSHandshakeDone(tls.ConnectionState{Version: tls.VersionTLS12, CurveID: tls.CurveP256}, nil)
	if again := probe.end(); again.version != "TLS1.3" {
		t.Errorf("a handshake after the request ended changed the probe to %+v", again)
	}
	failed := newGoProbe(time.Now())
	failed.trace().TLSHandshakeStart()
	failed.trace().TLSHandshakeDone(tls.ConnectionState{}, errors.New("remote error: tls: handshake failure"))
	if phases := failed.end(); phases.version != "" || phases.curve != "" || !phases.tlsDone.seen {
		t.Errorf("a failed handshake kept %+v, want its end and no version or curve", phases)
	}
}
