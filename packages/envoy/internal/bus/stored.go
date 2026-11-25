package bus

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// storedMessageAPI is the JetStream API subject of a stored-message read of the notification
// stream. It is the one grant the read needs beyond STREAM.INFO: a read that cannot create, change
// or delete anything (packages/envoy/infra/core/permissions.go, ListenerPermissions).
const storedMessageAPI = "$JS.API.STREAM.MSG.GET." + Stream

// storedMessageWait bounds a stored-message read whose caller set no deadline of its own.
const storedMessageWait = 10 * time.Second

// StoredMessage is one message the notification stream holds: its stream sequence, the concrete
// subject it was published on, when the stream stored it, its body, and the Nats-Msg-Id it was
// published under (empty for one published under none).
type StoredMessage struct {
	Sequence uint64
	Subject  string
	Time     time.Time
	Data     []byte
	MsgID    string
}

// NextStoredMessage reads the first message the notification stream holds at or after sequence seq
// on a subject that subject matches (wildcards allowed), with STREAM.MSG.GET and next_by_subj. A
// sequence below the stream's first one reads from the first message it still holds.
//
// Its errors are the stream's own API errors as *nats.APIError, so errors.Is matches
// nats.ErrMsgNotFound (nothing at or after seq on subject) and nats.ErrStreamNotFound (no such
// stream); nats.ErrNoResponders when no JetStream answers; ErrPublishDenied, wrapping the server's
// permissions violation, when the connection's user may not make the read, reported as soon as the
// server refuses it rather than once the wait runs out; and ctx's error when ctx ends first.
func (c *Client) NextStoredMessage(ctx context.Context, subject string, seq uint64) (StoredMessage, error) {
	return c.storedMessage(ctx, struct {
		Seq     uint64 `json:"seq"`
		NextFor string `json:"next_by_subj"`
	}{Seq: seq, NextFor: subject})
}

// LastStoredMessage reads the newest message the notification stream holds on a subject that
// subject matches (wildcards allowed), with STREAM.MSG.GET and last_by_subj. The listener's kept
// lanes are read this way: a role's kept subject and its drain cursor each hold the one message
// that counts, and that is the newest. It is the same request NextStoredMessage makes, over the
// same stored-message API subject (storedMessageAPI), so it needs no grant beyond the one the
// listener already holds for that read.
//
// Its errors are NextStoredMessage's: the stream's own API errors as *nats.APIError, so errors.Is
// matches nats.ErrMsgNotFound (the stream holds nothing on subject) and nats.ErrStreamNotFound (no
// such stream); nats.ErrNoResponders when no JetStream answers; ErrPublishDenied when the
// connection's user may not make the read; and ctx's error when ctx ends first.
func (c *Client) LastStoredMessage(ctx context.Context, subject string) (StoredMessage, error) {
	return c.storedMessage(ctx, struct {
		LastFor string `json:"last_by_subj"`
	}{LastFor: subject})
}

// KeptRoleClaimWindow is how long one walk's claim of a kept copy holds that role's kept lane
// against every other writer of the lane (KeptRoleCursor.Claiming, KeptRoleCursor.ClaimOutstanding):
// the walks of the other listeners of the deployment, and the handover Dispatch's outbox makes
// behind a keep of its own. A claim younger than it may still belong to a walk that is
// forwarding that copy right now, which nothing else must forward a second time, and a claim
// older than it belongs to a walk whose context has ended, so the copy it claimed is free for
// the next walk to forward.
//
// It lives here, in the one package both writers of a kept lane import, because two durations
// meant to be equal are how two writers of a lane drift apart. The listener bounds one walk of a
// lane by it too (cmd/listener/role_keep.go): a walk must not outlive its own claim, or a second
// walk would take a copy over while this one is still forwarding it, and a claim must not
// outlive the walk that wrote it, or a lane would stay held longer than the work behind the
// claim.
//
// The age is one process's clock against the time the stream stored the cursor, which is the
// server's: this window assumes the two are within a minute of each other, which a deployment
// whose hosts run NTP holds to by orders of magnitude. A listener an hour out of step drops every
// copy as past its own keep window anyway, and a clock that runs ahead costs one live claim taken
// over early - the duplicate the design already accepts - so no reader spends a round trip on the
// stream's own time.
const KeptRoleClaimWindow = 60 * time.Second

