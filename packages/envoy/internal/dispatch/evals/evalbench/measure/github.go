package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// `measure github` is k14's stand-in for GitHub on loopback, the API the server's GitHub App calls
// (DISPATCH_GITHUB_API_BASE): the settings route proves each github_workflow source's repository
// before it saves (api's checkSourceAccess, githubapp.Client.CheckSource), and the bench's server
// ran with no App, which refused that proof (409 DELIVERY_SETTINGS_ACCESS). The stand-in makes the
// App's key itself, a throwaway it writes to -key-out for the server's DISPATCH_APP_PEM_B64, so no
// key GitHub knows is ever loaded, and it serves the three reads CheckSource makes for a repository
// of an owner it is installed on (-owners):
//
//   - GET /repos/{owner}/{repo}/installation, with the App's JWT, signed by that key under
//     -client-id and not expired: the installation, with Contents read;
//   - POST /app/installations/{id}/access_tokens, with the App's JWT: a token of that installation,
//     an hour long;
//   - GET /repos/{owner}/{repo}, with a token it minted: the repository.
//
// The installation grants no Actions and no Pull-requests read, so the delivery reconcile's
// permission check (githubapp.Client.DeliveryPermissions) answers ErrNoActionsRead for every source
// repository and the reconcile walks no repository (delivery.Reconcile.runOnce): every five minutes
// it writes each source's last_error, bookkeeping that logs no settings change, and reads GitHub no
// further, so it brings no run into the fixture while k14 measures. Every other request, and one
// whose credential is not the App's or one of its tokens, is answered 404 or 401 as GitHub answers
// it. Each request is a line of the log (-log), which `measure k14` counts by route.
//
//	measure github -listen 127.0.0.1:<port> -key-out <file> -client-id <id> -owners acme -log <file>

// githubInstallationID is the one installation of the stand-in.
const githubInstallationID = int64(4242)

// githubTokenPrefix starts every token the stand-in mints, as GitHub's installation tokens start
// with ghs_.
const githubTokenPrefix = "ghs_evalbench_"

// gitHubFake is the stand-in: the App's public key and client id it verifies the App's JWT
// against, the owners it is installed on, the tokens it minted, and the log of its requests.
type gitHubFake struct {
	public   *rsa.PublicKey
	clientID string
	owners   []string
	now      func() time.Time
	log      *log.Logger

	mu     sync.Mutex
	minted map[string]time.Time
}

// newGitHubKey makes the throwaway App key and its PKCS#1 PEM.
func newGitHubKey() (*rsa.PrivateKey, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", fmt.Errorf("make the App's key: %w", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, string(encoded), nil
}

