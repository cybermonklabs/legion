// packages/envoy/internal/cipher/requests/private_list_fallback_test.go
// The broker's stored private list (store/known_private_projects.go) as the request machine reads
// it: the public fence's fallback when a read of Dispatch's list of private projects fails, the
// keeping of every fresh read, and the cut of a limited grant's channels to the ones it holds. Each
// test runs against Postgres (storetest.Open) and a Dispatch stand-in, and moves the database's
// time with the store's DatabaseClockOffset where a row turns on the 3-minute hold or the settle.
package requests

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// The projects and channels of the fallback's rows: SWITCH, a project an owner switched to private
// in a switch that did not commit, and FORMER, a project that was private and is public again.
const (
	switchProject = "SWITCH"
	formerProject = "FORMER"
	switchChannel = "C0SWITCH002"
	formerChannel = "C0FORMER003"
	addedChannel  = "C0ADDED0002"
)

// storeAged keeps every private row of rows, with its channels (unknown for a row with
// OmitSlackChannels), in st's stored private list as a read made more than slackChannelSettle ago
// did, so a limited grant waits no settle for them: what a broker that has read the list for a
// while holds. It moves the database's time back for the one read and forward again after.
func storeAged(t *testing.T, st *store.Store, rows map[string]projectaccesstest.Row) {
	t.Helper()
	var listed []store.KnownPrivateProject
	for key, row := range rows {
		if row.Visibility != "private" {
			continue
		}
		project := store.KnownPrivateProject{Key: key}
		if !row.OmitSlackChannels {
			project.SlackChannels = slices.Concat([]string{}, row.SlackChannels)
		}
		listed = append(listed, project)
	}
	back := slackChannelSettle + time.Minute
	st.DatabaseClockOffset.Set(st.DatabaseClockOffset.Get() - back)
	defer st.DatabaseClockOffset.Set(st.DatabaseClockOffset.Get() + back)
	storeList(t, st, listed...)
}

// storeUnknown keeps key in st's stored private list as a read named it with no slack_channels
// key: a held row of unknown channels, which the fallback cannot fence by.
func storeUnknown(t *testing.T, st *store.Store, key string) {
	t.Helper()
	storeList(t, st, store.KnownPrivateProject{Key: key})
}

// storeList keeps listed in st's stored private list as one read of Dispatch's list, stamped now.
func storeList(t *testing.T, st *store.Store, listed ...store.KnownPrivateProject) {
	t.Helper()
	stamp, err := st.StampPrivateList(context.Background())
	if err != nil {
		t.Fatalf("StampPrivateList: %v", err)
	}
	if _, err := st.KeepPrivateProjects(context.Background(), stamp, listed); err != nil {
		t.Fatalf("KeepPrivateProjects(%v): %v", listed, err)
	}
}

// storedRow is key's held row in st's stored private list, and whether it is held.
func storedRow(t *testing.T, st *store.Store, key string) (store.KnownPrivateProject, bool) {
	t.Helper()
	known, err := st.KnownPrivateProjects(context.Background())
	if err != nil {
		t.Fatalf("KnownPrivateProjects: %v", err)
	}
	for _, project := range known {
		if project.Key == key {
			return project, true
		}
	}
	return store.KnownPrivateProject{}, false
}

// storedRows is how many rows st's stored private list holds, held or not.
func storedRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), `select count(*) from known_private_projects`).Scan(&n); err != nil {
		t.Fatalf("count known_private_projects: %v", err)
	}
	return n
}

// freshClient points m at dispatch through a client of its own, whose cache holds nothing, as
// m's Projects and its PrivateList both, as cipherd wires one client.
func freshClient(m *Machine, dispatch *projectaccesstest.Server) {
	client := projectaccess.New(dispatch.URL, dispatch.Account, nil)
	m.Projects, m.PrivateList = client, client
}

