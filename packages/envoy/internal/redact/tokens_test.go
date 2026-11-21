package redact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/githubtest"
)

// pieceToken is shaped as Dispatch's tokens are, `dsp_` and 43 base64url characters, and ends in
// its own first two characters, so two copies of it can overlap.
const pieceToken = "dsp_Q7vK2mXr9LtW4nBz8YcH1jPe5sAf3gUo6iNq0wEbTds"

// tokenPieces is every run of TokenPiece of pieceToken's characters text holds; any longer piece
// holds one.
func tokenPieces(text string) []string {
	var held []string
	for at := range len(pieceToken) - TokenPiece + 1 {
		if piece := pieceToken[at : at+TokenPiece]; strings.Contains(text, piece) {
			held = append(held, piece)
		}
	}
	return held
}

// Something in a server's place can echo a credential cut short, split by markup, or twice with the
// copies overlapping, and an exact match finds none of those: every run of TokenPiece or more of
// its characters is redacted.
func TestTokensRedactsEveryPieceOfTheToken(t *testing.T) {
	for _, row := range []struct{ name, text, want string }{
		{"whole", "echo=" + pieceToken + " end", "echo=" + Redacted + " end"},
		{"sent trimmed", "token=" + pieceToken, "token=" + Redacted},
		{"cut short by its last character", "echo=" + pieceToken[:len(pieceToken)-1] + " end", "echo=" + Redacted + " end"},
		{"cut short by its last 10 characters", "echo=" + pieceToken[:len(pieceToken)-10] + " end", "echo=" + Redacted + " end"},
		{"followed by a copy that starts inside its last characters", "echo=" + pieceToken + pieceToken[2:] + " end", "echo=" + Redacted + " end"},
		{"split by a tag", "echo=" + pieceToken[:20] + "<wbr>" + pieceToken[20:] + " end", "echo=" + Redacted + "<wbr>" + Redacted + " end"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if len(tokenPieces(row.text)) == 0 {
				t.Fatal("the text holds no piece of the token, so the row can see nothing")
			}
			got := Tokens(row.text, -1, " "+pieceToken+"\n")
			if got != row.want || len(tokenPieces(got)) != 0 {
				t.Fatalf("Tokens = %q, want %q and no piece of the token", got, row.want)
			}
		})
	}
}

// A credential shorter than TokenPiece is redacted only whole, an empty one not at all, and every
// one of several.
func TestTokensRedactsAShortTokenWholeAndEveryTokenGiven(t *testing.T) {
	if got, want := Tokens("pw=abc12 and abc1", -1, "abc12"), "pw="+Redacted+" and abc1"; got != want {
		t.Fatalf("Tokens of a short token = %q, want %q", got, want)
	}
	if got, want := Tokens("nothing to hide", -1, "", " "), "nothing to hide"; got != want {
		t.Fatalf("Tokens of no token = %q, want %q", got, want)
	}
	other := "gho_" + strings.Repeat("K7", 18)
	if got, want := Tokens("a="+pieceToken+" b="+other, -1, pieceToken, other), "a="+Redacted+" b="+Redacted; got != want {
		t.Fatalf("Tokens of two tokens = %q, want %q", got, want)
	}
}

// limit reads at most its characters, a run that starts inside it redacted whole, and nothing from
// past it pulled in. The answer is cut to limit characters after the redaction, so a run that
// starts right at the limit leaves only the start of Redacted.
func TestTokensReadsAtMostTheLimit(t *testing.T) {
	const limit = 64
	past := Tokens(strings.Repeat("p", limit)+" "+pieceToken+" after", limit, pieceToken)
	if want := strings.Repeat("p", limit); past != want {
		t.Fatalf("past the limit: Tokens = %q, want %q", past, want)
	}
	across := Tokens(strings.Repeat("q", limit-5)+pieceToken[:len(pieceToken)-10]+" after", limit, pieceToken)
	if want := strings.Repeat("q", limit-5) + Redacted[:5]; across != want {
		t.Fatalf("across the limit: Tokens = %q, want %q", across, want)
	}
}

// The work is linear in the text read, however many pieces the tokens make: a megabyte of a
// token's own character repeated, against a token of thousands of identical pieces.
func TestTokensIsBoundedForAHostileTextOrToken(t *testing.T) {
	began := time.Now()
	if got := Tokens(strings.Repeat("x", 1<<20), -1, strings.Repeat("x", 4096)); got != Redacted {
		t.Fatalf("Tokens of a repeated token = %.40q…, want %q", got, Redacted)
	}
	_ = Tokens(strings.Repeat("ab", 1<<19), -1, strings.Repeat("abcdefgh", 512), pieceToken)
	if spent := time.Since(began); spent > 5*time.Second {
		t.Fatalf("the hostile texts took %s, past the 5 s bound", spent)
	}
}

