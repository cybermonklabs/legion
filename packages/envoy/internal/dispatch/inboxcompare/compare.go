package inboxcompare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// DB is the database both servers serve, which the comparison reads its people, their
// conversations and the reply chains from, and writes its run row to.
type DB interface {
	Querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Options is one comparison: the base build's address and this build's, and the people it is not
// to reach, each of whom it counts as uncovered.
type Options struct {
	OldURL  string
	NewURL  string
	Exclude []string
}

// Refusal is why a comparison read nothing and stored no run: an input it cannot use, or a server
// that serves no dev sign-in, through which alone it signs anybody in, so it never reads a
// deployment's rows.
type Refusal struct{ reason string }

func (refusal *Refusal) Error() string { return refusal.reason }

func refuse(format string, args ...any) error {
	return &Refusal{reason: fmt.Sprintf(format, args...)}
}

// Person is what the comparison found for one person.
type Person struct {
	Login string
	// Uncovered is why the run did not reach this person, and "" when it did.
	Uncovered string
	// Rows is how many rows each arm answered them, and Entries how many entries the one read did.
	Rows    map[string]int
	Entries int
	// Differences are the rows of theirs an old read answered and the one read did not.
	Differences []Difference
}

// Comparison is one run: what it found for each person, and the row it stored.
type Comparison struct {
	Old, New string
	People   []Person
	Run      Run
}

// requestTimeout bounds each request the comparison makes of either server.
const requestTimeout = time.Minute

// Compare signs each person the database holds in on both servers through dev sign-in, asks the
// three old reads on the old server and the one read on the new one, and stores the run's row.
//
// It reaches nobody until both servers have answered one person's dev sign-in with a session
// cookie, and refuses (Refusal) a server that does not: a deployment serves no dev sign-in, so the
// comparison reads no deployment's rows. A person named in Exclude, or one whose sign-in or read
// either server answered with anything but its rows, is uncovered: counted among the people
// checked and not compared, which leaves the gate unmet. The run is stored whatever it found, a
// missing shape and an uncovered person included, so a run that does not meet the gate stays
// readable as one.
func Compare(ctx context.Context, db DB, options Options) (*Comparison, error) {
	oldBase, err := serverURL("--old-url", options.OldURL)
	if err != nil {
		return nil, err
	}
	newBase, err := serverURL("--new-url", options.NewURL)
	if err != nil {
		return nil, err
	}
	logins, err := people(ctx, db)
	if err != nil {
		return nil, err
	}
	excluded := map[string]bool{}
	for _, login := range options.Exclude {
		canonical := model.CanonicalLogin(login)
		if !slices.Contains(logins, canonical) {
			return nil, refuse("--exclude names %s, whom the database holds as no person", login)
		}
		excluded[canonical] = true
	}
	probe := ""
	for _, login := range logins {
		if !excluded[login] {
			probe = login
			break
		}
	}
	if probe == "" {
		return nil, refuse("the database holds no person the run may reach (%d people, %d of them excluded), so there is nobody to compare",
			len(logins), len(excluded))
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	servers := []*server{{flag: "--old-url", base: oldBase, transport: transport}, {flag: "--new-url", base: newBase, transport: transport}}
	signedIn := map[*server]*http.Client{}
	for _, side := range servers {
		client, err := side.signIn(ctx, probe)
		if err != nil {
			return nil, refuse("%s %s refused the dev sign-in of %s, so the comparison reads nothing there: it signs in only through "+
				"GET /auth/_dev/signin, which a server serves under DISPATCH_DEV_SIGNIN=1 and a deployment never does: %v",
				side.flag, side.base, probe, err)
		}
		signedIn[side] = client
	}

	comparison := &Comparison{Old: oldBase.String(), New: newBase.String()}
	run := &comparison.Run
	run.PeopleChecked = len(logins)
	run.Shapes = map[string]int{}
	var reached []string
	for _, login := range logins {
		person := Person{Login: login, Rows: map[string]int{}}
		switch {
		case excluded[login]:
			person.Uncovered = "excluded by --exclude"
		default:
			var olds, news *http.Client
			if login == probe {
				olds, news = signedIn[servers[0]], signedIn[servers[1]]
			}
			rows, shapes, entries, err := compareOne(ctx, db, servers, login, olds, news)
			if err != nil {
				person.Uncovered = err.Error()
				break
			}
			reached = append(reached, login)
			person.Entries = len(entries)
			for _, row := range rows {
				person.Rows[row.arm]++
				if entries[row.node] {
					continue
				}
				person.Differences = append(person.Differences, Difference{
					Person: login, Arm: row.arm, Kind: row.kind, Row: row.row, Node: row.node,
				})
			}
			for shape, count := range shapes {
				run.Shapes[shape] += count
			}
			if len(rows) > 0 {
				run.Shapes[ShapePersonWithRows]++
			} else {
				run.Shapes[ShapePersonWithNone]++
			}
		}
		if person.Uncovered != "" {
			run.PeopleUncovered++
		}
		run.Differences = append(run.Differences, person.Differences...)
		comparison.People = append(comparison.People, person)
	}
	chains, err := replyChainsAtTheBound(ctx, db, reached)
	if err != nil {
		return nil, err
	}
	run.Shapes[ShapeReplyChain] = chains
	if err := Store(ctx, db, run); err != nil {
		return nil, err
	}
	return comparison, nil
}

// serverURL reads one server's address: an http or https URL naming a host, its path and query
// dropped, since every read below names its own path from the server's root.
func serverURL(flag, raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, refuse("%s %q is no http:// or https:// address of a server", flag, raw)
	}
	return &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}, nil
}