// KeptRoleCursor is the body of a role's drain cursor (KeptRoleCursorSubject): the two numbers
// that say how far that role's kept lane has been drained, and what the claim one of them names
// is for. Kept is the stream sequence on the role's kept subject the lane is finished with -
// every copy up to it was forwarded, dropped as expired or passed over - and a walk starts at the
// copy after it. Claiming is a copy one walk has claimed and not yet confirmed delivered, 0 when
// none is outstanding: a walk writes it before it forwards that copy and clears it with the same
// write that moves Kept past the copy, so a walk that ends between the two leaves Kept short of
// the copy and the next walk reads that copy again. A claim is also the lane's lock while it is
// outstanding (ClaimOutstanding).
//
// The listener's drain is its only writer, which writes it with Client.PublishCursorAt as this
// small body rather than an envelope (cmd/listener/role_keep.go). It lives here because
// Dispatch's outbox reads it as well, to tell whether the copy it has just kept is the one that
// lane's drain would hand over next (internal/dispatch/outbox): a body each package spelled out
// for itself would decode a drifted tag as zero, which reads as a lane drained to its start and
// leads every handover astray. A writer of this body writes every field at once, since a reader
// of any takes the whole body as one answer.
type KeptRoleCursor struct {
	Kept     uint64 `json:"kept"`
	Claiming uint64 `json:"claiming"`
	// DeadLetter names why the claim is a walk's claim of a copy it is ending with a dead letter
	// to that copy's sender, rather than a forward's. It is one of four values, a reason with or
	// without one qualifier: "delivery_mode" for a copy the role's live holder does not take for
	// its delivery mode, and "expired" for a copy whose hour ended; "delivery_mode_unconfirmed"
	// and "expired_unconfirmed" for the same two drops of a copy that a walk had claimed to
	// forward and whose forward no walk confirmed, so the copy may have reached the session that
	// held the role then. The listener writes and reads them in one place each
	// (cmd/listener/role_keep.go: keptRoleDrop.claimReason, keptCopyClaimOf). A walk that takes
	// such a claim over once it is no longer outstanding sends that dead letter again under the
	// same message id, for the reason and with the qualifier named here, whatever the copy's age
	// and whoever holds the role, where it judges a forward's claimed copy as any copy and writes
	// the qualifier into the claim of a dead letter that copy turns out to be owed. Both are
	// written into the claim because nothing else the walk that takes it over reads can tell
	// them: the copy's age is measured on that walk's own clock, the holder that refused the
	// copy's mode may hold the role no longer, and the forward's claim that showed the copy may
	// have reached the holder is replaced by the dead letter's. The two values of one reason
	// send the dead letter whose own reason is that reason, "expired" or "delivery_mode"; only
	// the claim tells the two apart.
	//
	// An empty value is a forward's claim where Claiming names a copy, and no claim at all where
	// it is 0, and it is left out of the body then, so those bodies are written exactly as before
	// the field existed. Dispatch's outbox decodes this same struct, so it compiles the field, but
	// it reads only Kept and Claiming; an earlier listener's struct has no such field, and
	// encoding/json drops it when that listener decodes the body.
	DeadLetter string `json:"dead_letter,omitempty"`
}

// ClaimOutstanding reports whether the copy this cursor claims is still one walk's to forward: a
// claim is outstanding while it names a copy and is younger than KeptRoleClaimWindow. written is
// the time the stream stored the cursor, which is how old the claim it carries is.
//
// now is the reader's clock rather than a time already read, and it is called only while a claim
// is named at all, so a lane nothing claims costs no read of the clock: a caller that judges
// many copies against one cursor reads its clock once per copy it judges and once more for the
// claim it found.
func (c KeptRoleCursor) ClaimOutstanding(written time.Time, now func() time.Time) bool {
	return c.Claiming != 0 && now().Sub(written) < KeptRoleClaimWindow
}

// StoredKeptRoleCursor is a role's drain cursor as the stream holds it (Client.ReadKeptRoleCursor).
type StoredKeptRoleCursor struct {
	// Cursor is the cursor's body, its zero value for a lane that has never been drained.
	Cursor KeptRoleCursor
	// Sequence is the stream sequence of the cursor message itself, which the next
	// compare-and-swap write of the cursor must name (Client.PublishCursorAt); 0 for a lane
	// whose cursor has never been written.
	Sequence uint64
	// Written is the time the stream stored the cursor, which is how old the claim it carries is
	// (KeptRoleCursor.ClaimOutstanding); the zero time when there is no cursor.
	Written time.Time
}

