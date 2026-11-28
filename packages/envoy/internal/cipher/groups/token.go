package groups

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/redirects"
)

// tokenCallTimeout bounds the mint: oauth2.TokenSource's Token takes no context, so a client with
// no timeout of its own would let one hung call hold every read.
const tokenCallTimeout = 30 * time.Second

// answerLimit is the most of an answer a call reads: a token or a refusal is a few hundred bytes,
// and a page of two hundred members a few tens of kilobytes.
const answerLimit = 1 << 20

// ErrNoDirectoryAccount is the directory token's refusal when the deployment names no reader
// account (BROKER_DIRECTORY_ACCOUNT): the broker then reads no group, and every group admits
// nobody.
var ErrNoDirectoryAccount = errors.New("no directory account")

// TokenOptions are the directory token's reader account, its endpoint, its transport and the
// credential it mints with. Account is the reader account (BROKER_DIRECTORY_ACCOUNT), a
// user-managed Google service account that holds Workspace's Groups Reader role; config.Load holds
// it to that form. IAMCredentialsURL is Google's when empty; a test names its stand-in. HTTP is a
// client with a 30-second timeout when nil, and follows no redirect either way. Default, which is
// required, is the broker pod's own credential (secrets.OwnGoogleCredential in cmd/cipherd), which
// must hold Token Creator on Account: the Admin SDK refuses that credential's own token, and IAM
// takes it to mint the one the Directory reads.
type TokenOptions struct {
	Account           string
	IAMCredentialsURL string
	HTTP              *http.Client
	Default           oauth2.TokenSource
}

// NewTokenSource answers the token the group read sends the Directory: the reader account's access
// token, minted through generateAccessToken with the one scope Scope, called with the pod's own
// credential. With no Account it answers ErrNoDirectoryAccount and mints nothing. A token is
// handed out again until it is about to end (oauth2.ReuseTokenSource). No error quotes a token.
func NewTokenSource(o TokenOptions) oauth2.TokenSource {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: tokenCallTimeout}
	}
	return oauth2.ReuseTokenSource(nil, directoryToken{o})
}

// directoryToken mints one directory token per Token call; NewTokenSource reuses it.
type directoryToken struct{ o TokenOptions }

func (d directoryToken) Token() (*oauth2.Token, error) {
	if d.o.Account == "" {
		return nil, ErrNoDirectoryAccount
	}
	// A token source's error can quote the credential it read, so only its class is reported.
	own, err := d.o.Default.Token()
	if err != nil {
		return nil, errors.New("the pod's own Google credentials gave no token")
	}
	// Only the email is escaped, and the verb is appended after, as the service-account minter
	// builds its URL: an escaped colon reads to Google as a resource of its own.
	endpoint := cmp.Or(d.o.IAMCredentialsURL, secrets.GoogleIAMCredentialsURL) +
		"/v1/projects/-/serviceAccounts/" + url.PathEscape(d.o.Account) + ":generateAccessToken"
	body, _ := json.Marshal(secrets.GenerateAccessTokenRequest{Scope: []string{Scope}, Lifetime: secrets.GoogleAccessTokenLifetime})
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("the token mint: %w", err)
	}
	own.SetAuthHeader(request)
	request.Header.Set("Content-Type", "application/json")
	payload, err := call(d.o.HTTP, request, "the token mint for "+d.o.Account)
	if err != nil {
		return nil, err
	}
	var minted secrets.GenerateAccessTokenAnswer
	if json.Unmarshal(payload, &minted) != nil || minted.AccessToken == "" {
		return nil, fmt.Errorf("the token mint for %s answered no access token", d.o.Account)
	}
	// A token with no end would be handed out forever, past the hour Google gives it.
	expiry, err := time.Parse(time.RFC3339, minted.ExpireTime)
	if err != nil {
		return nil, fmt.Errorf("the token mint for %s answered no expireTime", d.o.Account)
	}
	return &oauth2.Token{AccessToken: minted.AccessToken, TokenType: "Bearer", Expiry: expiry}, nil
}

// googleRefusal is a Google endpoint's answer other than 200: who answered, the status, and
// Google's own status and reason where its body names them. Its text holds no body beyond them.
type googleRefusal struct {
	who            string
	code           int
	status, reason string
}

func (e *googleRefusal) Error() string {
	text := fmt.Sprintf("%s answered %d", e.who, e.code)
	for _, word := range []string{e.status, e.reason} {
		if word != "" && !strings.Contains(text, word) {
			text += " " + word
		}
	}
	return text
}

// googleWord is the form of a status or reason Google answers (PERMISSION_DENIED,
// IAM_PERMISSION_DENIED, authError): a word that is not one is left out of the refusal, since
// a body that is not Google's could put anything there.
var googleWord = regexp.MustCompile(`^[A-Za-z0-9_.]{1,64}$`)

// call sends request and answers its body when the status is 200, and a *googleRefusal naming
// who answered otherwise. It follows no redirect (redirects.Refused): the request carries a token,
// which no 3xx may take anywhere. A call that fails is named by its class alone
// (secrets.TransportFailure), since Go's client can quote a malformed status line. An answer past
// answerLimit is refused rather than read in part.
func call(client *http.Client, request *http.Request, who string) ([]byte, error) {
	response, err := redirects.Refused(client).Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s", who, secrets.TransportFailure(request.Context(), err))
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, answerLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s", who, secrets.TransportFailure(request.Context(), err))
	}
	if len(payload) > answerLimit {
		return nil, fmt.Errorf("%s answered more than %d bytes", who, answerLimit)
	}
	if response.StatusCode != http.StatusOK {
		return nil, refusalOf(who, response.StatusCode, payload)
	}
	return payload, nil
}

// refusalOf reads Google's status and reason from an error body: the reason of its ErrorInfo
// detail, which IAM names IAM_PERMISSION_DENIED in, and else the first of its errors, as the
// Directory API names them.
func refusalOf(who string, code int, body []byte) *googleRefusal {
	refusal := &googleRefusal{who: who, code: code}
	var answer struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				Type   string `json:"@type"`
				Reason string `json:"reason"`
			} `json:"details"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return refusal
	}
	if googleWord.MatchString(answer.Error.Status) {
		refusal.status = answer.Error.Status
	}
	var reasons []string
	for _, detail := range answer.Error.Details {
		if strings.HasSuffix(detail.Type, "google.rpc.ErrorInfo") {
			reasons = append(reasons, detail.Reason)
		}
	}
	for _, e := range answer.Error.Errors {
		reasons = append(reasons, e.Reason)
	}
	for _, reason := range reasons {
		if googleWord.MatchString(reason) {
			refusal.reason = reason
			break
		}
	}
	return refusal
}
