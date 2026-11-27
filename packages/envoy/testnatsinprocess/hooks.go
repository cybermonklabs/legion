//go:build odatrace

package testnatsinprocess

import (
	"testing"

	internal "github.com/sjawhar/envoy/internal/testnats"
)

// Point, Event, Match, Hooks and Hold are internal/testnats's, as hooks.go there documents them.
type (
	Point = internal.Point
	Event = internal.Event
	Match = internal.Match
	Hooks = internal.Hooks
	Hold  = internal.Hold
)

// The points of internal/testnats a test of another module holds or awaits through this package:
// the hold points the daemon's forced-order tests take, and the two shutdown points a test awaits.
const (
	WritePreIO     = internal.WritePreIO
	WritePreRename = internal.WritePreRename
	StoreCreate    = internal.StoreCreate
	APIWorker      = internal.APIWorker
	ClientPublish  = internal.ClientPublish
	ServerShutdown = internal.ServerShutdown
	ServerDown     = internal.ServerDown
)

// Hook is internal/testnats.Hook: it installs the process's hook for t.
func Hook(t testing.TB) *Hooks {
	t.Helper()
	return internal.Hook(t)
}

// OnServer is internal/testnats.OnServer.
func OnServer(s *Server) Match { return internal.OnServer(s) }

// Consumer is internal/testnats.Consumer.
func Consumer(name string) Match { return internal.Consumer(name) }

// Stream is internal/testnats.Stream.
func Stream(name string) Match { return internal.Stream(name) }

// Subject is internal/testnats.Subject.
func Subject(subject string) Match { return internal.Subject(subject) }

// InAccount is internal/testnats.InAccount.
func InAccount(name string) Match { return internal.InAccount(name) }

// ClientNamed is internal/testnats.ClientNamed.
func ClientNamed(name string) Match { return internal.ClientNamed(name) }

// After is internal/testnats.After.
func After(mark int) Match { return internal.After(mark) }

// Where is internal/testnats.Where.
func Where(what string, ok func(Event) bool) Match { return internal.Where(what, ok) }