// people is every person the database holds, by canonical login, once each, in login order: the
// people table, which every sign-in records.
func people(ctx context.Context, db DB) ([]string, error) {
	rows, err := db.Query(ctx, `select email from people order by email`)
	if err != nil {
		return nil, fmt.Errorf("read the people: %w", err)
	}
	emails, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the people: %w", err)
	}
	var logins []string
	for _, email := range emails {
		if login := model.CanonicalLogin(email); !slices.Contains(logins, login) {
			logins = append(logins, login)
		}
	}
	slices.Sort(logins)
	return logins, nil
}

// server is one of the two servers the comparison reads.
type server struct {
	flag      string
	base      *url.URL
	transport http.RoundTripper
}

// signIn signs login in through dev sign-in and answers a client that carries their session
// cookie: GET /auth/_dev/signin?login=<login> answers 302 with that cookie on it, and anything
// else is refused, naming what the server answered.
func (side *server) signIn(ctx context.Context, login string) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Jar:       jar,
		Transport: side.transport,
		Timeout:   requestTimeout,
		// The sign-in answers 302 to the page with the cookie on it; the cookie is all a read needs.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	address := side.base.JoinPath("/auth/_dev/signin")
	address.RawQuery = url.Values{"login": {login}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 300))
	_ = response.Body.Close()
	carried, err := http.NewRequest(http.MethodGet, side.base.String(), nil)
	if err != nil {
		return nil, err
	}
	for _, cookie := range jar.Cookies(side.base) {
		carried.AddCookie(cookie)
	}
	if response.StatusCode != http.StatusFound || !auth.HasSessionCookie(carried) {
		return nil, fmt.Errorf("GET /auth/_dev/signin answered %d with no session cookie: %s", response.StatusCode, strings.TrimSpace(string(answer)))
	}
	return client, nil
}

// read asks path of the server as client's person and decodes its 200 answer into into.
func (side *server) read(ctx context.Context, client *http.Client, path string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, side.base.String()+path, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", side.flag, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read the answer: %w", side.flag, path, err)
	}
	if response.StatusCode != http.StatusOK {
		head := strings.TrimSpace(string(answer))
		if len(head) > 300 {
			head = head[:300]
		}
		return fmt.Errorf("%s answered %s with %d: %s", side.flag, path, response.StatusCode, head)
	}
	if err := json.Unmarshal(answer, into); err != nil {
		return fmt.Errorf("%s answered %s with a body the comparison cannot read: %w", side.flag, path, err)
	}
	return nil
}

// oldRow is one row an old read answered, and the node it stands for.
type oldRow struct {
	arm, kind, row, node string
	// shape is the corpus shape the row is a row of, "" for one that is no shape of its own.
	shape string
}

