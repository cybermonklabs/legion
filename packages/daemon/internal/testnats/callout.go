package testnats

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// calloutServiceUser is the CONNECT user a service presents beside its platform identity token
// (the envoy module's natscred.ServiceUser, which this fixture names itself rather than import so
// that what it checks is spelled independently of the client under test).
const calloutServiceUser = "service"

// CalloutServer is an in-process nats-server, with JetStream, whose clients an auth callout
// admits, and the test's stand-in for the NATS auth service answering it: the daemon module cannot
// import the envoy module's internal/testnats, so it is built from nats-server's own options. The
// stand-in refuses every CONNECT that is not user "service" with a token, and the first refusals
// CONNECTs of each token, and admits the rest as a user that may publish every subject and
// subscribe Subscribe, expiring when the token does (its 'exp', read unverified).
type CalloutServer struct {
	// URL is the server's client URL.
	URL string

	authUser, authPassword string
	subscribe              []string

	mu       sync.Mutex
	refusals int
	attempts map[string]int // token → CONNECTs it was presented on
	tokens   []string       // every token presented by the service user, in order
}

// StartCallout runs a CalloutServer for the test, stopped when it ends, whose stand-in refuses the
// first refusals CONNECTs of each token and admits the rest to subscribe subscribe.
func StartCallout(t testing.TB, refusals int, subscribe ...string) *CalloutServer {
	t.Helper()
	issuer, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	issuerPublic, err := issuer.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in service's own password is minted per run, so no credential-shaped literal sits
	// in the source.
	s := &CalloutServer{authUser: "auth", authPassword: rand.Text(), subscribe: subscribe,
		refusals: refusals, attempts: map[string]int{}}
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: server.RANDOM_PORT, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: t.TempDir(),
		Users:       []*server.User{{Username: s.authUser, Password: s.authPassword}},
		AuthCallout: &server.AuthCallout{Issuer: issuerPublic, Account: "$G", AuthUsers: []string{s.authUser}},
	})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("the in-process NATS server did not start")
	}
	s.URL = ns.ClientURL()
	service := s.Connect(t)
	if _, err := service.Subscribe("$SYS.REQ.USER.AUTH", func(msg *nats.Msg) { s.answer(t, issuer, msg) }); err != nil {
		t.Fatal(err)
	}
	if err := service.Flush(); err != nil {
		t.Fatal(err)
	}
	return s
}

// Connect connects to the server as its static user, which the auth callout never decides and
// which may publish and subscribe every subject, closed when the test ends.
func (s *CalloutServer) Connect(t testing.TB) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect(s.URL, nats.UserInfo(s.authUser, s.authPassword), nats.Name(t.Name()+" callout fixture"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// Presented is every token the service user presented, in order of its CONNECTs.
func (s *CalloutServer) Presented() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens...)
}

// answer answers one authorization request: a refusal, or the user the stand-in admits.
func (s *CalloutServer) answer(t testing.TB, issuer nkeys.KeyPair, msg *nats.Msg) {
	request, err := jwt.DecodeAuthorizationRequestClaims(string(msg.Data))
	if err != nil {
		t.Errorf("the callout fixture read no authorization request: %v", err)
		return
	}
	response := jwt.NewAuthorizationResponseClaims(request.UserNkey)
	response.Audience = request.Server.ID
	token := request.ConnectOptions.Password
	expiry, readable := expiryOf(token)
	switch {
	case s.refuse(request.ConnectOptions.Username, token):
		response.Error = "refused by the test's auth service"
	case !readable:
		// A token the stand-in cannot read an expiry from is a broken fixture, not a credential to
		// admit for a made-up life: the test fails, and the connection is refused.
		t.Errorf("the callout fixture was shown a token naming no expiry")
		response.Error = "the token names no expiry"
	default:
		user := jwt.NewUserClaims(request.UserNkey)
		user.Audience = "$G"
		user.Name = "service"
		user.Expires = expiry.Unix()
		user.Pub.Allow.Add(">")
		user.Sub.Allow.Add(s.subscribe...)
		if response.Jwt, err = user.Encode(issuer); err != nil {
			t.Errorf("the callout fixture could not sign a user: %v", err)
			return
		}
	}
	answer, err := response.Encode(issuer)
	if err != nil {
		t.Errorf("the callout fixture could not sign its answer: %v", err)
		return
	}
	_ = msg.Respond([]byte(answer))
}

// refuse records a CONNECT presenting user and token, and reports whether the stand-in refuses it.
func (s *CalloutServer) refuse(user, token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if user != calloutServiceUser || token == "" {
		return true
	}
	s.tokens = append(s.tokens, token)
	s.attempts[token]++
	return s.attempts[token] <= s.refusals
}

// expiryOf is token's 'exp', read unverified, and false when the token names none.
func expiryOf(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Expiry int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Expiry <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Expiry, 0), true
}
