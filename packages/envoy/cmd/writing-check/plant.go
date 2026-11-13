package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// The kinds of planted sentence: a fault the definition calls vacuous, and its matched control,
// the same verb in a sentence that states the fact.
const (
	kindFault   = "fault"
	kindControl = "control"
	plantedTag  = "planted"
)

// The shapes of a planted fault: the three the owner named, and "other" for a vacuous verb that
// is none of them.
const (
	shapeNames = "names"
	shapeShows = "shows"
	shapePays  = "pays"
	shapeOther = "other"
)

// verbForms is a verb's present for a singular subject, its base (a plural subject, or after
// "will") and its past.
type verbForms struct{ base, third, past string }

// subject is a sentence's subject and whether it is plural.
type subject struct {
	text   string
	plural bool
}

// plantVerb is one verb planted sentences are made from. A fault is a fault subject, the verb and
// a fault rest: a clause with no fact the verb stands for. A control is a control subject, the
// verb and a control rest: a clause that states the fact (a value, a holder, a date, an amount or
// an observed event), or the verb in its literal sense with a fact. Every subject goes with every
// rest of its kind, in the present, the past and the future.
type plantVerb struct {
	shape                  string
	forms                  verbForms
	faultSubjects          []subject
	faultRests             []string
	controlSubjects        []subject
	controlRests           []string
	faultSubjectsAreShared bool
}

// tenses are the tenses a planted sentence is written in.
var tenses = []string{"present", "past", "future"}

// sharedFaultSubjects are the subjects of every fault but the pays faults: texts and changes,
// none of them a person, a payer or a thing that literally displays, carries, holds or pays (no
// plan, log, page, view, output, table, report, queue or patch), so no verb with them reads in
// a literal sense.
var sharedFaultSubjects = []subject{
	{"The new section", false}, {"The update", false}, {"The second draft", false},
	{"This change", false}, {"The rewrite", false}, {"The proposal", false}, {"This approach", false},
	{"The new wording", false}, {"These changes", true}, {"Both drafts", true}, {"The two proposals", true},
}

func one(texts ...string) []subject {
	out := make([]subject, len(texts))
	for i, t := range texts {
		out[i] = subject{text: t}
	}
	return out
}

