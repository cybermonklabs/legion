package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// procNetTCPFixture is /proc/net/tcp as a Linux kernel writes it, with Dispatch listening on
// 127.0.0.1:10101 (0x2775) and 131 (0x83) connections waiting for its accept, another listener on
// 10102 with none, and the two ends of one accepted connection to 10101: the server's end has local
// port 10101 too, and is no LISTEN socket.
const procNetTCPFixture = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:2775 00000000:0000 0A 00000000:00000083 00:00000000 00000000  1000        0 3218795 1 0000000000000000 100 0 0 10 0
   1: 0100007F:2776 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3218799 1 0000000000000000 100 0 0 10 0
   2: 0100007F:2775 0100007F:A2C4 01 00000000:0000002A 00:00000000 00000000  1000        0 3220001 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:A2C4 0100007F:2775 01 00000000:00000000 00:00000000 00000000  1000        0 3220002 1 0000000000000000 20 4 30 10 -1
`

// procNetTCP6Fixture is /proc/net/tcp6 with one LISTEN socket on [::]:8080 (0x1F90).
const procNetTCP6Fixture = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000007 00:00000000 00000000     0        0 18764 1 0000000000000000 100 0 0 10 0
`

// snmpFixture is /proc/net/snmp's Ip and Tcp lines as a Linux kernel writes them.
func snmpFixture(active, passive int64) string {
	return "Ip: Forwarding DefaultTTL InReceives InHdrErrors\nIp: 1 64 914122 0\n" +
		"Tcp: RtoAlgorithm RtoMin RtoMax MaxConn ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs OutSegs RetransSegs InErrs OutRsts InCsumErrors\n" +
		fmt.Sprintf("Tcp: 1 200 120000 -1 %d %d 8396137 31125 24 572373470 584729545 2654807 0 16778200 0\n", active, passive) +
		"Udp: InDatagrams NoPorts\nUdp: 10 0\n"
}

// TestListenQueuesReadsTheListenLineOfThePort: the LISTEN socket on the port gives rx_queue and
// tx_queue; the accepted connection on the same local port, whose rx_queue is unread bytes, and the
// other listener are not counted; a port with no LISTEN socket is not found; tcp6's longer address
// is read by its last colon; a line too short to be a socket is an error.
func TestListenQueuesReadsTheListenLineOfThePort(t *testing.T) {
	for _, c := range []struct {
		text      string
		port      int
		rx, tx    int64
		found     bool
		errSubstr string
	}{
		{procNetTCPFixture, 10101, 131, 0, true, ""},
		{procNetTCPFixture, 10102, 0, 0, true, ""},
		{procNetTCPFixture, 10103, 0, 0, false, ""},
		{procNetTCP6Fixture, 8080, 7, 0, true, ""},
		{procNetTCP6Fixture, 10101, 0, 0, false, ""},
		{procNetTCPFixture + "   4: 0100007F:2775\n", 10101, 0, 0, false, "line 6 has 2 fields"},
	} {
		rx, tx, found, err := listenQueues(c.text, c.port)
		if c.errSubstr != "" {
			if err == nil || !strings.Contains(err.Error(), c.errSubstr) {
				t.Errorf("port %d: error %v, want one naming %q", c.port, err, c.errSubstr)
			}
			continue
		}
		if err != nil || rx != c.rx || tx != c.tx || found != c.found {
			t.Errorf("port %d: rx %d tx %d found %t err %v; want %d, %d, %t", c.port, rx, tx, found, err, c.rx, c.tx, c.found)
		}
	}
}

func TestSnmpOpensReadsTheTcpCounters(t *testing.T) {
	active, passive, err := snmpOpens(snmpFixture(4636740, 1784998))
	if err != nil || active != 4636740 || passive != 1784998 {
		t.Errorf("ActiveOpens %d PassiveOpens %d err %v; want 4636740, 1784998", active, passive, err)
	}
	if _, _, err := snmpOpens("Ip: Forwarding\nIp: 1\n"); err == nil {
		t.Error("a file with no Tcp lines read as counters")
	}
}

