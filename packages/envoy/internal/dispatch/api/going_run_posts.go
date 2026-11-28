package api

// going_run_posts.go withdraws a command-bar run (going_runs) once a comment or an issue message
// whose words it was given is deleted (dispatch://INFRA-1523, R3), as going.go's goingWithdrawal
// withdraws the short answer of a pause in typing. Every answer a call of the run gets is read for
// the posts whose words it carries before the answer reaches the run's agent, and each is recorded
// as the run's in going_run_posts (recordGoingRunPosts); a follow-up's claim records as its own the
// posts of each earlier run whose notes it is handed (claimGoingRun). A run is withdrawn once a
// post recorded for it no longer stands (goingRunWithdrawnOf), which every read of the run reads
// at the moment it reads, and which the delete never writes: the delete then knows nothing of
// runs, and one read covers what the run stored before the delete and what it stores after.
//
// Every read of a withdrawn run gives none of what the run stored that may quote the post: its
// stream writes one end frame in the place of the run's ending and no frame of the run
// (goingRunWithdrawal), its record reads stopped with goingRunWithdrawnReason (scanGoingRun), its
// plan gives no summary, no arguments and no value now (goingPlanStored.withdrawnAnswer) and is
// approved and revised by nobody, and a follow-up's claim hands on none of its notes. A run that
// has not ended is stopped with that reason through stopGoingRun, by whichever comes first of its
// stream, its record's read, its plan's routes, its next call and its next claim, so it stores
// nothing more. What the run stored stays as it is: what Dispatch wrote keeps its words, and every
// read masks them.
//
// A run withdrawn by its own deletes alone - each post recorded for it that no longer stands was
// deleted by a delete call of that run, which landed (goingRunWithdrawnByRun) - is withdrawn with
// goingRunWithdrawnByRunReason, and its person still reads what the run did, which is no word of
// the post: its stream writes its plan frames, which carry plan numbers and states, before the
// end frame, and its plan reads as an ended run's, its rows and their results, each deleted
// post's value as the delete's mark (maskDeletedPosts), and no summary, which the run's model
// wrote and may quote the post (goingPlanStored.withdrawnByRunAnswer). Everything else is as for
// any withdrawn run: no other frame, its record stopped, its notes handed on to no follow-up.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
)

// goingRunWithdrawnReason is the stop reason of a withdrawn run: the one its record reads with,
// the one its end frame carries, and the one a stop of it stores. Its code is the short answer's
// own (contracts.GoingErrorAnswerWithdrawn), so the bar says both in the same words.
const goingRunWithdrawnReason = contracts.GoingErrorAnswerWithdrawn +
	": a comment or an issue message this run read was deleted, so nothing the run wrote is shown"

// goingRunWithdrawnByRunReason is goingRunWithdrawnReason for a run withdrawn by its own deletes
// alone (goingRunWithdrawnByRun).
const goingRunWithdrawnByRunReason = contracts.GoingErrorAnswerWithdrawnByRun +
	": Dispatch withdrew this answer because this run deleted a comment or message it read."

// goingRunWithdrawer is who withdrew a run, as goingRunWithdrawerOf reads it: nobody, the run
// itself by its own deletes alone, or another writer.
type goingRunWithdrawer string

const (
	goingRunNotWithdrawn     goingRunWithdrawer = ""
	goingRunWithdrawnByRun   goingRunWithdrawer = "run"
	goingRunWithdrawnByOther goingRunWithdrawer = "other"
)

// reason is the stop reason of a run w withdrew.
func (w goingRunWithdrawer) reason() string {
	if w == goingRunWithdrawnByRun {
		return goingRunWithdrawnByRunReason
	}
	return goingRunWithdrawnReason
}

// frame is the end frame the stream of a run w withdrew writes.
func (w goingRunWithdrawer) frame() []byte {
	if w == goingRunWithdrawnByRun {
		return goingRunWithdrawnByRunFrame
	}
	return goingRunWithdrawnFrame
}

// codeGoingRunWithdrawn refuses an approval or a revision of the plan of a withdrawn run.
const codeGoingRunWithdrawn = "GOING_RUN_WITHDRAWN"

// writeGoingRunWithdrawn refuses an approval or a revision of a withdrawn run's plan, 409
// GOING_RUN_WITHDRAWN, with nothing written: the plan's changes may quote the deleted post, and a
// revision would ask the run's model for more of what it read.
func writeGoingRunWithdrawn(w http.ResponseWriter) {
	writeError(w, codeGoingRunWithdrawn, http.StatusConflict,
		"This run read a comment or an issue message that was deleted. Its plan cannot be approved or revised. Ask again in a new run.")
}

