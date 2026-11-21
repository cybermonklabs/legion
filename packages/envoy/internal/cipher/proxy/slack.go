// packages/envoy/internal/cipher/proxy/slack.go
// The checks a project-channels secret is spent under, a private project's Slack limit. The broker
// marks such a name Limited, with the channels every private project the credential names allows,
// when the credential names a private project, and otherwise gives it Excluded, every channel a
// private project's owner named (requests.AirlockSecret). serveBound runs slackPostAllowed on each
// request a Limited name would carry, and slackCallExcluded on each one a name with Excluded
// channels would carry, after the swap and before anything goes upstream. Both are Go code of this
// package's own, beside the Lean-modelled proxyrules.Decide and proxyrules.Rewrite rather than in
// them, so the model and its fuzz and mutation tests are unchanged.
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/requests"
)

const (
	// channelNotAllowed is that refusal's words; the rule the request broke follows them.
	channelNotAllowed = "a private project's grant of this secret posts only with POST /api/chat.postMessage, carrying channel, thread_ts and text, to a Slack channel its project's owner named"
	// channelsSettle ends a CHANNEL_NOT_ALLOWED refusal of a Limited name whose projects' list is
	// settling (requests.AirlockSecret.SettlesAt), before the time it settles at.
	channelsSettle = "; this project's channels changed, and posting to them reopens at "
	// privateChannel is the public fence's refusal's words (PRIVATE_CHANNEL); the rule the call
	// broke follows them.
	privateChannel = "this Slack channel belongs to a private project; only that project's credentials reach it"
	// privateChannelsUnread is the words of PRIVATE_CHANNELS_UNREAD, every call on a name whose
	// public fence the broker could not read (requests.AirlockSecret.ExcludedUnread).
	privateChannelsUnread = "the broker could not read which Slack channels belong to private projects, so this secret serves no Slack call until it can; it asks again within a minute"
	// slackPostPath is the one path a Limited name may be spent on.
	slackPostPath = "/api/chat.postMessage"
	// maxSlackPostBody is the largest body such a post may carry, and the largest body a call the
	// public fence reads may carry, 1 MiB: a message is a few kilobytes, and each check holds the
	// whole body in memory to read it before it goes on.
	maxSlackPostBody = 1 << 20
)

// channelRefusal is a CHANNEL_NOT_ALLOWED refusal's words for secret, which broke the rule why:
// ending, while the list of a private project the credential names is settling, with the time
// posting reopens.
func channelRefusal(secret requests.AirlockSecret, why string) string {
	text := channelNotAllowed + ": " + why
	if !secret.SettlesAt.IsZero() {
		text += channelsSettle + secret.SettlesAt.UTC().Format(time.RFC3339)
	}
	return text
}

// slackPostKeys are the keys such a post may carry, the three slack_post sends
// (packages/envoy-client/src/slack-post.ts). Every other key is refused, metadata first among
// them: Slack's reference says posted metadata "is accessible to any app or user who is a member
// of that workspace", so a post to an allowed channel could otherwise put a private project's
// content where every member reads it. blocks, attachments, reply_broadcast and every unknown
// key go the same way.
var slackPostKeys = map[string]bool{"channel": true, "thread_ts": true, "text": true}

