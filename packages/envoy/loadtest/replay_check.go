package main

// The replay seed's checks: replay-check-seed compares a seeded database with its bundle, and
// replay-checksum fingerprints one. replay-logical-dump, which writes one with every uuid and
// timestamp masked so that two seeds (a stock and a patched Dispatch, say) compare with cmp, is in
// replay_dump.go. Each reads Postgres only and prints keys, names, numbers and digests, never a
// text.

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// dbFlags are a check's flags: the stack (for its Postgres URL) and the database.
type dbFlags struct {
	ef endpointFlags
	db *string
}

func (f *dbFlags) register(fs *flag.FlagSet) {
	f.ef.register(fs)
	f.db = fs.String("db", "", "database name, overriding the Postgres URL's")
}

// url is the stack's endpoints and its Postgres URL with the database replaced.
func (f *dbFlags) url() (endpoints, string, error) {
	if *f.db == "" {
		return endpoints{}, "", fmt.Errorf("-db is required")
	}
	ep, err := f.ef.resolve()
	if err != nil {
		return endpoints{}, "", err
	}
	u, err := withDatabase(ep.pg, *f.db)
	return ep, u, err
}

func (f *dbFlags) connect(ctx context.Context) (*pgx.Conn, error) {
	_, u, err := f.url()
	if err != nil {
		return nil, err
	}
	return pgx.Connect(ctx, u)
}

// --- replay-check-seed ---------------------------------------------------------------------------