// goingRunWithdrawnOf is the condition that holds of the run whose id is run, a column or a
// parameter of the statement it is put in, while it is withdrawn, by whoever withdrew it
// (goingRunWithdrawerOf names who): a post recorded for it that no row answers with deleted_at
// unset, which is a deleted post and a post whose row is gone alike, as the short answer's verdict
// reads it (goingRunStateOf). going_run_posts_post leads with the run, so it is one probe of that
// index for each post the run read.
func goingRunWithdrawnOf(run string) string {
	return `exists (
		select 1
		from going_run_posts p
		left join comments c on c.id = p.comment_id and c.deleted_at is null
		left join messages m on m.id = p.message_id and m.deleted_at is null
		where p.run_id = ` + run + ` and c.id is null and m.id is null
	)`
}

// goingRunWithdrawerOf is the expression that reads who withdrew the run whose id is run, a
// column or a parameter of the statement it is put in (goingRunWithdrawer): the empty string
// while every post recorded for it stands; 'run' where each that no longer stands was deleted by
// the run's own delete call, a landing of it that sent DELETE to that post's own path; 'other'
// where any was deleted by another writer, or its row is gone. It reads the posts
// goingRunWithdrawnOf reads, and for each that no longer stands the run's own landings, which
// going_landings' key leads with.
func goingRunWithdrawerOf(run string) string {
	return `(
		select case when count(*) = 0 then '` + string(goingRunNotWithdrawn) + `'
		  when bool_and(exists (
		    select 1 from going_landings l
		    where l.run_id = p.run_id and l.phase = 'do' and l.method = 'DELETE'
		      and lower(l.path) = coalesce('/api/v1/comments/' || p.comment_id::text, '/api/v1/messages/' || p.message_id::text)
		  )) then '` + string(goingRunWithdrawnByRun) + `'
		  else '` + string(goingRunWithdrawnByOther) + `' end
		from going_run_posts p
		left join comments c on c.id = p.comment_id and c.deleted_at is null
		left join messages m on m.id = p.message_id and m.deleted_at is null
		where p.run_id = ` + run + ` and c.id is null and m.id is null
	)`
}

// goingRunWithdrawnFrame is the data of the one frame the stream of a withdrawn run writes: the
// run stopped with goingRunWithdrawnReason, offering nothing, whatever end the run stored.
var goingRunWithdrawnFrame = goingRunEndFrame(goingRunWithdrawnReason)

// goingRunWithdrawnByRunFrame is goingRunWithdrawnFrame for a run withdrawn by its own deletes
// alone, which its stream writes after the run's plan frames.
var goingRunWithdrawnByRunFrame = goingRunEndFrame(goingRunWithdrawnByRunReason)

// goingRunEndFrame is the data of an end frame of a run stopped with reason, offering nothing.
func goingRunEndFrame(reason string) []byte {
	frame, err := json.Marshal(agentwire.EndFrame{State: agentwire.StateStopped, Reason: reason, Offers: []string{}})
	if err != nil {
		panic(err)
	}
	return frame
}

