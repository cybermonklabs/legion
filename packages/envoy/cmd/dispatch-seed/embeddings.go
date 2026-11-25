package main

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// embeddingModel is the one `model` value every seeded vector carries, which the command prints
// so a meaning measurement binds the same value the rows hold ($11 of searchQueryMeaning).
const embeddingModel = "seed-embed-v1"

// embeddingDimensions is the width of `embeddings.embedding`, vector(1536) since migration 0072.
const embeddingDimensions = 1536

// embeddingCentres is how many clusters the vectors are drawn around, and embeddingJitter how far
// a row is drawn from its centre. Two random vectors of 1,536 dimensions are near orthogonal, so
// vectors drawn uniformly score about zero against a random query and every meaning arm of search
// would return nothing: below api/search.go's searchMeaningFloor of 0.25 a row never reaches the
// legs at all. A row drawn as normalise(centre + jitter * unit) scores about 1/sqrt(1+jitter^2)
// against its centre - 0.89 at this jitter - and about zero against any other centre, so a query
// taken from a centre finds that centre's rows and only those.
const (
	embeddingCentres = 50
	embeddingJitter  = 0.5
)

// embeddingBatch is how many rows one update statement carries. Each vector is about 11 KB of
// text, so a larger batch would make one statement several hundred megabytes.
const embeddingBatch = 200

// hnswBuildSettings keep the index build out of the host's shared memory. A parallel maintenance
// operation asks for a dynamic shared memory segment sized by maintenance_work_mem, which lives
// in /dev/shm, and a container holds that to 64 MB: CI's Postgres service refused one at
// 63,999,456 bytes ("could not resize shared memory segment … No space left on device") on two
// runs of this seed, whose embeddings heap of 13 MB at scale 0.01 is past
// min_parallel_table_scan_size and so earns the build its workers. A build with no maintenance
// workers holds its graph in the backend's own memory and asks the host for nothing, which is
// the whole of what is needed here. How much memory that graph may take is the cluster's to say
// (maintenance_work_mem), and the measurement protocol sets it: a bound written here would
// override it, and at this tool's default scale — about 285,000 vectors — too small a one puts
// pgvector back on the per-row on-disk insertions that dropping the index exists to avoid.
var hnswBuildSettings = []string{
	`set local max_parallel_maintenance_workers = 0`,
}