// plantVerbs is every verb plant writes sentences with. The sentences are written for this
// command; none comes from Dispatch.
var plantVerbs = []plantVerb{
	{
		shape: shapeNames, forms: verbForms{"name", "names", "named"}, faultSubjectsAreShared: true,
		faultRests: []string{"the risk.", "the gap.", "the owner.", "the cause.",
			"the problem.", "the next step.", "the difference.", "the issue."},
		controlSubjects: one("The config file", "The deploy script", "The release note", "The test fixture", "The new manifest", "The settings file"),
		controlRests: []string{"`us-west1` as the region.", "`v2.4.1` as the release.", "`build-cache` as the bucket.",
			"port `9090` for the metrics server.", "`alice` as the reviewer.", "`2026-12-01` as the cutoff date."},
	},
	{
		// A section, a draft or a rewrite is a text that literally shows what it holds, so the
		// shows faults take subjects that display nothing: a change, a fix, an approach.
		shape: shapeShows, forms: verbForms{"show", "shows", "showed"},
		faultSubjects: []subject{{"This change", false}, {"The fix", false}, {"This approach", false},
			{"The proposal", false}, {"The second attempt", false}, {"The new rule", false},
			{"These changes", true}, {"The two proposals", true}},
		faultRests: []string{"the gap.", "the cause.", "the real problem.", "the risk.", "the problem.",
			"the issue.", "the next step.", "the trade-off."},
		controlSubjects: one("The status page", "The dashboard", "The build log", "The job page", "The command's output", "The queue view"),
		controlRests: []string{"12 failed jobs since Monday.", "the exit code `2`.", "the line `ready on port 8080`.",
			"a deploy time of 09:40 UTC.", "3 new files under `docs/`.", "a queue depth of 417."},
	},
	{
		shape: shapePays, forms: verbForms{"pay", "pays", "paid"},
		faultSubjects: []subject{{"This branch", false}, {"The new cache", false}, {"The retry loop", false},
			{"The second pass", false}, {"The migration", false}, {"Each rebuild", false}, {"Both passes", true}, {"The two scans", true}},
		faultRests: []string{"the merge clash a second time.", "the scan twice.", "the full build again.",
			"off.", "the work twice.", "the risk up front.", "the problem a second time.", "the wait up front."},
		controlSubjects: []subject{{"The team", false}, {"The customer", false}, {"The platform team", false},
			{"The company", false}, {"Our finance team", false}, {"Both teams", true}},
		controlRests: []string{"$120 a month for the build runners.", "$4,300 for the license.", "the vendor $2,500 for the audit.",
			"$45 for each extra seat.", "an invoice of $860 to the host.", "$15 an hour for the test machines."},
	},
	// The controls of the other verbs state a fact of software work: a file, a command, a test, a
	// build, a config value, a count, a person or a date, so no reader takes the subject for a
	// thing of another domain.
	otherVerb("frame", "frames", "framed", []string{"the risk.", "the problem."},
		one("The RPC protocol", "The `wire` encoder"), []string{"each message with a 4-byte length header.", "every packet with a 2-byte checksum."}),
	otherVerb("capture", "captures", "captured", []string{"the intent.", "the lesson."},
		one("The script", "The test runner"), []string{"the exit code `3` in `run.log`.", "the full output of `make test` in `out.txt`."}),
	otherVerb("surface", "surfaces", "surfaced", []string{"the tension.", "the risk."},
		one("The `deploy` command", "The CLI"), []string{"the error `quota exceeded` on stderr.", "the warning `disk 91% full` in its first line of output."}),
	otherVerb("anchor", "anchors", "anchored", []string{"the approach.", "the discussion."},
		one("The pattern `^v[0-9]+`", "The regex in `tags.go`"), []string{"each match to the start of the tag name.", "the match at offset 0 of each line."}),
	otherVerb("carry", "carries", "carried", []string{"the idea forward.", "the work forward."},
		one("The commit", "The merge commit"), []string{"the line `Dispatch: KEY-1`.", "the trailer `Signed-off-by: kim`."}),
	otherVerb("unblock", "unblocks", "unblocked", []string{"the work.", "progress."},
		one("The admin", "The moderator"), []string{"the user `jdoe` at 10:15.", "the account `ops-bot` on Monday."}),
	otherVerb("bridge", "bridges", "bridged", []string{"the gap.", "the difference."},
		one("The `socat` command", "The proxy in `bridge.yml`"), []string{"port `5432` to `/tmp/pg.sock`.", "port `8080` to the `api` container."}),
	otherVerb("drive", "drives", "drove", []string{"the outcome.", "the alignment."},
		one("The load script", "The `k6` test"), []string{"500 requests a second at the `/search` route.", "the browser through 12 login steps."}),
	otherVerb("own", "owns", "owned", []string{"the outcome.", "the follow-through."},
		one("Kim", "The infra team"), []string{"the 12 GPUs in the `gpu-a` pool.", "the domain `example.org`."}),
	otherVerb("close", "closes", "closed", []string{"the loop.", "the gap."},
		one("Kim", "The release manager"), []string{"issue `ABC-12` at 10:15 on Monday.", "pull request #482 on Friday."}),
	otherVerb("track", "tracks", "tracked", []string{"the intent.", "the spirit of the work."},
		one("The exporter", "The `metrics` job"), []string{"the queue depth every 30 seconds.", "each request by its trace id `PX-114`."}),
	otherVerb("signal", "signals", "signaled", []string{"the direction.", "a shift."},
		one("The supervisor", "The `stop.sh` script"), []string{"`SIGTERM` to the worker after 30 seconds.", "the `nginx` process with `SIGHUP` at 02:00."}),
	otherVerb("reflect", "reflects", "reflected", []string{"the thinking.", "the new reality."},
		one("The `version` field", "The status badge"), []string{"the tag `v2.4.1`.", "the last run's exit code `2`."}),
	otherVerb("move", "moves", "moved", []string{"the needle.", "the work forward."},
		one("The migration", "The sync script"), []string{"4,000 rows to the `archive` table.", "`out.log` to `archive/` every night."}),
	otherVerb("hold", "holds", "held", []string{"the line.", "the balance."},
		one("The list", "The array"), []string{"three users: `alice`, `bob` and `carol`.", "two values: `7` and `9`."}),
	otherVerb("shape", "shapes", "shaped", []string{"the outcome.", "the work."},
		one("The rate limiter", "The `tc` rule in `net.sh`"), []string{"traffic to 40 requests a second.", "egress on `eth0` to 12 Mbit/s."}),
}

