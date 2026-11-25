// packages/envoy/internal/cipher/proxy/slack_test.go
//
// #t-d-12's proxy half (dispatch://INFRA-3862): a private project's grant of the Legion Slack bot
// token spends it only on POST /api/chat.postMessage, carrying channel, thread_ts and text, to a
// channel its project's owner named (slackPostAllowed). The first test drives every request shape
// through the rig's fake grants, and the three ways a list comes back empty through the real
// request machine; the three after it put that machine behind the proxy, on a store and a stand-in
// Dispatch of their own, so what they read is what the broker answers once an owner changes the
// list, a person loses the project, or a project the session names turns private.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// The Slack bot token as the broker releases it, and the channels the rows post to: two PRIV's
// owner named, one nobody named, and one TEAM's owner alone named.
const (
	slackName   = "SLACK_TOKEN"
	slackValue  = "the-real-slack-bot-value"
	channelA    = "C0ALLOWED1"
	channelB    = "C0ALLOWED2"
	unlisted    = "C0UNLISTED"
	teamOnly    = "C0TEAMONLY"
	slackThread = "1731700000.000100"
	// slackJSON is the Content-Type slack-post.ts sends (packages/envoy-client/src/slack-post.ts).
	slackJSON = "application/json; charset=utf-8"
	slackForm = "application/x-www-form-urlencoded"
)

// The placeholders of the fake grants, in the shape the broker mints them.
var (
	limitedPlaceholder   = placeholder.Prefix + "LimitedLimitedLimitedLimitedLim1"
	unlimitedPlaceholder = placeholder.Prefix + "UnlimitedUnlimitedUnlimitedUnli1"
	noListPlaceholder    = placeholder.Prefix + "NoListNoListNoListNoListNoListN1"
)

// slackKindJSON is the deployment's slack kind as the profiles file declares it, on the rig's
// host: the token swapped into Authorization, with no allow list, so the rules alone let it travel
// on any method and path.
const slackKindJSON = `[{"name": "slack", "hosts": ["` + boundHost + `"], "headers": ["authorization"], "ways": ["swap"]}]`

// slackRules is the airlock's rules of that kind with the token's name bound to it, a stored
// value swapped in.
func slackRules() proxyrules.Rules {
	return proxyrules.Rules{
		Kinds: []proxyrules.Kind{swapKind("slack", []string{boundHost}, "authorization")},
		Names: []proxyrules.Binding{swapBinding(slackName, "slack")},
	}
}

// slackPost is one request a row sends: its method, its path with any query, its headers in
// order (so a row can send one twice), and its body.
type slackPost struct {
	method, path string
	header       [][2]string
	body         string
}

func (p slackPost) String() string {
	return fmt.Sprintf("%s %s %v with the body %s", p.method, p.path, p.header, clip(p.body))
}

// clip is a body as a failure prints it: whole when short, its head and its length when not.
func clip(body string) string {
	if len(body) <= 160 {
		return fmt.Sprintf("%q", body)
	}
	return fmt.Sprintf("%q... (%d bytes)", body[:160], len(body))
}

