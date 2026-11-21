// Package readclass names what each read the server mounts reads, and so which check stands
// between a caller and its rows. A pattern is mounted through Handle, which takes its classes as an
// argument, so no mount compiles without saying them, and which records every pattern it mounted
// on each mux, so a walk of what the server serves (Patterns) reads the mounts themselves and no
// list kept beside them.
//
// The API route table's rows are classed in the api package (read_classes.go), whose register
// refuses a GET row with no class; every other mount goes through Handle.
// TestEveryMountedPatternHasAReadClass refuses a direct mux.Handle or mux.HandleFunc anywhere in
// the server's source but the two sites it names.
package readclass

import (
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"sync"
	"weak"
)

// Class is one kind of row a read answers, which names the check that decides what of it a
// caller may see. A read of several kinds of row carries each of their classes.
type Class string

const (
	// None reads no row a caller's access decides: the server's own facts, or its route index.
	None Class = "none"
	// Caller reads the caller's own rows, keyed on the caller's credential or person.
	Caller Class = "caller"
	// Project reads rows that belong to a project, each held to access.ProjectClause, or, for a
	// conversation that belongs to none, to access.PartyClause.
	Project Class = "project"
	// Graph reads the node and tuple store, through refs and its visibility registry.
	Graph Class = "graph"
	// People reads people, held to access.PersonClause.
	People Class = "people"
	// Agents reads agent sessions, held to access.SessionClause.
	Agents Class = "agents"
	// Deployment reads rows of the whole deployment, which no restricted member, nor a credential
	// acting for one, may read.
	Deployment Class = "deployment"
	// AccessAccount is what the secrets broker reads about the deployment's projects, read only by
	// a service account DISPATCH_ACCESS_ACCOUNTS names.
	AccessAccount Class = "accessAccount"
	// TestHook is a test hook, mounted only for a deployment that turns them on.
	TestHook Class = "testHook"
)

// Classes is every class, in the order this file declares them.
var Classes = []Class{None, Caller, Project, Graph, People, Agents, Deployment, AccessAccount, TestHook}

// Handle mounts h on mux at pattern, as mux.Handle does, and records the pattern as one mux
// serves, with its classes. classes is what the read reads: at least one, each one of Classes, or
// Handle panics before it mounts anything, as mux.Handle panics on a pattern it cannot serve.
func Handle(mux *http.ServeMux, classes []Class, pattern string, h http.Handler) {
	if len(classes) == 0 {
		panic(fmt.Sprintf("readclass.Handle: %s is mounted with no read class", pattern))
	}
	for _, class := range classes {
		if !slices.Contains(Classes, class) {
			panic(fmt.Sprintf("readclass.Handle: %s is mounted with the read class %q, which is none of readclass.Classes", pattern, class))
		}
	}
	mux.Handle(pattern, h)
	record(mux, mount{pattern: pattern, classes: slices.Clone(classes)})
}

// mount is one pattern Handle mounted and the classes it was mounted with.
type mount struct {
	pattern string
	classes []Class
}

// mounted is every pattern Handle mounted, by the mux it mounted it on. The key is a weak
// pointer, so the record holds no mux alive: the cleanup record adds removes a mux's entry once
// the mux is collected, and the many short-lived muxes of a test run leave nothing behind.
var (
	mountedMu sync.Mutex
	mounted   = map[weak.Pointer[http.ServeMux]][]mount{}
)

func record(mux *http.ServeMux, m mount) {
	key := weak.Make(mux)
	mountedMu.Lock()
	defer mountedMu.Unlock()
	mounts, known := mounted[key]
	if !known {
		runtime.AddCleanup(mux, forget, key)
	}
	mounted[key] = append(mounts, m)
}

func forget(key weak.Pointer[http.ServeMux]) {
	mountedMu.Lock()
	defer mountedMu.Unlock()
	delete(mounted, key)
}

// Patterns answers every pattern Handle mounted on mux, sorted, and none for a mux it mounted
// nothing on.
func Patterns(mux *http.ServeMux) []string {
	mountedMu.Lock()
	patterns := patternsOf(mounted[weak.Make(mux)])
	mountedMu.Unlock()
	slices.Sort(patterns)
	return patterns
}

// ClassesOf answers the classes Handle mounted pattern with on mux, and none for a pattern it did
// not mount there.
func ClassesOf(mux *http.ServeMux, pattern string) []Class {
	mountedMu.Lock()
	defer mountedMu.Unlock()
	for _, m := range mounted[weak.Make(mux)] {
		if m.pattern == pattern {
			return slices.Clone(m.classes)
		}
	}
	return nil
}

// AllPatterns answers every pattern Handle mounted on any mux of this process, each once, sorted:
// the router's own routes, both sockets, and the process's probes alike, which a walk over every
// route the process serves requests.
func AllPatterns() []string {
	mountedMu.Lock()
	var patterns []string
	for _, mounts := range mounted {
		patterns = append(patterns, patternsOf(mounts)...)
	}
	mountedMu.Unlock()
	slices.Sort(patterns)
	return slices.Compact(patterns)
}

// patternsOf is the pattern of each of mounts, in their order.
func patternsOf(mounts []mount) []string {
	patterns := make([]string, len(mounts))
	for i, m := range mounts {
		patterns[i] = m.pattern
	}
	return patterns
}

// recordedMuxes is how many muxes the record holds patterns of.
func recordedMuxes() int {
	mountedMu.Lock()
	defer mountedMu.Unlock()
	return len(mounted)
}