// slackPostAllowed reads r as a post of a Limited name and answers its body when r is one, or why
// it is not. It passes only a request every rule below holds for, and the rules give the body one
// reading, the one a strict receiver makes:
//
//  1. the method is POST, the path is exactly /api/chat.postMessage as sent (no escape in it),
//     and there is no query, since serveBound forwards r.URL's path and query as they came;
//  2. there is exactly one Content-Type, which mime.ParseMediaType reads as application/json or
//     application/x-www-form-urlencoded with at most a charset parameter, utf-8 in any case;
//  3. there is no Content-Encoding, and the body, read once, is at most maxSlackPostBody;
//  4. a JSON body is walked token by token, never decoded into a struct, whose case-insensitive
//     key match would read {"channel":"C_OTHER","CHANNEL":"C_ALLOWED"} as the allowed channel
//     while Slack posts to the other: one object, whose keys, byte for byte, are among
//     slackPostKeys, none twice, channel among them, every value a string, and nothing after it
//     but white space;
//  5. a form body is url.ParseQuery's, keys compared once percent-decoded (chann%65l is channel),
//     each among slackPostKeys with exactly one value, channel among them;
//  6. channel is one of channels byte for byte, so a channel's name, which only Slack resolves,
//     is refused.
//
// The body answered is the bytes read, which the caller serves upstream in place of r's own, so
// the post Slack receives is the post this read.
func slackPostAllowed(r *http.Request, channels []string) (body []byte, why string) {
	switch {
	case r.Method != http.MethodPost:
		return nil, "the method is not POST"
	case r.URL.Path != slackPostPath || r.URL.RawPath != "":
		return nil, "the path is not " + slackPostPath
	case r.URL.RawQuery != "" || r.URL.ForceQuery:
		return nil, "the request carries a query"
	}
	mediaType, why := slackBodyType(r)
	if why != "" {
		return nil, why
	}
	if len(r.Header.Values("Content-Encoding")) != 0 {
		return nil, "the body carries a Content-Encoding"
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSlackPostBody+1))
	switch {
	case err != nil:
		return nil, "the body could not be read"
	case len(body) > maxSlackPostBody:
		return nil, "the body is larger than 1 MiB"
	}
	var channel string
	if mediaType == "application/json" {
		channel, why = jsonPostChannel(body)
	} else {
		channel, why = formPostChannel(body)
	}
	switch {
	case why != "":
		return nil, why
	case !slices.Contains(channels, channel):
		return nil, "the channel is not one its project's owner named"
	}
	return body, ""
}

// jsonPostChannel is the channel of a JSON post as rule 4 reads it, or why the body is not one.
func jsonPostChannel(body []byte) (string, string) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return "", "the JSON body is not one object"
	}
	seen := map[string]bool{}
	var channel string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", "the JSON body cannot be read"
		}
		// An object's key is always a string token; a body that is no JSON fails above.
		key, _ := token.(string)
		switch {
		case !slackPostKeys[key]:
			return "", "the JSON body carries a key other than channel, thread_ts and text"
		case seen[key]:
			return "", "the JSON body carries a key twice"
		}
		seen[key] = true
		token, err = decoder.Token()
		value, isString := token.(string)
		if err != nil || !isString {
			return "", "a value of the JSON body is not a string"
		}
		if key == "channel" {
			channel = value
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return "", "the JSON body cannot be read"
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", "something follows the JSON body's object"
	}
	if !seen["channel"] {
		return "", "the JSON body names no channel"
	}
	return channel, ""
}

// formPostChannel is the channel of a form post as rule 5 reads it, or why the body is not one.
func formPostChannel(body []byte) (string, string) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return "", "the form body cannot be read"
	}
	for key, values := range form {
		switch {
		case !slackPostKeys[key]:
			return "", "the form body carries a key other than channel, thread_ts and text"
		case len(values) != 1:
			return "", "the form body carries a key more than once"
		}
	}
	channel, named := form["channel"]
	if !named {
		return "", "the form body names no channel"
	}
	return channel[0], ""
}

// slackBodyType is the media type of r's body, application/json or
// application/x-www-form-urlencoded, read from its one Content-Type by mime.ParseMediaType with
// at most a charset parameter of utf-8 in any case; or why r's Content-Type is not one of those.
func slackBodyType(r *http.Request) (mediaType, why string) {
	types := r.Header.Values("Content-Type")
	if len(types) != 1 {
		return "", "the request carries no Content-Type, or more than one"
	}
	mediaType, params, err := mime.ParseMediaType(types[0])
	if err != nil {
		return "", "the Content-Type cannot be read"
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return "", "the Content-Type carries a parameter other than charset=utf-8"
		}
	}
	if mediaType != "application/json" && mediaType != "application/x-www-form-urlencoded" {
		return "", "the Content-Type is neither application/json nor application/x-www-form-urlencoded"
	}
	return mediaType, ""
}

