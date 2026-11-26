package api

// post_deleted_reads_test.go is the rule the census rows of dispatch://INFRA-1523 (R2 and R3)
// hold each read to, apart from the reads themselves: a read made while the post stands gives the
// post's words, which is what lets the same read after the delete fail, and that read after the
// delete gives none of them and says, on every copy of the post its answer's shape carries, that
// the post is deleted.
//
// The row numbers in the subtest names of the post_deleted_*_reads tests are the rows of the
// issue's surface census, dispatch://INFRA-1523/artifact/surface-census-md.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// postCopies is what one read's answer carries of one post.
type postCopies struct {
	// kind and id are the post: "comment" or "message", and its id.
	kind, id string
	// by is the id of the actor that deletes it: a login, or a session's id.
	by string
	// posts is how many objects of the answer, at least, are the post itself: an object whose own
	// id is the post's, beside a field only a post or its graph node carries (body, author,
	// made_at). A parent's pointer to it ({kind, id, ref}) is none.
	posts int
	// quotes is how many replies' quotes of one of the post's blocks the answer carries, at
	// least: a reply_block whose node is a block of the post.
	quotes int
	// edited says the post's author edited it before the read: every copy of it carries the
	// time of that edit (edited_at).
	edited bool
	// absent says the delete takes the post out of the answer: once it is deleted, no object of
	// the answer names it by its id, as a related-item hit or a citing row the cleared body no
	// longer makes.
	absent bool
}

// postObjects is what one answer holds of a post: each copy of the post, each reply's quote of
// one of its blocks, and how many objects name it as their own id at all.
type postObjects struct {
	posts, quotes []map[string]any
	named         int
}

// eachAnswerObject decodes the answer read gave and calls visit for every object it holds,
// wherever it holds it (walkOpenAPI).
func eachAnswerObject(t *testing.T, read string, body []byte, visit func(path string, node map[string]any)) {
	t.Helper()
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("%s: decode the answer %s: %v", read, body, err)
	}
	walkOpenAPI(answer, "", visit)
}

// findPostObjects decodes body and collects what it holds of post.
func findPostObjects(t *testing.T, read string, body []byte, post postCopies) postObjects {
	t.Helper()
	var found postObjects
	eachAnswerObject(t, read, body, func(_ string, node map[string]any) {
		if node["id"] == post.id {
			found.named++
			_, body := node["body"]
			if body || node["author"] != nil || node["made_at"] != nil {
				found.posts = append(found.posts, node)
			}
		}
		if block, _ := node["node"].(string); strings.HasPrefix(block, post.kind+"/"+post.id+"#") {
			found.quotes = append(found.quotes, node)
		}
	})
	return found
}

// requireStandingPostRead fails t unless the answer of a read made while the post stands gives
// phrase, carries the copies of the post and the quotes of it post says, and marks none of them
// deleted; and, for a post its author edited, unless every copy carries the time of the edit.
// It is the control of each row: the read after the delete can only fail where this one holds.
func requireStandingPostRead(t *testing.T, read string, body []byte, phrase string, post postCopies) {
	t.Helper()
	if !bytes.Contains(body, []byte(phrase)) {
		t.Fatalf("%s before the delete gives no %q, so the read after it cannot fail: %s", read, phrase, body)
	}
	found := findPostObjects(t, read, body, post)
	if len(found.posts) < post.posts || len(found.quotes) < post.quotes {
		t.Fatalf("%s before the delete carries %d copies of %s %s and %d quotes of it, want at least %d and %d: %s",
			read, len(found.posts), post.kind, post.id, len(found.quotes), post.posts, post.quotes, body)
	}
	if post.absent && found.named == 0 {
		t.Fatalf("%s before the delete names no %s %s, so its absence after the delete proves nothing: %s", read, post.kind, post.id, body)
	}
	for _, held := range append(found.posts, found.quotes...) {
		if held["deleted_at"] != nil {
			t.Fatalf("%s before the delete marks %s %s deleted: %v", read, post.kind, post.id, held)
		}
	}
	if !post.edited {
		return
	}
	for _, held := range found.posts {
		if at, _ := held["edited_at"].(string); at == "" {
			t.Fatalf("%s gives the edited %s %s with no edited_at: %v", read, post.kind, post.id, held)
		}
	}
}

// requireDeletedPostRead fails t unless the answer of a read made after the post's delete gives
// no part of phrase and, where the answer still carries the post, says on every copy of it and on
// every reply's quote of one of its blocks that it is deleted, and by whom.
func requireDeletedPostRead(t *testing.T, read string, body []byte, phrase string, post postCopies) {
	t.Helper()
	if bytes.Contains(body, []byte(phrase)) {
		t.Fatalf("%s after the delete gives the deleted text %q: %s", read, phrase, body)
	}
	found := findPostObjects(t, read, body, post)
	if post.absent {
		if found.named != 0 || len(found.quotes) != 0 {
			t.Fatalf("%s after the delete still names %s %s: %s", read, post.kind, post.id, body)
		}
		return
	}
	if len(found.posts) < post.posts || len(found.quotes) < post.quotes {
		t.Fatalf("%s after the delete carries %d copies of %s %s and %d quotes of it, want at least %d and %d, each saying it is deleted: %s",
			read, len(found.posts), post.kind, post.id, len(found.quotes), post.posts, post.quotes, body)
	}
	for _, held := range append(found.posts, found.quotes...) {
		requireDeletedMark(t, read, held, post.by)
	}
}
