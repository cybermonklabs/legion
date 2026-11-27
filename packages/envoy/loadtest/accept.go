package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// acceptEvery is the accept-backlog sampler's interval: one reading a second.
const acceptEvery = time.Second

// The files the accept-backlog sampler reads. Each Linux kernel publishes them; gVisor publishes
// the first three too, but reads every LISTEN socket's queue as 0 (gvisorKernel).
const (
	procNetTCP    = "/proc/net/tcp"
	procNetTCP6   = "/proc/net/tcp6"
	procNetSNMP   = "/proc/net/snmp"
	procSomaxconn = "/proc/sys/net/core/somaxconn"
	procVersion   = "/proc/version"
)

// acceptSampler reads, once a second, the queue of connections that wait for Dispatch's API
// listener to accept them, and the box's TCP opens.
//
// The queue is the rx_queue of the LISTEN socket (state 0A) whose local port is Dispatch's, in
// /proc/net/tcp and /proc/net/tcp6: for a listening socket the kernel prints sk_ack_backlog there,
// the connections whose handshake completed and that no accept has taken yet. Its limit is not on
// that line: a LISTEN socket's tx_queue is write_seq - snd_una, the bytes it has not had
// acknowledged, which is 0 for a socket that sends nothing; the limit (sk_max_ack_backlog) is what
// `ss` shows as Send-Q and /proc/net/tcp does not. Go listens with the backlog
// /proc/sys/net/core/somaxconn holds, and the kernel caps every listen at that value, so the limit
// is read from there; tx_queue is kept in each reading as read.
//
// /proc/net/snmp's Tcp ActiveOpens and PassiveOpens are the box's connections opened and accepted
// since boot; their rise per second is how the failing 10x step showed its generator's burst.
type acceptSampler struct {
	port                                 int
	tcpFiles                             []string
	snmpFile, somaxconnFile, versionFile string
	// backlogErr is why no reading reads the queue, set before the first one: a Dispatch URL with
	// no port, or a kernel that does not publish the queue (gVisor).
	backlogErr string
	limit      *int64
	limitErr   string
	readings   []acceptReading
	failed     int
}

// acceptReading is one reading of accept-backlog.json. RxQueue and TxQueue are the LISTEN
// socket's two queue fields, summed over every LISTEN socket on the port; ActiveOpens and
// PassiveOpens /proc/net/snmp's counters, and each PerS its rise since the reading before,
// divided by the seconds between the two. A value the reading did not get is null, and Error
// says why.
type acceptReading struct {
	At               time.Time `json:"at"`
	InWindow         bool      `json:"in_window"`
	RxQueue          *int64    `json:"rx_queue"`
	TxQueue          *int64    `json:"tx_queue"`
	ActiveOpens      *int64    `json:"active_opens"`
	PassiveOpens     *int64    `json:"passive_opens"`
	ActiveOpensPerS  *float64  `json:"active_opens_per_s"`
	PassiveOpensPerS *float64  `json:"passive_opens_per_s"`
	Error            string    `json:"error,omitempty"`
}

// acceptFile is accept-backlog.json: the port read, the queue's limit and where it came from, why
// the queue was not read at all when it was not, and the readings from the step's start to the
// window's end.
type acceptFile struct {
	Port         int             `json:"port"`
	Limit        *int64          `json:"limit"`
	LimitSource  string          `json:"limit_source"`
	LimitError   string          `json:"limit_error,omitempty"`
	BacklogError string          `json:"backlog_error,omitempty"`
	Readings     []acceptReading `json:"readings"`
}