// compareOne asks login's three old reads of servers[0] and their one read of servers[1], signing
// them in on each where the client given is nil, and answers the old rows, the shapes those rows
// are, and the nodes the one read answered an entry of.
func compareOne(ctx context.Context, db DB, servers []*server, login string, olds, news *http.Client) ([]oldRow, map[string]int, map[string]bool, error) {
	old, current := servers[0], servers[1]
	var err error
	if olds == nil {
		if olds, err = old.signIn(ctx, login); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", old.flag, err)
		}
	}
	if news == nil {
		if news, err = current.signIn(ctx, login); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", current.flag, err)
		}
	}

	var asks []inboxAsk
	if err := old.read(ctx, olds, "/api/v1/inbox?assignee=me", &asks); err != nil {
		return nil, nil, nil, err
	}
	rows := inboxRows(asks)
	var replies repliesAnswer
	if err := old.read(ctx, olds, "/api/v1/me/replies?kind=reply,agent&state=inbox", &replies); err != nil {
		return nil, nil, nil, err
	}
	threads, err := repliesRows(replies.Rows)
	if err != nil {
		return nil, nil, nil, err
	}
	rows = append(rows, threads...)
	sessions, err := conversationSessions(ctx, db, login)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, session := range sessions {
		var conversations []conversation
		if err := old.read(ctx, olds, "/api/v1/agents/"+url.PathEscape(session)+"/messages", &conversations); err != nil {
			return nil, nil, nil, err
		}
		rows = append(rows, conversationRows(login, session, conversations)...)
	}

	var one oneRead
	if err := current.read(ctx, news, "/api/v1/me/inbox", &one); err != nil {
		return nil, nil, nil, err
	}
	if one.Person != login {
		return nil, nil, nil, fmt.Errorf("%s answered GET /api/v1/me/inbox for %q, signed in as %s", current.flag, one.Person, login)
	}
	entries := map[string]bool{}
	for _, entry := range one.Entries {
		entries[entry.Node.Kind+":"+entry.Node.ID] = true
	}
	shapes := map[string]int{}
	for _, row := range rows {
		if row.shape != "" {
			shapes[row.shape]++
		}
	}
	return rows, shapes, entries, nil
}

// inboxAsk is what the comparison reads of a row of GET /api/v1/inbox.
type inboxAsk struct {
	ID string `json:"id"`
}

// inboxRows maps each Inbox ask row to its ask.
func inboxRows(asks []inboxAsk) []oldRow {
	rows := make([]oldRow, 0, len(asks))
	for _, ask := range asks {
		rows = append(rows, oldRow{
			arm: ArmInbox, kind: KindAssignedAsk, row: "ask:" + ask.ID, node: "ask:" + ask.ID, shape: ShapeInboxAsk,
		})
	}
	return rows
}

// repliesAnswer is what the comparison reads of GET /api/v1/me/replies: its rows, each a thread
// of one kind or an Agent row.
type repliesAnswer struct {
	Rows []replyRow `json:"rows"`
}

type replyRow struct {
	ThreadID string `json:"thread_id"`
	Kind     string `json:"kind"`
}

// The shape a Reply row of each kind of thread is.
var threadShapes = map[string]string{
	"ask":     ShapeAskThread,
	"comment": ShapeCommentThread,
	"message": ShapeMessageThread,
}

// repliesRows maps each row of GET /api/v1/me/replies to its node: a Reply row to its thread's
// root, a node of the thread's own kind (an ask, a comment or a message), and an Agent row to its
// root message. A row of any other kind is no row the comparison can map, and is refused.
func repliesRows(replies []replyRow) ([]oldRow, error) {
	rows := make([]oldRow, 0, len(replies))
	for _, reply := range replies {
		if reply.Kind == "agent" {
			rows = append(rows, oldRow{
				arm: ArmReplies, kind: KindDirectReply, row: "agent:" + reply.ThreadID,
				node: "message:" + reply.ThreadID, shape: ShapeAgentRow,
			})
			continue
		}
		shape, known := threadShapes[reply.Kind]
		if !known {
			return nil, fmt.Errorf("GET /api/v1/me/replies answered thread %s of kind %q, which is no kind of row the comparison maps to a node", reply.ThreadID, reply.Kind)
		}
		rows = append(rows, oldRow{
			arm: ArmReplies, kind: KindThreadReply, row: "reply:" + reply.ThreadID,
			node: reply.Kind + ":" + reply.ThreadID, shape: shape,
		})
	}
	return rows, nil
}