// podOf is another pod of m's broker: the same policy, secrets and airlock, over st, which shares
// m's database, with a client of dispatch whose cache holds nothing.
func podOf(m *Machine, st *store.Store, dispatch *projectaccesstest.Server) *Machine {
	pod := &Machine{
		Store: st, Policy: m.Policy, Secrets: m.Secrets, PendingTTL: m.PendingTTL,
		Audience: m.Audience, Skew: m.Skew, Replay: m.Replay, ServicesByAccount: m.ServicesByAccount,
		Chain: m.Chain, Airlock: m.Airlock, AirlockProxyURL: m.AirlockProxyURL, AirlockCACertificate: m.AirlockCACertificate,
	}
	freshClient(pod, dispatch)
	return pod
}

// releaseSlack reads grantID's values on m, as the proxy does, and answers the Slack token's value
// and the time before the read.
func releaseSlack(t *testing.T, m *Machine, grantID string) (AirlockSecret, time.Time) {
	t.Helper()
	read := time.Now()
	values, _, err := m.AirlockValues(t.Context(), grantID)
	if err != nil {
		t.Fatalf("AirlockValues(%s): %v", slackToken, err)
	}
	return values[slackToken], read
}

// expiresWithin fails t unless got expires at the read time plus bound.
func expiresWithin(t *testing.T, what string, got AirlockSecret, read time.Time, bound time.Duration) {
	t.Helper()
	if got.Expires.Before(read.Add(bound)) || got.Expires.After(time.Now().Add(bound)) {
		t.Fatalf("%s: %s expires %v; want the read time plus %v", what, slackToken, got.Expires, bound)
	}
}

// todays fails t unless got is today's answer to a credential that names no private project: no
// Excluded, not unread, and the release's ten-minute bound (R9).
func todays(t *testing.T, row string, got AirlockSecret, read time.Time) {
	t.Helper()
	if len(got.Excluded) != 0 || got.ExcludedUnread || got.Limited {
		t.Fatalf("%s (an R9 row): %s = Excluded %#v ExcludedUnread %v Limited %v; want today's answer: no fence and no refusal", row, slackToken, got.Excluded, got.ExcludedUnread, got.Limited)
	}
	expiresWithin(t, row, got, read, airlockProjectRecheck)
}

// fencedBy fails t unless got is the stored fence of channels, read again within a minute.
func fencedBy(t *testing.T, row string, got AirlockSecret, read time.Time, channels ...string) {
	t.Helper()
	if got.ExcludedUnread || got.Limited || !slices.Equal(got.Excluded, channels) {
		t.Fatalf("%s: %s = Excluded %#v ExcludedUnread %v Limited %v; want Excluded %#v from the stored private list", row, slackToken, got.Excluded, got.ExcludedUnread, got.Limited, channels)
	}
	expiresWithin(t, row, got, read, AirlockMintAgainBefore+time.Minute)
}

// unread fails t unless got is ExcludedUnread, read again within a minute.
func unread(t *testing.T, row string, got AirlockSecret, read time.Time) {
	t.Helper()
	if !got.ExcludedUnread || got.Excluded != nil || got.Limited {
		t.Fatalf("%s: %s = ExcludedUnread %v Excluded %#v Limited %v; want ExcludedUnread, which the proxy refuses every call on", row, slackToken, got.ExcludedUnread, got.Excluded, got.Limited)
	}
	expiresWithin(t, row, got, read, AirlockMintAgainBefore+time.Minute)
}

// privRows is Dispatch holding PRIV private with C1 alone, and TEAM public.
func privRows() map[string]projectaccesstest.Row {
	return map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: otherPerson, Listed: []string{fixtureOperator, insider}, SlackChannels: []string{privChannel}},
		teamProject: publicTeam,
	}
}

