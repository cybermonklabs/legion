// packages/envoy/internal/cipher/proxy/slack_excluded_test.go
//
// The public Slack fence's proxy half: a project-channels name released to a credential that names
// no private project carries Excluded, every channel a private project's owner named, and the proxy
// refuses PRIVATE_CHANNEL any call on it that names one of them, or that it cannot read as naming
// none (slackCallExcluded). The first test drives every call shape through the rig's fake grants,
// each with its own control; the rest put the real request machine behind the proxy, on pods that
// share one store, one stand-in Dispatch and one clock, as a broker's pods share them.
package proxy

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// The fence's channels: PRIV's, which the fence holds; a public one, which it does not; and one a
// row adds to PRIV's list, or that PUB names before it turns private.
const (
	privChannel  = "C0PRIV00001"
	pubChannel   = "C0PUB000001"
	addedChannel = "C0ADDED0001"
	pubKey       = "PUB"
)

// channelSettle is the broker's settle time (requests.slackChannelSettle, which that package keeps
// to itself), spelled from the same parts: the project-access cache, the release's bound on a
// project-channels value, the proxy's own margin, and a minute of clock difference.
const channelSettle = projectaccess.CacheTTL + ownerChangeHeard + mintAgainBefore + time.Minute

// The placeholders of the fence's fake grants, in the shape the broker mints them.
var (
	fencedPlaceholder   = requests.PlaceholderPrefix + "FencedFencedFencedFencedFencedF1"
	unfencedPlaceholder = requests.PlaceholderPrefix + "UnfencedUnfencedUnfencedUnfence1"
)

// historyOf is a read of channel's messages, conversations.history, as a Slack client asks it: a
// method no repository code sends with the token, which the fence refuses whatever it names.
func historyOf(channel string) slackPost {
	return slackPost{method: http.MethodGet, path: "/api/conversations.history?channel=" + channel}
}

// readOf is the controller's read of a thread in channel, conversations.replies, as its skill
// sends it: a method the fence allows, checked by the channel it names.
func readOf(channel string) slackPost {
	return slackPost{method: http.MethodGet, path: "/api/conversations.replies?channel=" + channel + "&ts=" + slackThread}
}

// readAs is that read with its query spelled as query.
func readAs(query string) slackPost {
	return slackPost{method: http.MethodGet, path: "/api/conversations.replies?" + query}
}

// refusedAs fails t unless p was answered 403 code in the proxy's refusal shape, its words
// starting with words, with its one refusal record, and reached no service.
func (a slackAnswer) refusedAs(t *testing.T, what string, p slackPost, code, words string) {
	t.Helper()
	if a.status != http.StatusForbidden || len(a.seen) != 0 {
		t.Fatalf("%s: %s answered %d %q and reached the stand-in %d time(s); want 403 %s and no call", what, p, a.status, a.body, len(a.seen), code)
	}
	if !strings.HasPrefix(a.body, "airlock: "+words) {
		t.Errorf("%s: the refusal reads %q; want the airlock's words for %s", what, a.body, code)
	}
	if len(a.refusals) != 1 || a.refusals[0].fields["code"] != code || a.refusals[0].fields["status"] != int64(http.StatusForbidden) {
		t.Errorf("%s: the proxy recorded %v; want one refusal, 403 %s", what, a.refusals, code)
	}
}

// fenceCase is one call a fenced grant must not carry: the call, the rules of slackCallExcluded
// (by the numbers its comment gives them) that refuse it, and, where the call has one, the same
// call naming a public channel, which the grant carries. A case that one rule alone refuses is
// that rule's proof: with the rule taken out of the checker (an overlay of slack.go, which the
// pull request records for each rule), that case, and no case another rule also refuses, reaches
// the stand-in.
type fenceCase struct {
	name   string
	call   slackPost
	rules  []int
	public *slackPost
}