// acceptSummary is summary.json's accept: the window's readings of the backlog and of the opens.
// A value no in-window reading gave is null, and BacklogError, LimitError or OpensError says why,
// so a backlog that was not read never reads as an empty queue.
type acceptSummary struct {
	Port     int `json:"port"`
	Readings int `json:"readings"`
	// BacklogMax is the highest rx_queue of the window, BacklogLimit the queue's limit and
	// SecondsAboveHalf the in-window readings, one a second, whose rx_queue was above half of it.
	BacklogMax          *int64   `json:"backlog_max"`
	BacklogLimit        *int64   `json:"backlog_limit"`
	SecondsAboveHalf    *int     `json:"seconds_above_half"`
	BacklogError        string   `json:"backlog_error,omitempty"`
	LimitError          string   `json:"limit_error,omitempty"`
	ActiveOpensMaxPerS  *float64 `json:"active_opens_max_per_s"`
	PassiveOpensMaxPerS *float64 `json:"passive_opens_max_per_s"`
	OpensError          string   `json:"opens_error,omitempty"`
}

// newAcceptSampler is the sampler of the API listener that dispatchURL names, on this box's
// files. The queue is not read when the URL names no port, or on a gVisor kernel.
func newAcceptSampler(dispatchURL string) *acceptSampler {
	a := &acceptSampler{tcpFiles: []string{procNetTCP, procNetTCP6}, snmpFile: procNetSNMP,
		somaxconnFile: procSomaxconn, versionFile: procVersion}
	a.prepare(dispatchURL)
	return a
}

// prepare takes the port from dispatchURL, the limit from somaxconnFile, and decides whether the
// queue can be read at all.
func (a *acceptSampler) prepare(dispatchURL string) {
	if v, err := readInt(a.somaxconnFile); err != nil {
		a.limitErr = err.Error()
	} else {
		a.limit = &v
	}
	u, err := url.Parse(dispatchURL)
	if err == nil {
		a.port, err = strconv.Atoi(u.Port())
	}
	if err != nil || a.port <= 0 {
		a.backlogErr = fmt.Sprintf("the Dispatch URL %s names no port to find its LISTEN socket by", redactURL(dispatchURL))
		return
	}
	if why, ok := gvisorKernel(a.versionFile); ok {
		a.backlogErr = why
	}
}

// gvisorKernel says why the queue cannot be read when versionFile names a gVisor kernel. gVisor's
// /proc/net/tcp reads every LISTEN socket's rx_queue and tx_queue as 0 and lists no connection
// the server has not accepted (dispatch://INFRA-592, round 41: a listener that never accepts,
// dialled 1,500 times, read 00000000:00000000), so its 0 is no measurement.
func gvisorKernel(versionFile string) (string, bool) {
	b, err := os.ReadFile(versionFile)
	if err != nil || !strings.Contains(strings.ToLower(string(b)), "gvisor") {
		return "", false
	}
	return fmt.Sprintf("this kernel is gVisor (%s: %q), whose /proc/net/tcp reads every LISTEN socket's queue as 0 and lists no connection before its accept, so the accept backlog is not measured here",
		versionFile, strings.TrimSpace(string(b))), true
}

// readInt reads a file that holds one integer.
func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// loop reads at from, then every acceptEvery until winEnd, as poll does; a failed reading is
// counted and the first three are logged.
func (a *acceptSampler) loop(ctx context.Context, from, winStart, winEnd time.Time, lg *log.Logger) {
	if a.backlogErr != "" {
		lg.Printf("accept backlog: not read: %s", a.backlogErr)
	}
	a.failed = poll(ctx, from, winEnd, acceptEvery, "accept backlog", lg, func(context.Context) error {
		return a.read(time.Now(), winStart, winEnd)
	})
}

// read takes one reading at now.
func (a *acceptSampler) read(now, winStart, winEnd time.Time) error {
	r := acceptReading{At: now, InWindow: inWindow(now, winStart, winEnd)}
	var errs []error
	if a.backlogErr == "" {
		if rx, tx, err := a.listenQueues(); err != nil {
			errs = append(errs, err)
		} else {
			r.RxQueue, r.TxQueue = &rx, &tx
		}
	}
	if b, err := os.ReadFile(a.snmpFile); err != nil {
		errs = append(errs, err)
	} else if active, passive, err := snmpOpens(string(b)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", a.snmpFile, err))
	} else {
		r.ActiveOpens, r.PassiveOpens = &active, &passive
		if prev := a.lastOpens(); prev != nil {
			if dt := now.Sub(prev.At).Seconds(); dt > 0 {
				ap, pp := float64(active-*prev.ActiveOpens)/dt, float64(passive-*prev.PassiveOpens)/dt
				r.ActiveOpensPerS, r.PassiveOpensPerS = &ap, &pp
			}
		}
	}
	err := errors.Join(errs...)
	if err != nil {
		r.Error = err.Error()
	}
	a.readings = append(a.readings, r)
	return err
}