// goingRunWithdrawal is the sequence of the ending of run, withdrawn by withdrawer, in state as a
// read found it, which withdrawer's frame takes on its stream: a run that had not ended is stopped
// with withdrawer's reason first, through the stop its person's Stop takes (stopGoingRun), so it
// stores nothing more, and its ending is that stop's end frame; a run another writer ended
// meanwhile keeps its own. The ending is the run's last end frame, after which an ended run stores
// nothing, and 0 for a run that stored none.
func (s *server) goingRunWithdrawal(ctx context.Context, run, state string, withdrawer goingRunWithdrawer) (int64, error) {
	if !goingRunEnded(state) {
		if _, err := s.stopGoingRun(ctx, run, withdrawer.reason()); err != nil {
			return 0, err
		}
	}
	var ending int64
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(max(sequence), 0) from going_run_events where run_id = $1::uuid and name = $2
	`, run, agentwire.FrameEnd).Scan(&ending)
	return ending, err
}

// recordGoingRunPosts records, as run's, each comment and issue message whose words answer
// carries: the answer a call of the run got from the route at path, which the run's agent is about
// to be given. A post is named by its id anywhere in the answer - a row's id, a hit's, a reference
// to it or to a block of it, a link - or in path, which names the post a read of one post, its
// history or its blocks is of; and a comment is named too by each document the answer names, or
// path names by its owner and slug, one of whose versions its accepted suggestion named after its
// body (acceptedVersionName, artifact_versions.accepted_from), since a version's name prints those
// words where no id of the comment is. Its words are carried
// where a string of the answer holds goingGivenRun of them in a row as the post holds them, or
// holds nothing but words the post holds in a row (goingPostsGiven). So a post is read the same
// whichever field gives its words - a body, a search snippet, a reply's quote of it, an excerpt, a
// first line, a block's text, an event's payload, a version's name - and an id an answer names it
// by with none of its words - a reply's replies_to, a ref in a list - records nothing. A post's
// words are matched against the body it holds now and every body an edit or a delete replaced
// (post_body_history), which is what a read of its history gives, and what a read made just before
// the delete gave.
//
// The answer is read as JSON where it is JSON, its strings decoded, and a part the server cut, or
// an answer of another type, is read as the text it is. Only issue messages are recorded: a
// message on no issue is never edited or deleted (NOT_AN_ISSUE_MESSAGE).
func (s *server) recordGoingRunPosts(ctx context.Context, run, path string, answer []byte) error {
	given := goingAnswerText(answer)
	for _, id := range goingIDPattern.FindAllString(path, -1) {
		given.ids[strings.ToLower(id)] = true
	}
	owner, slug := "", ""
	if document := goingDocumentPath.FindStringSubmatch(path); document != nil {
		owner, slug = document[1], document[2]
	}
	if (len(given.ids) == 0 && slug == "") || len(given.texts) == 0 {
		return nil
	}
	ids := make([]string, 0, len(given.ids))
	for id := range given.ids {
		ids = append(ids, id)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		with documents as (
			select unnest($1::uuid[]) as id
			union
			select a.id from artifacts a
			where $3 <> '' and a.slug = $3 and (a.issue_key = $2 or (a.issue_key is null and a.project_key = $2))
		), named as (
			select unnest($1::uuid[]) as id
			union
			select v.accepted_from from artifact_versions v
			where v.artifact_id in (select id from documents) and v.accepted_from is not null
		)
		select 'comment', c.id::text, c.body, array(select h.body from post_body_history h where h.comment_id = c.id)
		from named join comments c on c.id = named.id
		union all
		select 'message', m.id::text, m.body, array(select h.body from post_body_history h where h.message_id = m.id)
		from messages m where m.id = any($1::uuid[]) and m.issue_key is not null
	`, ids, owner, slug)
	if err != nil {
		return err
	}
	var posts []goingPostText
	for rows.Next() {
		var post goingPostText
		var body string
		if err := rows.Scan(&post.kind, &post.id, &body, &post.bodies); err != nil {
			rows.Close()
			return err
		}
		post.bodies = append(post.bodies, body)
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var comments, messages []string
	for index, carried := range goingPostsGiven(given.texts, posts) {
		if !carried {
			continue
		}
		if posts[index].kind == "comment" {
			comments = append(comments, posts[index].id)
		} else {
			messages = append(messages, posts[index].id)
		}
	}
	if len(comments) == 0 && len(messages) == 0 {
		return nil
	}
	_, err = s.deps.Store.Pool.Exec(ctx, `
		insert into going_run_posts (run_id, comment_id, message_id)
		select $1::uuid, post, null::uuid from unnest($2::uuid[]) as post
		union all
		select $1::uuid, null::uuid, post from unnest($3::uuid[]) as post
		on conflict do nothing
	`, run, comments, messages)
	return err
}

// goingIDPattern is a uuid in any case, which is how every comment and message id is written.
var goingIDPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// goingDocumentPath is a path under one document of an issue or a project, named by its owner's
// key and its slug, which a read of the document, its text or one of its versions is sent to.
var goingDocumentPath = regexp.MustCompile(`^/api/v1/(?:issues|projects)/([^/]+)/artifacts/([^/]+)`)

// goingGivenRun is how many words of a post in a row a string must hold to carry the post's words,
// or fewer where the post holds fewer: few enough that a search snippet, a line's head and a short
// block of the post each hold them, and enough that a string of other words seldom does.
const goingGivenRun = 4

// goingNotText is each member whose string names, sorts or dates something and is never a post's
// words (and so is each member whose name ends in _id, _at or _key): a post whose words are such a
// value - "done", a login - is not given by a field that names it.
var goingNotText = map[string]bool{
	"id": true, "kind": true, "type": true, "state": true, "status": true, "turn": true, "role": true, "login": true,
	"ref": true, "href": true, "node": true, "key": true, "project": true, "slug": true,
}