func otherVerb(base, third, past string, faultRests []string, controlSubjects []subject, controlRests []string) plantVerb {
	return plantVerb{
		shape: shapeOther, forms: verbForms{base, third, past}, faultSubjectsAreShared: true,
		faultRests: faultRests, controlSubjects: controlSubjects, controlRests: controlRests,
	}
}

// sentence is a subject, the verb in a tense, and a rest.
func (v plantVerb) sentence(s subject, tense, rest string) string {
	form := v.forms.third
	switch {
	case tense == "past":
		form = v.forms.past
	case tense == "future":
		form = "will " + v.forms.base
	case s.plural:
		form = v.forms.base
	}
	return s.text + " " + form + " " + rest
}

func (v plantVerb) subjects(kind string) []subject {
	switch {
	case kind == kindControl:
		return v.controlSubjects
	case v.faultSubjectsAreShared:
		return sharedFaultSubjects
	}
	return v.faultSubjects
}

func (v plantVerb) rests(kind string) []string {
	if kind == kindControl {
		return v.controlRests
	}
	return v.faultRests
}

// sentenceMaker draws planted sentences by seed and never answers one sentence twice.
type sentenceMaker struct {
	rng  *rand.Rand
	used map[string]bool
}

// make draws a sentence of the kind with the verb, one no earlier call answered.
func (m *sentenceMaker) make(v plantVerb, kind string) (string, error) {
	subjects, rests := v.subjects(kind), v.rests(kind)
	for range 1000 {
		s := v.sentence(subjects[m.rng.IntN(len(subjects))], tenses[m.rng.IntN(len(tenses))], rests[m.rng.IntN(len(rests))])
		if !m.used[s] {
			m.used[s] = true
			return s, nil
		}
	}
	return "", fmt.Errorf("no unused %s sentence with %q is left", kind, v.forms.base)
}

// plantedCase is one line of plant's manifest.jsonl: the copy, which write it copies, where the
// sentence went, and what it is.
type plantedCase struct {
	Copy          string `json:"copy"`
	Pair          string `json:"pair"`
	Tag           string `json:"tag"`
	Kind          string `json:"kind"`
	Shape         string `json:"shape"`
	Verb          string `json:"verb"`
	Sentence      string `json:"sentence"`
	SentenceProse string `json:"sentence_prose"`
	EventID       int64  `json:"event_id"`
	SourceID      string `json:"source_id"`
	Family        string `json:"family"`
	Project       string `json:"project"`
	Field         string `json:"field"`
	Surface       string `json:"surface"`
	Position      int    `json:"position"`
	Boundary      int    `json:"boundary"`
	Boundaries    int    `json:"boundaries"`
	// NextToRelayed marks a sentence planted at the field's first start because every start
	// was next to relayed or quoted words (relayedStarts).
	NextToRelayed bool  `json:"next_to_relayed,omitempty"`
	Seed          int64 `json:"seed"`
}

