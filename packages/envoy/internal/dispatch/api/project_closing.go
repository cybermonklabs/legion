package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A switch to private first closes the project to developer boxes (access.StartClosing): Dispatch
// revokes every live personal token that lists it, the membership answer says it is closed, and the
// secrets broker mints and renews no developer-box token that lists it. The switch completes only
// once the broker counts no such token still live, and no sooner than developerBoxSettle after the
// closing state began; until then it answers 409 DEVELOPER_BOX_TOKENS_LIVE, and the owner's next
// switch completes it. The closing state is committed whatever the switch answers, so the broker
// reads it and the tokens stay revoked. The owner's switch to public ends it.
//
// The broker's count also records the project and the Slack channels it will hold in the broker's
// store of private projects, and answers the record's number. A switch that does not make the
// project private owes the broker the forget of that record (access.OweBrokerForget), which the
// broker_forgets sender delivers (sendBrokerForgets).

// privateSwitchDeadline is how long a switch to private may run, from before its transaction begins
// to its commit: one full lock wait (patchLockTimeout, 60 s), the broker's count (5 s, the
// agentsecrets client's own timeout), the hubs' admission proof (10 s) and 15 s of writes. The
// broker holds the record a switch's count leaves for as long, so a switch past it can no longer
// commit: the transaction runs on a context that ends at it, and the database ends the transaction
// at it too (transaction_timeout). A variable only so a test lowers it.
var privateSwitchDeadline = 90 * time.Second

// privateSwitchContextDeadline is whether the switch's own context ends at privateSwitchDeadline,
// beside transaction_timeout: always, but for a test that turns it off to show the database's
// deadline holds on its own.
var privateSwitchContextDeadline = true

// developerBoxSettle is how long after its closing state began a switch to private first trusts
// the broker's count of 0: the broker holds Dispatch's answer about a project for at most 30
// seconds (projectaccess.CacheTTL), and stores a developer box's token only while the answer it
// rests on is younger than that, so no token stored more than 30 seconds after the closing state
// began rests on an answer from before it. The other 30 seconds cover the two clocks, the broker's
// and Dispatch's database's, disagreeing. A variable only so a test lowers it.
var developerBoxSettle = time.Minute

// errDeveloperBoxTokensLive is a switch to private while the broker may still hold a developer-box
// token that lists the project: 409 DEVELOPER_BOX_TOKENS_LIVE, naming the count and when the
// closing state began.
var errDeveloperBoxTokensLive = errors.New("the switch to private completes once no developer-box token that lists the project is live")

// errDeveloperBoxTokensUnknown is a switch to private whose count of the broker's developer-box
// tokens could not be read: 503 DEVELOPER_BOX_TOKENS_UNAVAILABLE, naming the cause.
var errDeveloperBoxTokensUnknown = errors.New("the switch to private waits for the secrets broker's count of the developer-box tokens that list the project, and the broker did not answer it")

// errPrivateProjectsNeedBroker is a full switch to private on a deployment with no secrets broker
// (DISPATCH_AGENT_SECRETS_URL unset): 409 PRIVATE_PROJECTS_NEED_BROKER, with nothing changed. With
// no broker to count them, nothing shows that no developer box holds a token of the project.
var errPrivateProjectsNeedBroker = errors.New("a project turns private only once the secrets broker counts no developer-box token that lists it, and this Dispatch names no broker (DISPATCH_AGENT_SECRETS_URL): nothing was changed")

// privateSwitchTimedOut is the 503 PRIVATE_SWITCH_TIMED_OUT message of a switch to private that met
// privateSwitchDeadline, followed by what is known of its outcome. Its owner tries again.
func privateSwitchTimedOut(outcome string) string {
	return fmt.Sprintf("the switch to private did not finish within its %s deadline: %s", privateSwitchDeadline, outcome)
}

// refusalKept is a write's refusal that keeps what the write did before it: ownerWrite commits
// the write's transaction, publishes what it appended, and then answers err. The closing state is
// the one write that refuses so (closeForDeveloperBoxes), and it refuses before any other write
// of the body runs.
type refusalKept struct{ err error }