// ReadKeptRoleCursor reads role's drain cursor: the one read of it both writers of that role's
// kept lane make - the listener's own pass over the lane (cmd/listener/role_keep.go) and the
// handover behind Dispatch's outbox's own keep (internal/dispatch/outbox) - so that the two
// decode one body one way. A lane no walk has written a cursor for is no error but the zero
// StoredKeptRoleCursor its fields describe: every copy on such a lane is past its cursor, so a
// reader must take the zero cursor as a lane to be read from its first copy rather than as a
// lane with nothing to read.
//
// Its errors are LastStoredMessage's, named with the role and wrapped, and a cursor body no read
// can decode is one of them.
func (c *Client) ReadKeptRoleCursor(ctx context.Context, role string) (StoredKeptRoleCursor, error) {
	stored, err := c.LastStoredMessage(ctx, KeptRoleCursorSubject(role))
	if errors.Is(err, nats.ErrMsgNotFound) {
		return StoredKeptRoleCursor{}, nil
	}
	if err != nil {
		return StoredKeptRoleCursor{}, fmt.Errorf("read role %s's kept-lane cursor: %w", role, err)
	}
	read := StoredKeptRoleCursor{Sequence: stored.Sequence, Written: stored.Time}
	if err := json.Unmarshal(stored.Data, &read.Cursor); err != nil {
		return StoredKeptRoleCursor{}, fmt.Errorf("read role %s's kept-lane cursor stored at %d: %w", role, stored.Sequence, err)
	}
	return read, nil
}

// KeptRoleLanes reports the role of every kept lane the notification stream still holds a copy on
// (KeptRoleSubject), sorted, so each is read once and two passes over an unchanged stream read the
// same list. It is the listener's backstop over the lanes nobody is draining: a role whose kept
// copies were stranded by a listener that ended mid-drain is on it, and a role whose lane the
// stream holds nothing on is not, whatever its drain cursor says.
//
// It is one STREAM.INFO of the stream with the kept role subjects as its filter, which answers
// every such subject and how many messages it holds; the listener already holds that grant for the
// stream it owns (infra/core/permissions.go), and nats.go pages the answer itself, so a sweep
// costs one round trip whatever the deployment holds. A stream that holds no kept copy at all
// answers an empty list: the state carries a subject only while a message is stored on it.
//
// Its errors are the stream's own: nats.ErrStreamNotFound when the stream is gone,
// nats.ErrNoResponders when no JetStream answers, and ctx's error when ctx ends first.
func (c *Client) KeptRoleLanes(ctx context.Context) ([]string, error) {
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	info, err := c.JS().StreamInfo(Stream,
		&nats.StreamInfoRequest{SubjectsFilter: KeptRoleSubject(">")},
		nats.Context(ctx))
	if err != nil {
		return nil, fmt.Errorf("bus: read %s's kept role lanes: %w", Stream, err)
	}
	roles := make([]string, 0, len(info.State.Subjects))
	for subject, held := range info.State.Subjects {
		role, ok := strings.CutPrefix(subject, KeptRoleSubjectPrefix)
		if !ok || role == "" || held == 0 {
			continue
		}
		roles = append(roles, role)
	}
	slices.Sort(roles)
	return roles, nil
}