func runPlant(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check plant", flag.ContinueOnError)
	flags.SetOutput(stderr)
	in := flags.String("in", "", "a manifest naming the writes to copy: a split set manifest or a rule-cost manifest (required)")
	corpus := flags.String("corpus", "", "the cases file that holds those writes' text (required)")
	seed := flags.Int64("seed", 0, "the seed of the draw (required)")
	perShape := flags.Int("per-shape", 40, "faults of each named shape (names, shows, pays)")
	nolist := flags.Int("nolist", 40, "faults whose verb is none of name, show and pay")
	controls := flags.Bool("controls", false, "plant one matched control for each fault")
	out := flags.String("out", "", "the directory for planted.jsonl and manifest.jsonl (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *in == "" || *corpus == "" || *out == "" || !flagGiven(flags, "seed") || flags.NArg() > 0 || *perShape < 0 || *nolist < 0 || *perShape+*nolist == 0 {
		fmt.Fprintln(stderr, "writing-check plant: --in, --corpus, --seed and --out are required, at least one fault, and nothing else")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check plant: %v\n", err)
		return 1
	}
	keys, err := readWriteKeys(*in)
	if err != nil {
		return fail(err)
	}
	refs, err := scanWrites(*corpus)
	if err != nil {
		return fail(err)
	}
	cases, manifest, err := plant(refs, keys, *seed, *perShape, *nolist, *controls, func(keep map[writeKey]bool) (map[writeKey][]Case, error) {
		return loadWrites(*corpus, keep)
	})
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	plantedSum, err := writeJSONL(filepath.Join(*out, "planted.jsonl"), cases)
	if err != nil {
		return fail(err)
	}
	manifestSum, err := writeJSONL(filepath.Join(*out, "manifest.jsonl"), manifest)
	if err != nil {
		return fail(err)
	}
	sums := manifestSum + "  manifest.jsonl\n" + plantedSum + "  planted.jsonl\n"
	if err := os.WriteFile(filepath.Join(*out, "sha256"), []byte(sums), 0o644); err != nil {
		return fail(err)
	}
	counts := map[string]int{}
	families := map[string]int{}
	for _, m := range manifest {
		counts[m.Shape+" "+m.Kind]++
		families[m.Family]++
	}
	fmt.Fprintf(stdout, "planted cases: %d\n", len(manifest))
	for _, k := range sortedKeys(counts) {
		fmt.Fprintf(stdout, "  %s: %d\n", k, counts[k])
	}
	fmt.Fprintln(stdout, "by family:")
	for _, f := range familyOrder {
		if families[f] > 0 {
			fmt.Fprintf(stdout, "  %s: %d\n", f, families[f])
		}
	}
	fmt.Fprint(stdout, sums)
	return 0
}