// jsonPost is body posted to chat.postMessage as slack-post.ts posts it.
func jsonPost(body string) slackPost {
	return slackPost{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", slackJSON}}, body: body}
}

// formPost is body posted to chat.postMessage as a form.
func formPost(body string) slackPost {
	return slackPost{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", slackForm}}, body: body}
}

// postTo is slack-post.ts's own body for channel: JSON.stringify of channel, thread_ts and text.
func postTo(channel string) string {
	return `{"channel":"` + channel + `","thread_ts":"` + slackThread + `","text":"the release notes are out"}`
}

// paddedTo is a post to channelA whose text pads the whole body to exactly size bytes.
func paddedTo(size int) string {
	head, tail := `{"channel":"`+channelA+`","text":"`, `"}`
	return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
}

// gzipped is body compressed, as a client that sets Content-Encoding: gzip sends it.
func gzipped(t *testing.T, body string) string {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// multipartPost is a post to channelA as multipart/form-data, which Slack also reads.
func multipartPost(t *testing.T) slackPost {
	t.Helper()
	var out bytes.Buffer
	w := multipart.NewWriter(&out)
	for _, field := range [][2]string{{"channel", channelA}, {"text", "the release notes are out"}} {
		if err := w.WriteField(field[0], field[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return slackPost{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", w.FormDataContentType()}}, body: out.String()}
}

// slackRow is one named request.
type slackRow struct {
	name string
	post slackPost
}

// servedPosts are the posts a grant limited to channelA and channelB carries: slack-post.ts's own,
// and the other shapes the rules allow.
func servedPosts() []slackRow {
	return []slackRow{
		{"slack-post.ts's own post to a listed channel", jsonPost(postTo(channelA))},
		{"a form post to a listed channel", formPost("channel=" + channelB + "&thread_ts=" + slackThread + "&text=the+release+notes+are+out")},
		{"charset=UTF-8", slackPost{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", "application/json; charset=UTF-8"}}, body: postTo(channelA)}},
		{"a post naming no thread, as JSON.stringify leaves an undefined one out", jsonPost(`{"channel":"` + channelB + `","text":"the release notes are out"}`)},
		{"a body of exactly 1 MiB", jsonPost(paddedTo(maxSlackPostBody))},
	}
}

// refusedPosts are the posts that grant must not carry, each refused by one rule.
func refusedPosts(t *testing.T) []slackRow {
	t.Helper()
	jsonType := [][2]string{{"Content-Type", slackJSON}}
	return []slackRow{
		{"an unlisted channel", jsonPost(postTo(unlisted))},
		{"a channel name, which only Slack resolves", jsonPost(postTo("#general"))},
		{"chat.update", slackPost{method: http.MethodPost, path: "/api/chat.update", header: jsonType, body: postTo(channelA)}},
		{"files.uploadV2", slackPost{method: http.MethodPost, path: "/api/files.uploadV2", header: jsonType, body: postTo(channelA)}},
		{"GET /api/conversations.history", slackPost{method: http.MethodGet, path: "/api/conversations.history?channel=" + channelA}},
		{"a GET of chat.postMessage", slackPost{method: http.MethodGet, path: slackPostPath, header: jsonType, body: postTo(channelA)}},
		{"a query that carries channel", slackPost{method: http.MethodPost, path: slackPostPath + "?channel=" + unlisted, header: jsonType, body: postTo(channelA)}},
		{"a path spelled with an escape", slackPost{method: http.MethodPost, path: "/api/chat%2EpostMessage", header: jsonType, body: postTo(channelA)}},
		{"a body over 1 MiB", jsonPost(paddedTo(maxSlackPostBody + 1))},
		{"Content-Encoding: gzip", slackPost{method: http.MethodPost, path: slackPostPath,
			header: [][2]string{{"Content-Type", slackJSON}, {"Content-Encoding", "gzip"}}, body: gzipped(t, postTo(channelA))}},
		{"two Content-Type headers", slackPost{method: http.MethodPost, path: slackPostPath,
			header: [][2]string{{"Content-Type", slackJSON}, {"Content-Type", slackForm}}, body: postTo(channelA)}},
		{"no Content-Type", slackPost{method: http.MethodPost, path: slackPostPath, body: postTo(channelA)}},
		{"a text/plain body", slackPost{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", "text/plain; charset=utf-8"}}, body: postTo(channelA)}},
		{"a multipart/form-data body", multipartPost(t)},
		{"a charset other than utf-8", slackPost{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", "application/json; charset=iso-8859-1"}}, body: postTo(channelA)}},
		{"a Content-Type parameter other than charset", slackPost{method: http.MethodPost, path: slackPostPath, header: [][2]string{{"Content-Type", "application/json; boundary=x"}}, body: postTo(channelA)}},
		{"JSON naming channel and CHANNEL", jsonPost(`{"channel":"` + unlisted + `","CHANNEL":"` + channelA + `","text":"x"}`)},
		{"JSON repeating channel", jsonPost(`{"channel":"` + channelA + `","channel":"` + unlisted + `","text":"x"}`)},
		{"JSON carrying metadata", jsonPost(`{"channel":"` + channelA + `","text":"x","metadata":{"event_type":"release","event_payload":{"notes":"the private notes"}}}`)},
		{"JSON carrying blocks", jsonPost(`{"channel":"` + channelA + `","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"x"}}]}`)},
		{"JSON with a numeric channel", jsonPost(`{"channel":12345,"text":"x"}`)},
		{"JSON naming no channel", jsonPost(`{"text":"x"}`)},
		{"JSON that is no object", jsonPost(`["` + channelA + `"]`)},
		{"a second JSON value after the object", jsonPost(postTo(channelA) + `{"channel":"` + unlisted + `"}`)},
		{"a form naming channel again as chann%65l", formPost("channel=" + unlisted + "&chann%65l=" + channelA + "&text=x")},
		{"a form giving channel twice", formPost("channel=" + channelA + "&channel=" + channelB + "&text=x")},
		{"a form naming another key", formPost("channel=" + channelA + "&text=x&reply_broadcast=true")},
	}
}

// slackAnswer is what one request through the proxy came back with: the status and body the
// command read, what the stand-in service got for it, and what the proxy recorded refusing it.
type slackAnswer struct {
	status   int
	body     string
	seen     []seenRequest
	refusals []logLine
}

// sendSlack is p sent through the proxy on client, carrying placeholder where slack-post.ts carries
// the token: Authorization: Bearer.
func sendSlack(t *testing.T, r *rig, client *http.Client, placeholder string, p slackPost) slackAnswer {
	t.Helper()
	var body io.Reader
	if p.body != "" {
		body = strings.NewReader(p.body)
	}
	request, err := http.NewRequest(p.method, "https://"+boundHost+p.path, body)
	if err != nil {
		t.Fatalf("build %s: %v", p, err)
	}
	request.Header.Set("Authorization", "Bearer "+placeholder)
	for _, h := range p.header {
		request.Header.Add(h[0], h[1])
	}
	return sendRequest(t, r, client, request)
}

// sendRequest is request sent through the proxy on client, and what came back.
func sendRequest(t *testing.T, r *rig, client *http.Client, request *http.Request) slackAnswer {
	t.Helper()
	seen, refusals := len(r.requests()), len(r.logs.refusals())
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s through the proxy: %v", request.Method, request.URL, err)
	}
	read := bodyOf(t, answer)
	return slackAnswer{status: answer.StatusCode, body: read, seen: r.requests()[seen:], refusals: r.logs.refusals()[refusals:]}
}

// served fails t unless p reached the stand-in once, as it was sent, with value swapped in as its
// token.
func (a slackAnswer) served(t *testing.T, what string, p slackPost, value string) {
	t.Helper()
	if a.status != http.StatusOK || len(a.seen) != 1 {
		t.Fatalf("%s: %s answered %d %q and reached the stand-in %d time(s); want it served once", what, p, a.status, a.body, len(a.seen))
	}
	got := a.seen[0]
	// The stand-in reads the path decoded, as the service would.
	escaped, query, _ := strings.Cut(p.path, "?")
	path, err := url.PathUnescape(escaped)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got.Method != p.method || got.Path != path || got.Query != query {
		t.Errorf("%s: the stand-in got %s %s?%s; want %s", what, got.Method, got.Path, got.Query, p)
	}
	if want := "Bearer " + value; got.Header.Get("Authorization") != want {
		t.Errorf("%s: the stand-in got Authorization %q; want the real token swapped in", what, got.Header.Get("Authorization"))
	}
	if got.Body != p.body {
		t.Errorf("%s: the stand-in got the body %s; want %s byte for byte", what, clip(got.Body), clip(p.body))
	}
}

// refused fails t unless p was answered 403 CHANNEL_NOT_ALLOWED, in the proxy's refusal shape and
// with its one refusal record, and reached no service.
func (a slackAnswer) refused(t *testing.T, what string, p slackPost) {
	t.Helper()
	if a.status != http.StatusForbidden || len(a.seen) != 0 {
		t.Fatalf("%s: %s answered %d %q and reached the stand-in %d time(s); want 403 %s and no call", what, p, a.status, a.body, len(a.seen), codeChannelNotAllowed)
	}
	if !strings.HasPrefix(a.body, "airlock: "+channelNotAllowed) {
		t.Errorf("%s: the refusal reads %q; want the airlock's words for %s", what, a.body, codeChannelNotAllowed)
	}
	if len(a.refusals) != 1 || a.refusals[0].fields["code"] != codeChannelNotAllowed || a.refusals[0].fields["status"] != int64(http.StatusForbidden) {
		t.Errorf("%s: the proxy recorded %v; want one refusal, 403 %s", what, a.refusals, codeChannelNotAllowed)
	}
}

// grantSlack makes placeholder the password of a fake grant holding the token, released as secret.
func grantSlack(r *rig, placeholder, grantID string, secret requests.AirlockSecret) {
	r.grants.setConnection(placeholder, requests.AirlockConnection{
		GrantID: grantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{slackName: {Slots: map[string]string{requests.SlotValue: placeholder}}},
	})
	r.grants.setValues(grantID, map[string]requests.AirlockSecret{slackName: secret})
}

// TestAPrivateProjectsSlackGrantPostsOnlyToItsChannels: a Limited grant of the token carries
// slack-post.ts's post to a channel on its list and refuses every other request 403
// CHANNEL_NOT_ALLOWED before any of it reaches Slack; a Limited grant whose list is empty or
// missing, and a Limited name whose binding is no header swap, carry nothing; an unlimited grant
// carries every one of those requests as it did before the limit existed.
func TestAPrivateProjectsSlackGrantPostsOnlyToItsChannels(t *testing.T) {
	r := newRuledRig(t, slackRules(), Options{})
	value := map[string]string{requests.SlotValue: slackValue}
	grantSlack(r, limitedPlaceholder, "grant-slack-limited", requests.AirlockSecret{Slots: value, Limited: true, Channels: []string{channelA, channelB}})
	grantSlack(r, unlimitedPlaceholder, "grant-slack-unlimited", requests.AirlockSecret{Slots: value})
	grantSlack(r, noListPlaceholder, "grant-slack-no-list", requests.AirlockSecret{Slots: value, Limited: true})
	limited, unlimited := r.client(limitedPlaceholder), r.client(unlimitedPlaceholder)
	served, refused := servedPosts(), refusedPosts(t)

	for _, row := range served {
		t.Run("served/"+row.name, func(t *testing.T) {
			sendSlack(t, r, limited, limitedPlaceholder, row.post).served(t, row.name, row.post, slackValue)
		})
	}
	for _, row := range refused {
		t.Run("refused/"+row.name, func(t *testing.T) {
			sendSlack(t, r, limited, limitedPlaceholder, row.post).refused(t, row.name, row.post)
		})
	}
	for _, row := range append(served, refused...) {
		t.Run("unlimited/"+row.name, func(t *testing.T) {
			sendSlack(t, r, unlimited, unlimitedPlaceholder, row.post).served(t, row.name+", by a grant that is not Limited", row.post, slackValue)
		})
	}

	t.Run("Limited with no list at all", func(t *testing.T) {
		post := jsonPost(postTo(channelA))
		sendSlack(t, r, r.client(noListPlaceholder), noListPlaceholder, post).refused(t, "a Limited grant whose Channels is nil", post)
	})

	t.Run("a Limited name whose binding is no header swap", func(t *testing.T) {
		aws := newAWSRig(t, false)
		values := map[string]string{requests.SlotAWSAccessKeyID: awsRealKeyID, requests.SlotAWSSecretAccessKey: awsRealSecret}
		aws.grants.setValues(testGrantID, map[string]requests.AirlockSecret{awsName: {Slots: values, Limited: true, Channels: []string{channelA}}})
		// A call signed with the placeholder key, which the sign way would sign again and send on.
		post := jsonPost(postTo(channelA))
		request := awsRequest(t, post.method, "https://"+boundHost+post.path, post.body, "execute-api", false, "Content-Type", slackJSON)
		sendRequest(t, aws, aws.client(awsKeyPlaceholder), request).refused(t, "a Limited name of way sign", post)
	})

	// The three ways the broker answers Limited with an empty list, each read from the real
	// machine: a post to a channel that looks listed is refused in every one.
	for _, c := range []struct {
		name     string
		rows     map[string]projectaccesstest.Row
		projects []string
	}{
		{"the owner named no channel", map[string]projectaccesstest.Row{privKey: privateRow(nil, slackPerson)}, []string{privKey}},
		{"the two private projects share no channel", map[string]projectaccesstest.Row{
			privKey: privateRow([]string{channelA}, slackPerson),
			teamKey: privateRow([]string{teamOnly}, slackPerson),
		}, []string{privKey, teamKey}},
		{"Dispatch answered no slack_channels key", map[string]projectaccesstest.Row{privKey: {
			Visibility: "private", Owner: slackOwner, Listed: []string{slackPerson}, OmitSlackChannels: true,
		}}, []string{privKey}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newSlackBroker(t, c.rows, c.projects...)
			post := jsonPost(postTo(channelA))
			b.post(t, post).refused(t, c.name, post)
		})
	}
}

// ownerChangeHeard is how long a running proxy may go on spending an owner's old list: the
// release's own bound on a project-channels secret released to a credential that names a project,
// Limited or not (requests.airlockProjectRecheck, which that package keeps to itself).
const ownerChangeHeard = 10 * time.Minute

// TestAPrivateProjectsSlackGrantHearsTheOwnersChangeWithinTenMinutes: an owner who takes a channel
// off PRIV's list reaches a proxy already holding the token. Once its clock passes the release's
// bound less the proxy's own margin (mintAgainBefore), the proxy runs the release checks again,
// reads the new list, refuses the channel the owner took off and carries the one they kept.
func TestAPrivateProjectsSlackGrantHearsTheOwnersChangeWithinTenMinutes(t *testing.T) {
	b := newSlackBroker(t, map[string]projectaccesstest.Row{privKey: privateRow([]string{channelA, channelB}, slackPerson)}, privKey)
	toA := jsonPost(postTo(channelA))
	b.post(t, toA).served(t, "a post to "+channelA+" before the owner's change", toA, slackValue)

	changed := b.dispatch.serve(t, map[string]projectaccesstest.Row{privKey: privateRow([]string{channelB}, slackPerson)})
	b.clock.advance(ownerChangeHeard - mintAgainBefore + time.Second)

	b.post(t, toA).refused(t, "a post to "+channelA+" after the owner took it off and the proxy's clock moved on", toA)
	if changed.Calls() == 0 {
		t.Fatal("the refusal came from no read of the changed list: the stand-in Dispatch holding it was never asked")
	}
	toB := jsonPost(postTo(channelB))
	b.post(t, toB).served(t, "a post to "+channelB+", which the owner kept", toB, slackValue)
}

// TestAPrivateProjectsSlackGrantStaysLimitedWhenItsPersonLosesTheProject: the session's person is
// taken off PRIV's access list while a proxy holds the token. The limit is read from the projects
// the credential names, not the ones its person can still see, so once the proxy reads the grant
// again it is still Limited, to no channel at all: a channel nobody listed is refused, and so is
// the one PRIV's owner listed.
func TestAPrivateProjectsSlackGrantStaysLimitedWhenItsPersonLosesTheProject(t *testing.T) {
	b := newSlackBroker(t, map[string]projectaccesstest.Row{privKey: privateRow([]string{channelA}, slackPerson, slackInsider)}, privKey)
	toA := jsonPost(postTo(channelA))
	b.post(t, toA).served(t, "a post to "+channelA+" while the person is on PRIV's list", toA, slackValue)

	lost := b.dispatch.serve(t, map[string]projectaccesstest.Row{privKey: privateRow([]string{channelA}, slackInsider)})
	b.clock.advance(ownerChangeHeard - mintAgainBefore + time.Second)

	toUnlisted := jsonPost(postTo(unlisted))
	b.post(t, toUnlisted).refused(t, "a post to "+unlisted+", which no owner listed, after the person lost PRIV", toUnlisted)
	if lost.Calls() == 0 {
		t.Fatal("the refusal came from no read of the changed list: the stand-in Dispatch holding it was never asked")
	}
	b.post(t, toA).refused(t, "a post to "+channelA+" after the person lost PRIV", toA)
}

// TestAPrivateProjectsSlackGrantIsLimitedOnceItsProjectTurnsPrivate: a session names PRIV while
// PRIV is public, so the proxy holds the token unlimited and carries a post to a channel nobody
// listed. PRIV's owner then makes PRIV private. Whether a project is private is Dispatch's and
// moves no policy version, so the value the proxy holds is bounded all the same, because its
// credential names a project: an hour on, the proxy has read the grant again, finds it Limited to
// the channels PRIV's owner named, refuses the unlisted channel and carries the listed one.
func TestAPrivateProjectsSlackGrantIsLimitedOnceItsProjectTurnsPrivate(t *testing.T) {
	public := projectaccesstest.Row{Visibility: "public", Owner: slackOwner, SlackChannels: []string{channelA}}
	b := newSlackBroker(t, map[string]projectaccesstest.Row{privKey: public}, privKey)
	toUnlisted := jsonPost(postTo(unlisted))
	b.post(t, toUnlisted).served(t, "a post to "+unlisted+" while PRIV is public", toUnlisted, slackValue)

	private := b.dispatch.serve(t, map[string]projectaccesstest.Row{privKey: privateRow([]string{channelA}, slackPerson)})
	b.clock.advance(time.Hour)

	b.post(t, toUnlisted).refused(t, "a post to "+unlisted+" an hour after PRIV turned private", toUnlisted)
	if private.Calls() == 0 {
		t.Fatal("the refusal came from no read of PRIV turned private: the stand-in Dispatch holding it was never asked")
	}
	toA := jsonPost(postTo(channelA))
	b.post(t, toA).served(t, "a post to "+channelA+", which PRIV's owner named", toA, slackValue)
}

// The real machine's projects and people: PRIV and TEAM are private and slackOwner's; the
// session is slackPerson's; slackInsider is someone else on a list.
const (
	privKey       = "PRIV"
	teamKey       = "TEAM"
	slackOwner    = "alice@example.com"
	slackPerson   = "sami@example.com"
	slackInsider  = "carol@example.com"
	slackAudience = "broker"
)

// privateRow is a private project slackOwner owns, whose access list names listed and whose owner
// named channels.
func privateRow(channels []string, listed ...string) projectaccesstest.Row {
	return projectaccesstest.Row{Visibility: "private", Owner: slackOwner, Listed: listed, SlackChannels: channels}
}

// slackBroker is the real request machine behind the proxy: a store of its own, a policy serving
// the token tagged project-channels=slack and agent tier through a profile of its own name whose
// proxy block is slackRules' entry, a stand-in Dispatch, and one session of slackPerson's naming
// its projects, granted the token and released it as a command would be.
type slackBroker struct {
	*rig
	client   *http.Client
	password string
	dispatch *dispatchAt
	clock    *proxyClock
}

// post sends p as the session's command does.
func (b *slackBroker) post(t *testing.T, p slackPost) slackAnswer {
	t.Helper()
	return sendSlack(t, b.rig, b.client, b.password, p)
}

// newSlackBroker builds it, with Dispatch answering rows and the session naming projects. It skips
// t when BROKER_TEST_DATABASE_URL is unset (storetest.Open).
func newSlackBroker(t *testing.T, rows map[string]projectaccesstest.Row, projects ...string) *slackBroker {
	t.Helper()
	st := storetest.Open(t)
	// The token is behind the proxy by its own proxy tag, under the file's slack kind: no profile.
	file := policytest.KindsFileOf(slackKindJSON)
	token := policytest.Proxied(policytest.Secret(slackName, policy.OwnerShared, policy.TierAgent, slackValue), "slack")
	token.Tags[policy.TagProjectChannels] = policy.ProjectChannelsSlack
	local := secrets.NewLocal(token)
	loader := policytest.Loader(local)
	loader.ProjectChecks = true
	loader.Profiles = policytest.Profiles(t, file)
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	dispatch := &dispatchAt{}
	dispatch.serve(t, rows)
	m := &requests.Machine{
		Store: st, Policy: current, Secrets: policytest.Store(local), PendingTTL: 12 * time.Hour,
		Audience: slackAudience, Skew: time.Minute, Replay: replayOnce(st),
		Chain: requests.NewChainVerifier(st, slackAudience, time.Minute, current),
		// Two strings the release carries through unread: the command here is the rig's client,
		// which reaches the proxy and trusts its authority on its own.
		AirlockProxyURL:      "http://127.0.0.1:13381",
		AirlockCACertificate: "-----BEGIN CERTIFICATE-----\nslack-test-airlock-ca\n-----END CERTIFICATE-----\n",
		Projects:             dispatch,
	}

	enrollmentID, key := enrolSession(t, st, projects)
	compact, err := record.Sign(key, slackAudience, []record.AuthorizationDetail{{Type: "agent_secret", Identifier: slackName, Actions: []string{"inject"}}},
		"post the release notes (INFRA-3862)", "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	request, err := m.Create(t.Context(), enrollmentID, compact, "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create(%s) = %+v, %v; want a grant at once, as the token is shared and agent tier", slackName, request, err)
	}
	release, err := m.Values(t.Context(), *request.GrantID, enrollmentID, requests.ReleaseCall{})
	if err != nil || release.Airlock == nil {
		t.Fatalf("Values = %+v, %v; want %s held back behind the airlock", release, err, slackName)
	}

	clock := &proxyClock{}
	r := newRig(t, Options{Grants: m, Now: clock.now})
	return &slackBroker{rig: r, client: r.client(release.Airlock.ProxyPassword), password: release.Airlock.ProxyPassword, dispatch: dispatch, clock: clock}
}

// enrolSession writes a live session of slackPerson's naming projects, under a launcher credential
// of that person's, straight into the store, as requests' own tests write theirs, and answers the
// enrollment and the key its requests are signed with.
func enrolSession(t *testing.T, st *store.Store, projects []string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := uuid.New()
	if _, err := st.Pool.Exec(t.Context(), `insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,null,'devbox-test',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, slackPerson, uuid.NewString()); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	id := uuid.NewString()
	if _, err := st.Pool.Exec(t.Context(), `insert into enrollments (id, kind, runtime_id, operator, thumbprint, subject, launcher_credential_id, lease_expires_at, projects)
		values ($1,'session',$2,$3,$4,null,$5, now() + interval '1 hour', $6)`, id, "box-slack-"+t.Name(), slackPerson, thumbprint, credentialID, projects); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return id, key
}

// replayOnce is Machine.Replay over the proof_jtis table, as enroll.Service.Replay is.
func replayOnce(st *store.Store) func(context.Context, string, time.Time) (bool, error) {
	return func(ctx context.Context, jti string, expires time.Time) (bool, error) {
		_, err := st.Pool.Exec(ctx, `insert into proof_jtis (jti, expires_at) values ($1,$2)`, jti, expires)
		if store.IsUniqueViolation(err) {
			return false, nil
		}
		return err == nil, err
	}
}

// proxyClock is the proxy's Options.Now: the wall clock, moved on by what a row has advanced it.
// The machine behind it reads the wall clock, so a value it releases expires by that, and the
// proxy, reading this one, takes it as that much closer to its end.
type proxyClock struct{ ahead atomic.Int64 }

func (c *proxyClock) now() time.Time { return time.Now().Add(time.Duration(c.ahead.Load())) }

func (c *proxyClock) advance(d time.Duration) { c.ahead.Add(int64(d)) }

// dispatchAt is the machine's client of Dispatch's project-access routes (Machine.Projects): a
// real projectaccess.Client asking a projectaccesstest stand-in (projectaccesstest.Swappable). A
// row plays an owner's change by serving the changed rows from a stand-in of their own through a
// fresh client. That is what a running broker reads once its client's cache
// (projectaccess.CacheTTL, 30 seconds) has let the old answer go, which it has long before the
// proxy reads a grant again. The client's own clock cannot be moved from here (projectaccess sets
// it only in its own tests) and a stand-in's rows are fixed when it starts, so a fresh pair is the
// move, as requests' own answerFrom makes it.
type dispatchAt struct{ projectaccesstest.Swappable }

// serve points the machine at a stand-in Dispatch answering rows, and answers that stand-in.
func (d *dispatchAt) serve(t *testing.T, rows map[string]projectaccesstest.Row) *projectaccesstest.Server {
	t.Helper()
	stand := projectaccesstest.NewServer(t, rows, nil)
	d.Serve(stand)
	return stand
}
