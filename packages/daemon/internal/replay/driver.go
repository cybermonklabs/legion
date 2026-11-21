package replay

// FactWay is how a replay comes by one kind of fact the daemon applies. A daemon applies facts
// its Dispatch consumer decodes and facts its own producers make — the API, the ticks and boots,
// the outbox's executors, supervision, GitHub — and a replay has none of those producers, so each
// kind of fact is either rebuilt from the window's record, made by the replay as the daemon's own
// producer makes it, or left out under an assumption the run names.
type FactWay string

const (
	// FactInjected is a fact the replay applies at its recorded time, or at its recorded offset
	// from the start it depends on, built from the window's record: a Dispatch event through
	// intake.DecodeDispatchEvent, or a recorded line of the act that applied it.
	FactInjected FactWay = "injected"
	// FactProduced is a fact the replay's own driver or worker stub applies as the daemon's own
	// producer applies it, from the replay's own state: its ticks, its boots and the outbox rows
	// it executes.
	FactProduced FactWay = "produced"
	// FactLeftOut is a fact a replay never applies, under the assumption FactSource.Assumption
	// names: one that changes nothing the replay decides or measures.
	FactLeftOut FactWay = "left_out"
)

// FactSource is how a replay comes by one kind of fact: its way, what it builds the fact from or
// the act of its own that applies it, and, for a fact it leaves out, the assumption it leaves it
// out under.
type FactSource struct {
	// Way is how the replay comes by the fact.
	Way FactWay
	// From is what the replay builds the fact from — the recorded Dispatch event or line it reads
	// — or the act of the replay's own that applies it; empty for a fact left out.
	From string
	// Assumption is the number of the assumption (Assumptions) the fact is left out under; zero
	// for a fact the replay applies.
	Assumption int
}

// FactSources is how a replay comes by each kind of fact the daemon applies, by the name of its
// type in package intake: every type intake.Fact is, the ones Dispatch events decode to and the
// ones the daemon's other producers make (its API, ticks, boots, outbox executors, supervision and
// GitHub intake). A fact type with no entry here has no source in the replay, and the inventory
// test (TestEveryFactANonDispatchProducerAppliesHasASource) names it.
var FactSources = map[string]FactSource{}
