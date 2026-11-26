package holdmodel

// Prefix is a start or a prefix that the seed runner actually reached. Origin
// names the seed and the prefix length. Length includes the seed prefix, so a
// shortest witness is not just a short suffix of an arbitrarily long trace.
type Prefix struct {
	State  State
	Origin string
	Length int
}
type Witness struct {
	Origin string
	Steps  []Event
	Length int
}
type Stats struct {
	States, Edges, Sequences uint64
	Roots                    int
	Violations               map[uint16]Witness
}
type path struct {
	before *path
	ev     Event
	root   int
}
type reach struct {
	ways   uint64
	p      *path
	length int
}
type transition func(State, Event) (State, Effects)

// Explore expands exact comparable states at each remaining depth. It counts
// all paths, including paths that converge or stutter, via their multiplicity.
// Terminal states are checked on the incoming edge and need no expansion.
func Explore(prefixes []Prefix, depth int) Stats { return explore(prefixes, depth, run) }
func explore(roots []Prefix, depth int, step transition) Stats {
	out := Stats{Violations: map[uint16]Witness{}}
	front := map[State]reach{}
	for i, r := range roots {
		n, ok := front[r.State]
		if !ok || n.length > r.Length {
			front[r.State] = reach{1, &path{root: i}, r.Length}
		}
	}
	out.Roots = len(front)
	for level := 0; level < depth; level++ {
		next := map[State]reach{}
		for s, n := range front {
			out.States++
			visit := func(e Event, t State) {
				out.Edges++
				out.Sequences += n.ways
				bad := Check(t)
				if bad != 0 {
					for bit := uint16(1); bit != 0 && bit <= FatalClose; bit <<= 1 {
						if bad&bit == 0 {
							continue
						}
						old, found := out.Violations[bit]
						if found && old.Length <= n.length+1 {
							continue
						}
						p := n.p
						steps := make([]Event, level+1)
						steps[level] = e
						for j := level - 1; j >= 0; j-- {
							steps[j] = p.ev
							p = p.before
						}
						out.Violations[bit] = Witness{roots[p.root].Origin, steps, n.length + 1}
					}
				}
				if level+1 == depth {
					return
				}
				v, ok := next[t]
				v.ways += n.ways
				if !ok || v.length > n.length+1 {
					v.length = n.length + 1
					v.p = &path{before: n.p, ev: e}
				}
				next[t] = v
			}
			for _, e := range alphabet {
				if !s.base(e) {
					continue
				}
				t, x := step(s, e)
				visit(e, t)
				for fault, at := range x.cuts {
					e.Fault, e.At = fault, at
					t, _ = step(s, e)
					visit(e, t)
				}
			}
		}
		front = next
	}
	return out
}