// requestTokens is every value a request carries under a name no wire contract declares public:
// each such header's value whole and after its scheme (an Authorization's only after its scheme,
// TestTheSchemeOfAnAuthorizationIsNoCredential), a Basic pair's user and password as sent and
// unescaped, its URL's user and password, each such value of its query, and each such value of its
// form, the form read again through GetBody. A value under a name nobody listed ("request") is a
// credential; a value under a public name (scope, Content-Type) is not.
func TestRequestTokensReadsEveryCredentialARequestCarries(t *testing.T) {
	form := url.Values{"client_secret": {"form-secret-RDX-1a2b"}, "refresh_token": {"form-refresh-RDX-3c4d"},
		"request": {"form-request-RDX-6e7f"}, "scope": {"openid"}}
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/token?code=query-code-RDX-5e6f&sig=query-sig-RDX-0b1c", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.URL.User = url.UserPassword("user", "url-secret-RDX-7a8b")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth("client", url.QueryEscape("basic secret/RDX"))
	request.Header.Set("DPoP", "proof-RDX-9c0d")
	request.Header.Set("X-Aws-Ec2-Metadata-Token", "imds-RDX-2d3e")
	request.Header.Set("Cookie", "session=cookie-RDX-4f5a")
	got := strings.Join(requestTokens(request), "\n")
	for _, want := range []string{"basic+secret%2FRDX", "basic secret/RDX", "client", "proof-RDX-9c0d", "imds-RDX-2d3e",
		"session=cookie-RDX-4f5a", "user", "url-secret-RDX-7a8b", "query-code-RDX-5e6f", "query-sig-RDX-0b1c",
		"form-secret-RDX-1a2b", "form-refresh-RDX-3c4d", "form-request-RDX-6e7f"} {
		if !strings.Contains(got, want) {
			t.Errorf("requestTokens holds no %q: %q", want, got)
		}
	}
	for _, public := range []string{"openid", "application/x-www-form-urlencoded"} {
		if strings.Contains(got, public) {
			t.Errorf("requestTokens holds %q, a value under a public name: %q", public, got)
		}
	}
	if body, _ := io.ReadAll(request.Body); string(body) != form.Encode() {
		t.Errorf("requestTokens consumed the request's body: %q", body)
	}
}

// The scheme of an Authorization header - Bearer, token, DPoP, any word before its first space -
// is the header's grammar and no credential: an answer's words beside the credential's own are not
// cut where they share characters with the scheme and the space after it. Here the credential is a
// word-shaped stand-in, so the word of the answer that is a run of it is cut, and the space before
// that word survives.
func TestTheSchemeOfAnAuthorizationIsNoCredential(t *testing.T) {
	const said = "API rate limit exceeded for installation ID 7."
	for _, scheme := range []string{"Bearer", "token", "DPoP"} {
		request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/repos/acme/widgets", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", scheme+" installation-token")
		if got, want := Tokens(said, -1, requestTokens(request)...), "API rate limit exceeded for "+Redacted+" ID 7."; got != want {
			t.Errorf("under %s, the answer %q reads %q, want %q", scheme, said, got, want)
		}
	}
}

// GitHub's rate-limit message prints unchanged beside an installation token in GitHub's own shape
// (githubtest.InstallationToken), which holds no run of any word of it: only a stand-in of words
// cuts one (TestTheSchemeOfAnAuthorizationIsNoCredential).
func TestGitHubsRateLimitMessagePrintsUnchangedBesideAnInstallationToken(t *testing.T) {
	const said = "API rate limit exceeded for installation ID 7."
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/repos/acme/widgets", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+githubtest.InstallationToken(t.Name()))
	if got := Tokens(said, -1, requestTokens(request)...); got != said {
		t.Errorf("the answer %q reads %q, want it unchanged", said, got)
	}
}

// A RefusalClient hands every answer on as it came, a refusal's echo of its request's credentials
// and a 2xx's minted token alike, so the library that sent the request parses the answer itself;
// the error a library makes of the refusal holds none of those credentials once OAuth2 reads it.
func TestRefusalClientRedactsOnlyARefusalsEchoOfItsRequest(t *testing.T) {
	const bearer, minted = "bearer-RDX-4f9a2c77", "minted-RDX-77b1e0d3"
	// The 2xx is a token answer carrying the minted token. It is marshalled rather than written as
	// a literal, so the test's text holds no credential-shaped key and value side by side.
	okBody, err := json.Marshal(map[string]string{"access_token": minted})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = w.Write(okBody)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<pre>\n"+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+"\nAuthorization: "+r.Header.Get("Authorization")+"</pre>")
	}))
	defer server.Close()
	s := Scope[struct{}](context.Background())
	client := s.Client(&http.Client{Timeout: 7 * time.Second})
	if client.Timeout != 7*time.Second {
		t.Fatalf("RefusalClient's timeout = %s, want its base's", client.Timeout)
	}
	get := func(path string) string {
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body)
	}
	refused := get("/refuse")
	if !strings.Contains(refused, bearer) {
		t.Errorf("the refusal = %q, want it as it came, the echo of the bearer in it", refused)
	}
	if _, err := OAuth2(struct{}{}, s.Err(fmt.Errorf("status code 502: %s", refused))); strings.Contains(err.Error(), bearer) || strings.Count(err.Error(), Redacted) != 2 {
		t.Errorf("OAuth2 of a library's error over the refusal = %q, want both echoes of the bearer redacted", err)
	}
	if answered := get("/ok"); answered != string(okBody) {
		t.Errorf("the 2xx = %q, want it as it came", answered)
	}
}