// conversationSessions is every session login started a conversation with: the session each of
// their direct roots was sent to, a root on no issue that answers nothing, as
// unreadDirectRepliesCTE's direct_roots reads a person's (api/unread_replies.go).
func conversationSessions(ctx context.Context, db DB, login string) ([]string, error) {
	rows, err := db.Query(ctx, `
		select distinct substr(target, length('session:') + 1)
		from messages
		where issue_key is null and in_reply_to is null and target like 'session:%'
		  and author->>'kind' = 'user' and lower(author->>'id') = $1
		order by 1
	`, login)
	if err != nil {
		return nil, fmt.Errorf("read the conversations %s started: %w", login, err)
	}
	sessions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the conversations %s started: %w", login, err)
	}
	return sessions, nil
}

// conversation is what the comparison reads of a row of GET /api/v1/agents/{session_id}/messages:
// a root and the replies under it.
type conversation struct {
	Message conversationMessage   `json:"message"`
	Replies []conversationMessage `json:"replies"`
}

type conversationMessage struct {
	ID       string      `json:"id"`
	IssueKey *string     `json:"issue_key"`
	Target   *string     `json:"target"`
	Author   model.Actor `json:"author"`
}

// conversationRows maps login's own conversations with session, of those the route answered, to
// their nodes: each root login wrote, on no issue and sent to session, to its message, and each
// reply the session wrote under it to its own message. A root somebody else wrote, or one on an
// issue, is no conversation of login's.
func conversationRows(login, session string, conversations []conversation) []oldRow {
	var rows []oldRow
	for _, held := range conversations {
		root := held.Message
		if root.IssueKey != nil || root.Target == nil || *root.Target != "session:"+session ||
			root.Author.Kind != "user" || model.CanonicalLogin(root.Author.ID) != login {
			continue
		}
		rows = append(rows, oldRow{
			arm: ArmConversations, kind: KindDirectReply, row: "message:" + root.ID, node: "message:" + root.ID,
		})
		for _, reply := range held.Replies {
			if reply.Author.Kind != "session" {
				continue
			}
			rows = append(rows, oldRow{
				arm: ArmConversations, kind: KindDirectReply, row: "message:" + reply.ID,
				node: "message:" + reply.ID, shape: ShapeConversationReply,
			})
		}
	}
	return rows
}