// fenceCases are the calls naming PRIV's channel, calling a method the fence does not allow, or
// that the fence cannot read as naming none.
func fenceCases(t *testing.T) []fenceCase {
	t.Helper()
	public := func(p slackPost) *slackPost { return &p }
	rules := func(numbers ...int) []int { return numbers }
	get := func(path string) slackPost { return slackPost{method: http.MethodGet, path: path} }
	jsonType := [][2]string{{"Content-Type", slackJSON}}
	big := `{"channel":"` + pubChannel + `","text":"` + strings.Repeat("x", maxSlackPostBody) + `"}`
	return []fenceCase{
		// Rule 1: a method the fence does not allow, whatever it names.
		{"search.messages", get("/api/search.messages?query=release"), rules(1), nil},
		{"assistant.search.context", slackPost{method: http.MethodPost, path: "/api/assistant.search.context", header: jsonType, body: `{"query":"release"}`}, rules(1), nil},
		{"reactions.list", get("/api/reactions.list"), rules(1), nil},
		{"stars.list", get("/api/stars.list"), rules(1), nil},
		{"files.list naming no channel", get("/api/files.list"), rules(1), nil},
		{"files.info", get("/api/files.info?file=F0FILE00001"), rules(1), nil},
		{"conversations.list", get("/api/conversations.list"), rules(1), nil},
		{"conversations.history naming PRIV's channel in the query", historyOf(privChannel), rules(1, 5), nil},
		// Rule 2: a body the fence cannot read as Slack would.
		{"a non-empty text/plain body", slackPost{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", "text/plain; charset=utf-8"}}, body: "channel=" + privChannel + "&text=x"}, rules(2), nil},
		{"a body with no Content-Type", slackPost{method: http.MethodPost, path: slackPostPath, body: postTo(privChannel)}, rules(2), nil},
		{"a multipart body", multipartPost(t), rules(2), nil},
		{"a body over 1 MiB", jsonPost(big), rules(2), nil},
		// The fence reads the bytes as sent, which Slack would decode first, so any encoding is
		// refused, a body whose bytes read as a harmless form included.
		{"Content-Encoding on a form naming no channel", slackPost{method: http.MethodPost, path: slackPostPath,
			header: [][2]string{{"Content-Type", slackForm}, {"Content-Encoding", "br"}}, body: "text=the+release+notes+are+out"}, rules(2), nil},
		// Read as a form, the gzip bytes hold a key of bytes that are no letter, which rule 6
		// refuses too.
		{"Content-Encoding: gzip", slackPost{method: http.MethodPost, path: slackPostPath,
			header: [][2]string{{"Content-Type", slackForm}, {"Content-Encoding", "gzip"}}, body: gzipped(t, "channel="+privChannel+"&text=x")},
			rules(2, 6), nil},
		// Rule 3: a JSON body that is not one object, or whose channel is not a string.
		{"a JSON array", jsonPost(`[{"channel":"` + privChannel + `","text":"x"}]`), rules(3), nil},
		{`{"channel":["` + privChannel + `"]}`, jsonPost(`{"channel":["` + privChannel + `"],"text":"x"}`), rules(3), nil},
		{`{"channel":123}`, jsonPost(`{"channel":123,"text":"x"}`), rules(3), nil},
		{"a second JSON value after the object", jsonPost(postTo(pubChannel) + `{"channel":"` + privChannel + `"}`), rules(3), nil},
		// Rule 4: a query or a form url.ParseQuery refuses, whose part before the error names a
		// public channel.
		{"a query url.ParseQuery refuses", readAs("channel=" + pubChannel + "&ts=" + slackThread + "&cursor=%zz"), rules(4), nil},
		{"a form url.ParseQuery refuses", formPost("channel=" + pubChannel + "&text=%zz"), rules(4), nil},
		// Rule 5: PRIV's channel, by its id, in every place and spelling Slack reads it.
		{"conversations.replies naming it in the query", readOf(privChannel), rules(5), public(readOf(pubChannel))},
		{"chat.postMessage naming it in JSON", jsonPost(postTo(privChannel)), rules(5), public(jsonPost(postTo(pubChannel)))},
		{"chat.postMessage naming it in a form", formPost("channel=" + privChannel + "&text=the+release+notes+are+out"), rules(5),
			public(formPost("channel=" + pubChannel + "&text=the+release+notes+are+out"))},
		{"channels=" + pubChannel + "," + privChannel, readAs("channels=" + pubChannel + "," + privChannel + "&ts=" + slackThread), rules(5),
			public(readAs("channels=" + pubChannel + "&ts=" + slackThread))},
		{"channel_id", readAs("channel_id=" + privChannel + "&ts=" + slackThread), rules(5), public(readAs("channel_id=" + pubChannel + "&ts=" + slackThread))},
		{"channel=" + pubChannel + "&channel=" + privChannel, readAs("channel=" + pubChannel + "&channel=" + privChannel + "&ts=" + slackThread), rules(5), nil},
		{"CHANNEL in another case", jsonPost(`{"CHANNEL":"` + privChannel + `","text":"x"}`), rules(5), nil},
		{"chann%65l in a form", formPost("chann%65l=" + privChannel + "&text=x"), rules(5), nil},
		{"a JSON channel spelled with an escape", jsonPost(`{"channel":"C0PRIV\u00300001","text":"x"}`), rules(5), nil},
		// Rule 7 refuses this spelling too, since it is no id as written.
		{"channel=%20c0priv00001%20", readAs("channel=%20c0priv00001%20&ts=" + slackThread), rules(5, 7), nil},
		// Rule 6: a key Slack may read as channel although rule 5 reads another key: a receiver
		// that parses keys as PHP does drops the leading space of %20channel and +channel, ends
		// channel%00x at its NUL, and reads channel[] as channel. Each names PRIV's channel and is
		// refused whatever channel it names, so none has a public call.
		{"%20channel", readAs("%20channel=" + privChannel + "&ts=" + slackThread), rules(6), nil},
		{"+channel", readAs("+channel=" + privChannel + "&ts=" + slackThread), rules(6), nil},
		{"channel%00x", readAs("channel%00x=" + privChannel + "&ts=" + slackThread), rules(6), nil},
		{"channel[]", readAs("channel[]=" + privChannel + "&ts=" + slackThread), rules(6), nil},
		{"channel.", readAs("channel.=" + privChannel + "&ts=" + slackThread), rules(6), nil},
		{`a JSON key " channel"`, jsonPost(`{" channel":"` + privChannel + `","text":"x"}`), rules(6), nil},
		{"channel%5B0%5D in a form", formPost("channel%5B0%5D=" + privChannel + "&text=x"), rules(6), nil},
		// Rule 7: a channel given by its name, which chat.postMessage resolves for a public
		// channel, a public channel a private project's owner listed among them, while rule 5
		// reads ids alone.
		{"chat.postMessage naming its channel #priv-team", jsonPost(postTo("#priv-team")), rules(7), nil},
		{"conversations.replies naming its channel priv-team", readOf("priv-team"), rules(7), nil},
	}
}

// grantFenced makes placeholder the password of a fake grant holding the token, released as
// secret.
func grantFenced(r *rig, placeholder, grantID string, secret requests.AirlockSecret) {
	grantSlack(r, placeholder, grantID, secret)
}

// TestAPublicSlackGrantNeverNamesAPrivateChannel: a grant of the token whose Excluded holds PRIV's
// channel refuses 403 PRIVATE_CHANNEL, before any of it reaches Slack, every call of a method the
// fence does not allow (reactions.list, stars.list, files.list, files.info, conversations.list and
// the searches among them), every call that names PRIV's channel in any way Slack reads one, every
// call carrying a key Slack may read as channel while the fence reads another (%20channel,
// +channel, channel%00x, channel[], channel.), every call naming a channel other than by its id,
// and every call it cannot read as naming none. Each of slackCallExcluded's seven rules has a case
// that it alone refuses (fenceCase). The allowed calls naming a public channel, and auth.test,
// reach the stand-in with the real token, and a grant with no Excluded carries every one of them
// as before the fence. A name the broker could not read the fence of refuses every call
// PRIVATE_CHANNELS_UNREAD, while Dispatch's list fails and while a private project's entry carries
// no slack_channels, and serves again once a read answers.
func TestAPublicSlackGrantNeverNamesAPrivateChannel(t *testing.T) {
	r := newRig(t, Options{Rules: slackRules()})
	value := map[string]string{requests.SlotValue: slackValue}
	fenced := requests.AirlockSecret{Slots: value, Excluded: []string{privChannel}}
	grantFenced(r, fencedPlaceholder, "grant-slack-fenced", fenced)
	grantFenced(r, unfencedPlaceholder, "grant-slack-unfenced", requests.AirlockSecret{Slots: value})
	client, unfenced := r.client(fencedPlaceholder), r.client(unfencedPlaceholder)

	for _, c := range fenceCases(t) {
		t.Run("refused/"+c.name, func(t *testing.T) {
			what := fmt.Sprintf("%s (refused by rule %v)", c.name, c.rules)
			sendSlack(t, r, client, fencedPlaceholder, c.call).refusedAs(t, what, c.call, codePrivateChannel, privateChannel)
		})
		if c.public != nil {
			t.Run("public/"+c.name, func(t *testing.T) {
				sendSlack(t, r, client, fencedPlaceholder, *c.public).served(t, c.name+", naming a public channel", *c.public, slackValue)
			})
		}
		t.Run("unfenced/"+c.name, func(t *testing.T) {
			sendSlack(t, r, unfenced, unfencedPlaceholder, c.call).served(t, c.name+", by a grant with no Excluded", c.call, slackValue)
		})
	}

	t.Run("auth.test, which names no channel and answers no channel's content", func(t *testing.T) {
		check := slackPost{method: http.MethodPost, path: "/api/auth.test"}
		sendSlack(t, r, client, fencedPlaceholder, check).served(t, "auth.test by a fenced grant", check, slackValue)
	})

	t.Run("a fenced name whose entry is no header swap", func(t *testing.T) {
		aws := newAWSRig(t, false)
		values := map[string]string{requests.SlotAWSAccessKeyID: awsRealKeyID, requests.SlotAWSSecretAccessKey: awsRealSecret}
		aws.grants.setValues(testGrantID, map[string]requests.AirlockSecret{awsName: {Slots: values, Excluded: []string{privChannel}}})
		post := jsonPost(postTo(pubChannel))
		request := awsRequest(t, post.method, "https://"+boundHost+post.path, post.body, "execute-api", false, "Content-Type", slackJSON)
		sendRequest(t, aws, aws.client(awsKeyPlaceholder), request).refusedAs(t, "a fenced name of kind aws", post, codePrivateChannel, privateChannel)
	})

	unreadCalls := []slackPost{historyOf(pubChannel), jsonPost(postTo(pubChannel)), {method: http.MethodGet, path: "/api/auth.test"}}
	t.Run("unread while Dispatch's list fails", func(t *testing.T) {
		w := newFenceWorld(t, map[string]projectaccesstest.Row{privKey: privateRow([]string{privChannel}, slackPerson)}, true)
		// The list fails over a stored private list holding PRIV with channels it never read,
		// which the fallback cannot fence by.
		storeList(t, w.st, store.KnownPrivateProject{Key: privKey})
		w.dispatch.SetPrivateProjectsDown(true)
		p := w.pod(t, "public")
		for _, call := range unreadCalls {
			p.send(t, call).refusedAs(t, "while the list fails", call, codePrivateChannelsUnread, privateChannelsUnread)
		}
		w.dispatch.SetPrivateProjectsDown(false)
		w.clock.advance(time.Minute + time.Second)
		p.send(t, readOf(pubChannel)).served(t, "a public read once the list answers again", readOf(pubChannel), slackValue)
		p.send(t, readOf(privChannel)).refusedAs(t, "a read of PRIV's channel once the list answers again", readOf(privChannel), codePrivateChannel, privateChannel)
	})
	t.Run("unread while a private project's entry carries no slack_channels", func(t *testing.T) {
		omitted := privateRow([]string{privChannel}, slackPerson)
		omitted.OmitSlackChannels = true
		w := newFenceWorld(t, map[string]projectaccesstest.Row{privKey: omitted}, true)
		p := w.pod(t, "public")
		for _, call := range unreadCalls {
			p.send(t, call).refusedAs(t, "while PRIV's entry carries no slack_channels", call, codePrivateChannelsUnread, privateChannelsUnread)
		}
		w.dispatch.SetRow(privKey, privateRow([]string{privChannel}, slackPerson))
		w.clock.advance(time.Minute + time.Second)
		p.send(t, readOf(pubChannel)).served(t, "a public read once PRIV's entry carries its channels", readOf(pubChannel), slackValue)
		p.send(t, readOf(privChannel)).refusedAs(t, "a read of PRIV's channel once its entry carries it", readOf(privChannel), codePrivateChannel, privateChannel)
	})
}

// TestAPublicSlackGrantIsTodaysWithNoPrivateProject: while no private project lists a channel, a
// session naming no project is served every call as before the fence: a call the fence would
// refuse if it read it - a search, a body of no type it reads, a compressed body, a body over 1 MiB
// - is carried byte for byte, since with no channel excluded the proxy reads no body at all, and
// the value carries the release's ten-minute bound all the same. On a broker with no
// project-access client (R9's case) Dispatch is asked nothing; on one with it, Dispatch is asked
// its list of private projects, through its 30 s cache, and nothing else: a second release inside
// the cache's window asks nothing more.
func TestAPublicSlackGrantIsTodaysWithNoPrivateProject(t *testing.T) {
	for _, access := range []bool{false, true} {
		name := "a broker with no project-access client"
		if access {
			name = "a broker with project access and no private project"
		}
		t.Run(name, func(t *testing.T) {
			w := newFenceWorld(t, map[string]projectaccesstest.Row{privKey: {Visibility: "public", Owner: slackOwner, SlackChannels: []string{privChannel}}}, true)
			p := w.newPod(t, "public", access)
			unread := []slackPost{
				historyOf(privChannel),
				{method: http.MethodGet, path: "/api/search.messages?query=release"},
				{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", "text/plain; charset=utf-8"}}, body: "channel=" + privChannel},
				{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", slackJSON}, {"Content-Encoding", "gzip"}}, body: gzipped(t, postTo(privChannel))},
				jsonPost(`{"channel":"` + privChannel + `","text":"` + strings.Repeat("x", maxSlackPostBody) + `"}`),
			}
			for _, call := range unread {
				p.send(t, call).served(t, "a call with no private project", call, slackValue)
			}
			// A second release of the grant on the same pod inside projectaccess.CacheTTL, as the
			// proxy's read of a second grant comes: the list the first kept answers it.
			p.grants.again(t)
			w.clock.advance(ownerChangeHeard - mintAgainBefore + time.Second)
			p.send(t, historyOf(privChannel)).served(t, "a read after the proxy read the grant again", historyOf(privChannel), slackValue)
			reads := p.grants.all()
			if len(reads) != 3 {
				t.Fatalf("the grant was read %d time(s); want three times: at the first call, once more at once, and once after its bound", len(reads))
			}
			for i, values := range reads {
				got := values[slackName]
				if got.Limited || got.Excluded != nil || got.ExcludedUnread || got.Expires.IsZero() {
					t.Fatalf("read %d of %s = Limited %v Excluded %#v ExcludedUnread %v Expires %v; want unlimited and unfenced, bounded", i+1, slackName, got.Limited, got.Excluded, got.ExcludedUnread, got.Expires)
				}
			}
			lists, calls := w.dispatch.PrivateProjectsCalls(), w.dispatch.Calls()
			switch {
			case !access && calls != 0:
				t.Fatalf("Dispatch was asked %d time(s) by a broker with no project-access client; want none", calls)
			case access && (calls != lists || lists != 2):
				t.Fatalf("Dispatch was asked %d time(s), %d of them for the list of private projects, over 3 releases in two windows of %v; want the list alone, once in each window", calls, lists, projectaccess.CacheTTL)
			}
		})
	}
}

// switchKey and switchChannel are SWITCH, a project whose switch to private recorded it in the
// broker's stored private list and did not commit, and its own channel.
const (
	switchKey     = "SWITCH"
	switchChannel = "C0SWITCH002"
)

// TestAPublicSlackGrantKeepsTheStoredFenceWhileDispatchIsDown: through the proxy against the real
// request machine, a session naming no project and Dispatch down. Over a stored private list
// holding PRIV with C1, a read of C1 is refused PRIVATE_CHANNEL before Slack is called, while a
// post to a public channel and auth.test are served. With every project public and only a
// not-yet-forgotten record of SWITCH with C2 (a window of the R9 exception), conversations.replies
// and chat.postMessage naming C2 are refused, the same two naming a public channel by its id and
// auth.test are served, and conversations.history naming a public channel is refused by the method
// rule. Over an empty store, or with SWITCH forgotten, and with Dispatch down or the list route
// answering 404, every call is served as today with no body read, conversations.history of a
// public channel included (the R9 rows).
func TestAPublicSlackGrantKeepsTheStoredFenceWhileDispatchIsDown(t *testing.T) {
	auth := slackPost{method: http.MethodGet, path: "/api/auth.test"}
	t.Run("a store holding PRIV with C1, Dispatch down", func(t *testing.T) {
		w := newFenceWorld(t, map[string]projectaccesstest.Row{privKey: privateRow([]string{privChannel}, slackPerson)}, true)
		storeList(t, w.st, store.KnownPrivateProject{Key: privKey, SlackChannels: []string{privChannel}})
		w.dispatch.SetDown(true)
		p := w.pod(t, "public")
		p.send(t, readOf(privChannel)).refusedAs(t, "a read of PRIV's channel with Dispatch down", readOf(privChannel), codePrivateChannel, privateChannel)
		toPublic := jsonPost(postTo(pubChannel))
		p.send(t, toPublic).served(t, "a post to a public channel with Dispatch down", toPublic, slackValue)
		p.send(t, auth).served(t, "auth.test with Dispatch down", auth, slackValue)
	})

	public := map[string]projectaccesstest.Row{
		pubKey:    {Visibility: "public", Owner: slackOwner, SlackChannels: []string{pubChannel}},
		switchKey: {Visibility: "public", Owner: slackOwner, SlackChannels: []string{switchChannel}},
	}
	t.Run("every project public, SWITCH's record not yet forgotten, Dispatch down", func(t *testing.T) {
		w := newFenceWorld(t, public, true)
		if _, err := w.st.RecordPrivateProject(t.Context(), switchKey, []string{switchChannel}); err != nil {
			t.Fatalf("RecordPrivateProject(%s): %v", switchKey, err)
		}
		w.dispatch.SetDown(true)
		p := w.pod(t, "public")
		for _, call := range []slackPost{readOf(switchChannel), jsonPost(postTo(switchChannel))} {
			p.send(t, call).refusedAs(t, "a call naming SWITCH's channel in the window", call, codePrivateChannel, privateChannel)
		}
		for _, call := range []slackPost{readOf(pubChannel), jsonPost(postTo(pubChannel)), auth} {
			p.send(t, call).served(t, "a call naming a public channel by its id in the window", call, slackValue)
		}
		p.send(t, historyOf(pubChannel)).refusedAs(t, "conversations.history of a public channel in the window, by the method rule", historyOf(pubChannel), codePrivateChannel, privateChannel)
	})

	// What a fence would refuse if it read it: a method the fence does not allow, and a body of
	// no type it reads. Served, each proves the proxy read no body and applied no rule.
	todays := []slackPost{
		historyOf(pubChannel),
		{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", "text/plain; charset=utf-8"}}, body: "channel=" + pubChannel},
		readOf(pubChannel),
		auth,
	}
	for _, c := range []struct {
		name   string
		forget bool
		fail   func(*projectaccesstest.Server)
	}{
		{"an empty store, Dispatch down", false, func(s *projectaccesstest.Server) { s.SetDown(true) }},
		{"an empty store, the list route answering 404", false, func(s *projectaccesstest.Server) { s.SetPrivateProjectsAbsent(true) }},
		{"SWITCH forgotten, Dispatch down", true, func(s *projectaccesstest.Server) { s.SetDown(true) }},
		{"SWITCH forgotten, the list route answering 404", true, func(s *projectaccesstest.Server) { s.SetPrivateProjectsAbsent(true) }},
	} {
		t.Run("the R9 row: "+c.name, func(t *testing.T) {
			w := newFenceWorld(t, public, true)
			if c.forget {
				record, err := w.st.RecordPrivateProject(t.Context(), switchKey, []string{switchChannel})
				if err != nil {
					t.Fatalf("RecordPrivateProject(%s): %v", switchKey, err)
				}
				if answer, err := w.st.ForgetPrivateProject(t.Context(), switchKey, &record); err != nil || answer.Outcome != store.ForgetForgotten {
					t.Fatalf("ForgetPrivateProject(%s, %d) = %+v, %v; want forgotten", switchKey, record, answer, err)
				}
			}
			c.fail(w.dispatch)
			p := w.pod(t, "public")
			for _, call := range todays {
				p.send(t, call).served(t, "the R9 row ("+c.name+"): "+call.path, call, slackValue)
			}
		})
	}
}

// TestAValueReleasedBeforeTheFirstPrivateChannelLearnsIt: a value the proxy holds from before the
// fence could know of a private channel reads it again. Row 1: the token held before it carries
// its tag is read again at the next call after the retag, which moves the policy version, and is
// then bounded and fenced. Row 2: the token released to a public session while no project is
// private carries the release's ten-minute bound and cost Dispatch the list of private projects
// alone; PRIV then turns private and names a channel, and once the clocks pass that bound less
// the proxy's margin, plus the project-access cache, a read of that channel is refused
// PRIVATE_CHANNEL with no call to Slack.
func TestAValueReleasedBeforeTheFirstPrivateChannelLearnsIt(t *testing.T) {
	t.Run("a value released before the retag", func(t *testing.T) {
		w := newFenceWorld(t, map[string]projectaccesstest.Row{privKey: privateRow([]string{privChannel}, slackPerson)}, false)
		p := w.pod(t, "public")
		p.send(t, readOf(privChannel)).served(t, "a read before the token carries project-channels", readOf(privChannel), slackValue)
		if first := p.grants.last()[slackName]; !first.Expires.IsZero() || first.Excluded != nil {
			t.Fatalf("the untagged token was released with Expires %v Excluded %#v; want neither, as before the tag", first.Expires, first.Excluded)
		}
		w.retag(t)
		p.send(t, readOf(privChannel)).refusedAs(t, "a read of PRIV's channel at the first call after the retag", readOf(privChannel), codePrivateChannel, privateChannel)
		reads := p.grants.all()
		if len(reads) != 2 {
			t.Fatalf("the proxy read the grant %d time(s); want twice, the second at the first call after the retag", len(reads))
		}
		if got := reads[1][slackName]; got.Expires.IsZero() || !slices.Equal(got.Excluded, []string{privChannel}) {
			t.Fatalf("the read after the retag = Expires %v Excluded %#v; want bounded and fenced from %s", got.Expires, got.Excluded, privChannel)
		}
	})

	t.Run("a value released before the first private project", func(t *testing.T) {
		w := newFenceWorld(t, map[string]projectaccesstest.Row{privKey: {Visibility: "public", Owner: slackOwner, Listed: []string{slackPerson}}}, true)
		p := w.pod(t, "public")
		read := w.clock.now()
		p.send(t, readOf(privChannel)).served(t, "a read of "+privChannel+" while no project is private", readOf(privChannel), slackValue)
		// Each check of the first read reports and goes on, so a value that is not bounded is
		// also shown reaching Slack on PRIV's channel below.
		first := p.grants.last()[slackName]
		if first.Expires.Before(read.Add(ownerChangeHeard)) || first.Expires.After(w.clock.now().Add(ownerChangeHeard)) {
			t.Errorf("%s for a public session expires %v; want the read time plus %v", slackName, first.Expires, ownerChangeHeard)
		}
		if lists, calls := w.dispatch.PrivateProjectsCalls(), w.dispatch.Calls(); calls != lists || lists != 1 {
			t.Errorf("the release with no private project asked Dispatch %d time(s), %d of them for the list of private projects; want the list alone, once", calls, lists)
		}

		private := privateRow([]string{privChannel}, slackPerson)
		private.SlackChannelsChangedAt = w.clock.now()
		w.dispatch.SetRow(privKey, private)
		w.clock.advance(ownerChangeHeard - mintAgainBefore + projectaccess.CacheTTL + time.Second)
		p.send(t, readOf(privChannel)).refusedAs(t, "a read of "+privChannel+" once PRIV is private and names it", readOf(privChannel), codePrivateChannel, privateChannel)
	})
}

// TestAPrivateCredentialWaitsForTheSettleTime: pods P and Q share one store, as a broker's pods do,
// and the database's time follows the test's clock (proxyClock.db). P serves a session naming no
// private project and reads the channel just before T. At T PRIV's owner adds the channel (row 1),
// or PUB, which named it while public, turns private (row 2). Q, a session naming the project, is
// released at T+1 s; its release reads and keeps Dispatch's list before it reads the stored list,
// so the store holds the channel from S, the database's time of the first read that named it, which
// the test reads from the store row. At every 30 s step until S + slackChannelSettle, P's read and
// Q's post are never both carried, and Q's refusal names S + slackChannelSettle, the later of
// Dispatch's time and S; after it, Q posts and P is refused. Its control is by hand: a channelsOf
// that sets no SettlesAt finds Q posting at T+1 s while P reads the channel, naming it.
func TestAPrivateCredentialWaitsForTheSettleTime(t *testing.T) {
	for _, row := range []struct {
		name string
		// start is Dispatch's rows before T, given T0; change is the row the change at T makes,
		// which the run stamps T.
		start  func(t0 time.Time) map[string]projectaccesstest.Row
		change func() (string, projectaccesstest.Row)
		// project is the private session's project.
		project string
	}{
		{
			name: "PRIV's owner adds a channel",
			start: func(time.Time) map[string]projectaccesstest.Row {
				return map[string]projectaccesstest.Row{privKey: privateRow([]string{privChannel}, slackPerson)}
			},
			change: func() (string, projectaccesstest.Row) {
				return privKey, privateRow([]string{privChannel, addedChannel}, slackPerson)
			},
			project: privKey,
		},
		{
			name: "PUB turns private",
			start: func(t0 time.Time) map[string]projectaccesstest.Row {
				return map[string]projectaccesstest.Row{pubKey: {Visibility: "public", Owner: slackOwner, Listed: []string{slackPerson}, SlackChannels: []string{addedChannel}, SlackChannelsChangedAt: t0}}
			},
			change: func() (string, projectaccesstest.Row) {
				return pubKey, privateRow([]string{addedChannel}, slackPerson)
			},
			project: pubKey,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			if overlap := settleRun(t, row.start, row.change, row.project); overlap != "" {
				t.Fatal(overlap)
			}
		})
	}
}

// settleRun is one row of the settle test: Dispatch holds start's rows (T0 an hour before the run),
// P reads addedChannel just before T, the row's change is made at T and stamped T, and Q, a session
// naming project, is released at T+1 s. It reads S from the store, steps the clock 30 s at a time
// to S + channelSettle, P reading the channel and Q posting to it at each step, and answers the
// first step at which both reached Slack, naming the channel; "" when none did, after which Q posts
// and P is refused.
func settleRun(t *testing.T, start func(time.Time) map[string]projectaccesstest.Row, change func() (string, projectaccesstest.Row), project string) string {
	t.Helper()
	t0 := time.Now().Add(-time.Hour)
	w := newFenceWorld(t, start(t0), true)
	p := w.pod(t, "public")
	read := readOf(addedChannel)
	p.send(t, read).served(t, "P's read of "+addedChannel+" just before T", read, slackValue)

	w.clock.advance(time.Second)
	at := w.clock.now()
	key, changed := change()
	changed.SlackChannelsChangedAt = at
	w.dispatch.SetRow(key, changed)

	w.clock.advance(time.Second)
	q := w.pod(t, "private", project)
	post := jsonPost(postTo(addedChannel))
	first, pFirst := q.send(t, post), p.send(t, read)
	stored := storedAt(t, w.st, key, addedChannel)
	settles := stored.Add(channelSettle)
	if dispatchSettles := at.Add(channelSettle); dispatchSettles.After(settles) {
		settles = dispatchSettles
	}
	check := func(step string, pAnswer, qAnswer slackAnswer) string {
		pServed, qServed := pAnswer.status == http.StatusOK && len(pAnswer.seen) == 1, qAnswer.status == http.StatusOK && len(qAnswer.seen) == 1
		if pServed && qServed {
			return fmt.Sprintf("at %s, Q posted to %s while P read it: P answered %d, Q answered %d", step, addedChannel, pAnswer.status, qAnswer.status)
		}
		if !pServed {
			pAnswer.refusedAs(t, fmt.Sprintf("P's read of %s at %s", addedChannel, step), read, codePrivateChannel, privateChannel)
		}
		if !qServed {
			qAnswer.refusedAs(t, fmt.Sprintf("Q's post to %s at %s", addedChannel, step), post, codeChannelNotAllowed, channelNotAllowed)
			if want := channelsSettle + settles.UTC().Format(time.RFC3339); !strings.Contains(qAnswer.body, want) {
				t.Fatalf("Q's refusal at %s reads %q; want it to say posting reopens at %s, the later of Dispatch's time and the store's %v", step, qAnswer.body, settles.UTC().Format(time.RFC3339), stored)
			}
		}
		return ""
	}
	if overlap := check("T+1s", pFirst, first); overlap != "" {
		return overlap
	}
	for step := 30 * time.Second; stored.Add(step).Before(settles); step += 30 * time.Second {
		advanceTo(w.clock, stored.Add(step))
		if overlap := check(fmt.Sprintf("S+%v", step), p.send(t, read), q.send(t, post)); overlap != "" {
			return overlap
		}
	}
	advanceTo(w.clock, settles.Add(time.Second))
	q.send(t, post).served(t, "Q's post to "+addedChannel+" once its list settled", post, slackValue)
	p.send(t, read).refusedAs(t, "P's read of "+addedChannel+" once the list settled", read, codePrivateChannel, privateChannel)
	return ""
}

// storedAt is when st's stored private list first held channel for key, on the database's clock,
// failing t when it does not hold it.
func storedAt(t *testing.T, st *store.Store, key, channel string) time.Time {
	t.Helper()
	known, err := st.KnownPrivateProjects(t.Context())
	if err != nil {
		t.Fatalf("KnownPrivateProjects: %v", err)
	}
	for _, project := range known {
		if project.Key == key && slices.Contains(project.SlackChannels, channel) {
			return project.ChannelsStoredAt
		}
	}
	t.Fatalf("the stored private list does not hold %s for %s after Q's first release: %+v", channel, key, known)
	return time.Time{}
}

// advanceTo moves c on to at, and leaves it where it is when it has passed at already.
func advanceTo(c *proxyClock, at time.Time) {
	if ahead := at.Sub(c.now()); ahead > 0 {
		c.advance(ahead)
	}
}

// fenceWorld is the broker the fence's real-machine tests run: a store, the Slack token's policy,
// a stand-in Dispatch and a clock, which every pod of a test shares as a broker's pods share them.
type fenceWorld struct {
	st       *store.Store
	local    *secrets.Local
	policy   *policy.Current
	rules    proxyrules.Rules
	dispatch *projectaccesstest.Server
	clock    *proxyClock
}

// newFenceWorld builds it, with Dispatch answering rows and the token tagged project-channels=slack
// when tagged; its clock moves the database's time with the proxy's. It skips t when
// BROKER_TEST_DATABASE_URL is unset (storetest.Open).
func newFenceWorld(t *testing.T, rows map[string]projectaccesstest.Row, tagged bool) *fenceWorld {
	t.Helper()
	st := storetest.Open(t)
	w := &fenceWorld{st: st, rules: slackRules(), dispatch: projectaccesstest.NewServer(t, rows, nil), clock: &proxyClock{db: &st.DatabaseClockOffset}}
	w.local = secrets.NewLocal(slackToken(tagged))
	loader := policytest.Loader(w.local)
	loader.ProjectChecks = true
	loader.AirlockDigest = w.rules.Digest
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	w.policy = current
	return w
}

// slackToken is the token as the store holds it: shared, agent tier, and tagged
// project-channels=slack when tagged.
func slackToken(tagged bool) secrets.LocalSecret {
	token := policytest.Secret(slackName, policy.OwnerShared, policy.TierAgent, slackValue)
	if tagged {
		token.Tags[policy.TagProjectChannels] = policy.ProjectChannelsSlack
	}
	return token
}

// retag tags the token project-channels=slack in the store and has the broker read it again, as a
// retag the console makes and the broker's reread of the name do.
func (w *fenceWorld) retag(t *testing.T) {
	t.Helper()
	w.local.Put(slackToken(true))
	if lookup, err := w.policy.RefreshOne(t.Context(), slackName); err != nil || w.policy.Get().Secrets[slackName].ProjectChannels != policy.ProjectChannelsSlack {
		t.Fatalf("RefreshOne(%s) = %+v, %v; want the token read again carrying project-channels", slackName, lookup, err)
	}
}

// fencePod is one broker pod of a fenceWorld: its own request machine and client of Dispatch, the
// proxy in front of them, and one session of slackPerson's released the token on it.
type fencePod struct {
	*rig
	grants   *readsOf
	client   *http.Client
	password string
}

// pod builds one with a client of Dispatch's project access, its session on a runtime of its own
// named name, naming projects.
func (w *fenceWorld) pod(t *testing.T, name string, projects ...string) *fencePod {
	t.Helper()
	return w.newPod(t, name, true, projects...)
}

// newPod builds one, with a client of the stand-in Dispatch when access is true, and with none
// otherwise, a broker with no BROKER_PROJECT_ACCESS_URL.
func (w *fenceWorld) newPod(t *testing.T, name string, access bool, projects ...string) *fencePod {
	t.Helper()
	m := &requests.Machine{
		Store: w.st, Policy: w.policy, Secrets: policytest.Store(w.local), PendingTTL: 12 * time.Hour,
		Audience: slackAudience, Skew: time.Minute, Replay: replayOnce(w.st),
		Chain:   requests.NewChainVerifier(w.st, slackAudience, time.Minute, w.rules),
		Airlock: w.rules,
		// Two strings the release carries through unread, as newSlackBroker's.
		AirlockProxyURL:      "http://127.0.0.1:13381",
		AirlockCACertificate: "-----BEGIN CERTIFICATE-----\nslack-test-airlock-ca\n-----END CERTIFICATE-----\n",
		Now:                  w.clock.now,
	}
	if access {
		dispatch := &podDispatch{clock: w.clock, server: w.dispatch}
		m.Projects, m.PrivateList = dispatch, dispatch
	}
	password := releaseSlack(t, m, w.st, "box-fence-"+name+"-"+t.Name(), "post the release notes", projects)
	grants := &readsOf{Machine: m}
	r := newRig(t, Options{Rules: w.rules, Grants: grants, Now: w.clock.now})
	return &fencePod{rig: r, grants: grants, client: r.client(password), password: password}
}

// send sends p as the pod's session's command does.
func (p *fencePod) send(t *testing.T, call slackPost) slackAnswer {
	t.Helper()
	return sendSlack(t, p.rig, p.client, p.password, call)
}

// readsOf is a pod's request machine as its proxy reads it, keeping every value AirlockValues
// answered, and the grant it was asked for, so a row reads what the proxy held and how often it
// read the grant.
type readsOf struct {
	*requests.Machine
	mu      sync.Mutex
	reads   []map[string]requests.AirlockSecret
	grantID string
}

func (g *readsOf) AirlockValues(ctx context.Context, grantID string) (map[string]requests.AirlockSecret, string, error) {
	values, version, err := g.Machine.AirlockValues(ctx, grantID)
	if err == nil {
		g.mu.Lock()
		g.reads, g.grantID = append(g.reads, values), grantID
		g.mu.Unlock()
	}
	return values, version, err
}

// again reads the grant the proxy last read once more, as the proxy's read of another grant on
// the same pod would, and fails t when it cannot.
func (g *readsOf) again(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	grantID := g.grantID
	g.mu.Unlock()
	if grantID == "" {
		t.Fatal("the proxy has read no grant yet, so there is none to read again")
	}
	if _, _, err := g.AirlockValues(t.Context(), grantID); err != nil {
		t.Fatalf("AirlockValues(%s) again: %v", grantID, err)
	}
}

// all is every value read so far, in order.
func (g *readsOf) all() []map[string]requests.AirlockSecret {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.reads)
}

// last is the latest value read.
func (g *readsOf) last() map[string]requests.AirlockSecret {
	reads := g.all()
	if len(reads) == 0 {
		return nil
	}
	return reads[len(reads)-1]
}

// podDispatch is one pod's client of the stand-in Dispatch, whose cache ages by the test's clock
// rather than the wall's: a real projectaccess.Client, replaced by a new one, which holds no
// answer, once the clock has moved projectaccess.CacheTTL past when the one it replaces was made.
// That is the cache a running pod holds, whose answers the clock a test moves would otherwise keep
// young for ever, so a row that moves the proxy's clock moves this one with it.
type podDispatch struct {
	clock  *proxyClock
	server *projectaccesstest.Server
	mu     sync.Mutex
	client *projectaccess.Client
	born   time.Time
}

func (d *podDispatch) current() *projectaccess.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.now()
	if d.client == nil || now.Sub(d.born) >= projectaccess.CacheTTL {
		d.client, d.born = projectaccess.New(d.server.URL, d.server.Account, nil), now
	}
	return d.client
}

func (d *podDispatch) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return d.current().Access(ctx, project, person)
}

func (d *podDispatch) MemberKind(ctx context.Context, person string) (string, error) {
	return d.current().MemberKind(ctx, person)
}

func (d *podDispatch) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return d.current().States(ctx, projects)
}

func (d *podDispatch) PrivateProjects(ctx context.Context) ([]projectaccess.PrivateProject, error) {
	return d.current().PrivateProjects(ctx)
}

func (d *podDispatch) PrivateProjectsAsOf(ctx context.Context) ([]projectaccess.PrivateProject, time.Time, error) {
	return d.current().PrivateProjectsAsOf(ctx)
}

func (d *podDispatch) PrivateProjectsFresh(ctx context.Context) ([]projectaccess.PrivateProject, error) {
	return d.current().PrivateProjectsFresh(ctx)
}
