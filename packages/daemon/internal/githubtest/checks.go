package githubtest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// Checks is GitHub as a GitHub App's client reads the check runs of a commit: the App's
// installation on any repository, an installation token for it, the repository itself, and the
// check runs the test set on each commit (SetCheckRun), served at
// GET /repos/{owner}/{repo}/commits/{ref}/check-runs. Dispatch reads a check its CI job proves
// there, through its App client (packages/envoy/internal/dispatch/githubapp): it looks the
// installation up with the App's JWT, mints a token, and reads the commit's check runs with it,
// and it reads the repository with that token before it stores its delivery settings. Each read of
// a commit's check runs it answers is kept, in order, with the runs it served (Reads), so a test
// can say what Dispatch read and when.
//
// It checks each credential as GitHub does. The installation and the token mint take only a JWT
// the App's own key signed for the App's client id (App), and the repository and its check runs
// only a token the fake minted, which expires an hour after its mint. Each other request is
// answered 401 and kept (Refused).
//
// It reads the listing's filter, per_page and page as GitHub reads them (checkRunsQueryOf), and
// refuses any other parameter of it: a reader that asked for a part of a commit's runs and was
// served every one of them reads the same as a reader that asked for them all, so a fake that
// served the whole set whatever it was asked shows no rig a lost filter or a page never asked
// for. Envoy's poller asks filter=all&per_page=100&page=N and reads on while a page is full
// (githubapp's CommitCheckRuns).
//
// Every other route is answered 404 and kept (Unanswered): a Dispatch that holds an App calls
// GitHub for more than a test plans, such as its webhook redelivery sweep, so such a request is
// what a test that waited for nothing names, never a failure of its own.
type Checks struct {
	// URL is the server's root, which Dispatch is given as its DISPATCH_GITHUB_API_BASE.
	URL string
	// App is the one GitHub App the fake has an installation of, made for this server alone.
	App App

	public     *rsa.PublicKey
	mu         sync.Mutex
	runs       map[string][]CheckRun // by "owner/name@sha"
	tokens     map[string]time.Time  // each token minted, with its expiry
	reads      []CheckRunsRead
	unanswered []string
	refused    []string
}

// CheckRunsRead is one read of a commit's check runs the fake answered (Reads): when it answered,
// the repository and the commit read, the listing's filter, page size and page as the fake read
// them, defaults and all, and the runs it served on that page, as they stood at that moment.
type CheckRunsRead struct {
	At      time.Time
	Repo    string
	SHA     string
	Filter  string
	PerPage int
	Page    int
	Runs    []CheckRun
}

// App is a GitHub App as its client holds it: its client id, which its JWT names as the issuer,
// and its private key, a PEM, which signs that JWT. GitHub keeps the key's public half and checks
// each JWT against it, and so does the fake. Nothing of it is secret: it signs calls to the
// test's own fake alone.
type App struct {
	ClientID string
	Key      string
}

// CheckRun is one check run of a commit, with the fields Dispatch reads of each run it lists:
// GitHub's id, the job's name, its status and conclusion, its completion time, its page, its
// output, and the slug of the App that made it. GitHub keeps every attempt of a check, and
// Dispatch takes the attempt of the highest id of each name, so a run of a new id is a new
// attempt, and a run set with the id of one already set replaces it.
type CheckRun struct {
	ID          int64
	Name        string
	Status      string // queued, in_progress or completed
	Conclusion  string // empty until the run has completed
	CompletedAt time.Time
	HTMLURL     string // the run's page
	Output      CheckRunOutput
	AppSlug     string
}

// CheckRunOutput is a check run's output, as its job wrote it.
type CheckRunOutput struct {
	Title, Summary string
}

// checksInstallation is the id of the App's one installation, which covers every repository.
const checksInstallation = 1

// checksAppClientID is the client id of the App ServeChecks makes.
const checksAppClientID = "Iv1.scratchdispatch"

// ServeChecks starts Checks with an App of its own and no check run on any commit; it is closed
// when the test ends.
func ServeChecks(t *testing.T) *Checks {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("make the fake GitHub's App key: %v", err)
	}
	c := &Checks{
		App: App{ClientID: checksAppClientID,
			Key: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))},
		public: &key.PublicKey, runs: map[string][]CheckRun{}, tokens: map[string]time.Time{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", c.withAppJWT(c.installation))
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", c.withAppJWT(c.token))
	mux.HandleFunc("GET /repos/{owner}/{repo}", c.withToken(c.repository))
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{ref}/check-runs", c.withToken(c.checkRuns))
	mux.HandleFunc("/", c.notFound)
	c.URL = Server(t, Spec{Handle: mux.ServeHTTP})
	return c
}