// writeEmbeddings gives a vector to every row embeddedRows names: half the issues, and one window
// of each of half the documents, asks, comments and messages. It drops the HNSW index first and
// builds it again after: about 285,000 per-row insertions into an HNSW index at scale 1 would
// dominate the run, where one build at the cluster's maintenance memory is a fraction of it (the
// plan's Decision "The seed drops and rebuilds the HNSW index around its vector writes"). That
// build is serial and bounded, for the reason hnswBuildSettings gives.
func (r *seedRun) writeEmbeddings(ctx context.Context) error {
	if _, err := r.database.Pool.Exec(ctx, `drop index if exists embeddings_cosine`); err != nil {
		return fmt.Errorf("drop the HNSW index: %w", err)
	}
	centres := newEmbeddingCentres()
	kinds, ids, err := r.embeddedRows(ctx)
	if err != nil {
		return err
	}
	random := rand.New(rand.NewPCG(randomSeed+2, randomSeed^0xd1b54a32d192ed03))
	literal := make([]string, 0, embeddingBatch)
	at := baseTime.Add(seedSpan)
	if err := r.inBatches(ctx, "embeddings", len(ids), embeddingBatch,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			literal = literal[:0]
			for index := from; index < to; index++ {
				centre := centres[index%embeddingCentres]
				// The first row of each cluster is the centre itself, so the command can name a
				// row whose vector is exactly the query a measurement binds.
				if index < embeddingCentres {
					literal = append(literal, vectorLiteral(centre))
					continue
				}
				literal = append(literal, vectorLiteral(aroundCentre(centre, random)))
			}
			_, err := tx.Exec(ctx, `
				update embeddings e
				   set embedding = v.embedding::vector, model = $1, embedded_hash = e.content_hash,
				       embedded_at = $2, pending_since = $2, next_attempt_at = $2, attempt_count = 0
				  from unnest($3::text[], $4::text[], $5::text[]) as v (kind, id, embedding)
				 where e.kind = v.kind and e.id = v.id
			`, embeddingModel, at, kinds[from:to], ids[from:to], literal)
			return err
		}); err != nil {
		return err
	}
	if err := pgx.BeginFunc(ctx, r.database.Pool, func(tx pgx.Tx) error {
		for _, setting := range hnswBuildSettings {
			if _, err := tx.Exec(ctx, setting); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			create index embeddings_cosine on embeddings using hnsw (embedding vector_cosine_ops) where (embedding is not null)
		`)
		return err
	}); err != nil {
		return fmt.Errorf("build the HNSW index: %w", err)
	}
	r.subject.Model = embeddingModel
	r.subject.QueryVectorRow = fmt.Sprintf("%s %s", kinds[0], ids[0])
	return nil
}

// windowedKinds is each kind of row meaning search embeds in windows of its blocks rather than
// whole, in the order embeddedRows takes them: a document's, an ask's, a comment's and a
// message's. Each holds the embeddings kind of its windows, how many rows of the kind the plan
// holds, and the owner key of the plan's row at an index, which each of that row's windows' ids
// begins with (refs.writeWindows). refs.WindowKinds is the same kinds as a literal SQL list, the
// list embeddings_window_owner's partial predicate names (migration 20261102233630).
var windowedKinds = []struct {
	kind  string
	count func(plan *seedPlan) int
	owner func(plan *seedPlan, index int) string
}{
	{
		refs.DocumentWindowKind,
		func(plan *seedPlan) int { return len(plan.documents) },
		func(plan *seedPlan, index int) string { return plan.documents[index].ID },
	},
	{
		refs.ItemAsk.WindowKind(),
		func(plan *seedPlan) int { return len(plan.asks) },
		func(plan *seedPlan, index int) string {
			return refs.ItemAsk.WindowOwnerKey(plan.asks[index].ID)
		},
	},
	{
		refs.ItemComment.WindowKind(),
		func(plan *seedPlan) int { return len(plan.comments) },
		func(plan *seedPlan, index int) string {
			return refs.ItemComment.WindowOwnerKey(plan.comments[index].ID)
		},
	},
	{
		refs.ItemMessage.WindowKind(),
		func(plan *seedPlan) int { return len(plan.messages) },
		func(plan *seedPlan, index int) string {
			return refs.ItemMessage.WindowOwnerKey(plan.messages[index].ID)
		},
	},
}

// embeddedRows is every (kind, id) that gets a vector, in the order the vectors are drawn: every
// other issue in plan order, then the first window by id of every other document, ask, comment
// and message in plan order. An issue is embedded whole, through its own row; the other four are
// embedded in windows of their blocks, and since migrations 20261102233626 to 20261102233629 hold
// no row of their own kind, so a vector on a row of the kind itself would be a vector on nothing
// and every window arm of meaning search would read no row. One window for each owner keeps the
// count of the rule before windows, one vector for half the rows of each kind (about 285,000 at
// scale 1); an owner with no window gets none.
//
// The issues' rows are the triggers', queued when the seed wrote each issue, and the windows are
// the ones refs.WriteDocumentWindows and refs.WriteItemWindows wrote in the steps before this
// one, read back here, so every row named is an update, not an insert. The issues come first so
// the rows the clusters' centres go to (writeEmbeddings) and the query vector's row are issues,
// as they were before windows.
func (r *seedRun) embeddedRows(ctx context.Context) (kinds, ids []string, err error) {
	for index := 0; index < len(r.plan.issues); index += 2 {
		kinds = append(kinds, "issue")
		ids = append(ids, r.plan.issues[index].Key)
	}
	var ownerKinds, owners []string
	for _, windowed := range windowedKinds {
		for index := 0; index < windowed.count(r.plan); index += 2 {
			ownerKinds = append(ownerKinds, windowed.kind)
			owners = append(owners, windowed.owner(r.plan, index))
		}
	}
	// A window's id is "<owner key>#<its first block>" (refs.writeWindows). The first window of
	// every owner is read in one pass over the window rows and joined to the owners the rule takes.
	// A probe for each owner's first window by id (order by id limit 1) is one the planner answers
	// by walking the primary key of (kind, id) in order until a row of the owner turns up: at scale
	// 0.01 it read 1,850 comment windows for each comment, half of them, which at scale 1 is about
	// 185,000 rows for each of the 185,000 comments the rule takes.
	rows, err := r.database.Pool.Query(ctx, `
		select w.kind, w.id
		  from unnest($1::text[], $2::text[]) with ordinality as o (kind, owner_key, n)
		  join (select distinct on (kind, split_part(id, '#', 1)) kind, split_part(id, '#', 1) as owner_key, id
		          from embeddings
		         where kind in `+refs.WindowKinds+`
		         order by kind, split_part(id, '#', 1), id) w
		    on w.kind = o.kind and w.owner_key = o.owner_key
		 order by o.n
	`, ownerKinds, owners)
	if err != nil {
		return nil, nil, fmt.Errorf("read the first window of each owner: %w", err)
	}
	var kind, id string
	if _, err := pgx.ForEachRow(rows, []any{&kind, &id}, func() error {
		kinds = append(kinds, kind)
		ids = append(ids, id)
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("read the first window of each owner: %w", err)
	}
	return kinds, ids, nil
}

// newEmbeddingCentres is the cluster centres, each a unit vector from the run's fixed seed.
func newEmbeddingCentres() [][]float64 {
	random := rand.New(rand.NewPCG(randomSeed+3, randomSeed^0x2545f4914f6cdd1d))
	centres := make([][]float64, embeddingCentres)
	for index := range centres {
		centres[index] = unitVector(random)
	}
	return centres
}

// unitVector is a vector of embeddingDimensions normal components, normalised.
func unitVector(random *rand.Rand) []float64 {
	vector := make([]float64, embeddingDimensions)
	for index := range vector {
		vector[index] = random.NormFloat64()
	}
	return normalise(vector)
}

// aroundCentre is one row's vector: its centre pushed in a random direction by embeddingJitter
// and normalised again.
func aroundCentre(centre []float64, random *rand.Rand) []float64 {
	offset := unitVector(random)
	vector := make([]float64, embeddingDimensions)
	for index := range vector {
		vector[index] = centre[index] + embeddingJitter*offset[index]
	}
	return normalise(vector)
}

func normalise(vector []float64) []float64 {
	length := 0.0
	for _, value := range vector {
		length += value * value
	}
	length = math.Sqrt(length)
	if length == 0 {
		return vector
	}
	for index := range vector {
		vector[index] /= length
	}
	return vector
}

// vectorLiteral is a vector as pgvector reads one. Six decimals is far finer than the differences
// cosine distance turns on here and keeps a row's text to about 11 KB.
func vectorLiteral(vector []float64) string {
	out := make([]byte, 0, embeddingDimensions*10)
	out = append(out, '[')
	for index, value := range vector {
		if index > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendFloat(out, value, 'f', 6, 64)
	}
	return string(append(out, ']'))
}
