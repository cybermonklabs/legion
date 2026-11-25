package redact

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/oauth2"
)

// Class is err, from a request to a server other than the broker, as a message may print it: the
// class of the failure alone, in this package's fixed text ("not a valid URL", "name resolution
// failed", "connection refused", "timed out", "TLS failed", "the server answered HTTP <n>", or
// "the request failed"), and none of err's own text, except words an earlier Error, Cause or Named
// already gave. net/url quotes the whole value it could not parse, net/http's *url.Error leaves a
// user name, a query and a fragment in the URL it names, and x/oauth2 keeps the body of an answer,
// so a server a setting names is reported by its class, beside the server as the message itself
// names it: the sign-in client (cipherclient's DeviceFlow) prints each failure of a request to the
// sign-in issuer, or to an endpoint its discovery document names, this way. An OAuth answer's
// class carries its error code where that code is a registered one (answerCodes), which this
// package spells, never the server; a failure already put in words (a classified) keeps those
// words alone, with no code added to them again. It unwraps to err, so errors.Is and errors.As
// still classify it.
func Class(err error) error {
	if err == nil {
		return nil
	}
	text := class(err, "the server")
	var worded *classified
	var re *oauth2.RetrieveError
	if !errors.As(err, &worded) && errors.As(err, &re) && re.Response != nil && answerCodes[re.ErrorCode] {
		text += " " + re.ErrorCode
	}
	return &classified{text, err}
}

// Masked is err printed as text alone: for a failure a caller has already put in words it may
// print - a status its transport recorded, fixed text - where err's own text quotes what it may
// not. text is a class: fixed words naming the kind of failure, never a URL, a path, an id or
// other detail of a request, since every Error, Cause and Class over the result prints text as the
// failure's class, in lines of their own. It unwraps to err, so errors.Is and errors.As still
// classify it. Masked(nil, text) is nil.
func Masked(err error, text string) error {
	if err == nil {
		return nil
	}
	return &classified{text, err}
}

// BrokerAnswer is the OAuth error answer (*oauth2.RetrieveError) err holds as the broker's own: the
// first one Unwrap reaches, as errors.As walks err's tree, on no branch through an error Class,
// Masked, File, Error, Cause or Named made. Those hold another server's answer, or one whose text
// they have already withheld, and keep it reachable so errors.As still classifies the failure; a
// printer that took that answer's words for the broker's would print the very text they withheld.
// A printer of the broker's own refusals outside this package, such as the helper's
// oauthBrokerError, reads the answer through this function, never through errors.As. Error and
// Cause, whose trusted answers print whole (trustedAnswer), keep the same rule by another road:
// printable checks nothing under an error those makers returned, so no answer beneath one is ever
// the broker's to them.
func BrokerAnswer(err error) (*oauth2.RetrieveError, bool) {
	switch e := err.(type) {
	case nil, *classified, *called:
		return nil, false
	case *oauth2.RetrieveError:
		return e, true
	case interface{ Unwrap() error }:
		return BrokerAnswer(e.Unwrap())
	case interface{ Unwrap() []error }:
		for _, branch := range e.Unwrap() {
			if re, ok := BrokerAnswer(branch); ok {
				return re, true
			}
		}
	}
	return nil, false
}

// Answered is a server's refusal, an HTTP status with the body it came with, as a message may
// print it: "the server answered HTTP <status>", followed by the answer's error code where body
// names a registered one (answerCodes) as an OAuth error (`{"error":"invalid_grant"}`) or as a
// Google API error's status (`{"error":{"status":"PERMISSION_DENIED"}}`). Nothing else of body
// prints: a server writes what it likes in a description, a message and a code of its own, an
// echo of the request that carried a credential among them, and a Google error body has carried
// a token before. The sign-in client prints an issuer's refusal of its discovery document this
// way, from the status its transport recorded (oauthrefusal.Recorder), since go-oidc's own error
// quotes the issuer and the body.
func Answered(status int, body []byte) string {
	text := fmt.Sprintf("the server answered HTTP %d", status)
	var answer struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &answer) != nil || len(answer.Error) == 0 {
		return text
	}
	var code string
	if json.Unmarshal(answer.Error, &code) != nil {
		var google struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(answer.Error, &google) != nil {
			return text
		}
		code = google.Status
	}
	if answerCodes[code] {
		text += " " + code
	}
	return text
}

// answerCodes are the error codes a refusal may print: the ones the OAuth specifications register
// (RFC 6749, 6750, 8628, 8693, 9449 and OpenID Connect Core) and Google's canonical API statuses.
// A code outside them is the server's own text, which prints nowhere.
var answerCodes = map[string]bool{
	"invalid_request": true, "invalid_client": true, "invalid_grant": true, "unauthorized_client": true,
	"unsupported_grant_type": true, "invalid_scope": true, "invalid_token": true, "insufficient_scope": true,
	"authorization_pending": true, "slow_down": true, "access_denied": true, "expired_token": true,
	"invalid_target": true, "invalid_dpop_proof": true, "use_dpop_nonce": true, "server_error": true,
	"temporarily_unavailable": true, "interaction_required": true, "login_required": true, "consent_required": true,
	"INVALID_ARGUMENT": true, "FAILED_PRECONDITION": true, "OUT_OF_RANGE": true, "UNAUTHENTICATED": true,
	"PERMISSION_DENIED": true, "NOT_FOUND": true, "ABORTED": true, "ALREADY_EXISTS": true,
	"RESOURCE_EXHAUSTED": true, "CANCELLED": true, "DATA_LOSS": true, "UNKNOWN": true, "INTERNAL": true,
	"UNIMPLEMENTED": true, "UNAVAILABLE": true, "DEADLINE_EXCEEDED": true,
}

// File is err, from a call on a file or a directory, as a message may print it: the call's
// operation and the system's reason ("open: no such file or directory"), and never the path it
// was handed, which the error of every *fs.PathError and *os.LinkError repeats. A setting that
// names a secret's file is where a person who pasted the secret itself puts it, so a message
// names that setting rather than its value: the files AGENT_SECRETS_MACHINE_TOKEN_FILE and
// AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE name, the sign-in store (AGENT_SECRETS_SIGNIN_DIR),
// enroll's key.pem and key directory (--out or AGENT_SECRETS_KEY_DIR), --pod-token-file, and the
// helper's machine file, identity key, operator file and session records
// (AGENT_SECRETS_STATE_FILE, whose records hold each session's private key). A file whose content
// does not parse is refused by its caller in fixed text, never with the parser's error, since
// encoding/json's, time's and x509's errors quote the value they could not read. An error that
// holds no system errno prints as the operation and "failed", and one that is no file call's as
// "the call failed". It unwraps to err, so errors.Is(err, fs.ErrNotExist) still holds.
func File(err error) error {
	if err == nil {
		return nil
	}
	var path *fs.PathError
	var link *os.LinkError
	var call *os.SyscallError
	switch {
	case errors.As(err, &path):
		return &classified{operation(path.Op, path.Err), err}
	case errors.As(err, &link):
		return &classified{operation(link.Op, link.Err), err}
	case errors.As(err, &call):
		return &classified{operation(call.Syscall, call.Err), err}
	default:
		return &classified{"the call failed", err}
	}
}

// operation is op with the system's reason for its failure, cause, where cause is an errno, whose
// text is the system's own fixed words.
func operation(op string, cause error) string {
	var errno syscall.Errno
	if errors.As(cause, &errno) {
		return op + ": " + errno.Error()
	}
	return op + " failed"
}
