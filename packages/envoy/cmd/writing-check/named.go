package main

// dataset named builds cases from the document versions its --doc flags list and the asks its
// --ask flags list, read from the live Dispatch. A text the dataset's snapshot does not hold, such
// as a version written after its end time, becomes a case that `harness load` and `harness score`
// read as they read a build's. It reads the event log of each issue into the index build reads a
// snapshot into, so a case's fields and its record come from build's own code. Every file it
// writes holds Dispatch text, so each belongs under $TMPDIR and never in a repository. README.md,
// "dataset", "named", says what it does.

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
)

// dsIssueLogPage is the events one read of an issue's log asks for, the route's most.
const dsIssueLogPage = 200

// dsNamedDoc is one --doc value: versions of one document of an issue. The document goes by its
// slug or its file name, as Dispatch's issue routes take it.
type dsNamedDoc struct {
	issue, document string
	versions        []int
}

// dsNamedDocs is the --doc flag, which a command line may give more than once.
type dsNamedDocs []dsNamedDoc

func (d *dsNamedDocs) String() string {
	values := make([]string, len(*d))
	for i, doc := range *d {
		numbers := make([]string, len(doc.versions))
		for j, n := range doc.versions {
			numbers[j] = strconv.Itoa(n)
		}
		values[i] = doc.issue + "/" + doc.document + "@" + strings.Join(numbers, ",")
	}
	return strings.Join(values, " ")
}

// Set reads one value: <issue>/<document>@<version>[,<version>...].
func (d *dsNamedDocs) Set(v string) error {
	ref, numbers, hasVersions := strings.Cut(v, "@")
	issue, document, hasDocument := strings.Cut(ref, "/")
	if !hasVersions || !hasDocument || issue == "" || document == "" || strings.Contains(document, "/") {
		return fmt.Errorf("%q is not <issue>/<document>@<version>[,<version>...]", v)
	}
	doc := dsNamedDoc{issue: issue, document: document}
	for _, s := range strings.Split(numbers, ",") {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return fmt.Errorf("%q: version %q is not a number above 0", v, s)
		}
		doc.versions = append(doc.versions, n)
	}
	*d = append(*d, doc)
	return nil
}

// dsNamedAsk is one --ask value: an ask of an issue, by its id.
type dsNamedAsk struct {
	issue, id string
}

// dsNamedAsks is the --ask flag, which a command line may give more than once.
type dsNamedAsks []dsNamedAsk

func (a *dsNamedAsks) String() string {
	values := make([]string, len(*a))
	for i, ask := range *a {
		values[i] = ask.issue + "/" + ask.id
	}
	return strings.Join(values, " ")
}

// Set reads one value: <issue>/<ask id>.
func (a *dsNamedAsks) Set(v string) error {
	issue, id, found := strings.Cut(v, "/")
	if !found || issue == "" || id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("%q is not <issue>/<ask id>", v)
	}
	*a = append(*a, dsNamedAsk{issue: issue, id: id})
	return nil
}

// dsNamedPrefix opens the id of a case of `dataset named`, before <issue>/<slug>@<version> or
// <issue>/ask:<id>.
const dsNamedPrefix = dsGroupPlain + "/named:"

// dsNamedCounts is the counts.json `dataset named` writes: the owner, whom `harness load` reads
// from it, the versions and the asks the cases are (<issue>/<slug>@<version>, <issue>/ask:<id>),
// the sessions listed in sessions.tsv and left out of it, and the SHA-256 of the other files. It
// holds no text.
type dsNamedCounts struct {
	Owner            string            `json:"owner"`
	Named            []string          `json:"named"`
	Cases            int               `json:"cases"`
	SessionsListed   int               `json:"sessions_listed"`
	SessionsUnlisted int               `json:"sessions_not_a_store_id"`
	SHA256           map[string]string `json:"sha256"`
}

// dsNamedResult is what `dataset named` wrote.
type dsNamedResult struct {
	cases       []*dsCase
	casesSHA    string
	documentSHA string
	sessionsSHA string
}