// goingAnswerStrings is what an answer holds that a post's words and its id may be in: each string
// whose member may hold words (goingNotText), and every uuid any string or member name holds.
type goingAnswerStrings struct {
	texts []string
	ids   map[string]bool
}

// goingAnswerText reads answer as goingAnswerStrings: as JSON, each string decoded, and what
// follows the last whole token where the answer is no JSON or the server cut it, as one string.
func goingAnswerText(answer []byte) goingAnswerStrings {
	read := goingAnswerStrings{ids: map[string]bool{}}
	add := func(member, text string) {
		for _, id := range goingIDPattern.FindAllString(text, -1) {
			read.ids[strings.ToLower(id)] = true
		}
		if !goingNotText[member] && !strings.HasSuffix(member, "_id") && !strings.HasSuffix(member, "_at") &&
			!strings.HasSuffix(member, "_key") {
			read.texts = append(read.texts, text)
		}
	}
	// Each open object or list, with the member it is the value of (its own member's, once an
	// object names one) and whether an object waits for a member's name.
	type open struct {
		object, name bool
		member       string
	}
	var stack []open
	valued := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].name = true
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(answer))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				add("", string(answer[decoder.InputOffset():]))
			}
			return read
		}
		n := len(stack)
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				member := ""
				if n > 0 {
					member = stack[n-1].member
				}
				stack = append(stack, open{object: delimiter == '{', name: delimiter == '{', member: member})
			default:
				stack = stack[:n-1]
				valued()
			}
			continue
		}
		if n > 0 && stack[n-1].object && stack[n-1].name {
			name, _ := token.(string)
			for _, id := range goingIDPattern.FindAllString(name, -1) {
				read.ids[strings.ToLower(id)] = true
			}
			stack[n-1].member, stack[n-1].name = name, false
			continue
		}
		if text, ok := token.(string); ok {
			member := ""
			if n > 0 {
				member = stack[n-1].member
			}
			add(member, text)
		}
		valued()
	}
}

// goingPostText is a post an answer names: its kind (comment or message), its id, and every body
// it has held, the one it holds now among them.
type goingPostText struct {
	kind, id string
	bodies   []string
}

// goingGram is goingGivenRun words at most, the ones a gram of fewer leaves empty.
type goingGram [goingGivenRun]string

// goingGramOf is a post one of whose bodies holds a gram, and how many words in a row of that body
// carry it (goingGivenRun, or all of them where it holds fewer).
type goingGramOf struct {
	post, run int
}

// goingPostsGiven reports, for each of posts, whether texts carry its words: a text holds run
// words in a row as one of the post's bodies holds them, run being goingGivenRun or the body's
// every word where it holds fewer; or a text's every word, fewer than run, is words the body holds
// in a row, as a line's head cut short is. A body with no word is carried by a text that holds it
// whole. Snippet marks (search.go's markSnippet) are read as the words they mark.
func goingPostsGiven(texts []string, posts []goingPostText) []bool {
	given := make([]bool, len(posts))
	index := map[goingGram][]goingGramOf{}
	type wordless struct {
		post int
		body string
	}
	var bare []wordless
	for post, of := range posts {
		for _, body := range of.bodies {
			words := goingWords(body)
			if len(words) == 0 {
				if trimmed := strings.TrimSpace(body); trimmed != "" {
					bare = append(bare, wordless{post: post, body: trimmed})
				}
				continue
			}
			run := min(goingGivenRun, len(words))
			for size := 1; size <= run; size++ {
				for start := 0; start+size <= len(words); start++ {
					var gram goingGram
					copy(gram[:], words[start:start+size])
					index[gram] = append(index[gram], goingGramOf{post: post, run: run})
				}
			}
		}
	}
	for _, text := range texts {
		text = goingSnippetMarks.Replace(text)
		for _, of := range bare {
			if strings.Contains(text, of.body) {
				given[of.post] = true
			}
		}
		words := goingWords(text)
		for start := range words {
			for size := 1; size <= goingGivenRun && start+size <= len(words); size++ {
				var gram goingGram
				copy(gram[:], words[start:start+size])
				for _, of := range index[gram] {
					if size == of.run || size == len(words) {
						given[of.post] = true
					}
				}
			}
		}
	}
	return given
}

// goingSnippetMarks takes out the marks a search snippet wraps its matched words in.
var goingSnippetMarks = strings.NewReplacer("<mark>", "", "</mark>", "")

// goingWords is text's words, lowercased: each run of letters and digits, which reads a post's
// markdown and the plain text a read makes of it alike.
func goingWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
