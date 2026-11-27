package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// --- replay-logical-dump -------------------------------------------------------------------------

func runReplayLogicalDump(args []string) error {
	fs := flag.NewFlagSet("replay-logical-dump", flag.ContinueOnError)
	var df dbFlags
	df.register(fs)
	out := fs.String("out", "", "output file")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	ctx := context.Background()
	conn, err := df.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	n, err := logicalDump(ctx, conn, w)
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d lines to %s\n", n, *out)
	return nil
}

var (
	uuidPattern      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}(:?\d{2})?)?`)
)

// ordinals numbers ids in order of first appearance within one scope.
type ordinals map[string]int

func (o ordinals) of(id string) int {
	if n, ok := o[id]; ok {
		return n
	}
	o[id] = len(o) + 1
	return o[id]
}

// maskText replaces every uuid in s by its ordinal in o and every timestamp by "<ts>".
func maskText(s string, o ordinals) string {
	s = uuidPattern.ReplaceAllStringFunc(s, func(u string) string { return fmt.Sprintf("<id%d>", o.of(strings.ToLower(u))) })
	return timestampPattern.ReplaceAllString(s, "<ts>")
}

// maskJSON masks a JSON value: every string through maskText, every object's keys kept.
func maskJSON(raw []byte, o ordinals) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "<undecodable>"
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return maskText(x, o)
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		case map[string]any:
			for k := range x {
				x[k] = walk(x[k])
			}
			return x
		}
		return v
	}
	return walk(v)
}

// dumpAnchor is a stored anchor (model.Anchor at 0114), its ids as ordinals in the document's
// scope. 0114 keeps no mark position in any column: a mark lives in the document's Yjs state only.
type dumpAnchor struct {
	Doc      int    `json:"doc"`
	Quote    string `json:"quote"`
	Block    int    `json:"block,omitempty"`
	Mark     int    `json:"mark"`
	Version  int    `json:"version"`
	Orphaned bool   `json:"orphaned"`
}

// logicalDump writes one JSON line per issue, document version, comment and ask, in an order and
// with ids that two seeds of the same bundle share: documents by issue key and name, comments and
// asks by issue key and creation order, every uuid as an ordinal (per document for block and mark
// ids, per issue otherwise) and every timestamp masked.
func logicalDump(ctx context.Context, conn *pgx.Conn, w *bufio.Writer) (int, error) {
	lines := 0
	emit := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		lines++
		_, err = w.Write(append(b, '\n'))
		return err
	}
	type issueRow struct {
		Key, Title, Status      string
		Priority                *int
		Labels                  []string
		Parent, Route, Assignee *string
		Blocked, Claim, Creator []byte
		ClaimedAt               *string
	}
	rows, err := conn.Query(ctx, `select i.key, i.title, i.status, i.priority, i.labels, i.parent_key, i.route, i.assignee,
		coalesce((select json_agg(l.target_key order by l.target_key) from public.issue_links l where l.issue_key = i.key and l.kind = 'blocked_by'), '[]'),
		i.claimed_by, i.created_by, i.claimed_at::text
		from public.issues i order by i.project_key, i.number`)
	if err != nil {
		return lines, fmt.Errorf("issues: %w", err)
	}
	issues, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (issueRow, error) {
		var x issueRow
		return x, r.Scan(&x.Key, &x.Title, &x.Status, &x.Priority, &x.Labels, &x.Parent, &x.Route, &x.Assignee,
			&x.Blocked, &x.Claim, &x.Creator, &x.ClaimedAt)
	})
	if err != nil {
		return lines, fmt.Errorf("issues: %w", err)
	}
	scopes := map[string]ordinals{} // issue key or document id -> ordinals
	scope := func(k string) ordinals {
		if scopes[k] == nil {
			scopes[k] = ordinals{}
		}
		return scopes[k]
	}
	for _, x := range issues {
		o := scope(x.Key)
		if err := emit(map[string]any{"issue": x.Key, "title": x.Title, "status": x.Status, "priority": x.Priority,
			"labels": x.Labels, "parent": x.Parent, "route": x.Route, "assignee": x.Assignee, "blocked_by": maskJSON(x.Blocked, o),
			"claimed_by": maskJSON(x.Claim, o), "claimed": x.ClaimedAt != nil, "created_by": maskJSON(x.Creator, o)}); err != nil {
			return lines, err
		}
	}
	// Documents and their versions; a document's ordinal is its place in this order within its issue.
	docOrd := map[string]int{} // local artifact id -> ordinal within its issue
	rows, err = conn.Query(ctx, `select id::text, coalesce(issue_key, project_key), name, slug, kind, is_primary
		from public.artifacts order by 2, name, slug`)
	if err != nil {
		return lines, fmt.Errorf("artifacts: %w", err)
	}
	type art struct {
		id, key, name, slug, kind string
		primary                   bool
	}
	arts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (art, error) {
		var a art
		return a, r.Scan(&a.id, &a.key, &a.name, &a.slug, &a.kind, &a.primary)
	})
	if err != nil {
		return lines, fmt.Errorf("artifacts: %w", err)
	}
	perIssue := map[string]int{}
	for _, a := range arts {
		perIssue[a.key]++
		docOrd[a.id] = perIssue[a.key]
		if err := emit(map[string]any{"artifact": a.key, "ordinal": docOrd[a.id], "name": a.name, "slug": a.slug, "kind": a.kind, "primary": a.primary}); err != nil {
			return lines, err
		}
		vrows, err := conn.Query(ctx, `select number, coalesce(markdown, ''), coalesce(sha256, ''), coalesce(size, 0), coalesce(mime, ''),
			named, coalesce(summary, ''), authors
			from public.artifact_versions where artifact_id = $1 order by number`, a.id)
		if err != nil {
			return lines, fmt.Errorf("artifact_versions: %w", err)
		}
		var number, size int
		var md, sha, mimeType, summary string
		var named bool
		var authors []byte
		o := scope(a.id)
		if _, err := pgx.ForEachRow(vrows, []any{&number, &md, &sha, &size, &mimeType, &named, &summary, &authors}, func() error {
			return emit(map[string]any{"version": a.key, "doc": docOrd[a.id], "number": number, "named": named,
				"summary": maskText(summary, o), "size": size, "mime": mimeType, "sha256": sha,
				"authors": maskJSON(authors, o), "markdown": maskText(md, o)})
		}); err != nil {
			return lines, err
		}
	}
	// Asks before comments, so a comment names its ask's ordinal.
	askOrd := map[string]int{}
	type askRow struct {
		ID, Key, Kind, State, Question, Urgency string
		Options, Anchor, Answer, Approval, Res  []byte
		Multiple                                bool
		BlockID                                 *string
		BlockArtifact                           *string
	}
	rows, err = conn.Query(ctx, `select id::text, issue_key, kind, state, question, urgency, options, anchor, answer, approval,
		resolution, multiple, block_id, block_artifact_id::text
		from public.asks order by issue_key, created_at, coalesce(block_id, ''), question, id`)
	if err != nil {
		return lines, fmt.Errorf("asks: %w", err)
	}
	asks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (askRow, error) {
		var x askRow
		return x, r.Scan(&x.ID, &x.Key, &x.Kind, &x.State, &x.Question, &x.Urgency, &x.Options, &x.Anchor, &x.Answer,
			&x.Approval, &x.Res, &x.Multiple, &x.BlockID, &x.BlockArtifact)
	})
	if err != nil {
		return lines, fmt.Errorf("asks: %w", err)
	}
	perIssue = map[string]int{}
	for _, x := range asks {
		perIssue[x.Key]++
		askOrd[x.ID] = perIssue[x.Key]
		o := scope(x.Key)
		line := map[string]any{"ask": x.Key, "ordinal": askOrd[x.ID], "kind": x.Kind, "state": x.State,
			"question": x.Question, "urgency": x.Urgency, "multiple": x.Multiple, "options": maskJSON(x.Options, o),
			"answer": maskJSON(x.Answer, o), "approval": maskJSON(x.Approval, o), "resolution": maskJSON(x.Res, o),
			"anchor": dumpAnchorOf(x.Anchor, docOrd, scope)}
		if x.BlockArtifact != nil {
			line["block_doc"] = docOrd[*x.BlockArtifact]
			if x.BlockID != nil {
				line["block"] = scope(*x.BlockArtifact).of(strings.ToLower(*x.BlockID))
			}
		}
		if err := emit(line); err != nil {
			return lines, err
		}
	}
	type commentRow struct {
		ID, Key, Body                   string
		Author, Anchor, Sug, ResolvedBy []byte
		ReplyTo, AskID, ArtifactID      *string
		Resolved                        bool
		Turn                            *string
	}
	rows, err = conn.Query(ctx, `select id::text, issue_key, body, author, anchor, suggestion, resolved_by, reply_to::text, ask_id::text,
		artifact_id::text, resolved, turn
		from public.comments order by issue_key, created_at, id`)
	if err != nil {
		return lines, fmt.Errorf("comments: %w", err)
	}
	comments, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (commentRow, error) {
		var x commentRow
		return x, r.Scan(&x.ID, &x.Key, &x.Body, &x.Author, &x.Anchor, &x.Sug, &x.ResolvedBy, &x.ReplyTo, &x.AskID,
			&x.ArtifactID, &x.Resolved, &x.Turn)
	})
	if err != nil {
		return lines, fmt.Errorf("comments: %w", err)
	}
	commentOrd := map[string]int{}
	perIssue = map[string]int{}
	for _, x := range comments {
		perIssue[x.Key]++
		commentOrd[x.ID] = perIssue[x.Key]
		o := scope(x.Key)
		line := map[string]any{"comment": x.Key, "ordinal": commentOrd[x.ID], "body": x.Body, "resolved": x.Resolved,
			"resolved_by": maskJSON(x.ResolvedBy, o), "author": maskJSON(x.Author, o), "suggestion": maskJSON(x.Sug, o),
			"turn": x.Turn, "anchor": dumpAnchorOf(x.Anchor, docOrd, scope)}
		if x.ArtifactID != nil {
			line["doc"] = docOrd[*x.ArtifactID]
		}
		if x.ReplyTo != nil {
			line["reply_to"] = commentOrd[*x.ReplyTo]
		}
		if x.AskID != nil {
			line["ask"] = askOrd[*x.AskID]
		}
		if err := emit(line); err != nil {
			return lines, err
		}
	}
	return lines, nil
}

// dumpAnchorOf masks a stored anchor; nil when there is none.
func dumpAnchorOf(raw []byte, docOrd map[string]int, scope func(string) ordinals) *dumpAnchor {
	if len(raw) == 0 {
		return nil
	}
	var a struct {
		ArtifactID string  `json:"artifact_id"`
		BlockID    *string `json:"block_id"`
		MarkID     string  `json:"mark_id"`
		Version    int     `json:"version"`
		Quote      string  `json:"quote"`
		Orphaned   bool    `json:"orphaned"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return &dumpAnchor{Quote: "<undecodable>"}
	}
	o := scope(a.ArtifactID)
	d := &dumpAnchor{Doc: docOrd[a.ArtifactID], Quote: a.Quote, Mark: o.of("mark:" + a.MarkID), Version: a.Version, Orphaned: a.Orphaned}
	if a.BlockID != nil {
		d.Block = o.of(strings.ToLower(*a.BlockID))
	}
	return d
}