// publicRows is Dispatch with every project public: TEAM, SWITCH and FORMER.
func publicRows() map[string]projectaccesstest.Row {
	return map[string]projectaccesstest.Row{
		teamProject:   publicTeam,
		switchProject: {Visibility: "public", Owner: otherPerson, SlackChannels: []string{switchChannel}},
		formerProject: {Visibility: "public", Owner: otherPerson, SlackChannels: []string{formerChannel}},
	}
}

// TestAFailedPrivateListReadFencesByTheStoredList: a read of Dispatch's list of private projects
// that fails, whatever the failure, falls back to the broker's stored private list. Over an empty
// store the Slack token released to a session naming no project carries no fence and no refusal,
// today's answer (R9); over a store holding PRIV with C1 it carries Excluded [C1] and is read again
// within a minute, on a fresh pod too; over a held row of unknown channels, or a store the broker
// cannot read, it is ExcludedUnread. Outside the airlock the token is released as today over an
// empty store and denied over one holding C1. A fresh read keeps the list once per CacheTTL. With
// every project public, a held row is the R9 exception's window alone: a refused switch's record
// until its forget, a formerly private project until its forget, each fencing its own channel, and
// nothing once forgotten or past the hold; a record made through a pod whose clock is 90 s behind
// the pod that forgot the row holds the row again.
func TestAFailedPrivateListReadFencesByTheStoredList(t *testing.T) {
	ctx := context.Background()

	t.Run("(a) an empty store with Dispatch down: today's answer", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, privRows(), true)
		dispatch.SetDown(true)
		got, read := releaseSlack(t, m, grantID)
		todays(t, "(a)", got, read)
	})

	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"404 NOT_FOUND", 404, `{"code":"NOT_FOUND","error":"no such route"}`},
		{"404 PROJECT_NOT_FOUND", 404, `{"code":"PROJECT_NOT_FOUND","error":"no such project"}`},
		{"401", 401, `{"code":"UNAUTHORIZED","error":"the bearer is not one Dispatch takes"}`},
	} {
		t.Run("(b) an empty store with the list route answering "+c.name+": today's answer", func(t *testing.T) {
			m, grantID, dispatch := publicGrant(t, privRows(), true)
			dispatch.SetPrivateProjectsAnswer(c.status, c.body)
			got, read := releaseSlack(t, m, grantID)
			todays(t, "(b) "+c.name, got, read)
		})
	}

	t.Run("(c) a store holding PRIV with C1 and Dispatch down: [C1], read again within a minute", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, privRows(), true)
		storeList(t, m.Store, store.KnownPrivateProject{Key: privProject, SlackChannels: []string{privChannel}})
		dispatch.SetDown(true)
		got, read := releaseSlack(t, m, grantID)
		fencedBy(t, "(c)", got, read, privChannel)
	})

	t.Run("(d) the same with the list route answering 404: [C1]", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, privRows(), true)
		storeList(t, m.Store, store.KnownPrivateProject{Key: privProject, SlackChannels: []string{privChannel}})
		dispatch.SetPrivateProjectsAbsent(true)
		got, read := releaseSlack(t, m, grantID)
		fencedBy(t, "(d)", got, read, privChannel)
	})

	t.Run("(e) a fresh pod over a store another pod filled, Dispatch down from its start: [C1]", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, privRows(), true)
		if got, _ := releaseSlack(t, m, grantID); !slices.Equal(got.Excluded, []string{privChannel}) {
			t.Fatalf("the first pod's release while Dispatch answers = Excluded %#v; want [%s]", got.Excluded, privChannel)
		}
		dispatch.SetDown(true)
		pod := podOf(m, m.Store, dispatch)
		got, read := releaseSlack(t, pod, grantID)
		fencedBy(t, "(e) the fresh pod's first release", got, read, privChannel)
	})

	t.Run("(f) a held row of unknown channels with Dispatch down: unread", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, privRows(), true)
		storeUnknown(t, m.Store, privProject)
		dispatch.SetDown(true)
		got, read := releaseSlack(t, m, grantID)
		unread(t, "(f)", got, read)
	})

	t.Run("(g) a store it cannot read, with Dispatch down: unread", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, privRows(), true)
		if _, err := m.Store.Pool.Exec(ctx, `drop table known_private_projects`); err != nil {
			t.Fatalf("drop known_private_projects: %v", err)
		}
		dispatch.SetDown(true)
		got, read := releaseSlack(t, m, grantID)
		unread(t, "(g)", got, read)
	})

	t.Run("(h) outside the airlock: released over an empty store, denied over one holding C1", func(t *testing.T) {
		empty := newOutsideBroker(t, privRows())
		empty.dispatch.SetDown(true)
		if request, err := empty.create(t); err != nil || request.GrantID == nil {
			t.Fatalf("(h) %s outside the airlock over an empty store with Dispatch down = %+v, %v; want granted, as today", slackToken, request, err)
		}
		holding := newOutsideBroker(t, privRows())
		storeList(t, holding.m.Store, store.KnownPrivateProject{Key: privProject, SlackChannels: []string{privChannel}})
		holding.dispatch.SetDown(true)
		request, err := holding.create(t)
		deniedFor(t, "(h) "+slackToken+" outside the airlock over a store holding "+privChannel+" with Dispatch down", request, err, slackToken, policy.ReasonProjectChannelsNeedAirlock)
	})

	t.Run("(i) a fresh read keeps the list, and a cached one writes nothing", func(t *testing.T) {
		m, grantID, _ := publicGrant(t, privRows(), true)
		releaseSlack(t, m, grantID)
		row, held := storedRow(t, m.Store, privProject)
		if !held || !slices.Equal(row.SlackChannels, []string{privChannel}) {
			t.Fatalf("(i) after one fresh read the store holds PRIV = %+v, held %v; want held with [%s]", row, held, privChannel)
		}
		before := listedOf(t, m.Store, privProject)
		releaseSlack(t, m, grantID)
		if after := listedOf(t, m.Store, privProject); after != before {
			t.Fatalf("(i) a second release inside CacheTTL moved PRIV's listing from %d to %d; want the cached answer to write nothing", before, after)
		}
	})

	// Every project public: a held row is the R9 exception's window alone.
	recordSwitch := func(t *testing.T, st *store.Store, key string, channels ...string) int64 {
		t.Helper()
		record, err := st.RecordPrivateProject(ctx, key, channels)
		if err != nil {
			t.Fatalf("RecordPrivateProject(%s): %v", key, err)
		}
		return record
	}
	forget := func(t *testing.T, st *store.Store, key string, record *int64) {
		t.Helper()
		answer, err := st.ForgetPrivateProject(ctx, key, record)
		if err != nil || answer.Outcome != store.ForgetForgotten {
			t.Fatalf("ForgetPrivateProject(%s) = %+v, %v; want forgotten", key, answer, err)
		}
	}
	advance := func(st *store.Store, d time.Duration) { st.DatabaseClockOffset.Set(st.DatabaseClockOffset.Get() + d) }

	t.Run("(j) a refused switch's record, forgotten with its record, Dispatch down: today's answer", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, publicRows(), true)
		record := recordSwitch(t, m.Store, switchProject, switchChannel)
		forget(t, m.Store, switchProject, &record)
		dispatch.SetDown(true)
		got, read := releaseSlack(t, m, grantID)
		todays(t, "(j) SWITCH's refused record, forgotten", got, read)
	})

	t.Run("(k) the same with its forget not yet taken, within 3 min: [C2] alone", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, publicRows(), true)
		recordSwitch(t, m.Store, switchProject, switchChannel)
		dispatch.SetDown(true)
		got, read := releaseSlack(t, m, grantID)
		fencedBy(t, "(k) SWITCH's record before its forget", got, read, switchChannel)
	})

	t.Run("(l) past 3 min a good read not naming SWITCH removes it, and the next outage is today's", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, publicRows(), true)
		recordSwitch(t, m.Store, switchProject, switchChannel)
		advance(m.Store, store.KnownPrivateHold+time.Second)
		releaseSlack(t, m, grantID)
		if n := storedRows(t, m.Store); n != 0 {
			t.Fatalf("(l) the stored private list holds %d row(s) after a good read more than 3 min after SWITCH's record; want SWITCH gone", n)
		}
		dispatch.SetDown(true)
		freshClient(m, dispatch)
		got, read := releaseSlack(t, m, grantID)
		todays(t, "(l) the outage after SWITCH left", got, read)
	})

	t.Run("(m) a record of no channel, not forgotten, Dispatch down: no Excluded", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, publicRows(), true)
		recordSwitch(t, m.Store, switchProject)
		dispatch.SetDown(true)
		got, _ := releaseSlack(t, m, grantID)
		if len(got.Excluded) != 0 || got.ExcludedUnread || got.Limited {
			t.Fatalf("(m) an R9 row: %s over SWITCH's record of no channel = Excluded %#v ExcludedUnread %v; want no fence and no refusal", slackToken, got.Excluded, got.ExcludedUnread)
		}
	})

	former := func(t *testing.T, st *store.Store) {
		t.Helper()
		storeList(t, st, store.KnownPrivateProject{Key: formerProject, SlackChannels: []string{formerChannel}})
		recordSwitch(t, st, formerProject, formerChannel)
		advance(st, store.KnownPrivateHold+time.Second)
	}

	t.Run("(n) a formerly private project made public and forgotten, Dispatch down: today's answer", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, publicRows(), true)
		former(t, m.Store)
		forget(t, m.Store, formerProject, nil)
		dispatch.SetDown(true)
		got, read := releaseSlack(t, m, grantID)
		todays(t, "(n) FORMER, forgotten", got, read)
	})

	t.Run("(o) the same with its forget not yet taken: [C3] alone", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, publicRows(), true)
		former(t, m.Store)
		dispatch.SetDown(true)
		got, read := releaseSlack(t, m, grantID)
		fencedBy(t, "(o) FORMER before its forget", got, read, formerChannel)
	})

	t.Run("(p) a record through a pod 90 s behind the pod that forgot the row holds it", func(t *testing.T) {
		m, grantID, dispatch := publicGrant(t, publicRows(), true)
		// Pod A runs 45 s ahead and pod B 45 s behind: under the store's rules only the
		// database's clock counts, so a pod's clock moves nothing, and these offsets matter only
		// to a store that would decide by times a pod passes.
		ahead, behind := &store.Store{Pool: m.Store.Pool}, &store.Store{Pool: m.Store.Pool}
		ahead.DatabaseClockOffset.Set(45 * time.Second)
		behind.DatabaseClockOffset.Set(-45 * time.Second)
		first := recordSwitch(t, m.Store, switchProject, switchChannel)
		forget(t, ahead, switchProject, &first)
		recordSwitch(t, behind, switchProject, switchChannel)
		dispatch.SetDown(true)
		pod := podOf(m, behind, dispatch)
		got, read := releaseSlack(t, pod, grantID)
		fencedBy(t, "(p) SWITCH recorded through pod B after pod A forgot it", got, read, switchChannel)
	})
}