// acceptFixture is a sampler over files of its own in dir: /proc/net/tcp, /proc/net/snmp,
// somaxconn 1024 and the kernel's version line.
func acceptFixture(t *testing.T, version string) (*acceptSampler, func(tcp string, active, passive int64)) {
	t.Helper()
	dir := t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	write := func(name, text string) {
		if err := os.WriteFile(path(name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("somaxconn", "1024\n")
	write("version", version)
	a := &acceptSampler{tcpFiles: []string{path("tcp")}, snmpFile: path("snmp"), somaxconnFile: path("somaxconn"), versionFile: path("version")}
	a.prepare("http://127.0.0.1:10101")
	return a, func(tcp string, active, passive int64) {
		write("tcp", tcp)
		write("snmp", snmpFixture(active, passive))
	}
}

// withBacklog is procNetTCPFixture with rx connections waiting on 10101.
func withBacklog(rx int) string {
	return strings.Replace(procNetTCPFixture, "0A 00000000:00000083", fmt.Sprintf("0A 00000000:%08X", rx), 1)
}

// TestAcceptSamplerSummarizesTheWindow: one reading a second, the warm-up's left out of the
// summary: the window's highest backlog against somaxconn's limit, the readings above half of it,
// and the most ActiveOpens in one second, a burst of 3,540 after a quiet second of 30.
func TestAcceptSamplerSummarizesTheWindow(t *testing.T) {
	a, set := acceptFixture(t, "Linux version 6.1.0-28-cloud-amd64 (debian-kernel@lists.debian.org)\n")
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	winStart, winEnd := at.Add(time.Second), at.Add(5*time.Second)
	opens := int64(1_000_000)
	for i, c := range []struct {
		rx       int
		newOpens int64
	}{{900, 0}, {100, 30}, {600, 3540}, {700, 400}, {12, 20}} {
		opens += c.newOpens
		set(withBacklog(c.rx), opens, opens/2)
		if err := a.read(at.Add(time.Duration(i)*time.Second), winStart, winEnd); err != nil {
			t.Fatal(err)
		}
	}
	s := a.summary()
	if s.Port != 10101 || s.Readings != 4 || *s.BacklogMax != 700 || *s.BacklogLimit != 1024 || *s.SecondsAboveHalf != 2 {
		t.Errorf("summary %s; want port 10101, 4 readings, max 700 of 1,024, 2 s above half (the warm-up's 900 left out)", s.text())
	}
	if *s.ActiveOpensMaxPerS != 3540 || s.BacklogError != "" || s.OpensError != "" {
		t.Errorf("ActiveOpens max %v/s, errors %q %q; want 3,540 and none", *s.ActiveOpensMaxPerS, s.BacklogError, s.OpensError)
	}
	if got := a.file(); got.Limit == nil || *got.Limit != 1024 || len(got.Readings) != 5 || *got.Readings[0].RxQueue != 900 || got.Readings[0].InWindow {
		t.Errorf("accept-backlog.json %+v; want limit 1,024 and 5 readings, the first the warm-up's 900", got)
	}
}

// TestAcceptSamplerSaysWhyItReadNoBacklog: on a gVisor kernel the queue is not read at all, since
// its LISTEN lines read 0 whatever waits, and the summary says so rather than holding a measured
// 0, while the opens are still read; on a box where the port has no LISTEN socket, each reading
// says so and so does the summary; a table shows - for either.
func TestAcceptSamplerSaysWhyItReadNoBacklog(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	winStart, winEnd := at, at.Add(time.Hour)
	g, set := acceptFixture(t, "Linux version 4.19.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016\n")
	for i := range 3 {
		set(withBacklog(0), int64(100+40*i), 10)
		if err := g.read(at.Add(time.Duration(i)*time.Second), winStart, winEnd); err != nil {
			t.Fatal(err)
		}
	}
	s := g.summary()
	if s.BacklogMax != nil || s.SecondsAboveHalf != nil || !strings.Contains(s.BacklogError, "this kernel is gVisor") {
		t.Errorf("gVisor: backlog max %v, above half %v, error %q; want null, null and the gVisor reason", s.BacklogMax, s.SecondsAboveHalf, s.BacklogError)
	}
	if s.ActiveOpensMaxPerS == nil || *s.ActiveOpensMaxPerS != 40 {
		t.Errorf("gVisor: ActiveOpens max %v, want 40/s: the opens are read all the same", s.ActiveOpensMaxPerS)
	}
	if g.file().Readings[0].RxQueue != nil {
		t.Error("gVisor: a reading holds an rx_queue")
	}

	n, set := acceptFixture(t, "Linux version 6.1.0\n")
	set(strings.ReplaceAll(procNetTCPFixture, ":2775", ":2777"), 100, 10)
	if err := n.read(at, winStart, winEnd); err == nil || !strings.Contains(err.Error(), "no LISTEN socket on port 10101") {
		t.Errorf("a box with no listener on the port read %v, want no LISTEN socket on port 10101", err)
	}
	s = n.summary()
	if s.BacklogMax != nil || !strings.Contains(s.BacklogError, "no LISTEN socket on port 10101") {
		t.Errorf("no listener: backlog max %v, error %q", s.BacklogMax, s.BacklogError)
	}
	md := renderTables([]stepData{{name: "gvisor", sum: summary{Config: summaryConfig{Step: "gvisor"}, Accept: &s}}})
	pace := section(md, "Generator pacing and accept backlog")
	if !strings.Contains(pace, "| gvisor | - | - | - | - | - | - | - | - |") || !strings.Contains(pace, "- gvisor: no reading in the window read the queue") {
		t.Errorf("the table shows a backlog it did not read, or not why:\n%s", pace)
	}
}
