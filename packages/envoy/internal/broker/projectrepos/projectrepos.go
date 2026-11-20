// Package projectrepos is the broker's read of Dispatch's answer to "which repositories do a
// project's issues resolve to" (GET /api/v1/access/projects/{key}/repositories), which the broker
// relays to an edge listener (GET /v1/project-repositories, internal/broker/api). Dispatch
// computes that answer from the project's issues, by the one rule its GitHub proxy follows too,
// so an edge listener that relays the GitHub events of those repositories and Dispatch cannot
// disagree about which they are.
//
// Nothing here is kept: every read asks Dispatch, so an answer is never older than the call that
// relays it, and anything but a whole answer about the project asked is an error, never a list, a
// shorter one or an empty one. What an error means is the caller's: the route answers 503.
//
// It reads the Dispatch internal/broker/projectaccess reads, at the same base URL with the same
// account token (BROKER_PROJECT_ACCESS_URL, BROKER_PROJECT_ACCESS_TOKEN_FILE), so the broker has
// one Dispatch and one account there. It is a package of its own rather than a method of that
// client because projectaccess is the membership answer, which is to carry no list of a project's
// repositories, so that no reader of the membership answer can take one from it.
package projectrepos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// Timeout bounds one read of Dispatch, the dial included: past it the read is an error.
const Timeout = 2 * time.Second

// MaxAnswerBytes caps the answer read from Dispatch. An answer past it is refused rather than
// held, and never cut to a shorter list.
const MaxAnswerBytes = 1 << 20

// The route's path around the project key.
const (
	pathPrefix = "/api/v1/access/projects/"
	pathSuffix = "/repositories"
)

// Client reads Dispatch's route at one base URL, presenting the broker's own account token.
type Client struct {
	base    string
	account brokertoken.Account
	http    *http.Client
}

// New is a client of Dispatch at baseURL (BROKER_PROJECT_ACCESS_URL), presenting account
// (BROKER_PROJECT_ACCESS_TOKEN_FILE's reader, the one projectaccess presents) on every call. A nil
// httpClient is http.DefaultClient; each call is bounded by Timeout whatever the client.
func New(baseURL string, account brokertoken.Account, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), account: account, http: httpClient}
}

// Repositories is Dispatch's list for project, read now: each repository, as "owner/name", that
// an issue of the project resolves to, and an empty list, never nil, for a project none of whose
// issues resolves to one. Every other outcome is an error naming what failed and never the token,
// with no list: an account token it cannot read (Dispatch is then asked nothing), a transport
// failure or Timeout passing, any status but 200 (Dispatch's 503, a project it does not hold, a
// Dispatch that serves no such route, one refusing the broker's token), and a 200 whose body is
// past MaxAnswerBytes or is not one JSON object naming project and a list of "owner/name"
// strings.
func (c *Client) Repositories(ctx context.Context, project string) ([]string, error) {
	bearer, err := c.account.Read()
	if err != nil {
		return nil, fmt.Errorf("projectrepos: BROKER_PROJECT_ACCESS_TOKEN_FILE could not be read: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+pathPrefix+url.PathEscape(project)+pathSuffix, nil)
	if err != nil {
		return nil, fmt.Errorf("projectrepos: build the request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("projectrepos: ask dispatch: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxAnswerBytes+1))
	if err != nil {
		return nil, fmt.Errorf("projectrepos: read dispatch's answer: %w", err)
	}
	if len(body) > MaxAnswerBytes {
		return nil, fmt.Errorf("projectrepos: dispatch's answer about %s is over %d bytes", project, MaxAnswerBytes)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("projectrepos: dispatch answered %d with the code %q about %s", response.StatusCode, refusalCode(body), project)
	}
	return readAnswer(body, project)
}

// readAnswer is the list a 200's body carries about project, or an error saying why the body is
// no whole answer about it.
func readAnswer(body []byte, project string) ([]string, error) {
	var answer struct {
		Project      string    `json:"project"`
		Repositories *[]string `json:"repositories"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&answer); err != nil {
		return nil, fmt.Errorf("projectrepos: decode dispatch's answer about %s: %w", project, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("projectrepos: dispatch's answer about %s holds more than one value", project)
	}
	switch {
	case answer.Project != project:
		return nil, fmt.Errorf("projectrepos: dispatch answered about %q, asked about %s", answer.Project, project)
	case answer.Repositories == nil:
		return nil, fmt.Errorf("projectrepos: dispatch's answer about %s carries no list of repositories", project)
	}
	repositories := *answer.Repositories
	for _, repository := range repositories {
		if !ownerName(repository) {
			return nil, fmt.Errorf("projectrepos: dispatch's answer about %s lists %q, which is no owner/name", project, repository)
		}
	}
	return repositories, nil
}

// ownerName reports whether repository is "owner/name": one slash, neither side empty, and no
// space or control character anywhere.
func ownerName(repository string) bool {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}
	return !strings.ContainsFunc(repository, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

// refusalCode is the code a refusal's body names, empty for one that names none: the status has
// already decided the read failed, and the code only adds to its error.
func refusalCode(body []byte) string {
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	return refusal.Code
}