func runDatasetNamed(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset named", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var docs dsNamedDocs
	var asks dsNamedAsks
	flags.Var(&docs, "doc", "versions of one document of an issue, <issue>/<document>@<version>[,<version>...], the document by its slug or its file name (repeatable; give --doc, --ask or both)")
	flags.Var(&asks, "ask", "an ask of an issue, <issue>/<ask id>: the ask as its writer opened it, its question and its options (repeatable; give --doc, --ask or both)")
	out := flags.String("out", "", "the directory for cases.jsonl.gz, documents.jsonl.gz, sessions.tsv and counts.json (required; keep it under $TMPDIR)")
	owner := flags.String("owner", "", "the person the texts are written to, by login: harness load gives each case a reader in his place (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	base, tokenFile := os.Getenv("DISPATCH_URL"), os.Getenv("DISPATCH_TOKEN_FILE")
	switch {
	case flags.NArg() > 0:
		fmt.Fprintf(stderr, "writing-check dataset named: unexpected argument %q\n", flags.Arg(0))
		return 2
	case len(docs) == 0 && len(asks) == 0 || *out == "" || strings.TrimSpace(*owner) == "":
		fmt.Fprintln(stderr, "writing-check dataset named: --doc or --ask, --out and --owner are required")
		return 2
	case base == "" || tokenFile == "":
		fmt.Fprintln(stderr, "writing-check dataset named: DISPATCH_URL and DISPATCH_TOKEN_FILE must be set")
		return 2
	}
	if err := harness.OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check dataset named: %v\n", err)
		return 2
	}
	started := time.Now()
	dispatch := newClient(base, tokenFile)
	result, err := buildNamed(ctx, dispatch, docs, asks, *owner, *out, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check dataset named: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "cases: %d, group %s, part %s\n", len(result.cases), dsGroupPlain, dsDevelopment)
	for _, c := range result.cases {
		fmt.Fprintf(stdout, "%s: event %d at %s by %s %s, %d bytes, %d documents in its record\n",
			c.ID, c.Agent.EventID, c.WriteTime, c.Agent.ActorKind, c.Agent.Actor, len(c.Agent.Text), len(c.Record.Documents))
	}
	fmt.Fprintf(stdout, "requests: %d, wall time %s\n", dispatch.requests, time.Since(started).Round(time.Second))
	fmt.Fprintf(stdout, "documents.jsonl.gz sha256: %s\n", result.documentSHA)
	fmt.Fprintf(stdout, "sessions.tsv sha256: %s\n", result.sessionsSHA)
	fmt.Fprintf(stdout, "cases.jsonl.gz sha256: %s\n", result.casesSHA)
	return 0
}