// storedMessage makes one stored-message read of the notification stream and reads its answer, for
// NextStoredMessage and LastStoredMessage alike: request is the body each builds, naming the
// message it wants. The stream's error answer comes back as the *nats.APIError it is, and an
// answer that names neither a message nor an error is this package's own error.
func (c *Client) storedMessage(ctx context.Context, request any) (StoredMessage, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return StoredMessage{}, err
	}
	reply, err := c.requestConfirmed(ctx, storedMessageAPI, body)
	if err != nil {
		return StoredMessage{}, err
	}
	var response struct {
		Message *struct {
			Subject  string    `json:"subject"`
			Sequence uint64    `json:"seq"`
			Header   []byte    `json:"hdrs"`
			Data     []byte    `json:"data"`
			Time     time.Time `json:"time"`
		} `json:"message"`
		Error *nats.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &response); err != nil {
		return StoredMessage{}, fmt.Errorf("bus: read the stored-message answer from %s: %w", Stream, err)
	}
	if response.Error != nil {
		return StoredMessage{}, response.Error
	}
	if response.Message == nil {
		return StoredMessage{}, fmt.Errorf("bus: the stored-message answer from %s holds no message", Stream)
	}
	stored := StoredMessage{
		Sequence: response.Message.Sequence,
		Subject:  response.Message.Subject,
		Time:     response.Message.Time,
		Data:     response.Message.Data,
	}
	// A header that does not decode leaves the message id empty, as a message published under
	// none has it. The id of a takeover's copy names the key of the frame it was taken from, and
	// the only reader is cmd/listener's walk, which names the message by that key in an exception
	// about the copy (takenFrameOriginal); no reader judges a copy's owner, wake or dead letter by it.
	if len(response.Message.Header) > 0 {
		if header, err := nats.DecodeHeadersMsg(response.Message.Header); err == nil {
			stored.MsgID = header.Get(nats.MsgIdHdr)
		}
	}
	return stored, nil
}

// requestConfirmed sends data to subject as a request and returns its one reply. The publish is
// confirmed as a forward's publish is (publishConfirmed), so a request the server refuses under the
// connection's publish permissions returns ErrPublishDenied at once; a plain request would wait
// out its whole deadline, since core NATS answers a denied publish to no one but the async error
// callback. One publishConfirmed cannot confirm for another reason still waits for its reply, and
// returns that reason if ctx ends first. A no-responders status reply is nats.ErrNoResponders.
func (c *Client) requestConfirmed(ctx context.Context, subject string, data []byte) (*nats.Msg, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, storedMessageWait)
		defer cancel()
	}
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	conn := c.Connection()
	inbox := conn.NewInbox()
	replies, err := conn.SubscribeSync(inbox)
	if err != nil {
		return nil, err
	}
	defer replies.Unsubscribe()
	err = c.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Reply: inbox, Data: data}, conn.PublishMsg)
	if err != nil && !errors.Is(err, ErrPublishUnconfirmed) {
		return nil, err
	}
	reply, waitErr := replies.NextMsgWithContext(ctx)
	if waitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, cmp.Or(err, ctxErr)
		}
		return nil, waitErr
	}
	if len(reply.Data) == 0 && reply.Header.Get("Status") == "503" {
		return nil, nats.ErrNoResponders
	}
	return reply, nil
}

// RequestCore sends a core NATS request on subject carrying header and no body, and returns its
// one reply, which it takes on the reply subject reply names. It is the request the listener's
// connection probe makes (cmd/listener): one on a session's relay subject (DeliverSubject)
// carrying the Envoy-Probe header, on a reply subject that names that session
// (SessionReplySubject), whose answer says whether a plugin of that session still subscribes the
// subject. A session's own grant (natsgrant.Session) publishes SessionReplies in place of every
// _INBOX. subject, so among the sessions on such grants only that session may answer it; a
// holder of the shared session user or of a host user reads every relay subject and may publish
// to every _INBOX. subject, so it still can. Its publish is confirmed as
// requestConfirmed's is (publishConfirmed), so a request the server refuses under the
// connection's publish permissions returns ErrPublishDenied at once; one publishConfirmed cannot
// confirm for another reason still waits for its reply, and returns that reason if ctx ends
// first. A no-responders status reply is nats.ErrNoResponders, as requestConfirmed maps it, and a
// ctx with no deadline waits storedMessageWait.
//
// The reply is taken over a channel subscription of this request's own, not conn.SubscribeSync
// as requestConfirmed takes it: that call sizes its channel at nats.Options.SubChanLen, 65,536
// slots by default, so each request would allocate and zero half a megabyte to hold the one reply
// it waits for, and the send route probes once per send to a session with an entry. The channel
// holds nats.RequestChanLen replies, as nats.go's own request does: a request has one reply, and
// the slack is for a subject that draws a second (two processes of one session subscribed to its
// relay subject), which a one-slot channel would drop as a slow consumer and record on the whole
// connection, failing another goroutine's publish as unconfirmed.
//
// The one error this shape cannot give is the connection's own: nats.go closes a sync
// subscription's channel when the connection closes and leaves a channel subscription's open, so
// a request whose connection is closed or drained under it waits out ctx and returns ctx's error,
// where requestConfirmed returns nats.ErrConnectionClosed at once. The probe counts both as
// connected, and the case is a shutdown race.
func (c *Client) RequestCore(ctx context.Context, subject, reply string, header nats.Header) (*nats.Msg, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, storedMessageWait)
		defer cancel()
	}
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	conn := c.Connection()
	replies := make(chan *nats.Msg, nats.RequestChanLen)
	subscription, err := conn.ChanSubscribe(reply, replies)
	if err != nil {
		return nil, err
	}
	defer subscription.Unsubscribe()
	err = c.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Reply: reply, Header: header}, conn.PublishMsg)
	if err != nil && !errors.Is(err, ErrPublishUnconfirmed) {
		return nil, err
	}
	select {
	case answer := <-replies:
		if len(answer.Data) == 0 && answer.Header.Get("Status") == "503" {
			return nil, nats.ErrNoResponders
		}
		return answer, nil
	case <-ctx.Done():
		return nil, cmp.Or(err, ctx.Err())
	}
}