func (r refusalKept) Error() string { return r.err.Error() }
func (r refusalKept) Unwrap() error { return r.err }

// privateSwitch is what ownerWrite keeps of one full switch to private: the number of the record
// the broker's count left (record, nil until the broker answered one), which the switch owes the
// broker the forget of when it does not commit.
type privateSwitch struct {
	record *int64
}

// closeForDeveloperBoxes is the closing step of project key's switch to private, in the switch's
// transaction tx under the project's owner lock. A deployment with no secrets broker refuses it
// errPrivateProjectsNeedBroker before anything is written. It starts the closing state where the
// project is in none, and answers what that appended. It then asks the broker's count, sending
// channels, the project's Slack channels as the switch will leave them (the body's when it names
// them, nil for the stored ones), and keeps the record number the broker answers in sw. It answers
// nil when the switch may go on: the broker counts no developer-box token of the project live and
// the closing state is developerBoxSettle old. Otherwise it answers a refusalKept of
// errDeveloperBoxTokensLive, or of errDeveloperBoxTokensUnknown where the broker did not answer,
// so the closing state commits, and owes the broker the forget of the record it answered, in tx.
func (s *server) closeForDeveloperBoxes(ctx context.Context, tx pgx.Tx, key string, by model.Actor, channels *[]string, sw *privateSwitch) ([]model.Event, error) {
	if s.deps.AgentSecrets == nil {
		return nil, errPrivateProjectsNeedBroker
	}
	state, err := access.ReadClosing(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	var appended []model.Event
	since, started := state.Since, state.Since == nil
	if started {
		_, event, revoked, err := access.StartClosing(ctx, tx, s.deps.Events, key, by)
		if err != nil {
			return nil, err
		}
		appended = append(appended, event)
		since = &state.Now
		slog.Info("dispatch: a project's switch to private started its closing state, revoking the personal tokens that list it",
			"project", key, "personal_tokens_revoked", revoked)
	}
	held := state.SlackChannels
	if channels != nil {
		held = *channels
	}
	count, err := s.deps.AgentSecrets.DeveloperBoxTokens(ctx, key, held, privateSwitchDeadline)
	if err != nil {
		return appended, refusalKept{fmt.Errorf("%w (%v); %s stays closing and its switch completes on a later request", errDeveloperBoxTokensUnknown, err, key)}
	}
	sw.record = count.Record
	settled := since.Add(developerBoxSettle)
	if count.Live == 0 && !started && !state.Now.Before(settled) {
		return appended, nil
	}
	if count.Record != nil {
		if err := access.OweBrokerForget(ctx, tx, key, count.Record); err != nil {
			return nil, err
		}
	}
	last := ""
	if count.LastExpiresAt != nil {
		last = ", the last expiring at " + count.LastExpiresAt.UTC().Format(time.RFC3339)
	}
	return appended, refusalKept{fmt.Errorf("%s is closing since %s, and the secrets broker holds %d live developer-box tokens that list it%s; %w and no sooner than %s",
		key, since.UTC().Format(time.RFC3339), count.Live, last, errDeveloperBoxTokensLive, settled.UTC().Format(time.RFC3339))}
}

// oweForgetAfter is ownerWrite's forget of a switch to private whose transaction did not commit:
// record N when the broker's count answered it and no commit was sent, the project's record
// whatever its number (nil) when the commit's outcome is unknown. It runs in a short transaction of
// its own, on a context the request's end does not cancel. A forget it cannot write is logged: the
// broker's hold drops the record at its first good read three minutes on.
func (s *server) oweForgetAfter(ctx context.Context, key string, record *int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := access.OweBrokerForget(ctx, s.deps.Store.Pool, key, record); err != nil {
		slog.Error("dispatch: a switch to private that did not commit could not owe the secrets broker its forget",
			"project", key, "error", err)
	}
}
