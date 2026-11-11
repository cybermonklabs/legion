// Package experiment draws the variant of a declared experiment (config.Experiment): the arm one
// unit — a tree or an issue, whichever the experiment's unit names — is given when that unit is
// opened, which the daemon records in the same breath and reads back from the record at every
// launch of the unit's roles. The draw is made here and nowhere else, so every path that opens a
// unit draws the same way from the same declaration.
//
// # GrowthBook's hashing, and why this package does not implement it
//
// GrowthBook's SDKs derive a unit's arm by hashing it, rather than by drawing once and storing
// what was drawn. Their published specification (https://docs.growthbook.io/lib/build-your-own,
// read on 2026-11-11) gives a helper hash(seed, value, version) that answers a float in [0, 1),
// where seed is the experiment's own seed and value is the unit's hash attribute, its id unless
// the experiment names another:
//
//	version 2: n = fnv32a(fnv32a(seed + value) + "");  bucket = (n % 10000) / 10000
//	version 1: n = fnv32a(value + seed);               bucket = (n % 1000) / 1000
//
// `+` is string concatenation, and version 2's outer hash takes the inner hash's integer written
// out in decimal and nothing else — the hash value is not concatenated a second time. fnv32a
// hashes the string's UTF-16 code units, as JavaScript's charCodeAt gives them, so a character
// outside the Basic Multilingual Plane is two units; an ASCII string hashes the same over its
// bytes. Only a string or a number is a hash value, a number written as JavaScript writes it, so
// 1.0 hashes as "1"; an empty string, a 0 and any other type count as missing. Version 1 is the
// earlier algorithm and biases experiments that run in parallel, which is why version 2 exists; a
// version number the SDK does not know gives no bucket at all.
//
// The bucket is then compared against the variants' cumulative weight ranges. getBucketRanges
// turns the weights, which must sum to 1, and the experiment's coverage c into one range per
// variant, [start, start + c*weight), where start is the sum of the weights declared before it;
// chooseVariation answers the first variant whose range holds the bucket, its start inclusive and
// its end exclusive. Draw's walk below has that shape — the variants in their declared order,
// their cumulative weights, the first arm the drawn value falls below — over whole weights and
// one drawn integer rather than over floats summing to 1.
//
// None of it is implemented here, and GrowthBook is no dependency of this daemon, because the
// hashing buys what this daemon does not need. An off-the-shelf engine earns its keep by computing
// an arm from a published seed and the unit's key wherever that computation runs, which spares a
// system that keeps no durable record of what it drew. This daemon keeps one: the assignment is
// written in the transaction that opens the unit and read from that record at every launch, and
// nothing outside the daemon derives an arm for itself, since the measurement job reads each
// unit's exposure from the state API. Against that, the dependency carries a hash version that has
// changed once already, and a changed default would silently re-bucket every unit of an assignment
// already stored. What would change the decision is a second system that must derive a unit's arm
// without asking the daemon; that is the day the specification above is implemented here.
package experiment

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// Draw is the variant experiment gives one unit, by the experiment's mode. ExperimentOff gives the
// empty string: a unit under an off experiment is recorded at no variant at all. ExperimentOn
// gives the variant on_variant names. ExperimentRandom draws one value below the variants' total
// weight and walks the variants' cumulative weights in their declared order, giving the first
// variant whose cumulative weight the value falls below; the order is the file's, so one source
// value always names the same arm for the same declaration.
//
// source takes the exclusive upper bound of the draw and answers a value uniformly below it. The
// two settled modes read it not at all, since a unit under them is given its variant without a
// draw. CryptoSource is the source the daemon hands in; a test hands in one of its own.
//
// A mode the switch does not know is refused naming it: the loader takes only the three words
// (config.readExperiment), so reaching that arm is a programming error and never an operator's
// file.
func Draw(experiment config.Experiment, source func(uint64) (uint64, error)) (string, error) {
	switch experiment.Mode {
	case config.ExperimentOff:
		return "", nil
	case config.ExperimentOn:
		return experiment.OnVariant, nil
	case config.ExperimentRandom:
		return drawRandom(experiment, source)
	default:
		return "", fmt.Errorf("draw a variant of the experiment %q: unknown experiment mode %q",
			experiment.Name, experiment.Mode)
	}
}

// drawRandom is ExperimentRandom's half of Draw: the total weight, the one value below it, and the
// cumulative walk that turns that value into an arm.
func drawRandom(experiment config.Experiment, source func(uint64) (uint64, error)) (string, error) {
	if len(experiment.Variants) == 0 {
		return "", fmt.Errorf("draw a variant of the experiment %q: it declares no variant to draw among",
			experiment.Name)
	}
	var total uint64
	for _, variant := range experiment.Variants {
		weight := uint64(variant.Weight)
		// The sum is checked before each addition rather than after it, because a sum that wrapped
		// would name an arm quietly and would hand source a bound no draw of it means. The width
		// is the source's own, so a total that does not fit is a total no source could answer
		// below. config.Variant.Weight is an int the loader has already held above zero
		// (config.readVariants), so a sum that passes the width is many large weights added up,
		// not one negative weight dragging the total around.
		if total > math.MaxUint64-weight {
			return "", fmt.Errorf(
				"draw a variant of the experiment %q: the variants' weights total more than %d, which is more than one draw carries",
				experiment.Name, uint64(math.MaxUint64))
		}
		total += weight
	}
	if total == 0 {
		return "", fmt.Errorf(
			"draw a variant of the experiment %q: the variants' weights total zero, and a draw among them would divide by it",
			experiment.Name)
	}
	drawn, err := source(total)
	if err != nil {
		return "", fmt.Errorf("draw a variant of the experiment %q: %w", experiment.Name, err)
	}
	var cumulative uint64
	for _, variant := range experiment.Variants {
		cumulative += uint64(variant.Weight)
		if drawn < cumulative {
			return variant.Name, nil
		}
	}
	// The walk falls through only where source broke its word and answered a value its bound
	// excludes, which no arm's cumulative weight covers.
	return "", fmt.Errorf(
		"draw a variant of the experiment %q: the source answered %d, which is not below the variants' total weight %d",
		experiment.Name, drawn, total)
}

// CryptoSource is the random source the daemon draws `random` with: one value uniformly below
// bound, from crypto/rand.Int over crypto/rand.Reader, which is the operating system's own source.
// A draw decides what a unit runs under for the whole of its life and is compared across units, so
// it is taken from that source rather than from a seeded math/rand sequence, which a reader of the
// code could replay and a second process with the same seed would repeat.
//
// A bound of zero is refused rather than handed on: crypto/rand.Int panics on a bound that is not
// above zero, and Draw refuses a total weight of zero before any source is read.
func CryptoSource(bound uint64) (uint64, error) {
	if bound == 0 {
		return 0, fmt.Errorf("read a random value below %d: there is no value below zero", bound)
	}
	drawn, err := rand.Int(rand.Reader, new(big.Int).SetUint64(bound))
	if err != nil {
		return 0, fmt.Errorf("read a random value below %d from crypto/rand: %w", bound, err)
	}
	return drawn.Uint64(), nil
}