// lastOpens is the latest reading that read the opens, nil before the first.
func (a *acceptSampler) lastOpens() *acceptReading {
	for i := len(a.readings) - 1; i >= 0; i-- {
		if a.readings[i].ActiveOpens != nil {
			return &a.readings[i]
		}
	}
	return nil
}

// listenQueues sums the queues of the port's LISTEN sockets over tcpFiles; an error when no file
// could be read or none holds such a socket.
func (a *acceptSampler) listenQueues() (rx, tx int64, err error) {
	read, found := 0, false
	var errs []error
	for _, f := range a.tcpFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		read++
		r, t, ok, err := listenQueues(string(b), a.port)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		if ok {
			rx, tx, found = rx+r, tx+t, true
		}
	}
	switch {
	case read == 0:
		return 0, 0, errors.Join(errs...)
	case !found:
		errs = append(errs, fmt.Errorf("no LISTEN socket on port %d in %s", a.port, strings.Join(a.tcpFiles, " or ")))
		return 0, 0, errors.Join(errs...)
	}
	return rx, tx, nil
}

// listenQueues reads one /proc/net/tcp or /proc/net/tcp6 text: the summed rx_queue and tx_queue
// of its LISTEN sockets (state 0A) whose local port is port, and whether it holds one. A line is
// "sl: local_address rem_address st tx_queue:rx_queue ...", every number in hex, the local port
// after the last colon of local_address (tcp6 writes the address as 32 hex digits).
func listenQueues(text string, port int) (rx, tx int64, found bool, err error) {
	for i, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "sl" {
			continue
		}
		if len(fields) < 5 {
			return 0, 0, false, fmt.Errorf("line %d has %d fields, not the 5 or more of a socket: %q", i+1, len(fields), line)
		}
		if fields[3] != "0A" {
			continue
		}
		local := fields[1]
		p, err := strconv.ParseUint(local[strings.LastIndexByte(local, ':')+1:], 16, 16)
		if err != nil {
			return 0, 0, false, fmt.Errorf("line %d: local address %q: %w", i+1, local, err)
		}
		if int(p) != port {
			continue
		}
		txText, rxText, ok := strings.Cut(fields[4], ":")
		t, terr := strconv.ParseInt(txText, 16, 64)
		r, rerr := strconv.ParseInt(rxText, 16, 64)
		if !ok || terr != nil || rerr != nil {
			return 0, 0, false, fmt.Errorf("line %d: queues %q are not tx_queue:rx_queue in hex", i+1, fields[4])
		}
		rx, tx, found = rx+r, tx+t, true
	}
	return rx, tx, found, nil
}

// snmpOpens reads /proc/net/snmp's Tcp ActiveOpens and PassiveOpens: the first "Tcp:" line names
// the counters and the second holds their values.
func snmpOpens(text string) (active, passive int64, err error) {
	var names []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Tcp:" {
			continue
		}
		if names == nil {
			names = fields[1:]
			continue
		}
		values := fields[1:]
		if len(values) != len(names) {
			return 0, 0, fmt.Errorf("the Tcp lines hold %d names and %d values", len(names), len(values))
		}
		got := 0
		for i, n := range names {
			var dst *int64
			switch n {
			case "ActiveOpens":
				dst = &active
			case "PassiveOpens":
				dst = &passive
			default:
				continue
			}
			if *dst, err = strconv.ParseInt(values[i], 10, 64); err != nil {
				return 0, 0, fmt.Errorf("Tcp %s: %w", n, err)
			}
			got++
		}
		if got != 2 {
			return 0, 0, fmt.Errorf("the Tcp lines name no ActiveOpens or no PassiveOpens")
		}
		return active, passive, nil
	}
	return 0, 0, fmt.Errorf("no Tcp: counters")
}

