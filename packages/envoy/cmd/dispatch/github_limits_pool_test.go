package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// rateLimitReadStatement is the read of the limits in force on a scope, as pg_stat_activity shows
// it: the one githubapp.Client makes before every request it sends (ratelimit.Store.InForce,
// whose `scope = any($1)` no other reader of github_rate_limits sends).
const rateLimitReadStatement = "where scope = any($1) and until > now()"

// The delivery reconcile, the delivery intakes and the architecture ticker call GitHub through
// one client of their own, whose shared rate limits (github_rate_limits) it reads before every
// request and records after a limited answer on the background pool: each is a loop no client
// waits on, so none of its GitHub calls may take a connection the requests are served from. They
// called GitHub through the routes' client, whose limits are on the request pool.
//
// This reads the running process, as the webhook sweep's row does: github_rate_limits is held
// under an access exclusive lock from before the process starts, the reconcile's first pass runs
// as the process starts (DISPATCH_TEST_RECONCILE_AT_START) over the delivery settings seeded here,
// and its first GitHub call's read of the limits queues behind the lock, so pg_stat_activity names
// the pool of the connection it waits on. With no NATS the intakes and the webhook sweep do not
// run, and with no architecture source the ticker calls GitHub for nothing, so that read is the
// reconcile's. The three loops take the one client (cmd/dispatch/main.go), so the reconcile's
// pool is theirs.
func TestTheBackgroundLoopsReadTheirGitHubRateLimitsOnTheBackgroundPool(t *testing.T) {
	database := storetest.Open(t)
	seedDeliverySettings(t, database)
	github := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
	gate := holdTableExclusively(t, database, "github_rate_limits")
	env := githubAppEnv(t, github)
	env["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: env})
	application := applicationBehind(t, database, gate, process, rateLimitReadStatement, time.Minute)
	// Released as soon as the name is read, so the pass runs on and the process stops in order.
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release github_rate_limits: %v", err)
	}
	if want := "dispatch-" + store.BackgroundPool; application != want {
		t.Fatalf("the delivery reconcile read its GitHub rate limits on a %s connection, want %s: a loop's GitHub calls belong on the background pool, not on the connections requests are served from", application, want)
	}
}