// plant copies writes named by keys and plants one sentence into each copy. It draws the faults
// (perShape of each named shape and nolist of other verbs, the other verbs taken in turn), the
// writes to copy (by family in turn, each family's writes in a seeded order, so every family with
// a body is drawn from) and, for each fault, a paragraph start of the write's main field. With
// controls, a second copy of the same write takes the fault's matched control at the same start.
// load reads the whole cases of the writes it is given.
func plant(refs []*writeRef, keys []writeKey, seed int64, perShape, nolist int, controls bool,
	load func(map[writeKey]bool) (map[writeKey][]Case, error)) ([]Case, []plantedCase, error) {
	rng := rand.New(rand.NewPCG(uint64(seed), 0x706c616e74))
	maker := &sentenceMaker{rng: rng, used: map[string]bool{}}

	var specs []plantVerb
	var others []plantVerb
	for _, v := range plantVerbs {
		if v.shape == shapeOther {
			others = append(others, v)
			continue
		}
		for range perShape {
			specs = append(specs, v)
		}
	}
	rng.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
	for i := range nolist {
		specs = append(specs, others[i%len(others)])
	}
	rng.Shuffle(len(specs), func(i, j int) { specs[i], specs[j] = specs[j], specs[i] })

	inSet := make(map[writeKey]bool, len(keys))
	for _, k := range keys {
		inSet[k] = true
	}
	byFamily := map[string][]*writeRef{}
	for _, r := range refs {
		if inSet[r.Key] && !lineFields[mainSurface(r.Surfaces)] {
			byFamily[r.Family] = append(byFamily[r.Family], r)
		}
	}
	for _, f := range familyOrder {
		pool := byFamily[f]
		rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	}
	var queue []*writeRef
	for round := 0; ; round++ {
		added := false
		for _, f := range familyOrder {
			if round < len(byFamily[f]) {
				queue = append(queue, byFamily[f][round])
				added = true
			}
		}
		if !added {
			break
		}
	}
	// Some writes take no planted sentence cleanly (planting.place), so twice the writes needed
	// are read.
	queue = queue[:min(len(queue), 2*len(specs)+20)]
	keep := make(map[writeKey]bool, len(queue))
	for _, r := range queue {
		keep[r.Key] = true
	}
	texts, err := load(keep)
	if err != nil {
		return nil, nil, err
	}

	var out []Case
	var manifest []plantedCase
	next := 0
	for i, verb := range specs {
		fault, err := maker.make(verb, kindFault)
		if err != nil {
			return nil, nil, err
		}
		sentences := []string{fault}
		if controls {
			control, err := maker.make(verb, kindControl)
			if err != nil {
				return nil, nil, err
			}
			sentences = append(sentences, control)
		}
		placed := false
		for ; next < len(queue) && !placed; next++ {
			ref := queue[next]
			copies, rows, ok := place(texts[ref.Key], ref, sentences, rng)
			if !ok {
				continue
			}
			pair := fmt.Sprintf("pair-%04d", i+1)
			for j := range copies {
				id := fmt.Sprintf("p%04d", len(manifest)+1)
				for k := range copies[j] {
					copies[j][k].Copy, copies[j][k].Tag = id, plantedTag
				}
				out = append(out, copies[j]...)
				rows[j].Copy, rows[j].Pair, rows[j].Tag, rows[j].Seed = id, pair, plantedTag, seed
				rows[j].Shape, rows[j].Verb = verb.shape, verb.forms.base
				rows[j].Kind = kindFault
				if j == 1 {
					rows[j].Kind = kindControl
				}
				manifest = append(manifest, rows[j])
			}
			placed = true
		}
		if !placed {
			return nil, nil, fmt.Errorf("the set has too few writes with a body for %d faults (%d placed)", len(specs), i)
		}
	}
	return out, manifest, nil
}

// place copies a write once for each sentence and plants the sentence, as its own paragraph, at
// one paragraph start of the main field drawn by rng, the same start for every sentence. It tries
// the starts that relayedStarts does not mark, in a seeded order, and then, where none of them
// serves, the first start of the field. A start serves only when, in every copy, the prose reader
// reads the planted sentence as one sentence of the main field that the field holds nowhere else.
// place answers false when no start serves.
func place(cases []Case, ref *writeRef, sentences []string, rng *rand.Rand) ([][]Case, []plantedCase, bool) {
	if len(cases) == 0 {
		return nil, nil, false
	}
	m := mainCase(cases)
	field := cases[m]
	starts := paragraphStarts(field.Text)
	relayed := relayedStarts(field.Text, starts)
	var order []int
	for _, b := range rng.Perm(len(starts)) {
		if !relayed[b] {
			order = append(order, b)
		}
	}
	if len(starts) > 0 && relayed[0] {
		order = append(order, 0)
	}
	readAs := make([]string, len(sentences))
	for j, s := range sentences {
		read := prose.ProseSentences(field.Field, s)
		if len(read) != 1 {
			return nil, nil, false
		}
		readAs[j] = read[0].Text
	}
	for _, b := range order {
		copies := make([][]Case, len(sentences))
		rows := make([]plantedCase, len(sentences))
		clean := true
		for j, s := range sentences {
			text, at := insertParagraph(field.Text, starts[b], s)
			if count(prose.ProseSentences(field.Field, text), readAs[j]) != 1 {
				clean = false
				break
			}
			cp := slices.Clone(cases)
			cp[m].Text, cp[m].SHA256 = text, sha256Hex([]byte(text))
			copies[j] = cp
			rows[j] = plantedCase{
				Sentence: s, SentenceProse: readAs[j],
				EventID: ref.Key.EventID, SourceID: ref.Key.SourceID, Family: ref.Family, Project: ref.Project,
				Field: field.Field, Surface: field.Surface, Position: at, Boundary: b, Boundaries: len(starts),
				NextToRelayed: relayed[b],
			}
		}
		if clean {
			return copies, rows, true
		}
	}
	return nil, nil, false
}