// SetCheckRun sets run on commit sha of repo, "owner/name", from the next read on.
func (c *Checks) SetCheckRun(repo, sha string, run CheckRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := repo + "@" + sha
	runs := c.runs[key]
	if at := slices.IndexFunc(runs, func(held CheckRun) bool { return held.ID == run.ID }); at >= 0 {
		runs[at] = run
		return
	}
	c.runs[key] = append(runs, run)
}

// Unanswered is each request the server answered 404, as "METHOD /path", in the order it came.
func (c *Checks) Unanswered() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.unanswered)
}

// Reads is each read of a commit's check runs the server answered, in the order it answered them,
// with the runs each served. A read the server refused (Refused) served no run and is not in it.
func (c *Checks) Reads() []CheckRunsRead {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reads)
}

// Refused is each request the server refused, in the order it came: a credential GitHub would not
// take, as "METHOD /path: why" with its 401, and a listing the fake will not answer as asked, as
// "METHOD /path?query: why" with its 400 (refuseQuery).
func (c *Checks) Refused() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.refused)
}

// withAppJWT serves a route of the App's own, which GitHub answers only for a JWT the App's key
// signed: RS256, the App's client id as its issuer, issued by now and expiring within ten
// minutes. Any other request is answered 401 as GitHub answers it.
func (c *Checks) withAppJWT(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if why := c.appJWTRefusal(r.Header.Get("Authorization")); why != "" {
			c.refuse(w, r, "A JSON web token could not be decoded", why)
			return
		}
		next(w, r)
	}
}

// appJWTRefusal is why authorization is no App JWT GitHub would take, or "" where it is one.
func (c *Checks) appJWTRefusal(authorization string) string {
	jwt, ok := strings.CutPrefix(authorization, "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		return "no App JWT"
	}
	var header struct {
		Algorithm string `json:"alg"`
	}
	if encoded, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil || json.Unmarshal(encoded, &header) != nil {
		return "the JWT's header does not decode"
	}
	if header.Algorithm != "RS256" {
		return fmt.Sprintf("the JWT is signed with %q, not RS256", header.Algorithm)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "the JWT's signature does not decode"
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(c.public, crypto.SHA256, digest[:], signature) != nil {
		return "the JWT does not verify against the App's key"
	}
	var claims struct {
		Issuer  string `json:"iss"`
		Issued  int64  `json:"iat"`
		Expires int64  `json:"exp"`
	}
	if encoded, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil || json.Unmarshal(encoded, &claims) != nil {
		return "the JWT's claims do not decode"
	}
	switch now := time.Now().Unix(); {
	case claims.Issuer != c.App.ClientID:
		return fmt.Sprintf("the JWT's issuer is %q, not the App's client id %q", claims.Issuer, c.App.ClientID)
	case claims.Issued == 0 || claims.Issued > now:
		return fmt.Sprintf("the JWT's issue time %d is not by now, %d", claims.Issued, now)
	case claims.Expires <= now || claims.Expires > now+int64((10*time.Minute).Seconds()):
		return fmt.Sprintf("the JWT expires at %d, not within ten minutes of now, %d", claims.Expires, now)
	}
	return ""
}

// withToken serves a read GitHub answers only for an installation token it minted: a request
// with no token is answered 401 Requires authentication, and one with a token the fake did not
// mint, or one past its expiry, 401 Bad credentials.
func (c *Checks) withToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if authorization == "" {
			c.refuse(w, r, "Requires authentication", "no token")
			return
		}
		token, ok := strings.CutPrefix(authorization, "Bearer ")
		if !ok {
			token, ok = strings.CutPrefix(authorization, "token ")
		}
		c.mu.Lock()
		expires, minted := c.tokens[token]
		c.mu.Unlock()
		switch {
		case !ok || !minted:
			c.refuse(w, r, "Bad credentials", "a token the fake did not mint")
			return
		case !time.Now().Before(expires):
			c.refuse(w, r, "Bad credentials", "a token past its expiry")
			return
		}
		next(w, r)
	}
}

