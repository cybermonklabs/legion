// Package dispatchread is the broker's one GET of a Dispatch access route, the read both of the
// broker's clients of the Dispatch at BROKER_PROJECT_ACCESS_URL make: internal/cipher/projectaccess
// (whether a person may see a project, and their member kind) and internal/cipher/projectrepos
// (the repositories a project's issues resolve to). It holds what the two must not do two ways:
// the read of the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE), the bound on one
// call, the headers, the bounded read of the body, and which failures are that token's and not
// Dispatch's (ErrBrokerRefused). It decodes no answer: each client reads its own route's body, so
// no field of one answer reaches a reader of the other.
package dispatchread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// ErrBrokerRefused is the broker's own account token not working: Dispatch answering 401 or 403 to
// it (Dispatch does not list the broker's account as one that may ask, or does not accept the
// token, one minted for another audience, say), or the broker unable to read it from
// BROKER_PROJECT_ACCESS_TOKEN_FILE at all (missing, empty or unreadable). Either is a
// misconfiguration and not an outage, so no caller mints, enrolls or answers through it as it would
// through a failure to reach Dispatch: every read made with that credential would be wrong.
var ErrBrokerRefused = errors.New("dispatchread: the broker's own account token was refused or could not be read")

// Reader reads Dispatch's routes at one base URL, presenting the broker's own account token, each
// call bounded by its timeout and each body by its limit.
type Reader struct {
	base    string
	account ciphertoken.Account
	http    *http.Client
	timeout time.Duration
	limit   int
}

// New is a reader of the Dispatch at baseURL (BROKER_PROJECT_ACCESS_URL), presenting account
// (BROKER_PROJECT_ACCESS_TOKEN_FILE's reader) on every call. A nil httpClient is
// http.DefaultClient. Each call, the dial and the body's read included, is bounded by timeout
// whatever the client, and a body longer than limit bytes is an error.
func New(baseURL string, account ciphertoken.Account, httpClient *http.Client, timeout time.Duration, limit int) Reader {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return Reader{base: strings.TrimRight(baseURL, "/"), account: account, http: httpClient, timeout: timeout, limit: limit}
}

// Answer is what Dispatch answered one call: its status, and its whole body.
type Answer struct {
	Status int
	Body   []byte
}

// Get asks path, with query when it holds any, and answers Dispatch's status and body for the
// caller to read, whatever the status, but for these errors, each naming what failed and never the
// token. An account token it cannot read is ErrBrokerRefused naming
// BROKER_PROJECT_ACCESS_TOKEN_FILE, and Dispatch is asked nothing. A 401 or a 403 is
// ErrBrokerRefused too, Dispatch refusing that token, decided by the status alone before the body
// is read: a body cut short or past the limit only leaves the error without the code it names, and
// never turns the refusal into a failure to ask. A transport failure, the timeout passing, and a
// body it could not read or one past the limit are errors.
func (r Reader) Get(ctx context.Context, path string, query url.Values) (Answer, error) {
	bearer, err := r.account.Read()
	if err != nil {
		return Answer{}, fmt.Errorf("%w: BROKER_PROJECT_ACCESS_TOKEN_FILE could not be read: %w", ErrBrokerRefused, err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	target := r.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Answer{}, fmt.Errorf("dispatchread: build the request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	response, err := r.http.Do(request)
	if err != nil {
		return Answer{}, fmt.Errorf("dispatchread: ask dispatch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		code, _ := io.ReadAll(io.LimitReader(response.Body, int64(r.limit)))
		return Answer{}, fmt.Errorf("%w: dispatch answered %s %d with the code %q", ErrBrokerRefused, path, response.StatusCode, RefusalCode(code))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(r.limit)+1))
	if err != nil {
		return Answer{}, fmt.Errorf("dispatchread: read dispatch's answer to %s: %w", path, err)
	}
	if len(body) > r.limit {
		return Answer{}, fmt.Errorf("dispatchread: dispatch's answer to %s is over %d bytes", path, r.limit)
	}
	return Answer{Status: response.StatusCode, Body: body}, nil
}

// RefusalCode is the code a refusal's body names, empty for a body that is cut short, holds more
// than one value or names none: the status has already decided what the answer is, and the code
// only adds to its error.
func RefusalCode(body []byte) string {
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	return refusal.Code
}
