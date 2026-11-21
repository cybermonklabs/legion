// packages/envoy/internal/cipher/proxy/slack.go
// The check a private project's grant of a project-channels secret spends its value under
// (dispatch://INFRA-3862, #t-d-12). The broker marks such a name Limited, with the channels every
// private project the credential names allows (requests.AirlockSecret), and serveBound runs
// slackPostAllowed on each request a Limited name would carry, after the swap and before anything
// goes upstream. It is Go code of this package's own, beside the Lean-modelled proxyrules.Decide
// and proxyrules.Rewrite rather than in them, so the model and its fuzz and mutation tests are
// unchanged.
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	// channelNotAllowed is that refusal's words; the rule the request broke follows them.
	channelNotAllowed = "a private project's grant of this secret posts only with POST /api/chat.postMessage, carrying channel, thread_ts and text, to a Slack channel its project's owner named"
	// slackPostPath is the one path a Limited name may be spent on.
	slackPostPath = "/api/chat.postMessage"
	// maxSlackPostBody is the largest body such a post may carry, 1 MiB: a message is a few
	// kilobytes, and the check holds the whole body in memory to read it before it goes on.
	maxSlackPostBody = 1 << 20
)

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
	types := r.Header.Values("Content-Type")
	if len(types) != 1 {
		return nil, "the request carries no Content-Type, or more than one"
	}
	mediaType, params, err := mime.ParseMediaType(types[0])
	if err != nil {
		return nil, "the Content-Type cannot be read"
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return nil, "the Content-Type carries a parameter other than charset=utf-8"
		}
	}
	if mediaType != "application/json" && mediaType != "application/x-www-form-urlencoded" {
		return nil, "the Content-Type is neither application/json nor application/x-www-form-urlencoded"
	}
	if len(r.Header.Values("Content-Encoding")) != 0 {
		return nil, "the body carries a Content-Encoding"
	}
	body, err = io.ReadAll(io.LimitReader(r.Body, maxSlackPostBody+1))
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