// refuse answers r 401 with GitHub's message and keeps it, with why.
func (c *Checks) refuse(w http.ResponseWriter, r *http.Request, message, why string) {
	c.mu.Lock()
	c.refused = append(c.refused, r.Method+" "+r.URL.Path+": "+why)
	c.mu.Unlock()
	writeJSON(w, http.StatusUnauthorized, map[string]string{"message": message, "documentation_url": "https://docs.github.com/rest"})
}

// refuseQuery answers r 400 with why as GitHub writes a message, and keeps the request with its
// query beside the credentials the fake refused (Refused). GitHub passes over a parameter it
// does not know; the fake refuses instead, since a reader it served the whole commit after
// asking for a part of it would read as one that asked for the whole, and the rig would call
// that a pass.
func (c *Checks) refuseQuery(w http.ResponseWriter, r *http.Request, why string) {
	c.mu.Lock()
	c.refused = append(c.refused, r.Method+" "+r.URL.RequestURI()+": "+why)
	c.mu.Unlock()
	writeJSON(w, http.StatusBadRequest, map[string]string{"message": why, "documentation_url": "https://docs.github.com/rest"})
}

// installation answers the App's installation on any repository, with the permissions a read of
// a repository's contents and check runs needs.
func (c *Checks) installation(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"id": checksInstallation, "app_slug": "scratch-dispatch", "permissions": checksPermissions})
}

// token mints an installation token of the one installation, a new one at each mint, valid for
// an hour.
func (c *Checks) token(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != strconv.Itoa(checksInstallation) {
		c.notFound(w, r)
		return
	}
	random := make([]byte, 18)
	_, _ = rand.Read(random)
	token, expires := "ghs_"+hex.EncodeToString(random), time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	c.mu.Lock()
	c.tokens[token] = expires
	c.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "expires_at": expires.Format(time.RFC3339), "permissions": checksPermissions})
}

// checksPermissions is what the installation and each token it mints may read.
var checksPermissions = map[string]string{"metadata": "read", "contents": "read", "checks": "read", "pull_requests": "read"}

// repository answers any repository, on its default branch main.
func (c *Checks) repository(w http.ResponseWriter, r *http.Request) {
	owner, name := r.PathValue("owner"), r.PathValue("repo")
	writeJSON(w, http.StatusOK, map[string]any{"id": 1, "name": name, "full_name": owner + "/" + name,
		"owner": map[string]string{"login": owner}, "private": false, "default_branch": "main"})
}

// checkRuns answers the check runs set on the commit as GitHub's listing answers them: the runs
// the query's filter leaves, the page its per_page and page name, and total_count every run the
// filter leaves rather than the page's, which is what GitHub counts there. A query the fake will
// not answer as asked is refused (checkRunsQueryOf, refuseQuery) and is no read. It keeps each
// read it answers, with the parameters it read and the runs it served (Reads).
func (c *Checks) checkRuns(w http.ResponseWriter, r *http.Request) {
	repo, sha := r.PathValue("owner")+"/"+r.PathValue("repo"), r.PathValue("ref")
	asked, why := checkRunsQueryOf(r.URL.Query())
	if why != "" {
		c.refuseQuery(w, r, why)
		return
	}
	c.mu.Lock()
	left := slices.Clone(c.runs[repo+"@"+sha])
	served := left
	c.reads = append(c.reads, CheckRunsRead{At: time.Now(), Repo: repo, SHA: sha,
		Filter: asked.filter, PerPage: asked.perPage, Page: asked.page, Runs: served})
	c.mu.Unlock()
	listed := make([]map[string]any, 0, len(served))
	for _, run := range served {
		listed = append(listed, run.listed(sha))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(left), "check_runs": listed})
}

// checkRunsQuery is one listing's query as GitHub reads it: its filter, latest or all; how many
// runs a page holds; and which page, counted from 1.
type checkRunsQuery struct {
	filter  string
	perPage int
	page    int
}

// checkRunsLatest and checkRunsAll are the two filters GitHub's listing takes, latest its
// default; checkRunsPerPage is its default page size and checkRunsPerPageMost the largest it
// takes, which it cuts a larger one down to.
const (
	checkRunsLatest      = "latest"
	checkRunsAll         = "all"
	checkRunsPerPage     = 30
	checkRunsPerPageMost = 100
)

