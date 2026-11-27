// Package readclass names what each read the server mounts reads, and so which check stands
// between a caller and its rows. A pattern beside the API route table is mounted through Handle,
// which takes its classes as an argument, so no such mount compiles without saying them.
//
// The API route table's rows are classed in the api package (readSources, read_sources.go), whose
// register refuses a GET row with no class; every other mount goes through Handle. Handle keeps
// no record of its own: a test hands the server's mounting code a mux of its own that is a
// Recorder, and reads back what was mounted on it and with which classes
// (TestEveryMountedPatternHasAReadClass here, and the routes package's
// TestEveryGETRouteHasAReadClass).
package readclass

import (
	"fmt"
	"net/http"
	"slices"
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

// Mux is what Handle mounts on: an *http.ServeMux in a server, or the mux a test hands the
// server's mounting code (api.RouteMux), which keeps what is mounted on it.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Recorder is a Mux that keeps the classes of each pattern Handle mounts on it. Its owner, a test,
// hands it to the server's mounting code and reads back which patterns were mounted with which
// classes, and which were mounted on it directly with none. A server's own mux is no Recorder, so
// Handle records nothing for it.
type Recorder interface {
	Mux
	// Record is called once Handle has mounted pattern on the Recorder, with the classes it was
	// mounted with, a copy the Recorder may keep.
	Record(pattern string, classes []Class)
}

// Handle mounts h on mux at pattern, as mux.Handle does, and when mux is a Recorder records the
// pattern's classes on it. classes is what the read reads: at least one, each one of Classes, or
// Handle panics before it mounts anything, as mux.Handle panics on a pattern it cannot serve.
func Handle(mux Mux, classes []Class, pattern string, h http.Handler) {
	if len(classes) == 0 {
		panic(fmt.Sprintf("readclass.Handle: %s is mounted with no read class", pattern))
	}
	for _, class := range classes {
		if !slices.Contains(Classes, class) {
			panic(fmt.Sprintf("readclass.Handle: %s is mounted with the read class %q, which is none of readclass.Classes", pattern, class))
		}
	}
	mux.Handle(pattern, h)
	if recorder, ok := mux.(Recorder); ok {
		recorder.Record(pattern, slices.Clone(classes))
	}
}