func runReplayCheckSeed(args []string) error {
	fs := flag.NewFlagSet("replay-check-seed", flag.ContinueOnError)
	var df dbFlags
	df.register(fs)
	bundleDir := fs.String("bundle", "", "replay bundle directory")
	keysFile := fs.String("keys-file", "", "the keys file the seed was given")
	out := fs.String("out", "", "write the results as JSON to this file")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *bundleDir == "" {
		return fmt.Errorf("-bundle is required")
	}
	b, err := readBundle(*bundleDir)
	if err != nil {
		return err
	}
	var keys map[string]bool
	if *keysFile != "" {
		if keys, err = readKeysFile(*keysFile); err != nil {
			return err
		}
	}
	plans, err := planIssues(b.State, keys)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := df.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	ids, err := readReplayIDs(ctx, conn)
	if err != nil {
		return err
	}
	r, err := checkSeed(ctx, conn, b, plans, ids)
	if err != nil {
		return err
	}
	sa, err := checkSizesAndAnchors(ctx, conn, b, plans, ids)
	if err != nil {
		return err
	}
	for _, d := range r.Differences {
		fmt.Printf("difference %s %s %s: expected %s, found %s\n", d.Issue, d.Artifact, d.What, d.Expected, d.Found)
	}
	for _, m := range sa.SizeMismatches {
		fmt.Printf("version size mismatch %s %s version %d: expected change %d, found %d\n", m.Issue, m.Artifact, m.Version, m.Expected, m.Found)
	}
	for _, a := range sa.Outside {
		fmt.Printf("anchor outside its chosen block %s %s %s: %s\n", a.Issue, a.Kind, a.LoggedID, a.Why)
	}
	fmt.Printf("documents compared %d; differences %d; fallbacks %d (comments %d, asks %d)\n",
		r.Documents, len(r.Differences), r.FallbackComments+r.FallbackAsks, r.FallbackComments, r.FallbackAsks)
	fmt.Printf("version sizes: %d changes compared, %d mismatches, %d sign flipped, %d without a listed size\n",
		sa.SizeChanges, len(sa.SizeMismatches), sa.SignFlipped, sa.SizesMissing)
	fmt.Printf("anchors: %d recorded, %d outside the block their rule chose; by rule %s\n", sa.Anchors, len(sa.Outside), fmtCounts(sa.Rules))
	fmt.Printf("documents with answered or resolved typed ask blocks %d, compared by text and by their asks' answers: %d equal\n", r.AskBlockDocs, r.AskBlockDocsEqual)
	for _, d := range r.Distribution {
		fmt.Printf("open anchored roots on written primary documents (%s): documents %d, median %.1f, write-weighted mean %.2f\n",
			d.When, d.Documents, d.Median, d.WeightedMean)
	}
	res := checkResult{
		Documents: r.Documents, Differences: len(r.Differences), VersionSizeMismatches: len(sa.SizeMismatches),
		AnchorsOutsideLoggedBlock: len(sa.Outside), SignFlipped: sa.SignFlipped,
		AnchorRules:    checkAnchorRules{sa.Rules[ruleLoggedQuote], sa.Rules[ruleInBlock], sa.Rules[ruleNearest]},
		AllAnchorRules: sa.Rules, DifferenceList: r.Differences, SizeMismatchList: sa.SizeMismatches, OutsideList: sa.Outside,
	}
	res.Passed = res.Differences == 0 && res.VersionSizeMismatches == 0 && res.AnchorsOutsideLoggedBlock == 0
	fmt.Printf("passed %v\n", res.Passed)
	if *out != "" {
		data, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	if !res.Passed {
		return fmt.Errorf("check failed: %d differences, %d version size mismatches, %d anchors outside their block", res.Differences, res.VersionSizeMismatches, res.AnchorsOutsideLoggedBlock)
	}
	return nil
}

// checkResult is replay-check-seed -out's file. Another team's tool reads these field names.
type checkResult struct {
	Documents                 int              `json:"documents"`
	Differences               int              `json:"differences"`
	VersionSizeMismatches     int              `json:"version_size_mismatches"`
	AnchorsOutsideLoggedBlock int              `json:"anchors_outside_logged_block"`
	AnchorRules               checkAnchorRules `json:"anchor_rules"`
	SignFlipped               int              `json:"sign_flipped"`
	Passed                    bool             `json:"passed"`
	// Detail: every branch's count (no_block_map, unanchorable included) and the lists, ids and
	// numbers only.
	AllAnchorRules   map[string]int   `json:"anchor_rules_all"`
	DifferenceList   []seedDifference `json:"difference_list"`
	SizeMismatchList []sizeMismatch   `json:"version_size_mismatch_list"`
	OutsideList      []anchorOutside  `json:"anchors_outside_list"`
}

type checkAnchorRules struct {
	LoggedQuote           int `json:"logged_quote"`
	InLoggedBlock         int `json:"in_logged_block"`
	NearestSurvivingBlock int `json:"nearest_surviving_block"`
}

type sizeMismatch struct {
	Issue    string `json:"issue"`
	Artifact string `json:"artifact"`
	Version  int    `json:"version"`
	Expected int    `json:"expected"`
	Found    int    `json:"found"`
}

type anchorOutside struct {
	Issue    string `json:"issue"`
	Kind     string `json:"kind"`
	LoggedID string `json:"logged_id"`
	Why      string `json:"why"`
}

type sizeAnchorCheck struct {
	SizeChanges, SignFlipped, SizesMissing int
	SizeMismatches                         []sizeMismatch
	Anchors                                int
	Rules                                  map[string]int
	Outside                                []anchorOutside
}

// checkSizesAndAnchors compares each seeded document's version size changes (Dispatch's
// coalesce(size, octet_length(markdown))) with production's (VersionSizes): equal, or equal with
// the sign flipped (counted, not a mismatch), else a mismatch; and checks every anchored item the
// seed should have placed: its branch is recorded in public.loadtest_replay_anchors and its
// stored quote lies, once, inside the span of the text at At its rule chose. An answer version's
// expected change counts its answers' answered_at as the seed's answer wrote it: Dispatch writes
// the time of the answer with its trailing zeros trimmed, so its length is not production's (the
// text comparison leaves out every server-owned attribute of an ask block, this one among them:
// servedText). A resolve version needs no such count: a resolve writes only state="resolved" into
// its block, no time.
func checkSizesAndAnchors(ctx context.Context, conn *pgx.Conn, b *replayBundle, plans []seedIssuePlan, ids map[[2]string]string) (sizeAnchorCheck, error) {
	r := sizeAnchorCheck{Rules: map[string]int{}}
	recorded, err := readReplayAnchors(ctx, conn)
	if err != nil {
		return r, err
	}
	for _, p := range plans {
		if p.placeholder() {
			continue
		}
		is := p.Issue
		textByLocal := map[string]string{} // a document's text by its local id ("" for an unmapped one)
		for i := range is.Artifacts {
			a := &is.Artifacts[i]
			if a.Kind != "doc" {
				continue
			}
			raw, err := b.readFile(a.TextPath)
			if err != nil {
				return r, err
			}
			text := string(raw)
			local, ok := ids[[2]string{idArtifact, a.ID}]
			textByLocal[local] = text
			if !ok {
				continue
			}
			vrows, err := conn.Query(ctx, `select coalesce(size, octet_length(markdown)) from public.artifact_versions where artifact_id = $1 order by number`, local)
			if err != nil {
				return r, err
			}
			sizes, err := pgx.CollectRows(vrows, pgx.RowTo[int])
			if err != nil {
				return r, err
			}
			plan, err := docPlan(is, a, text)
			if err != nil {
				return r, err
			}
			answeredAt := map[int]int{} // version -> seeded less production answered_at bytes
			for v, steps := range plan.AskAt {
				for _, st := range steps {
					ask := &is.Asks[st.Ask]
					localAsk, ok := ids[[2]string{idAsk, ask.ID}]
					if st.Resolve || !ok || ask.Answer == nil {
						continue
					}
					var seeded string
					if err := conn.QueryRow(ctx, `select coalesce(answer->>'at', '') from public.asks where id = $1`, localAsk).Scan(&seeded); err != nil {
						return r, err
					}
					if seeded != "" {
						answeredAt[v] += len(seeded) - len(ask.Answer.At.UTC().Format(time.RFC3339Nano))
					}
				}
			}
			for n := 2; n <= min(len(sizes), len(a.VersionSizes)); n++ {
				if a.VersionSizes[n-1] < 0 || a.VersionSizes[n-2] < 0 {
					r.SizesMissing++
					continue
				}
				r.SizeChanges++
				want, got := a.VersionSizes[n-1]-a.VersionSizes[n-2]+answeredAt[n], sizes[n-1]-sizes[n-2]
				switch {
				case got == want:
				case got == -want:
					r.SignFlipped++
				default:
					r.SizeMismatches = append(r.SizeMismatches, sizeMismatch{p.Key, a.Name, n, want, got})
				}
			}
		}
		check := func(kind, logged string) {
			row, ok := recorded[[2]string{kind, logged}]
			if !ok {
				r.Outside = append(r.Outside, anchorOutside{p.Key, kind, logged, "no recorded branch"})
				return
			}
			r.Anchors++
			r.Rules[row.branch]++
			local := ids[[2]string{kind, logged}]
			table := "public.comments"
			if kind == idAsk {
				table = "public.asks"
			}
			var quote, doc string
			if err := conn.QueryRow(ctx, `select coalesce(anchor->>'quote', ''), coalesce(anchor->>'artifact_id', '') from `+table+` where id::text = $1`, local).Scan(&quote, &doc); err != nil {
				r.Outside = append(r.Outside, anchorOutside{p.Key, kind, logged, "seeded item not found"})
				return
			}
			text := textByLocal[doc]
			i := strings.Index(text, quote)
			switch {
			case quote == "" || text == "":
				r.Outside = append(r.Outside, anchorOutside{p.Key, kind, logged, "no stored quote or document"})
			case row.quoteAt >= 0 && row.quoteAt+len(quote) <= len(text) && text[row.quoteAt:row.quoteAt+len(quote)] == quote &&
				row.quoteAt >= row.start && row.quoteAt+len(quote) <= row.end:
				// A repeated logged quote pinned to its occurrence inside the span.
			case strings.Count(text, quote) != 1 || i < row.start || i+len(quote) > row.end || row.end > len(text):
				r.Outside = append(r.Outside, anchorOutside{p.Key, kind, logged, fmt.Sprintf("stored quote at byte %d, chosen span [%d, %d)", i, row.start, row.end)})
			}
		}
		plan := planComments(is)
		steps := append([]commentStep{}, plan.Main...)
		for _, t := range plan.ByAsk {
			steps = append(steps, t...)
		}
		for _, s := range steps {
			if s.Kind == commentAnchored {
				check(idComment, s.C.ID)
			}
		}
		for _, a := range is.Asks {
			if a.BlockID == nil && a.Approval == nil && a.Anchor != nil {
				check(idAsk, a.ID)
			}
		}
	}
	return r, nil
}

type seedDifference struct {
	Issue, Artifact, What, Expected, Found string
}

type openRootsDistribution struct {
	When         string
	Documents    int
	Median       float64
	WeightedMean float64
}

type seedCheck struct {
	Documents                       int
	Differences                     []seedDifference
	FallbackComments, FallbackAsks  int
	AskBlockDocs, AskBlockDocsEqual int
	Distribution                    []openRootsDistribution
}

// replayRowQuerier is what the replay's own two tables are read from: a pool or a connection (as
// openAnchored takes a QueryRow).
type replayRowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// readReplayIDs reads public.loadtest_replay_ids, so the seed and the check have one reader.
func readReplayIDs(ctx context.Context, q replayRowQuerier) (map[[2]string]string, error) {
	rows, err := q.Query(ctx, `select kind, logged_id, local_id from public.loadtest_replay_ids`)
	if err != nil {
		return nil, fmt.Errorf("read public.loadtest_replay_ids: %w", err)
	}
	out := map[[2]string]string{}
	var k, l, v string
	_, err = pgx.ForEachRow(rows, []any{&k, &l, &v}, func() error {
		out[[2]string{k, l}] = v
		return nil
	})
	return out, err
}

// anchorRow is one row of public.loadtest_replay_anchors as the check reads it: the branch
// chooseAnchor took and the span its rule chose, with the pinned occurrence's byte offset
// (quote_at, -1 when the quote has to be unique).
type anchorRow struct {
	branch              string
	start, end, quoteAt int
}

// readReplayAnchors reads public.loadtest_replay_anchors, which the seed records an anchor's rule
// in (replay_seed.go recordAnchor), by kind and logged id.
func readReplayAnchors(ctx context.Context, q replayRowQuerier) (map[[2]string]anchorRow, error) {
	rows, err := q.Query(ctx, `select kind, logged_id, branch, span_start, span_end, quote_at from public.loadtest_replay_anchors`)
	if err != nil {
		return nil, fmt.Errorf("read public.loadtest_replay_anchors: %w", err)
	}
	out := map[[2]string]anchorRow{}
	var kind, logged, branch string
	var start, end, at int
	_, err = pgx.ForEachRow(rows, []any{&kind, &logged, &branch, &start, &end, &at}, func() error {
		out[[2]string{kind, logged}] = anchorRow{branch, start, end, at}
		return nil
	})
	return out, err
}

// textDigest names a text by its length and sha256 prefix.
func textDigest(s string) string {
	return fmt.Sprintf("%d bytes sha256 %s", len(s), sha256Hex([]byte(s))[:12])
}

func checkSeed(ctx context.Context, conn *pgx.Conn, b *replayBundle, plans []seedIssuePlan, ids map[[2]string]string) (seedCheck, error) {
	var r seedCheck
	diff := func(issue, artifact, what, expected, found string) {
		r.Differences = append(r.Differences, seedDifference{issue, artifact, what, expected, found})
	}
	ww := countWindowWrites(b.Window)
	var atSeed, withWindow, weights []int
	for _, p := range plans {
		if p.placeholder() {
			continue
		}
		is := p.Issue
		anchored := expectedOpenAnchored(is)
		for _, c := range is.Comments {
			if _, ok := ids[[2]string{idFallbackComment, c.ID}]; ok {
				r.FallbackComments++
			}
		}
		for _, a := range is.Asks {
			if _, ok := ids[[2]string{idFallbackAsk, a.ID}]; ok {
				r.FallbackAsks++
			}
		}
		for i := range is.Artifacts {
			a := &is.Artifacts[i]
			if a.Kind != "doc" {
				continue
			}
			r.Documents++
			local, ok := ids[[2]string{idArtifact, a.ID}]
			if !ok {
				diff(p.Key, a.Name, "mapping", "a local document", "none")
				continue
			}
			var maxV int
			var latest string
			err := conn.QueryRow(ctx, `select coalesce(max(number), 0),
				coalesce((select markdown from public.artifact_versions where artifact_id = $1 order by number desc limit 1), '')
				from public.artifact_versions where artifact_id = $1`, local).Scan(&maxV, &latest)
			if err != nil {
				return r, err
			}
			if maxV != a.Version {
				diff(p.Key, a.Name, "version", fmt.Sprint(a.Version), fmt.Sprint(maxV))
			}
			open, err := openAnchored(ctx, conn, local)
			if err != nil {
				return r, err
			}
			if open != anchored[a.ID] {
				diff(p.Key, a.Name, "open-anchored", fmt.Sprint(anchored[a.ID]), fmt.Sprint(open))
			}
			raw, err := b.readFile(a.TextPath)
			if err != nil {
				return r, err
			}
			// The text is compared as the text route serves it, and the answer of each answered or
			// resolved typed ask block as its ask's node holds it: a version this build writes
			// keeps no answer's words or selection and no open ask's state, so no answer is read
			// off the text.
			same := true
			if want, got := servedText(string(raw)), servedText(latest); want != got {
				diff(p.Key, a.Name, "text", textDigest(want), textDigest(got))
				same = false
			}
			stepped := steppedBlockAsks(is, a)
			if len(stepped) == 0 {
				continue
			}
			r.AskBlockDocs++
			served, err := servedAnswers(ctx, conn, local)
			if err != nil {
				return r, err
			}
			for _, ask := range stepped {
				want := ask.answerDigest()
				got := "no ask"
				if answer, ok := served[*ask.BlockID]; ok {
					got = answer.digest()
				}
				if want != got {
					diff(p.Key, a.Name, "answer of block "+*ask.BlockID, want, got)
					same = false
				}
			}
			if same {
				r.AskBlockDocsEqual++
			}
		}
		if w := ww.Writes[p.Key]; w > 0 {
			pa := primaryArtifact(is)
			local, ok := ids[[2]string{idArtifact, pa.ID}]
			if !ok {
				continue
			}
			var n int
			if err := conn.QueryRow(ctx, `select count(*) from public.comments where anchor->>'artifact_id' = $1 and reply_to is null and not resolved`, local).Scan(&n); err != nil {
				return r, err
			}
			atSeed = append(atSeed, n)
			withWindow = append(withWindow, n+ww.AnchoredRoots[pa.ID])
			weights = append(weights, w)
		}
	}
	for _, d := range []struct {
		when   string
		values []int
	}{{"at the seed", atSeed}, {"with the window's anchored roots", withWindow}} {
		med, mean := distribution(d.values, weights)
		r.Distribution = append(r.Distribution, openRootsDistribution{d.when, len(d.values), med, mean})
	}
	return r, nil
}

// --- replay-checksum -----------------------------------------------------------------------------

func runReplayChecksum(args []string) error {
	fs := flag.NewFlagSet("replay-checksum", flag.ContinueOnError)
	var df dbFlags
	df.register(fs)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ctx := context.Background()
	conn, err := df.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	sum, err := replayChecksum(ctx, conn)
	if err != nil {
		return err
	}
	out, _ := json.Marshal(sum)
	fmt.Println(string(out))
	return nil
}

type replaySum struct {
	Issues           int64  `json:"issues"`
	Artifacts        int64  `json:"artifacts"`
	ArtifactVersions int64  `json:"artifact_versions"`
	Comments         int64  `json:"comments"`
	Asks             int64  `json:"asks"`
	Events           int64  `json:"events"`
	MD5              string `json:"md5"`
}

// md5Fields feeds each field length-prefixed, so field boundaries are part of the digest.
func md5Fields(h io.Writer, fields ...string) {
	var n [8]byte
	for _, f := range fields {
		binary.LittleEndian.PutUint64(n[:], uint64(len(f)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(f))
	}
}

func replayChecksum(ctx context.Context, conn *pgx.Conn) (replaySum, error) {
	var s replaySum
	if err := conn.QueryRow(ctx, `select (select count(*) from public.issues), (select count(*) from public.artifacts),
		(select count(*) from public.artifact_versions), (select count(*) from public.comments), (select count(*) from public.asks),
		(select count(*) from public.events)`).Scan(&s.Issues, &s.Artifacts, &s.ArtifactVersions, &s.Comments, &s.Asks, &s.Events); err != nil {
		return s, err
	}
	h := md5.New()
	var key, name, slug, md, sha string
	var number int
	rows, err := conn.Query(ctx, `select coalesce(a.issue_key, ''), a.name, a.slug, v.number, coalesce(v.markdown, ''), coalesce(v.sha256, '')
		from public.artifact_versions v join public.artifacts a on a.id = v.artifact_id
		order by 1, 2, 3, 4`)
	if err != nil {
		return s, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&key, &name, &slug, &number, &md, &sha}, func() error {
		// A typed ask block's answered_at is the time the seed answered it, so it is masked.
		md5Fields(h, "version", key, name, slug, fmt.Sprint(number), maskAnsweredAt(md), sha)
		return nil
	}); err != nil {
		return s, err
	}
	var body string
	rows, err = conn.Query(ctx, `select issue_key, body from public.comments order by issue_key, created_at, id`)
	if err != nil {
		return s, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&key, &body}, func() error {
		md5Fields(h, "comment", key, body)
		return nil
	}); err != nil {
		return s, err
	}
	s.MD5 = hex.EncodeToString(h.Sum(nil))
	return s, nil
}
