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
)

// Google's own addresses for the two calls the directory token makes: the GKE metadata server,
// which names the account the pod runs as, and IAM Service Account Credentials, which mints that
// account's access token.
const (
	googleMetadataURL       = "http://metadata.google.internal"
	googleIAMCredentialsURL = "https://iamcredentials.googleapis.com"
)

// emailPath is the metadata server's path for the email of the account the pod runs as.
const emailPath = "/computeMetadata/v1/instance/service-accounts/default/email"

// tokenLifetime is the minted token's life, Google's maximum for a default-configured project:
// the hourly read mints again when the token it holds is about to end.
const tokenLifetime = "3600s"

// tokenCallTimeout bounds each of the token's two calls: oauth2.TokenSource's Token takes no
// context, so a client with no timeout of its own would let one hung call hold every read.
const tokenCallTimeout = 30 * time.Second

// answerLimit is the most of an answer a call reads: an email, a token or a refusal is a few
// hundred bytes, and a page of two hundred members a few tens of kilobytes.
const answerLimit = 1 << 20

// ErrNoServiceAccount is the directory token's refusal when the metadata server names no Google
// service account: the pod runs as a Workload Identity principal, whose metadata token the Admin
// SDK refuses and which can mint no token of its own, so it reads no group.
var ErrNoServiceAccount = errors.New("no service account")

// TokenOptions are the directory token's endpoints, its transport and the credential it mints
// with. MetadataURL and IAMCredentialsURL are Google's when empty; a test names its stand-ins.
// HTTP is a client with a 30-second timeout when nil. Default, which is required, is the pod's
// own credential, the application default credentials at the cloud-platform scope, which in a
// GKE pod are the metadata server's token for the account it runs as: the Admin SDK refuses that
// token, and IAM takes it to mint the one the Directory reads.
type TokenOptions struct {
	MetadataURL       string
	IAMCredentialsURL string
	HTTP              *http.Client
	Default           oauth2.TokenSource
}

// NewTokenSource answers the token the group read sends the Directory: the pod's own service
// account's access token, minted for that same account through generateAccessToken with the one
// scope Scope. Each new token reads the account's email from the metadata server, so no setting
// names the account and none can point the read at an account the broker impersonates for a
// grant; an email that names no service account is ErrNoServiceAccount, and nothing is minted.
// The account mints its own token only with Token Creator on itself. A token is handed out again
// until it is about to end (oauth2.ReuseTokenSource). No error quotes a token.
func NewTokenSource(o TokenOptions) oauth2.TokenSource {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: tokenCallTimeout}
	}
	return oauth2.ReuseTokenSource(nil, directoryToken{o})
}

// directoryToken mints one directory token per Token call; NewTokenSource reuses it.
type directoryToken struct{ o TokenOptions }

func (d directoryToken) Token() (*oauth2.Token, error) {
	account, err := d.account()
	if err != nil {
		return nil, err
	}
	// A token source's error can quote the credential it read, so only its class is reported.
	own, err := d.o.Default.Token()
	if err != nil {
		return nil, errors.New("the pod's own Google credentials gave no token")
	}
	// Only the email is escaped, and the verb is appended after, as the service-account minter
	// builds its URL: an escaped colon reads to Google as a resource of its own.
	endpoint := cmp.Or(d.o.IAMCredentialsURL, googleIAMCredentialsURL) +
		"/v1/projects/-/serviceAccounts/" + url.PathEscape(account) + ":generateAccessToken"
	body, _ := json.Marshal(struct {
		Scope    []string `json:"scope"`
		Lifetime string   `json:"lifetime"`
	}{[]string{Scope}, tokenLifetime})
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("the token mint: %w", err)
	}
	own.SetAuthHeader(request)
	request.Header.Set("Content-Type", "application/json")
	payload, err := call(d.o.HTTP, request, "the token mint for "+account)
	if err != nil {
		return nil, err
	}
	var minted struct {
		AccessToken string `json:"accessToken"`
		ExpireTime  string `json:"expireTime"`
	}
	if json.Unmarshal(payload, &minted) != nil || minted.AccessToken == "" {
		return nil, fmt.Errorf("the token mint for %s answered no access token", account)
	}
	// A token with no end would be handed out forever, past the hour Google gives it.
	expiry, err := time.Parse(time.RFC3339, minted.ExpireTime)
	if err != nil {
		return nil, fmt.Errorf("the token mint for %s answered no expireTime", account)
	}
	return &oauth2.Token{AccessToken: minted.AccessToken, TokenType: "Bearer", Expiry: expiry}, nil
}

// account is the email of the account the pod runs as, as the metadata server names it, refused
// with ErrNoServiceAccount unless it is one Google service account (secrets.ValidServiceAccountEmail).
func (d directoryToken) account() (string, error) {
	request, err := http.NewRequest(http.MethodGet, cmp.Or(d.o.MetadataURL, googleMetadataURL)+emailPath, nil)
	if err != nil {
		return "", fmt.Errorf("the metadata server: %w", err)
	}
	request.Header.Set("Metadata-Flavor", "Google")
	payload, err := call(d.o.HTTP, request, "the metadata server")
	if err != nil {
		return "", err
	}
	email := strings.TrimSpace(string(payload))
	if !secrets.ValidServiceAccountEmail(email) {
		return "", fmt.Errorf("%w: the metadata server names %q", ErrNoServiceAccount, email)
	}
	return email, nil
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
// who answered otherwise. An answer past answerLimit is refused rather than read in part.
func call(client *http.Client, request *http.Request, who string) ([]byte, error) {
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", who, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, answerLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read the answer: %w", who, err)
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
