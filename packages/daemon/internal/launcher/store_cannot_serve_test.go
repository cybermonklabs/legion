package launcher

// A Postgres server that is starting up or shutting down (SQLSTATE 57P03: a restart, a failover, a
// maintenance window) or that holds all the connections it can take (53300) answers every new
// connection with a refusal of its own and serves none: it does not answer the start's question,
// so a resume whose store answers that way is a store fault the daemon waits out a minute at a
// time, as the daemon's own store reads those classes (sqlstate.NotJudged). The control is a
// password the server refuses (28P01), a configuration fault the daemon charges, which also shows
// that the stand-in's answer reaches the launcher as the server's own (testdb.Refusing).
//
// Each case decides by the server's answer, which its refusal must name, within the one bound the
// lookup has (shortenResumeLookup); no URL sets a bound of its own on a dial. A dial the lookup's
// bound cuts short says nothing the answer before it did not (sessionDial.dial), so a server that
// holds a connection before it answers, past a second but within the lookup's bound
// (heldAnswer), is still read by its answer.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// heldAnswer is the URL of a stand-in that answers each connection's startup with a FATAL code and
// message (testdb.Refusing) once it has held the startup for half the lookup's bound: a server that
// is ready but slow to answer, past the one second a dial was once given.
func heldAnswer(t *testing.T, code, message string) string {
	t.Helper()
	return testdb.Delayed(t, "postgres://legion@"+testdb.Refusing(t, code, message)+"/none?sslmode=disable", resumeLookupTimeout/2)
}

func TestAnOhMyPiResumeWhoseStoreCannotServeAConnectionNowIsAStoreFault(t *testing.T) {
	shortenResumeLookup(t)
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0009.jsonl")
	for name, tc := range map[string]struct {
		code, message string
		storeFault    bool
		// held is whether the server holds each startup before it answers (heldAnswer).
		held bool
	}{
		"the server is starting up (57P03)":                {code: "57P03", message: "the database system is starting up", storeFault: true},
		"the server is shutting down (57P03)":              {code: "57P03", message: "the database system is shutting down", storeFault: true},
		"the server takes no more clients (53300)":         {code: "53300", message: "sorry, too many clients already", storeFault: true},
		"control: the server refuses the password (28P01)": {code: "28P01", message: `password authentication failed for user "legion"`},
		"control: the server refuses the password (28P01) after it held the startup past a second": {code: "28P01",
			message: `password authentication failed for user "legion"`, held: true},
	} {
		t.Run(name, func(t *testing.T) {
			urlFile := filepath.Join(t.TempDir(), "dsn")
			url := "postgres://legion@" + testdb.Refusing(t, tc.code, tc.message) + "/none?sslmode=disable"
			if tc.held {
				url = heldAnswer(t, tc.code, tc.message)
			}
			if err := replaceURLFile(urlFile, url); err != nil {
				t.Fatal(err)
			}
			g := newRig(t)
			marker := filepath.Join(t.TempDir(), "marker")
			command := g.childStart(3, marker, nil)
			command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+urlFile)
			command.ResumeFile = sessionFile
			got := g.start(command)
			want := shimwire.SessionRefusal("")
			if tc.storeFault {
				want = shimwire.SessionStoreUnreachable
			}
			if got.OK || got.Session != want || !strings.Contains(got.Error, tc.message) {
				t.Fatalf("start = %#v, want a refusal naming %q with session refusal %q", got, tc.message, want)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused start started its child")
			}
		})
	}
}

// A store that takes a connection and ends it while it is being opened, before any answer of its
// own, by a reset or by a close (an end of file), as a server that crashes or restarts, or a TCP
// balancer in front of a store that is down, ends it, does not answer the start either: under a
// URL file the launcher's own connection saw the end (ompsessions.ConnectURL), so an Oh My Pi
// resume's lookup and a fresh Claude Code start's keeper are refused as a store fault the daemon
// waits out. The controls are a server that answers 57P03, a store fault by the server's own
// answer, and one that refuses the password (28P01), a configuration fault the daemon charges, at
// once and after it held the startup past a second (heldAnswer): each refusal names the answer.
func TestAStartWhoseStoreEndsTheConnectionWhileItOpensIsAStoreFault(t *testing.T) {
	shortenResumeLookup(t)
	at := func(address string) string { return "postgres://legion@" + address + "/none?sslmode=disable" }
	const starting, refused = "the database system is starting up", `password authentication failed for user "legion"`
	for name, tc := range map[string]struct {
		// url is the store's URL; names, when set, is the server's answer the refusal must name.
		url        func(t *testing.T) string
		names      string
		storeFault bool
	}{
		"the store resets the connection after its startup":       {url: func(t *testing.T) string { return at(testdb.Ending(t, true)) }, storeFault: true},
		"the store closes the connection after its startup (EOF)": {url: func(t *testing.T) string { return at(testdb.Ending(t, false)) }, storeFault: true},
		"control: the server is starting up (57P03)": {names: starting, storeFault: true, url: func(t *testing.T) string {
			return at(testdb.Refusing(t, "57P03", starting))
		}},
		"control: the server refuses the password (28P01)": {names: refused, url: func(t *testing.T) string {
			return at(testdb.Refusing(t, "28P01", refused))
		}},
		"control: the server refuses the password (28P01) after it held the startup past a second": {names: refused,
			url: func(t *testing.T) string { return heldAnswer(t, "28P01", refused) }},
	} {
		want := shimwire.SessionRefusal("")
		if tc.storeFault {
			want = shimwire.SessionStoreUnreachable
		}
		t.Run(name+", an Oh My Pi resume's lookup", func(t *testing.T) {
			urlFile := filepath.Join(t.TempDir(), "dsn")
			if err := replaceURLFile(urlFile, tc.url(t)); err != nil {
				t.Fatal(err)
			}
			g := newRig(t)
			marker := filepath.Join(t.TempDir(), "marker")
			command := g.childStart(3, marker, nil)
			command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+urlFile)
			command.ResumeFile = filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0011.jsonl")
			if got := g.start(command); got.OK || got.Session != want || !strings.Contains(got.Error, tc.names) {
				t.Fatalf("start = %#v, want it refused naming %q with session refusal %q", got, tc.names, want)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused start started its child")
			}
		})
		t.Run(name+", a fresh Claude Code start's keeper", func(t *testing.T) {
			_, dsnFile := testdb.DSNFile(t, "legion_launcher_store_ends_open_test")
			c := claudeRigOn(t, newSessionRig(t, Config{}), dsnFile)
			if err := replaceURLFile(c.dsnFile, tc.url(t)); err != nil {
				t.Fatal(err)
			}
			if got := c.start(c.command(1, seeing(c.transcript, filepath.Join(t.TempDir(), "seen")), false)); got.OK || got.Session != want ||
				!strings.Contains(got.Error, tc.names) {
				t.Fatalf("start = %#v, want it refused naming %q with session refusal %q", got, tc.names, want)
			}
		})
	}
}