// listedOf is key's listed number in st's stored private list, 0 when it has none.
func listedOf(t *testing.T, st *store.Store, key string) int64 {
	t.Helper()
	var listed *int64
	if err := st.Pool.QueryRow(context.Background(), `select listed from known_private_projects where project = $1`, key).Scan(&listed); err != nil {
		t.Fatalf("read %s's listing: %v", key, err)
	}
	if listed == nil {
		return 0
	}
	return *listed
}

// TestAPrivateCredentialPostsOnlyToChannelsTheStoreHolds: a limited grant posts only to channels
// the broker's stored private list holds for its project. PRIV names C1, stored long ago; while
// the list route answers 404 and the project route answers, the owner adds C2: past
// slackChannelSettle from that change, a credential naming PRIV is limited to [C1] without C2,
// while an unlimited grant's Excluded is the stored [C1]. Once the list route answers again, the
// read stores C2 at S, the credential gets no channel with SettlesAt at S plus slackChannelSettle,
// decided on the database's time, and then [C1 C2]. A credential naming a private project with no
// held row gets no channel.
func TestAPrivateCredentialPostsOnlyToChannelsTheStoreHolds(t *testing.T) {
	rules := proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule(slackToken, "")}}
	m, enr, key, _ := newProjectFixture(t, rules, publicTeam)
	rows := privRows()
	storeAged(t, m.Store, rows)
	dispatch := projectaccesstest.NewServer(t, rows, nil)
	release := func(t *testing.T, named ...string) AirlockSecret {
		t.Helper()
		nameProjects(t, m, enr, named...)
		freshClient(m, dispatch)
		_, grantID := grantOf(t, m, enr, key, "post the release notes", slackToken)
		got, _ := releaseSlack(t, m, grantID)
		return got
	}

	dispatch.SetPrivateProjectsAbsent(true)
	priv := rows[privProject]
	priv.SlackChannels = []string{privChannel, addedChannel}
	priv.SlackChannelsChangedAt = time.Now().Add(-slackChannelSettle - time.Minute)
	dispatch.SetRow(privProject, priv)

	if got := release(t, privProject); !got.Limited || !slices.Equal(got.Channels, []string{privChannel}) || !got.SettlesAt.IsZero() {
		t.Fatalf("a credential naming PRIV while the list route answers 404, past the settle of the owner's change = Limited %v Channels %#v SettlesAt %v; want [%s] without %s, which the store does not hold", got.Limited, got.Channels, got.SettlesAt, privChannel, addedChannel)
	}
	if got := release(t); !slices.Equal(got.Excluded, []string{privChannel}) {
		t.Fatalf("an unlimited grant while the list route answers 404 = Excluded %#v; want the stored [%s]", got.Excluded, privChannel)
	}

	dispatch.SetPrivateProjectsAbsent(false)
	got := release(t, privProject)
	row, held := storedRow(t, m.Store, privProject)
	if !held || !slices.Contains(row.SlackChannels, addedChannel) || row.ChannelsStoredAt.IsZero() {
		t.Fatalf("PRIV in the store once the list route answers = %+v, held %v; want %s stored with its time", row, held, addedChannel)
	}
	if want := row.ChannelsStoredAt.Add(slackChannelSettle); !got.Limited || len(got.Channels) != 0 || !got.SettlesAt.Equal(want) {
		t.Fatalf("a credential naming PRIV once the read stored %s at %v = Limited %v Channels %#v SettlesAt %v; want no channel until %v", addedChannel, row.ChannelsStoredAt, got.Limited, got.Channels, got.SettlesAt, want)
	}
	m.Store.DatabaseClockOffset.Set(m.Store.DatabaseClockOffset.Get() + slackChannelSettle + time.Second)
	if got := release(t, privProject); !got.Limited || !slices.Equal(got.Channels, []string{addedChannel, privChannel}) {
		t.Fatalf("a credential naming PRIV past the settle of %s's store = Limited %v Channels %#v; want [%s %s]", addedChannel, got.Limited, got.Channels, addedChannel, privChannel)
	}

	dispatch.SetPrivateProjectsAbsent(true)
	synth := projectaccesstest.Row{Visibility: "private", Owner: otherPerson, Listed: []string{fixtureOperator, insider}, SlackChannels: []string{synthChannel}}
	dispatch.SetRow(synthProject, synth)
	if got := release(t, synthProject); !got.Limited || got.Channels == nil || len(got.Channels) != 0 {
		t.Fatalf("a credential naming SYNTH, private with no held row = Limited %v Channels %#v; want limited to no channel", got.Limited, got.Channels)
	}
}