// file is accept-backlog.json.
func (a *acceptSampler) file() acceptFile {
	return acceptFile{Port: a.port, Limit: a.limit, LimitSource: a.somaxconnFile, LimitError: a.limitErr,
		BacklogError: a.backlogErr, Readings: a.readings}
}

// summary reduces the in-window readings.
func (a *acceptSampler) summary() acceptSummary {
	s := acceptSummary{Port: a.port, BacklogLimit: a.limit, LimitError: a.limitErr}
	var backlogWhy, opensWhy string
	above := 0
	for _, r := range a.readings {
		if !r.InWindow {
			continue
		}
		s.Readings++
		if r.RxQueue != nil {
			if s.BacklogMax == nil || *r.RxQueue > *s.BacklogMax {
				s.BacklogMax = r.RxQueue
			}
			if a.limit != nil && 2**r.RxQueue > *a.limit {
				above++
			}
		} else if backlogWhy == "" {
			backlogWhy = r.Error
		}
		if r.ActiveOpensPerS != nil {
			if s.ActiveOpensMaxPerS == nil || *r.ActiveOpensPerS > *s.ActiveOpensMaxPerS {
				s.ActiveOpensMaxPerS = r.ActiveOpensPerS
			}
			if s.PassiveOpensMaxPerS == nil || *r.PassiveOpensPerS > *s.PassiveOpensMaxPerS {
				s.PassiveOpensMaxPerS = r.PassiveOpensPerS
			}
		} else if opensWhy == "" && r.Error != "" {
			opensWhy = r.Error
		}
	}
	if s.BacklogMax != nil && a.limit != nil {
		s.SecondsAboveHalf = &above
	}
	switch {
	case a.backlogErr != "":
		s.BacklogError = a.backlogErr
	case s.BacklogMax == nil && backlogWhy != "":
		s.BacklogError = "no reading in the window read the queue: " + backlogWhy
	case s.BacklogMax == nil:
		s.BacklogError = "no reading in the window"
	}
	switch {
	case s.ActiveOpensMaxPerS == nil && opensWhy != "":
		s.OpensError = "no two readings in the window read the opens: " + opensWhy
	case s.ActiveOpensMaxPerS == nil:
		s.OpensError = "fewer than two readings of the opens"
	}
	return s
}

// optInt64 is *v, "-" when v is nil.
func optInt64(v *int64) string {
	if v == nil {
		return "-"
	}
	return num(float64(*v), 0)
}

// backlogCell is "max / limit" of the window's backlog, "-" when it was not read; a summary from
// before the accept reading (nil) is "-" too.
func (s *acceptSummary) backlogCell() string {
	if s == nil || s.BacklogMax == nil {
		return "-"
	}
	return optInt64(s.BacklogMax) + " / " + optInt64(s.BacklogLimit)
}

// aboveHalfCell is the window's readings above half the limit, "-" when either was not read.
func (s *acceptSummary) aboveHalfCell() string {
	if s == nil || s.SecondsAboveHalf == nil {
		return "-"
	}
	return num(float64(*s.SecondsAboveHalf), 0)
}

// opensCell is the window's most ActiveOpens and PassiveOpens in a second, "active / passive".
func (s *acceptSummary) opensCell() string {
	if s == nil || s.ActiveOpensMaxPerS == nil {
		return "-"
	}
	return num(*s.ActiveOpensMaxPerS, 0) + " / " + num(*s.PassiveOpensMaxPerS, 0)
}

// text is the window's accept reading as run.log writes it, with why a part of it was not read.
func (s *acceptSummary) text() string {
	t := fmt.Sprintf("port %d, %d readings in the window; backlog max / limit %s, %s s above half the limit; opens max/s active / passive %s",
		s.Port, s.Readings, s.backlogCell(), s.aboveHalfCell(), s.opensCell())
	for _, why := range []string{s.BacklogError, s.LimitError, s.OpensError} {
		if why != "" {
			t += "; " + why
		}
	}
	return t
}