// buildNamed writes a build directory to out with one case for each version of docs and each ask
// of asks: a plain case of the development part whose text is the version's whole Markdown, or the
// ask's question and options as its writer opened it, and whose record is the one build gives
// that text. notes takes a line for each text whose author's writes the writing check does not
// read.
func buildNamed(ctx context.Context, dispatch *client, docs []dsNamedDoc, asks []dsNamedAsk, owner, out string, notes io.Writer) (dsNamedResult, error) {
	var res dsNamedResult
	src := newLiveSource(dispatch)
	ix := newDSIndex(owner, "")

	// The texts to build, and the events of their issues in id order, as build reads the log.
	type namedVersion struct {
		ref      string
		artifact string
		number   int
	}
	var named []namedVersion
	var issues []string
	counts := dsNamedCounts{Owner: ix.owner}
	for _, d := range docs {
		var doc struct {
			ID       string  `json:"id"`
			IssueKey *string `json:"issue_key"`
			Slug     string  `json:"slug"`
			Kind     string  `json:"kind"`
		}
		err := dispatch.get(ctx, "/api/v1/issues/"+url.PathEscape(d.issue)+"/artifacts/"+url.PathEscape(d.document), nil, &doc)
		switch {
		case err != nil:
			return res, fmt.Errorf("document %s of %s: %w", d.document, d.issue, err)
		case doc.ID == "" || doc.Slug == "" || deref(doc.IssueKey) == "":
			return res, fmt.Errorf("document %s of %s: Dispatch answered no id, slug or issue for it", d.document, d.issue)
		case doc.Kind != "doc":
			return res, fmt.Errorf("%s of %s is not a document: Dispatch gives its kind as %s", d.document, d.issue, doc.Kind)
		}
		issue := *doc.IssueKey
		if !slices.Contains(issues, issue) {
			issues = append(issues, issue)
		}
		for _, n := range d.versions {
			ref := issue + "/" + doc.Slug + "@" + strconv.Itoa(n)
			if !slices.Contains(counts.Named, ref) {
				counts.Named = append(counts.Named, ref)
				named = append(named, namedVersion{ref: ref, artifact: doc.ID, number: n})
			}
		}
	}
	type namedAsk struct {
		ref, id string
	}
	var namedAsks []namedAsk
	for _, a := range asks {
		var got struct {
			Ask struct {
				ID       string  `json:"id"`
				IssueKey *string `json:"issue_key"`
			} `json:"ask"`
		}
		err := dispatch.get(ctx, "/api/v1/asks/"+url.PathEscape(a.id), nil, &got)
		switch {
		case err != nil:
			return res, fmt.Errorf("ask %s of %s: %w", a.id, a.issue, err)
		case got.Ask.ID == "":
			return res, fmt.Errorf("ask %s of %s: Dispatch answered no id for it", a.id, a.issue)
		case !strings.EqualFold(deref(got.Ask.IssueKey), a.issue):
			return res, fmt.Errorf("ask %s is not an ask of %s: Dispatch gives its issue as %q", a.id, a.issue, deref(got.Ask.IssueKey))
		}
		issue := *got.Ask.IssueKey
		if !slices.Contains(issues, issue) {
			issues = append(issues, issue)
		}
		ref := issue + "/ask:" + got.Ask.ID
		if !slices.Contains(counts.Named, ref) {
			counts.Named = append(counts.Named, ref)
			namedAsks = append(namedAsks, namedAsk{ref: ref, id: got.Ask.ID})
		}
	}
	var events []dsLogEvent
	for _, issue := range issues {
		log, err := issueEvents(ctx, dispatch, issue)
		if err != nil {
			return res, err
		}
		events = append(events, log...)
	}
	slices.SortFunc(events, func(a, b dsLogEvent) int { return cmp.Compare(a.id, b.id) })
	for _, ev := range events {
		if err := ix.addEvent(ev.raw); err != nil {
			return res, err
		}
	}
	// An issue's log holds its own parent only, so each ancestor's parent is read: a case's tree
	// is its issue's root.
	for _, issue := range issues {
		key := ix.issue(issue).parent
		for range 64 {
			if key == "" {
				break
			}
			ancestor, known := ix.issues[key]
			if !known {
				parent, err := src.issueParent(ctx, key)
				if err != nil {
					return res, fmt.Errorf("issue %s, an ancestor of %s: %w", key, issue, err)
				}
				ancestor = ix.issue(key)
				ancestor.parent = parent
			}
			key = ancestor.parent
		}
	}

	// Each case's text, and each version in its record, read once into the index as loadVersions
	// reads a snapshot's. A version Dispatch does not serve stays out of it.
	read := func(k dsVerKey) error {
		if _, done := ix.versionsIn[k]; done {
			return nil
		}
		text, err := src.documentVersion(ctx, k.artifact, k.number)
		switch {
		case unreadable(err):
			ix.versionsIn[k] = false
			return nil
		case err != nil:
			return fmt.Errorf("version %d of document %s: %w", k.number, k.artifact, err)
		}
		ix.versionsIn[k], ix.versionSHA[k], ix.versionText[k] = true, sha256Hex([]byte(text)), text
		return nil
	}
	// add makes the case of ref from its text t, with each version of its record read, and a note
	// where the writing check does not read the writes of t's author.
	add := func(ref string, t *dsText) error {
		for _, k := range ix.recordVersions(t) {
			if err := read(k); err != nil {
				return err
			}
		}
		if !t.Agent {
			fmt.Fprintf(notes, "writing-check dataset named: %s was written by %s %s, whose writes the writing check does not read: the case has no writer session\n", ref, t.ActorKind, t.ActorID)
		}
		made := &dsCase{ID: dsNamedPrefix + ref, Group: dsGroupPlain, Part: dsDevelopment, agent: t}
		ix.fill(made)
		res.cases = append(res.cases, made)
		return nil
	}
	for _, n := range named {
		texts := ix.texts[versionKey(n.artifact, n.number)]
		if len(texts) == 0 {
			return res, fmt.Errorf("%s: no event of its issue wrote this version", n.ref)
		}
		t := texts[0]
		own := dsVerKey{n.artifact, n.number}
		if err := read(own); err != nil {
			return res, err
		}
		if !ix.versionsIn[own] {
			return res, fmt.Errorf("%s: Dispatch does not serve this version", n.ref)
		}
		// The text is the whole version, where build takes the blocks the version before it does
		// not hold (fillVersionText): --doc gives a version, not a part of one.
		t.Text = ix.versionText[own]
		if err := add(n.ref, t); err != nil {
			return res, err
		}
	}
	// An ask's text is its question and its options as its ask.opened wrote them: the first text
	// of its key in the index. Each later edit is a later text of that key.
	for _, n := range namedAsks {
		texts := ix.texts["ask:"+n.id]
		if len(texts) == 0 || texts[0].EventType != "ask.opened" {
			return res, fmt.Errorf("%s: no event of its issue opened this ask with words", n.ref)
		}
		if err := add(n.ref, texts[0]); err != nil {
			return res, err
		}
	}
	slices.SortFunc(res.cases, func(a, b *dsCase) int { return strings.Compare(a.ID, b.ID) })
	dsSplitGroups(res.cases)

	// The files, as build writes them.
	var lines []dsDocumentLine
	written := map[dsVerKey]bool{}
	for _, made := range res.cases {
		for _, d := range made.Record.Documents {
			if k := (dsVerKey{d.ArtifactID, d.Version}); !d.Missing && !written[k] {
				written[k] = true
				lines = append(lines, dsDocumentLine{ArtifactID: d.ArtifactID, Version: d.Version, SHA256: d.SHA256, Markdown: ix.versionText[k]})
			}
		}
	}
	slices.SortFunc(lines, func(a, b dsDocumentLine) int {
		return cmp.Or(strings.Compare(a.ArtifactID, b.ArtifactID), cmp.Compare(a.Version, b.Version))
	})
	if err := os.MkdirAll(out, 0o755); err != nil {
		return res, err
	}
	var err error
	if res.casesSHA, err = dsWriteGzipJSONL(filepath.Join(out, "cases.jsonl.gz"), res.cases); err != nil {
		return res, err
	}
	if res.documentSHA, err = dsWriteGzipJSONL(filepath.Join(out, "documents.jsonl.gz"), lines); err != nil {
		return res, err
	}
	if res.sessionsSHA, counts.SessionsListed, counts.SessionsUnlisted, err = writeSessionList(filepath.Join(out, "sessions.tsv"), res.cases); err != nil {
		return res, err
	}
	slices.Sort(counts.Named)
	counts.Cases = len(res.cases)
	counts.SHA256 = map[string]string{"cases.jsonl.gz": res.casesSHA, "documents.jsonl.gz": res.documentSHA, "sessions.tsv": res.sessionsSHA}
	return res, writeJSON(filepath.Join(out, "counts.json"), counts)
}