// oneRead is what the comparison reads of GET /api/v1/me/inbox: the person it answered for, and
// the node of each entry.
type oneRead struct {
	Person  string `json:"person"`
	Entries []struct {
		Node struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"node"`
	} `json:"entries"`
}

// replyChainsAtTheBound counts the comment reply chains ChainLinks links long, from a comment
// nothing answers up reply_to to its root, that hold a comment one of logins wrote: the reply
// chain at the outbox walk's bound, written by a person the run reached, which is what makes it a
// shape of their reads rather than a chain nobody compared. The walk up stops at ChainLinks, so a
// cycle ends it, and a chain longer than the bound is no chain at it.
func replyChainsAtTheBound(ctx context.Context, db DB, logins []string) (int, error) {
	if len(logins) == 0 {
		return 0, nil
	}
	var chains int
	if err := db.QueryRow(ctx, `
		with recursive up(parent, links, reached) as (
			select c.reply_to, 0, c.author->>'kind' = 'user' and lower(c.author->>'id') = any($2)
			from comments c
			where c.reply_to is not null
			  and not exists (select 1 from comments answer where answer.reply_to = c.id)
			union all
			select p.reply_to, up.links + 1,
			       up.reached or (p.author->>'kind' = 'user' and lower(p.author->>'id') = any($2))
			from up join comments p on p.id = up.parent
			where up.links < $1
		)
		select count(*) from up where parent is null and links = $1 and reached
	`, ChainLinks, logins).Scan(&chains); err != nil {
		return 0, fmt.Errorf("count the reply chains at the walk's bound: %w", err)
	}
	return chains, nil
}

// Write prints the comparison: one PASS: or FAIL: line per assertion, then a summary line the
// drivers read. A shape the corpus lacks refuses the run as proof, so no zero count of that run is
// printed as a pass; every difference it found is still printed.
func (comparison *Comparison) Write(out io.Writer) {
	run := comparison.Run
	missing := run.MissingShapes()
	fmt.Fprintf(out, "inbox-compare: the old reads on %s, the one read on %s, over %d people\n",
		comparison.Old, comparison.New, run.PeopleChecked)
	for _, shape := range Shapes {
		if rows := run.Shapes[shape]; rows > 0 {
			fmt.Fprintf(out, "PASS: shape %s: %d in the corpus\n", shape, rows)
		} else {
			fmt.Fprintf(out, "FAIL: shape %s: the corpus holds none of it, so this run is refused as proof\n", shape)
		}
	}
	for _, person := range comparison.People {
		if person.Uncovered != "" {
			fmt.Fprintf(out, "FAIL: uncovered person=%s: the run did not reach them: %s\n", person.Login, person.Uncovered)
		}
	}
	for _, difference := range run.Differences {
		fmt.Fprintf(out, "FAIL: difference person=%s arm=%s kind=%s row=%s node=%s: %s answers it and GET /api/v1/me/inbox does not\n",
			difference.Person, difference.Arm, difference.Kind, difference.Row, difference.Node, ArmRead(difference.Arm))
	}
	verdict := func(failed bool, format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		switch {
		case failed:
			fmt.Fprintf(out, "FAIL: %s\n", line)
		case len(missing) == 0:
			fmt.Fprintf(out, "PASS: %s\n", line)
		}
	}
	byKind := map[string]int{}
	for _, person := range comparison.People {
		if person.Uncovered != "" {
			continue
		}
		byArm := map[string]int{}
		for _, difference := range person.Differences {
			byArm[difference.Arm]++
			byKind[difference.Kind]++
		}
		for _, arm := range Arms {
			verdict(byArm[arm] > 0, "arm person=%s arm=%s: %d differences of the %d rows %s answered, beside the %d entries of the one read",
				person.Login, arm, byArm[arm], person.Rows[arm], ArmRead(arm), person.Entries)
		}
	}
	for _, kind := range Kinds {
		verdict(byKind[kind] > 0, "kind %s: %d differences", kind, byKind[kind])
	}
	verdict(run.DifferenceCount > 0, "differences: %d", run.DifferenceCount)
	verdict(run.PeopleUncovered > 0, "uncovered: %d of the %d people checked", run.PeopleUncovered, run.PeopleChecked)
	if len(missing) > 0 {
		fmt.Fprintf(out, "FAIL: comparison refused: the corpus lacks %s, and a count over a corpus without every shape proves nothing of it\n",
			strings.Join(missing, ", "))
	}
	fmt.Fprintf(out, "PASS: stored run %d at %s\n", run.ID, run.RanAt.UTC().Format(time.RFC3339Nano))
	if run.Met() {
		fmt.Fprintf(out, "PASS: gate met: no differences, nobody uncovered and every shape in the corpus\n")
	} else {
		fmt.Fprintf(out, "FAIL: gate not met: %s\n", strings.Join(unmet(run, missing), "; "))
	}
	missed := "none"
	if len(missing) > 0 {
		missed = strings.Join(missing, ",")
	}
	gate := "not-met"
	if run.Met() {
		gate = "met"
	}
	fmt.Fprintf(out, "inbox-compare: run=%d difference_count=%d people_checked=%d people_uncovered=%d missing_shapes=%s gate=%s\n",
		run.ID, run.DifferenceCount, run.PeopleChecked, run.PeopleUncovered, missed, gate)
}

// unmet says why run does not meet the gate, one clause a reason.
func unmet(run Run, missing []string) []string {
	var reasons []string
	if run.DifferenceCount > 0 {
		reasons = append(reasons, fmt.Sprintf("%d differences", run.DifferenceCount))
	}
	if run.PeopleUncovered > 0 {
		reasons = append(reasons, fmt.Sprintf("%d of %d people uncovered", run.PeopleUncovered, run.PeopleChecked))
	}
	if len(missing) > 0 {
		reasons = append(reasons, "the corpus lacks "+strings.Join(missing, ", "))
	}
	return reasons
}

// IsRefusal reports whether err is a Refusal: a run that read nothing and stored no row.
func IsRefusal(err error) bool {
	var refusal *Refusal
	return errors.As(err, &refusal)
}
