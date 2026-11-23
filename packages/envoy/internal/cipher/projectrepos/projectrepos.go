// Package projectrepos is the broker's read of Dispatch's answer to "which repositories do a
// project's issues resolve to" (GET /api/v1/access/projects/{key}/repositories), which the broker
// relays to an edge listener (GET /v1/project-repositories, internal/cipher/api). That route is
// Dispatch's (#4635) and is not in this tree yet. Its answer and Dispatch's GitHub proxy read one
// rule of which repositories a project owns: the ones the project's issues resolve to.
//
// Nothing here is kept: every read asks Dispatch, so an answer is never older than the call that
// relays it, and anything but a whole answer about the project asked is an error, never a list, a
// shorter one or an empty one. What an error means is the caller's: the route answers 503.
//
// It reads the Dispatch internal/cipher/projectaccess reads, through the same read
// (internal/cipher/dispatchread), at the same base URL with the same account token
// (BROKER_PROJECT_ACCESS_URL, BROKER_PROJECT_ACCESS_TOKEN_FILE), so the broker has one Dispatch and
// one account there, and a token that does not work is the same error to both. It is a package of
// its own rather than a method of that client so that the list it reads reaches no reader of the
// membership answer. That answer still carries a list of its own (projectaccess.Answer's
// Repositories), which is to be removed; until then this package keeps the edge listener's list
// out of it.
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

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/ciphertoken"
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
	read dispatchread.Reader
}

// New is a client of Dispatch at baseURL (BROKER_PROJECT_ACCESS_URL), presenting account
// (BROKER_PROJECT_ACCESS_TOKEN_FILE's reader, the one projectaccess presents) on every call. A nil
// httpClient is http.DefaultClient; each call is bounded by Timeout whatever the client.
func New(baseURL string, account ciphertoken.Account, httpClient *http.Client) *Client {
	return &Client{read: dispatchread.New(baseURL, account, httpClient, Timeout, MaxAnswerBytes)}
}

// Repositories is Dispatch's list for project, read now: each repository, as "owner/name", that
// an issue of the project resolves to, sorted byte-wise and each once, and an empty list, never
// nil, for a project none of whose issues resolves to one. Every other outcome is an error naming
// what failed and never the token, with no list. An account token it cannot read (Dispatch is
// then asked nothing) and Dispatch's 401 or 403 to it are dispatchread.ErrBrokerRefused; so are
// they to projectaccess. A transport failure or Timeout passing, any other status but 200
// (Dispatch's 503, a project it does not hold, a Dispatch that serves no such route), and a 200
// whose body is past MaxAnswerBytes or is no whole answer about project (readAnswer) are errors.
func (c *Client) Repositories(ctx context.Context, project string) ([]string, error) {
	answer, err := c.read.Get(ctx, pathPrefix+url.PathEscape(project)+pathSuffix, nil)
	if err != nil {
		return nil, fmt.Errorf("projectrepos: read %s's repositories: %w", project, err)
	}
	if answer.Status != http.StatusOK {
		return nil, fmt.Errorf("projectrepos: dispatch answered %d with the code %q about %s", answer.Status, dispatchread.RefusalCode(answer.Body), project)
	}
	return readAnswer(answer.Body, project)
}

// readAnswer is the list a 200's body carries about project, or an error saying why the body is
// no whole answer about it. The body is read as Dispatch writes it and no other way: one JSON
// object holding the key "project", the project asked, and the key "repositories", a list of
// "owner/name" strings sorted byte-wise, each once; each key in its own case and once, and no
// other key. encoding/json alone would read a key in any case and the last of a key written twice,
// so the object is read key by key.
func readAnswer(body []byte, project string) ([]string, error) {
	refused := func(why string) error {
		return fmt.Errorf("projectrepos: dispatch's answer about %s %s", project, why)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return nil, refused("is no JSON object")
	}
	var answered *string
	var repositories *[]string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, refused("does not decode: " + err.Error())
		}
		switch key, _ := token.(string); {
		case key == "project" && answered == nil:
			answered = new(string)
			if err := decoder.Decode(answered); err != nil {
				return nil, refused("names no project: " + err.Error())
			}
		case key == "repositories" && repositories == nil:
			// null leaves list nil, and [] makes it an empty list that is not nil.
			var list []string
			if err := decoder.Decode(&list); err != nil {
				return nil, refused("carries no list of repository names: " + err.Error())
			}
			if list == nil {
				return nil, refused("carries no list of repositories")
			}
			repositories = &list
		default:
			return nil, refused(fmt.Sprintf("holds the key %q, which is neither project nor repositories or is written twice", dispatchread.Clip(key)))
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, refused("does not decode")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, refused("holds more than one value")
	}
	switch {
	case answered == nil:
		return nil, refused("names no project")
	case *answered != project:
		return nil, fmt.Errorf("projectrepos: dispatch answered about %q, asked about %s", dispatchread.Clip(*answered), project)
	case repositories == nil:
		return nil, refused("carries no list of repositories")
	}
	list := *repositories
	for index, repository := range list {
		if !ownerName(repository) {
			return nil, refused(fmt.Sprintf("lists %q, which is no owner/name", dispatchread.Clip(repository)))
		}
		if index > 0 && list[index-1] >= repository {
			return nil, refused(fmt.Sprintf("lists %q after %q: the list is sorted byte-wise and names each repository once", dispatchread.Clip(repository), dispatchread.Clip(list[index-1])))
		}
	}
	return list, nil
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