// dsLogEvent is one event of an issue's log, with its id in the whole log.
type dsLogEvent struct {
	id  int64
	raw json.RawMessage
}

// issueEvents reads an issue's whole event log: GET /api/v1/issues/{key}/events, whose rows are
// those of GET /api/v1/events/log that the issue owns, its documents' among them. The route pages
// by the events' sequence on the issue, so each read asks for the events after the last sequence
// read.
func issueEvents(ctx context.Context, dispatch *client, key string) ([]dsLogEvent, error) {
	var events []dsLogEvent
	after := int64(0)
	for {
		var page []json.RawMessage
		query := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(dsIssueLogPage)}}
		if err := dispatch.get(ctx, "/api/v1/issues/"+url.PathEscape(key)+"/events", query, &page); err != nil {
			return nil, fmt.Errorf("reading the events of %s after sequence %d: %w", key, after, err)
		}
		if len(page) == 0 {
			return events, nil
		}
		for _, raw := range page {
			var head struct {
				ID  int64 `json:"id"`
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				return nil, fmt.Errorf("an event of %s after sequence %d: %w", key, after, err)
			}
			if head.Seq <= after {
				return nil, fmt.Errorf("event %d of %s has sequence %d, not above the sequence %d the read asked after", head.ID, key, head.Seq, after)
			}
			after = head.Seq
			events = append(events, dsLogEvent{id: head.ID, raw: raw})
		}
	}
}