// fencedMethods are the Slack methods a name the public fence holds may call, by their decoded
// path: the ones this repository sends with the Legion Slack bot token, and no other. Two read or
// write one channel, which rule 5 checks by the channel they name: chat.postMessage, slack_post's
// reply (packages/envoy-client/src/slack-post.ts), and conversations.replies, the controller's
// read of a mention's thread (skills/legion-controller/references/slack-mentions.md). auth.test,
// the token's own check (skills/agent-secrets/SKILL.md, scripts/airlock/slack.ts), names no
// channel and answers none of a channel's content. Every other method is refused, whatever it
// names: one that names no channel can still answer a private channel's content (reactions.list,
// stars.list, files.list, files.info, search.messages) or its name and topic (conversations.list),
// so no list of methods to refuse could be complete, and a method this file does not know of
// cannot reach Slack.
var fencedMethods = map[string]bool{
	slackPostPath:                true,
	"/api/conversations.replies": true,
	"/api/auth.test":             true,
}

// slackCallExcluded reads r as a Slack call on a name the public fence holds and answers its body
// when the call names none of excluded, every channel a private project's owner named, or why it
// may. A credential that names no private project may not read or write such a channel with the
// bot token, so the call is refused when:
//
//  1. its method, the decoded path, is not one fencedMethods names;
//  2. its body, read once, is non-empty and has no single Content-Type that mime.ParseMediaType
//     reads as application/json or application/x-www-form-urlencoded with at most a charset of
//     utf-8, or has a Content-Encoding, or is over maxSlackPostBody;
//  3. a JSON body, walked by tokens, is not one object followed by nothing but white space, or a
//     channel, channel_id or channels key of that object holds, at any of its occurrences, a
//     value that is not a JSON string;
//  4. its query, or a form body, is one url.ParseQuery refuses;
//  5. any value, at every occurrence of channel, channel_id or channels in the query, the JSON
//     object or the form (keys compared once percent-decoded and in any case, so chann%65l and
//     CHANNEL are channel), split on commas, trimmed of white space and upper-cased, is one of
//     excluded upper-cased, as read and once more percent-decoded;
//  6. a key of the query, the JSON object or the form, as decoded, holds a character other than
//     an ASCII letter, a digit or _. Every argument of the methods fencedMethods names is spelled
//     so, and a receiver that parses keys as PHP does drops a key's leading spaces, ends it at a
//     NUL and reads channel[] as channel, so " channel", "channel\x00x" and "channel[]" would be
//     channel there while rule 5 reads them as other keys;
//  7. a value of channel, channel_id or channels, split on commas, is not a conversation's id
//     (slackID: C, D or G, then at least 8 capital letters or digits). Rule 5 reads ids alone,
//     and a private project's owner may list a public Slack channel, which chat.postMessage also
//     takes by its name: a name would reach a listed channel that rule 5 does not see. Slack's
//     reference resolves a name for a public channel only; a private one is reached by its id,
//     which rule 5 reads.
//
// Each rule is checked in one place below, and each refuses an input no other rule refuses, which
// TestAPublicSlackGrantNeverNamesAPrivateChannel drives. An empty body holds no key. The body
// answered is the bytes read, which the caller serves upstream in place of r's own, so the call
// Slack receives is the call this read.
func slackCallExcluded(r *http.Request, excluded []string) (body []byte, why string) {
	if !fencedMethods[r.URL.Path] {
		return nil, "the Slack method is not one this secret is spent on outside a private project (chat.postMessage, conversations.replies, auth.test)"
	}
	if r.URL.RawQuery != "" {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return nil, "the query cannot be read"
		}
		if why := argsExcluded(formArgs(query), excluded); why != "" {
			return nil, "the query " + why
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSlackPostBody+1))
	switch {
	case err != nil:
		return nil, "the body could not be read"
	case len(body) == 0:
		return body, ""
	case len(body) > maxSlackPostBody:
		return nil, "the body is larger than 1 MiB"
	case len(r.Header.Values("Content-Encoding")) != 0:
		return nil, "the body carries a Content-Encoding"
	}
	mediaType, why := slackBodyType(r)
	if why != "" {
		return nil, why
	}
	var args []slackArg
	where := "the JSON body"
	if mediaType == "application/json" {
		if args, why = jsonArgs(body); why != "" {
			return nil, why
		}
	} else {
		where = "the form body"
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, "the form body cannot be read"
		}
		args = formArgs(form)
	}
	if why := argsExcluded(args, excluded); why != "" {
		return nil, where + " " + why
	}
	return body, ""
}