// checkRunsQueryOf is query as GitHub's listing reads it, with GitHub's own defaults for what it
// leaves out, and why the fake will not answer it, "" where it will: a parameter the fake does
// not model — GitHub's own check_name, status and app_id among them — and a value GitHub itself
// refuses are each a refusal, so a reader that starts to send one is told, rather than served
// runs it asked to leave out.
func checkRunsQueryOf(query url.Values) (checkRunsQuery, string) {
	asked := checkRunsQuery{filter: checkRunsLatest, perPage: checkRunsPerPage, page: 1}
	for name := range query {
		if name != "filter" && name != "per_page" && name != "page" {
			return asked, fmt.Sprintf("the listing's %s is no parameter this fake models", name)
		}
	}
	if raw := query.Get("filter"); raw != "" {
		if raw != checkRunsLatest && raw != checkRunsAll {
			return asked, fmt.Sprintf("filter %q is neither latest nor all", raw)
		}
		asked.filter = raw
	}
	if raw := query.Get("per_page"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 {
			return asked, fmt.Sprintf("per_page %q is no whole number above zero", raw)
		}
		asked.perPage = min(size, checkRunsPerPageMost)
	}
	if raw := query.Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			return asked, fmt.Sprintf("page %q is no whole number above zero", raw)
		}
		asked.page = page
	}
	return asked, ""
}

// checkRunsFiltered is the runs the listing's filter leaves, in the order they were set: every
// one for all, and for latest the newest attempt of each check name, which is the run of the
// highest id, as Dispatch reads an attempt (CheckRun).
func checkRunsFiltered(runs []CheckRun, filter string) []CheckRun {
	if filter == checkRunsAll {
		return slices.Clone(runs)
	}
	latest := make([]CheckRun, 0, len(runs))
	for _, run := range runs {
		newer := func(other CheckRun) bool { return other.Name == run.Name && other.ID > run.ID }
		if !slices.ContainsFunc(runs, newer) {
			latest = append(latest, run)
		}
	}
	return latest
}

// checkRunsPage is the page of runs per_page and page name, and none at all past the end. The
// page is tested against the pages there are before it is counted out, so a page number far past
// the end cannot overflow its product.
func checkRunsPage(runs []CheckRun, perPage, page int) []CheckRun {
	if len(runs) == 0 || page-1 > (len(runs)-1)/perPage {
		return nil
	}
	from := (page - 1) * perPage
	return runs[from:min(from+perPage, len(runs))]
}

// listed is run as GitHub lists it on commit sha: a field the run does not have yet is null.
func (run CheckRun) listed(sha string) map[string]any {
	listed := map[string]any{
		"id": run.ID, "name": run.Name, "head_sha": sha, "status": run.Status, "conclusion": nil, "completed_at": nil,
		"html_url": run.HTMLURL, "output": map[string]any{"title": nil, "summary": nil}, "app": map[string]any{"slug": run.AppSlug},
	}
	if run.Conclusion != "" {
		listed["conclusion"] = run.Conclusion
	}
	if !run.CompletedAt.IsZero() {
		listed["completed_at"] = run.CompletedAt.UTC().Format(time.RFC3339)
	}
	if run.Output.Title != "" || run.Output.Summary != "" {
		listed["output"] = map[string]any{"title": run.Output.Title, "summary": run.Output.Summary}
	}
	return listed
}