// Signs that a paragraph relays or quotes someone's words.
var (
	// quoteMarks are the quotation marks a quoted paragraph holds. A straight or curly apostrophe
	// is not one: contractions hold it.
	quoteMarks = `"“”„«»‘`
	// attribution is a line that attributes words to a person: "X wrote:", "X said …",
	// "From X:", "X asks: …".
	attribution = regexp.MustCompile(`(?im)^\s*from\s+[^:\n]{1,80}:|\b(?:wrote|said|asked|replied|answered)\b|\b(?:writes|says|asks|replies)\s*(?::|["“])`)
	heading     = regexp.MustCompile(`^\s{0,3}#{1,6}\s`)
)

// textBlock is a run of non-blank lines, by its byte offsets.
type textBlock struct{ start, end int }

// relayedStarts marks each paragraph start of text at which a planted sentence could be read as
// someone's relayed or quoted words: the start right after a block quote, a paragraph holding a
// quotation mark, or a paragraph ending in a colon (a colon-introduced quotation); the start right
// before a block quote or a paragraph holding a quotation mark; and every start after a line
// that attributes words to a person (attribution), up to the next heading.
func relayedStarts(text string, starts []int) []bool {
	var blocks []textBlock
	for offset, open := 0, -1; offset <= len(text); {
		end := strings.IndexByte(text[offset:], '\n')
		next := len(text) + 1
		if end >= 0 {
			next = offset + end + 1
		}
		line := text[offset:min(next, len(text))]
		switch blank := strings.TrimSpace(line) == ""; {
		case !blank && open < 0:
			open = offset
		case blank && open >= 0:
			blocks = append(blocks, textBlock{open, offset})
			open = -1
		}
		if next > len(text) {
			if open >= 0 {
				blocks = append(blocks, textBlock{open, len(text)})
			}
			break
		}
		offset = next
	}
	quoted := func(b textBlock) bool {
		p := text[b.start:b.end]
		return strings.HasPrefix(strings.TrimSpace(p), ">") || strings.ContainsAny(p, quoteMarks)
	}
	introduces := func(b textBlock) bool {
		return strings.HasSuffix(strings.TrimSpace(text[b.start:b.end]), ":")
	}
	marks := make([]bool, len(starts))
	for i, s := range starts {
		relay := false
		var prev, next *textBlock
		for k := range blocks {
			b := &blocks[k]
			if b.start >= s {
				next = b
				break
			}
			prev = b
			switch {
			case heading.MatchString(text[b.start:b.end]):
				relay = false
			case attribution.MatchString(text[b.start:b.end]):
				relay = true
			}
		}
		marks[i] = relay ||
			prev != nil && (quoted(*prev) || introduces(*prev)) ||
			next != nil && quoted(*next)
	}
	return marks
}

// count is how many sentences read exactly text.
func count(sentences []prose.Sentence, text string) int {
	n := 0
	for _, s := range sentences {
		if s.Text == text {
			n++
		}
	}
	return n
}

// readPlanted reads plant's manifest.jsonl.
func readPlanted(path string) ([]plantedCase, error) {
	var rows []plantedCase
	err := eachLine(path, func(line []byte, _ int) error {
		var p plantedCase
		if err := json.Unmarshal(line, &p); err != nil {
			return err
		}
		if p.Copy == "" || (p.Kind != kindFault && p.Kind != kindControl) {
			return errors.New("not a planted case: no copy id or no kind")
		}
		rows = append(rows, p)
		return nil
	})
	if err == nil && len(rows) == 0 {
		err = errors.New("the planted manifest holds no case")
	}
	return rows, err
}