// slackArg is one occurrence of a key of a Slack call, as decoded, with its value when the key is
// a channel key (channelKey). A query's and a form's values are strings; a JSON body's channel
// value that is not a string never becomes one (rule 3).
type slackArg struct {
	key, value string
}

// formArgs is a query or a form, as url.ParseQuery read it, as its keys' occurrences: keys in
// order, so the one a refusal names is the same on every read, and each value of a key in turn.
func formArgs(values url.Values) []slackArg {
	var args []slackArg
	for _, key := range slices.Sorted(maps.Keys(values)) {
		for _, value := range values[key] {
			args = append(args, slackArg{key: key, value: value})
		}
	}
	return args
}

// jsonArgs is rule 3's reading of a JSON body, walked token by token: never decoded into a
// struct, whose case-insensitive match keeps one of two keys that differ in case, nor into a map,
// which keeps one occurrence of a key given twice. It answers every key of the one object, each
// occurrence on its own, with a channel key's string value; every other key's value is skipped
// whole.
func jsonArgs(body []byte) ([]slackArg, string) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, "the JSON body is not one object"
	}
	var args []slackArg
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, "the JSON body cannot be read"
		}
		// An object's key is always a string token; a body that is no JSON fails above.
		key, _ := token.(string)
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, "the JSON body cannot be read"
		}
		arg := slackArg{key: key}
		if channelKey(key) && json.Unmarshal(raw, &arg.value) != nil {
			return nil, "the JSON body's " + key + " is not a string"
		}
		args = append(args, arg)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, "the JSON body cannot be read"
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, "something follows the JSON body's object"
	}
	return args, ""
}

// argsExcluded is rules 5, 6 and 7 over one place's keys: why they name one of excluded, or a
// key or a channel Slack may read otherwise than the fence does, or "" when they name none.
func argsExcluded(args []slackArg, excluded []string) string {
	for _, arg := range args {
		if channelKey(arg.key) && namesExcluded(arg.value, excluded) {
			return "names a private project's channel under " + arg.key
		}
	}
	for _, arg := range args {
		if !plainKey(arg.key) {
			return "carries a key with a character other than an ASCII letter, a digit or _, which Slack may read as another key"
		}
	}
	for _, arg := range args {
		if channelKey(arg.key) && slices.ContainsFunc(strings.Split(arg.value, ","), func(part string) bool { return !slackID.MatchString(part) }) {
			return "names a channel under " + arg.key + " by something other than its id, which only Slack resolves"
		}
	}
	return ""
}

// channelKeys are the keys a Slack call names a channel under.
var channelKeys = []string{"channel", "channel_id", "channels"}

// channelKey reports whether key, as decoded, is one of channelKeys, in any case.
func channelKey(key string) bool {
	return slices.ContainsFunc(channelKeys, func(name string) bool { return strings.EqualFold(key, name) })
}

// plainKey reports whether key, as decoded, is ASCII letters, digits and _ alone (rule 6).
func plainKey(key string) bool {
	return !strings.ContainsFunc(key, func(c rune) bool {
		return c != '_' && (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z')
	})
}

// slackID is a Slack conversation's id as rule 7 reads one: C for a channel, G for an older
// private channel, D for a direct message, then at least 8 capital letters or digits. A channel's
// name, which Slack spells in lower case, with "-" or after "#", is none.
var slackID = regexp.MustCompile(`^[CDG][A-Z0-9]{8,}$`)

// namesExcluded reports whether value names one of excluded: rule 5's reading of one value.
func namesExcluded(value string, excluded []string) bool {
	readings := []string{value}
	if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
		readings = append(readings, decoded)
	}
	for _, reading := range readings {
		for _, part := range strings.Split(reading, ",") {
			part = strings.ToUpper(strings.TrimSpace(part))
			if slices.ContainsFunc(excluded, func(id string) bool { return strings.ToUpper(id) == part }) {
				return true
			}
		}
	}
	return false
}