// notFound answers a request no route models 404 and keeps it.
func (c *Checks) notFound(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.unanswered = append(c.unanswered, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ChecksSettlement is what the listener's CI summary loop settles once every check run of a
// commit of a pull request has finished: the repository, "owner/name", the pull request's
// number, the commit, the newest attempt of each check run there, one run of each name, and the
// newest commit status of each context.
type ChecksSettlement struct {
	Repo     string
	Number   int
	SHA      string
	Runs     []CheckRun
	Statuses []CommitStatus
	// Generation numbers the listener's settlements of one commit from 1; zero is 1.
	Generation uint64
	// Superseded says the listener settled the commit before, which the payload's
	// superseded_settlement says.
	Superseded bool
}

// CommitStatus is one commit status of a commit: its context, its state, success, failure or
// error, and when GitHub created it, which the payload carries as GitHub writes it, RFC 3339 text
// in UTC.
type CommitStatus struct {
	Context   string
	State     string
	CreatedAt time.Time
}

// DispatchDeliveryDurable is the durable Dispatch's delivery intake reads GitHub's envelopes
// from, on the notification stream, the settlements PublishChecksSettlement publishes among them
// (Envoy's bus.DispatchDeliveryDurable).
const DispatchDeliveryDurable = "delivery-events"

// PublishChecksSettlement publishes s on the NATS server at natsURL as the listener's CI summary
// loop publishes a settlement (packages/envoy/internal/cistore, settlementEnvelope): an Envoy
// envelope on notifications.github.<owner>.<name>.pr.<number>.checks whose payload is the
// settlement in the shape of cistore.Summary, kind "checks" with its repo, number and sha, each
// run's name, id and conclusion, the groups of check names the listener's own rule puts each run
// in (cistore's classify), each commit status with its state and created_at, and
// superseded_settlement for a commit the listener settled before. It is stored on the
// notification stream, ENVOY_NOTIFICATIONS, which Dispatch's delivery intake and the daemon's
// intake both read; the call returns the stream sequence of the message once the stream has
// taken it. The shared fixture packages/contracts/fixtures/checks-settlement/published.json holds
// what the listener publishes, and this package's test holds the copy to it.
func PublishChecksSettlement(t *testing.T, natsURL string, s ChecksSettlement) uint64 {
	t.Helper()
	published, err := publishedSettlementOf(s)
	if err != nil {
		t.Fatalf("settle the checks of %s#%d at %s: %v", s.Repo, s.Number, s.SHA, err)
	}
	payload, err := json.Marshal(published.payload)
	if err != nil {
		t.Fatalf("encode the settlement of %s#%d at %s: %v", s.Repo, s.Number, s.SHA, err)
	}
	id := fmt.Sprintf("checks-%s-%d-%s-g%d-%d", strings.ReplaceAll(s.Repo, "/", "-"), s.Number, s.SHA, published.payload.Generation,
		time.Now().UnixNano())
	envelope, err := json.Marshal(map[string]any{
		"event_id": id, "source": "github", "source_event_id": id + "-settlement", "topic": published.subject,
		"dedupe_key":      published.dedupeKey,
		"issued_at":       time.Now().UnixMilli(),
		"payload_summary": fmt.Sprintf("checks settled on %s#%d @ %.7s", s.Repo, s.Number, s.SHA),
		"payload":         string(payload), "trace_id": id + "-trace",
	})
	if err != nil {
		t.Fatalf("encode the envelope of the settlement of %s#%d at %s: %v", s.Repo, s.Number, s.SHA, err)
	}
	js, err := jetstream.New(testnats.Connect(t, natsURL))
	if err != nil {
		t.Fatalf("open JetStream on %s: %v", natsURL, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ack, err := js.Publish(ctx, published.subject, envelope)
	if err != nil {
		t.Fatalf("publish the settlement of %s#%d at %s on %s: %v", s.Repo, s.Number, s.SHA, published.subject, err)
	}
	return ack.Sequence
}

// publishedSettlement is what the listener's publish makes of a settlement that its readers read
// (cistore's settlementEnvelope): the subject it is published on, the envelope's dedupe key and the
// payload.
type publishedSettlement struct {
	subject, dedupeKey string
	payload            settlement
}

// publishedSettlementOf is s as the listener publishes it.
func publishedSettlementOf(s ChecksSettlement) (publishedSettlement, error) {
	owner, name, ok := strings.Cut(s.Repo, "/")
	if !ok {
		return publishedSettlement{}, fmt.Errorf("a repository is owner/name, not %q", s.Repo)
	}
	generation := max(s.Generation, 1)
	return publishedSettlement{
		subject:   fmt.Sprintf("notifications.github.%s.%s.pr.%d.checks", subjectSegment(owner), subjectSegment(name), s.Number),
		dedupeKey: fmt.Sprintf("github.checks.%s.pr.%d.%s.g%d", s.Repo, s.Number, s.SHA, generation),
		payload:   settlementOf(s, generation),
	}, nil
}

// settlement is cistore.Summary's JSON, member for member and in its order.
type settlement struct {
	Kind                 string             `json:"kind"`
	Repo                 string             `json:"repo"`
	Number               string             `json:"number"`
	SHA                  string             `json:"sha"`
	CheckRuns            []settledRun       `json:"check_runs"`
	Generation           uint64             `json:"generation"`
	Snapshot             string             `json:"snapshot"`
	SupersededSettlement string             `json:"superseded_settlement,omitempty"`
	Failed               settledGroup       `json:"failed"`
	Running              settledGroup       `json:"running"`
	Passed               settledGroup       `json:"passed"`
	Queued               settledGroup       `json:"queued"`
	Cancelled            settledGroup       `json:"cancelled"`
	Skipped              settledGroup       `json:"skipped"`
	FailingChecks        []settledFailedRun `json:"failing_checks"`
	Statuses             []settledStatus    `json:"statuses,omitempty"`
}

type settledRun struct {
	Name       string `json:"name"`
	ID         int64  `json:"id"`
	Conclusion string `json:"conclusion,omitempty"`
}

type settledGroup struct {
	Count  int      `json:"count"`
	Checks []string `json:"checks"`
}

type settledFailedRun struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type settledStatus struct {
	Context   string `json:"context"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

// settlementOf is s as the listener renders it (cistore's renderSummary): its runs sorted by
// name, each group's names sorted, every group present, its commit statuses in context order and
// left out where it has none, and its snapshot the listener's fingerprint of its runs and
// statuses (settlementSnapshot).
func settlementOf(s ChecksSettlement, generation uint64) settlement {
	runs := slices.Clone(s.Runs)
	slices.SortFunc(runs, func(a, b CheckRun) int { return strings.Compare(a.Name, b.Name) })
	statuses := slices.Clone(s.Statuses)
	slices.SortFunc(statuses, func(a, b CommitStatus) int { return strings.Compare(a.Context, b.Context) })
	groups := map[string][]string{}
	failing := []settledFailedRun{}
	settled := settlement{Kind: "checks", Repo: s.Repo, Number: strconv.Itoa(s.Number), SHA: s.SHA, CheckRuns: []settledRun{},
		Generation: generation, Snapshot: settlementSnapshot(runs, statuses)}
	if s.Superseded {
		settled.SupersededSettlement = "true"
	}
	for _, run := range runs {
		settled.CheckRuns = append(settled.CheckRuns, settledRun{Name: run.Name, ID: run.ID, Conclusion: run.Conclusion})
		group := settledGroupOf(run)
		groups[group] = append(groups[group], run.Name)
		if group == "failed" {
			failing = append(failing, settledFailedRun{Name: run.Name, URL: run.HTMLURL})
		}
	}
	named := func(group string) settledGroup {
		names := append([]string{}, groups[group]...)
		slices.Sort(names)
		return settledGroup{Count: len(names), Checks: names}
	}
	settled.Failed, settled.Running, settled.Passed = named("failed"), named("running"), named("passed")
	settled.Queued, settled.Cancelled, settled.Skipped = named("queued"), named("cancelled"), named("skipped")
	settled.FailingChecks = failing
	for _, status := range statuses {
		settled.Statuses = append(settled.Statuses, settledStatus{Context: status.Context, State: status.State,
			CreatedAt: status.CreatedAt.UTC().Format(time.RFC3339)})
	}
	return settled
}

// settlementSnapshot is the listener's fingerprint of a record (cistore's State.Hash) of runs,
// sorted by name, and statuses, sorted by context, and of no check suite: each run's name, id,
// status and conclusion, then each status's context and state.
func settlementSnapshot(runs []CheckRun, statuses []CommitStatus) string {
	hash := sha256.New()
	for _, run := range runs {
		hash.Write([]byte("check\x00" + run.Name + "\x00" + strconv.FormatInt(run.ID, 10) + "\x00" + run.Status + "\x00" + run.Conclusion + "\x01"))
	}
	for _, status := range statuses {
		hash.Write([]byte("status\x00" + status.Context + "\x00" + status.State + "\x01"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// settledGroupOf is the group the listener puts run in (cistore's classify): a completed run by
// its conclusion, a conclusion it does not know failed; any other by its status, one it does not
// know queued.
func settledGroupOf(run CheckRun) string {
	if run.Status == "completed" {
		switch run.Conclusion {
		case "cancelled":
			return "cancelled"
		case "skipped":
			return "skipped"
		case "neutral", "success", "":
			return "passed"
		default:
			return "failed"
		}
	}
	if run.Status == "in_progress" {
		return "running"
	}
	return "queued"
}

// subjectSegment is value as one NATS subject segment, as Envoy's contracts write it
// (SanitizeSubjectSegment): a dot, whitespace and the wildcards become underscores.
func subjectSegment(value string) string {
	return subjectSegmentReplacer.Replace(value)
}

var subjectSegmentReplacer = strings.NewReplacer(".", "_", " ", "_", "\t", "_", "\r", "_", "\n", "_", "*", "_", ">", "_")