// CheckFilterSubject refuses a subject a stored-message read cannot filter on: one NATS does not
// accept in a subject (checkSubject), or one with a `>` anywhere but as its last token.
func CheckFilterSubject(subject string) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	tokens := strings.Split(subject, ".")
	for _, token := range tokens[:len(tokens)-1] {
		if token == ">" {
			return fmt.Errorf("%w: %q has a `>` before its last token", ErrInvalidSubject, subject)
		}
	}
	return nil
}

// SubjectContains reports whether every subject that subject matches is also matched by pattern,
// under NATS wildcard rules: `*` matches one token and `>`, as the last token, one or more. Either
// may hold wildcards; a wildcard in subject is contained only by a wildcard in pattern at least as
// wide (`a.>` contains `a.*.b` and `a.>`, `a.*` contains `a.*` but not `a.>`).
func SubjectContains(pattern, subject string) bool {
	return tokensContain(strings.Split(pattern, "."), strings.Split(subject, "."))
}

// tokensContain is SubjectContains on two subjects already split into their tokens, for a caller
// that weighs each subject against many others and splits it once.
func tokensContain(patternTokens, subjectTokens []string) bool {
	for index, patternToken := range patternTokens {
		if patternToken == ">" {
			return index < len(subjectTokens)
		}
		if index >= len(subjectTokens) {
			return false
		}
		subjectToken := subjectTokens[index]
		if subjectToken == ">" {
			return false
		}
		if patternToken != "*" && patternToken != subjectToken {
			return false
		}
	}
	return len(patternTokens) == len(subjectTokens)
}

// ReduceFilterSubjects returns subjects as a set of consumer filters no two of which overlap, which
// JetStream requires of one consumer's filters, matching every subject any of them matched: a
// subject another contains (SubjectContains) is dropped, and two that overlap without either
// containing the other are replaced by their longest common prefix of whole tokens followed by `>`
// (`>` alone when the first tokens differ), until no two overlap. `X` and `X.>` do not overlap (one
// matches X alone, the other only what lies under X), so both stay. The result is sorted and holds
// each subject once, so the same set always reduces to the same list.
//
// A registration reduces its topics twice (sessionConsumerTarget), inside the request's own time
// budget, and a session can register hundreds of them. So each subject is split into its tokens
// once, and every pair is weighed on those tokens; a drop does not restart the scan of the pairs
// (reduceFilterPairs). SessionConsumerReduction makes the same reduction, through the same body
// (reduceFilters), and says which subjects fed each filter.
func ReduceFilterSubjects(subjects []string) []string {
	// An empty list reduces to itself, given back nil or empty as it came.
	if len(subjects) == 0 {
		return slices.Clone(subjects)
	}
	reduced, _ := reduceFilters(subjects, nil)
	filters := make([]string, len(reduced))
	for index, filter := range reduced {
		filters[index] = filter.subject
	}
	return filters
}