// serveGitHub runs the stand-in until it is sent SIGTERM or SIGINT.
func serveGitHub(listen, keyOut, clientID, owners, logPath string) error {
	switch {
	case listen == "" || keyOut == "" || clientID == "" || owners == "":
		return errors.New("github needs -listen, -key-out, -client-id and -owners")
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return fmt.Errorf("the stand-in listens on loopback alone, not %q", listen)
	}
	key, encoded, err := newGitHubKey()
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyOut, []byte(encoded), 0o600); err != nil {
		return fmt.Errorf("write the App's key: %w", err)
	}
	out := os.Stderr
	if logPath != "" {
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer file.Close()
		out = file
	}
	fake := &gitHubFake{public: &key.PublicKey, clientID: clientID, owners: strings.Split(owners, ","), now: time.Now,
		log: log.New(out, "", 0), minted: map[string]time.Time{}}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: fake, ReadHeaderTimeout: 10 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Fprintf(os.Stderr, "measure github: serving %s for the App %s, installed on %s\n", listener.Addr(), clientID, owners)
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ServeHTTP answers one request and logs it: `github route=<route> method=<m> path=<p>
// status=<code>`.
func (f *gitHubFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, status, body := f.answer(r)
	f.log.Printf("github at=%s route=%s method=%s path=%s status=%d", f.now().UTC().Format(time.RFC3339Nano), route, r.Method, r.URL.Path, status)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// answer is the route a request takes, the status it is answered and its body.
func (f *gitHubFake) answer(r *http.Request) (string, int, any) {
	notFound := map[string]string{"message": "Not Found", "documentation_url": "https://docs.github.com/rest"}
	refused := map[string]string{"message": "Bad credentials", "documentation_url": "https://docs.github.com/rest"}
	credential, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if rest, ok := strings.CutPrefix(r.URL.Path, "/app/installations/"); ok && r.Method == http.MethodPost {
		id, isToken := strings.CutSuffix(rest, "/access_tokens")
		if !isToken {
			return "other", http.StatusNotFound, notFound
		}
		if err := f.verifyJWT(credential); err != nil {
			return "token", http.StatusUnauthorized, refused
		}
		if id != strconv.FormatInt(githubInstallationID, 10) {
			return "token", http.StatusNotFound, notFound
		}
		token, expires := f.mint()
		return "token", http.StatusCreated, map[string]any{"token": token, "expires_at": expires.Format(time.RFC3339),
			"permissions": map[string]string{"contents": "read", "metadata": "read"}}
	}
	rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
	if !isRepository || r.Method != http.MethodGet {
		return "other", http.StatusNotFound, notFound
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "other", http.StatusNotFound, notFound
	}
	owner, name := parts[0], parts[1]
	installed := slices.ContainsFunc(f.owners, func(one string) bool { return strings.EqualFold(one, owner) })
	switch {
	case len(parts) == 3 && parts[2] == "installation":
		if err := f.verifyJWT(credential); err != nil {
			return "installation", http.StatusUnauthorized, refused
		}
		if !installed {
			return "installation", http.StatusNotFound, notFound
		}
		return "installation", http.StatusOK, map[string]any{"id": githubInstallationID, "app_slug": "evalbench",
			"permissions": map[string]string{"contents": "read", "metadata": "read"}}
	case len(parts) == 2:
		if !f.heldToken(credential) {
			return "repository", http.StatusUnauthorized, refused
		}
		if !installed {
			return "repository", http.StatusNotFound, notFound
		}
		id := fnv.New32a()
		id.Write([]byte(strings.ToLower(owner + "/" + name)))
		return "repository", http.StatusOK, map[string]any{"id": id.Sum32(), "name": name, "full_name": owner + "/" + name,
			"private": true, "owner": map[string]string{"login": owner}, "default_branch": "main"}
	}
	return "other", http.StatusNotFound, notFound
}

// mint is a new installation token, an hour long.
func (f *gitHubFake) mint() (string, time.Time) {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	token := githubTokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	expires := f.now().Add(time.Hour)
	f.mu.Lock()
	f.minted[token] = expires
	f.mu.Unlock()
	return token, expires
}

// heldToken reports whether token is one the stand-in minted and it has not expired.
func (f *gitHubFake) heldToken(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	expires, held := f.minted[token]
	return held && f.now().Before(expires)
}

// verifyJWT checks that token is an RS256 JWT the App's key signed, issued by the App's client id
// and not expired, as GitHub checks the App's JWT.
func (f *gitHubFake) verifyJWT(token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	for at, into := range []any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[at])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, into); err != nil {
			return err
		}
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch {
	case header.Alg != "RS256":
		return fmt.Errorf("the JWT is signed %s, not RS256", header.Alg)
	case rsa.VerifyPKCS1v15(f.public, crypto.SHA256, digest[:], signature) != nil:
		return errors.New("the JWT is not signed by the App's key")
	case claims.Iss != f.clientID:
		return fmt.Errorf("the JWT is issued by %q, not the App %q", claims.Iss, f.clientID)
	case !f.now().Before(time.Unix(claims.Exp, 0)):
		return errors.New("the JWT has expired")
	}
	return nil
}
