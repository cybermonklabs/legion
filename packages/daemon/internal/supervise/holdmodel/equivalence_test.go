package holdmodel

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unsafe"
)

var (
	surfaceOutput = flag.String("hold-surface-output", "", "write the model's behaviour surface to this path")
	surfaceDepth  = flag.Int("hold-surface-depth", 2, "continuation depth of the behaviour surface")
)

// TestModelSurface writes the whole behaviour of the model, as one line per
// seed prefix. A line's digest covers, for that prefix and for every
// continuation of it up to -hold-surface-depth: every byte of the State (the
// memory row, the committed store, the world, the pending write, every other
// field of the Machine and the driver's ledger), the oracles' verdict there,
// the set of events enabled there, and every byte of the effects of each.
// Two trees whose surfaces are byte for byte equal decide the same thing
// everywhere the seeds can reach.
//
// The mutation runner writes this file for the unmutated model and for each
// mutant that no oracle, seed or plant caught, and fails the mutant whose
// file differs: a surviving mutant is allowed only when the run shows it
// changes nothing (rule 7), never on a written argument.
func TestModelSurface(t *testing.T) {
	if *surfaceOutput == "" {
		t.Skip("set -hold-surface-output to write the behaviour surface")
	}
	requirePlainBytes(t)
	f, err := os.Create(*surfaceOutput)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	var notes []string
	var roots []Prefix
	seen := map[State]bool{}
	for _, c := range allSeeds() {
		for _, p := range surfaceReplay(c, func(n string) { notes = append(notes, n) }) {
			if seen[p.State] {
				continue
			}
			seen[p.State] = true
			roots = append(roots, p)
		}
	}
	sort.Strings(notes)
	for _, n := range notes {
		fmt.Fprintln(w, n)
	}
	memo := map[surfaceKey]uint64{}
	for _, r := range roots {
		fmt.Fprintf(w, "%s\t%016x\n", r.Origin, surfaceDigest(r.State, *surfaceDepth, memo))
	}
	fmt.Printf("surface roots=%d depth=%d notes=%d expanded=%d\n", len(roots), *surfaceDepth, len(notes), len(memo))
}

// requirePlainBytes is the control for the digest below, which reads each
// value as its own bytes: that is complete only while every field of a State
// and of its effects is plain data. A pointer, slice, map, string, interface,
// channel or function would make the bytes an address and the digest
// meaningless, so the surface refuses to run.
func requirePlainBytes(t *testing.T) {
	t.Helper()
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(ty reflect.Type, path string) {
		if seen[ty] {
			return
		}
		seen[ty] = true
		switch ty.Kind() {
		case reflect.Bool, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uintptr:
		case reflect.Array:
			walk(ty.Elem(), path+"[]")
		case reflect.Struct:
			for i := range ty.NumField() {
				walk(ty.Field(i).Type, path+"."+ty.Field(i).Name)
			}
		default:
			t.Fatalf("%s is a %s: the behaviour surface can only digest plain data", path, ty.Kind())
		}
	}
	walk(reflect.TypeOf(State{}), "State")
	walk(reflect.TypeOf(Effects{}), "Effects")
	walk(reflect.TypeOf(Event{}), "Event")
}

// surfaceReplay replays one seed and returns every prefix it reached. It
// reports a failed verdict, a disabled event and an invariant as a note, so a
// mutant a seed catches still writes a surface rather than no file at all.
func surfaceReplay(c seed, note func(string)) []Prefix {
	s := c.start
	ps := []Prefix{{s, c.name + "[0]", 0}}
	count := 0
	for _, word := range strings.Fields(c.trace) {
		if v, ok := strings.CutPrefix(word, ":"); ok {
			if !verdict(s, v) {
				note(fmt.Sprintf("SEED %s[%d] want %s, got %s", c.name, count, v, s.Class()))
			}
			continue
		}
		e, err := parseWord(word)
		if err != nil {
			note(fmt.Sprintf("SEED %s[%d] %v", c.name, count, err))
			return ps
		}
		if !Enabled(s, e) {
			note(fmt.Sprintf("SEED %s[%d] disabled %s", c.name, count, word))
			return ps
		}
		s, _ = Step(s, e)
		count++
		if bad := Check(s); bad != 0 {
			note(fmt.Sprintf("SEED %s[%d] after %s mask %d", c.name, count, word, bad))
			return ps
		}
		ps = append(ps, Prefix{s, fmt.Sprintf("%s[%d]", c.name, count), count})
	}
	return ps
}

type surfaceKey struct {
	s State
	d uint8
}

const (
	fnvStart uint64 = 14695981039346656037
	fnvPrime uint64 = 1099511628211
)

func fnvBytes(h uint64, b []byte) uint64 {
	for _, c := range b {
		h ^= uint64(c)
		h *= fnvPrime
	}
	return h
}
func fnvWord(h, v uint64) uint64 {
	for range 8 {
		h ^= v & 0xff
		h *= fnvPrime
		v >>= 8
	}
	return h
}
func bytesOf[T any](v *T) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(v)), unsafe.Sizeof(*v))
}

// surfaceDigest is the digest of one state and of every continuation of it to
// the given depth. It is a pure function of the two, so the memo below is
// exact. The states of the last level are not memoized: there are two orders
// of magnitude more of them than of the states that are expanded.
func surfaceDigest(s State, depth int, memo map[surfaceKey]uint64) uint64 {
	key := surfaceKey{s, uint8(depth)}
	if depth > 0 {
		if v, ok := memo[key]; ok {
			return v
		}
	}
	h := fnvWord(fnvBytes(fnvStart, bytesOf(&s)), uint64(Check(s)))
	if depth == 0 {
		return h
	}
	for _, e := range alphabet {
		if !s.base(e) {
			continue
		}
		t, x := run(s, e)
		h = surfaceEdge(h, e, x, surfaceDigest(t, depth-1, memo))
		for fault, at := range x.cuts {
			e := e
			e.Fault, e.At = fault, at
			t, x := run(s, e)
			h = surfaceEdge(h, e, x, surfaceDigest(t, depth-1, memo))
		}
	}
	memo[key] = h
	return h
}
func surfaceEdge(h uint64, e Event, x Effects, child uint64) uint64 {
	return fnvWord(fnvBytes(fnvBytes(h, bytesOf(&e)), bytesOf(&x)), child)
}

// surfaceDiff names the first line on which two surfaces differ, "" when they
// are equal.
func surfaceDiff(left, right string) (string, error) {
	a, err := os.ReadFile(left)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(right)
	if err != nil {
		return "", err
	}
	if string(a) == string(b) {
		return "", nil
	}
	as, bs := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := range max(len(as), len(bs)) {
		x, y := "", ""
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			return fmt.Sprintf("line %d: unmutated %q, mutant %q", i+1, x, y), nil
		}
	}
	return "the files differ", nil
}
