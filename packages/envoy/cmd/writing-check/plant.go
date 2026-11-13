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
	"slices"

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

// sharedFaultSubjects are the subjects of most faults: texts and changes, none of them a person,
// a payer or a display surface, so no verb with them reads in a literal sense.
var sharedFaultSubjects = []subject{
	{"The new section", false}, {"The update", false}, {"The second draft", false},
	{"This change", false}, {"The plan", false}, {"The summary", false}, {"The rewrite", false},
	{"The latest patch", false}, {"The two notes", true}, {"These changes", true}, {"Both drafts", true},
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
		faultRests: []string{"the risk.", "the missing step.", "the owner of the change.", "the cause of the delay.",
			"the open question.", "the next action.", "the broken method.", "every affected file."},
		controlSubjects: one("The config file", "The deploy script", "The release note", "The test fixture", "The new manifest", "The settings file"),
		controlRests: []string{"`us-west1` as the region.", "`v2.4.1` as the release.", "`build-cache` as the bucket.",
			"port `9090` for the metrics server.", "`alice` as the reviewer.", "`2026-12-01` as the cutoff date."},
	},
	{
		shape: shapeShows, forms: verbForms{"show", "shows", "showed"}, faultSubjectsAreShared: true,
		faultRests: []string{"the gap.", "the cause.", "the real problem.", "nothing new.", "no strain.",
			"the trade-off.", "real progress.", "what the fix does."},
		controlSubjects: one("The status page", "The dashboard", "The build log", "The job page", "The command's output", "The queue view"),
		controlRests: []string{"12 failed jobs since Monday.", "the exit code `2`.", "the line `ready on port 8080`.",
			"a deploy time of 09:40 UTC.", "3 new files under `docs/`.", "a queue depth of 417."},
	},
	{
		shape: shapePays, forms: verbForms{"pay", "pays", "paid"},
		faultSubjects: []subject{{"This branch", false}, {"The new cache", false}, {"The retry loop", false},
			{"The second pass", false}, {"The migration", false}, {"Each rebuild", false}, {"Both jobs", true}, {"The two scans", true}},
		faultRests: []string{"the merge clash a second time.", "the startup cost once.", "the scan twice.",
			"the full build again.", "off.", "for itself.", "the cost of the lock again.", "the wait up front."},
		controlSubjects: []subject{{"The team", false}, {"The customer", false}, {"Our lab", false},
			{"The company", false}, {"The client", false}, {"Both teams", true}},
		controlRests: []string{"$120 a month for the build runners.", "$4,300 for the license.", "the vendor $2,500 for the audit.",
			"$45 for each extra seat.", "an invoice of $860 to the host.", "$15 an hour for the test machines."},
	},
	otherVerb("frame", "frames", "framed", []string{"the risk.", "the decision."},
		one("The protocol", "The encoder"), []string{"each message with a 4-byte length header.", "every packet with a 2-byte checksum."}),
	otherVerb("capture", "captures", "captured", []string{"the intent.", "the lesson."},
		one("The script", "The test runner"), []string{"the exit code `3` in `run.log`.", "the full output of `make test` in `out.txt`."}),
	otherVerb("surface", "surfaces", "surfaced", []string{"the concern.", "the tension."},
		one("The submarine", "The diver"), []string{"at 06:10 near the north dock.", "200 m from the boat."}),
	otherVerb("anchor", "anchors", "anchored", []string{"the approach.", "the discussion."},
		one("The crew", "The captain"), []string{"the boat 200 m offshore.", "the ship in 12 m of water."}),
	otherVerb("land", "lands", "landed", []string{"the fix.", "the idea."},
		one("The flight", "The plane"), []string{"at 14:05 on Friday.", "on runway 2 at 09:30."}),
	otherVerb("carry", "carries", "carried", []string{"the idea forward.", "the weight of the decision."},
		one("The truck", "The van"), []string{"40 boxes of paper.", "1,200 kg of cement."}),
	otherVerb("unblock", "unblocks", "unblocked", []string{"the path.", "the next phase."},
		one("The admin", "The moderator"), []string{"the user `jdoe` at 10:15.", "the account `ops-bot` on Monday."}),
	otherVerb("cover", "covers", "covered", []string{"the concern.", "the hard part."},
		one("The insurance policy", "The warranty"), []string{"$50,000 of damage.", "repairs for 24 months."}),
	otherVerb("drive", "drives", "drove", []string{"the outcome.", "the alignment."},
		one("Ana", "The courier"), []string{"the van to the depot every Tuesday.", "120 km to the plant each day."}),
	otherVerb("own", "owns", "owned", []string{"the outcome.", "the follow-through."},
		one("The lab", "Our team"), []string{"12 GPUs.", "the domain `example.org`."}),
	otherVerb("close", "closes", "closed", []string{"the loop.", "the gap."},
		one("The store", "The office"), []string{"at 21:00 on weekdays.", "for two days in December."}),
	otherVerb("track", "tracks", "tracked", []string{"the intent.", "the spirit of the plan."},
		one("The GPS unit", "The tracker"), []string{"the truck's position every 30 seconds.", "each parcel by its code `PX-114`."}),
	otherVerb("signal", "signals", "signaled", []string{"the direction.", "a shift."},
		one("The driver", "The cyclist"), []string{"a left turn at the junction.", "a stop 30 m before the light."}),
	otherVerb("reflect", "reflects", "reflected", []string{"the thinking.", "the new reality."},
		one("The mirror", "The panel"), []string{"90% of the light.", "about 40% of the heat."}),
	otherVerb("mirror", "mirrors", "mirrored", []string{"the intent.", "the old approach."},
		one("The job", "The sync script"), []string{"the `main` branch to `backup-repo` every hour.", "the bucket `logs-eu` to `logs-us` each night."}),
	otherVerb("hold", "holds", "held", []string{"the context.", "the line."},
		one("The list", "The table"), []string{"three users: `alice`, `bob` and `carol`.", "two rows: `id=7` and `id=9`."}),
	otherVerb("gate", "gates", "gated", []string{"the work.", "the next step."},
		one("The feature flag `beta_search`", "The flag `--strict`"), []string{"the new search page for 5% of users.", "the new parser in the `v3` build."}),
	otherVerb("back", "backs", "backed", []string{"the claim.", "the direction."},
		one("The bank", "The fund"), []string{"the loan with $2 million.", "the startup with $500,000."}),
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
	Seed          int64  `json:"seed"`
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
// one paragraph start of the main field drawn by rng, the same start for every sentence. A start
// is taken only when, in every copy, the prose reader reads the planted sentence as one sentence
// of the main field that the field holds nowhere else; place tries the starts in a seeded order
// and answers false when none is clean.
func place(cases []Case, ref *writeRef, sentences []string, rng *rand.Rand) ([][]Case, []plantedCase, bool) {
	if len(cases) == 0 {
		return nil, nil, false
	}
	m := mainCase(cases)
	field := cases[m]
	starts := paragraphStarts(field.Text)
	order := rng.Perm(len(starts))
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
			}
		}
		if clean {
			return copies, rows, true
		}
	}
	return nil, nil, false
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