// reduceFilters is the reduction ReduceFilterSubjects makes of subjects, which are not empty, with
// the provenance of each filter when fedBy is given: fedBy[i] is what subjects[i] stands for, and
// a filter's fed holds the fedBy of each subject it stands for, its own where it is that subject
// as it came, and every one each merge that made it took in, through each merge before it. A
// subject another contains is dropped with its provenance, since nothing of it went into the
// filter that contains it, and two equal subjects are kept once with the provenance of both
// (compactFilters). With a nil fedBy every fed stays nil, and the reduction allocates nothing for
// it. It reports beside the filters how many pairs of subjects it weighed: the reduction's work,
// counted alike on every machine, which ReduceFilterSubjects and SessionConsumerReduction discard
// and TestReducingTheLargestRegistrationInProductionWeighsEachPairOnceBetweenMerges holds to the
// pairs its subjects make.
func reduceFilters(subjects []string, fedBy []int) ([]filterSubject, int) {
	reduced := make([]filterSubject, 0, len(subjects))
	for index, subject := range subjects {
		filter := filterSubject{subject: subject, tokens: strings.Split(subject, ".")}
		if fedBy != nil {
			filter.fed = []int{fedBy[index]}
		}
		reduced = append(reduced, filter)
	}
	weighed := 0
	for merged := true; merged; {
		slices.SortFunc(reduced, func(a, b filterSubject) int { return strings.Compare(a.subject, b.subject) })
		reduced = compactFilters(reduced)
		var pairs int
		reduced, merged, pairs = reduceFilterPairs(reduced)
		weighed += pairs
	}
	return reduced, weighed
}

// filterSubject is one subject ReduceFilterSubjects weighs, with its tokens and, where its caller
// asks for it, its provenance (reduceFilters).
type filterSubject struct {
	subject string
	tokens  []string
	fed     []int
}

// compactFilters keeps each subject of subjects, which are sorted, once, with the provenance of
// every copy of it on the one kept. It changes subjects in place.
func compactFilters(subjects []filterSubject) []filterSubject {
	kept := 0
	for _, subject := range subjects {
		if kept > 0 && subjects[kept-1].subject == subject.subject {
			if subject.fed != nil {
				subjects[kept-1].fed = slices.Concat(subjects[kept-1].fed, subject.fed)
			}
			continue
		}
		subjects[kept] = subject
		kept++
	}
	clear(subjects[kept:])
	return subjects[:kept]
}

// reduceFilterPairs weighs the pairs of subjects, which are sorted and each held once, in order:
// it drops each subject another contains, and at the first pair that overlaps without either
// containing the other it merges that pair, with the provenance of both, and reports true, or
// reports false when it reaches the last pair with no merge. It changes subjects in place, and
// reports beside them how many pairs it weighed.
//
// A pair that does not overlap is weighed by that test alone, which settles most pairs: a subject
// that contains another overlaps it, so such a pair is neither a drop nor a merge.
//
// A drop does not restart the scan. Every pair before the one that dropped a subject was weighed
// and holds no overlap, and a drop changes no other subject and keeps the rest in their order, so
// a scan from the first pair would weigh those pairs again to the same end. The scan goes on at
// the dropped subject's place instead: with the same first subject when the second went, and with
// the subject that follows when the first went. A merge does restart it, from a sorted list: the
// merged subject is new, so every subject must be weighed against it, and it takes its own place
// in the order.
func reduceFilterPairs(subjects []filterSubject) ([]filterSubject, bool, int) {
	weighed := 0
	for i := 0; i < len(subjects); i++ {
		for j := i + 1; j < len(subjects); {
			a, b := subjects[i], subjects[j]
			weighed++
			switch {
			case !tokensOverlap(a.tokens, b.tokens):
				j++
			case tokensContain(a.tokens, b.tokens):
				subjects = slices.Delete(subjects, j, j+1)
			case tokensContain(b.tokens, a.tokens):
				subjects = slices.Delete(subjects, i, i+1)
				j = i + 1
			default:
				tokens := append(commonTokenPrefix(a.tokens, b.tokens), ">")
				merged := filterSubject{subject: strings.Join(tokens, "."), tokens: tokens}
				if a.fed != nil || b.fed != nil {
					merged.fed = slices.Concat(a.fed, b.fed)
				}
				subjects = slices.Delete(subjects, j, j+1)
				subjects = slices.Delete(subjects, i, i+1)
				return append(subjects, merged), true, weighed
			}
		}
	}
	return subjects, false, weighed
}

// commonTokenPrefix is the tokens a and b begin with alike, up to the first that differs or is `>`,
// none when their first tokens differ. It is a's own leading tokens with no room past them, so an
// append to it copies them rather than writing over a.
func commonTokenPrefix(a, b []string) []string {
	index := 0
	for index < len(a) && index < len(b) && a[index] == b[index] && a[index] != ">" {
		index++
	}
	return a[:index:index]
}
