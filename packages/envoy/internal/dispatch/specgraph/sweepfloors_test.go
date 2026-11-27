package specgraph

import (
	"maps"
	"slices"
	"testing"
	"testing/fstest"
)

// sweepFloors is, for each sweep and each base (sweepBases), the count of inputs that the reader and
// the derivation read and the count that they refused (sweepCount) when the table was written. The
// sweeps are the tree sweep (TestNoInputThatSaysNothingMovesAStateTowardTrue), the members added
// (TestAddingAMemberMovesNoStateTowardTrue), the texts inside a document and the members left out
// of an approved document (TestNoNameOfNothingInsideADocumentMovesAStateTowardTrue), and the refusals added
// (TestAddingARefusalOfCIMovesNoStateTowardTrue). A sweep that reads
// or refuses fewer inputs of a base fails by name (atFloor), and so does a base with no floor or a
// floor of no base (floorsCover), so a sweep that builds no input of a base, or drops some, does not
// pass as one that judged them. The counts are floors: a sweep that gains inputs passes, and its row
// may then be written again from a run's counts. It is a function that gives a new table at each
// call.
func sweepFloors() map[string]map[string]sweepFloor {
	return map[string]map[string]sweepFloor{
		"tree": {
			"carry": {read: 92, refused: 887},
			"carry with a later failing example planted by no step":                      {read: 104, refused: 934},
			"carry with a later failing example planted on c9, no commit of the history": {read: 106, refused: 936},
			"carry with m2's failure and a later pass of m2":                             {read: 107, refused: 1024},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 107, refused: 1010},
			"carry with its failing example planted by no step":                          {read: 88, refused: 878},
			"earlier-text":           {read: 37, refused: 397},
			"one-commit-two-answers": {read: 68, refused: 724},
			"production-and-graph":   {read: 28, refused: 454},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                      {read: 29, refused: 461},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                                   {read: 29, refused: 461},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                     {read: 29, refused: 461},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                               {read: 29, refused: 461},
			"production-and-graph with a pass and a caught failing example of the test of m2 before the failure of the test of c1": {read: 38, refused: 525},
			"production-and-graph with its program check of production as a program check of the graph":                            {read: 28, refused: 454},
			"production-and-graph with its program check of production as an agent of production":                                  {read: 38, refused: 444},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":       {read: 30, refused: 456},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":                    {read: 30, refused: 456},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production":      {read: 30, refused: 456},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                                {read: 30, refused: 456},
			"refusal":    {read: 93, refused: 894},
			"silent-leg": {read: 46, refused: 621},
			"silent-leg with its matrix dropped at m2": {read: 46, refused: 605},
			"wait-cycle": {read: 72, refused: 672},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 92, refused: 815},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 72, refused: 672},
		},
		"members": {
			"carry": {read: 103, refused: 22},
			"carry with a later failing example planted by no step":                      {read: 132, refused: 26},
			"carry with a later failing example planted on c9, no commit of the history": {read: 132, refused: 27},
			"carry with m2's failure and a later pass of m2":                             {read: 180, refused: 32},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 180, refused: 32},
			"carry with its failing example planted by no step":                          {read: 108, refused: 21},
			"earlier-text":           {read: 33, refused: 7},
			"one-commit-two-answers": {read: 150, refused: 21},
			"production-and-graph":   {read: 107, refused: 26},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                      {read: 107, refused: 26},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                                   {read: 107, refused: 26},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                     {read: 107, refused: 26},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                               {read: 107, refused: 26},
			"production-and-graph with a pass and a caught failing example of the test of m2 before the failure of the test of c1": {read: 133, refused: 34},
			"production-and-graph with its program check of production as a program check of the graph":                            {read: 107, refused: 26},
			"production-and-graph with its program check of production as an agent of production":                                  {read: 107, refused: 26},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":       {read: 104, refused: 26},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":                    {read: 104, refused: 26},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production":      {read: 104, refused: 26},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                                {read: 104, refused: 26},
			"refusal":    {read: 103, refused: 22},
			"silent-leg": {read: 140, refused: 31},
			"silent-leg with its matrix dropped at m2": {read: 140, refused: 31},
			"wait-cycle": {read: 97, refused: 13},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 127, refused: 17},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 97, refused: 13},
		},
		"texts": {
			"carry": {read: 188, refused: 387},
			"carry with a later failing example planted by no step":                      {read: 188, refused: 387},
			"carry with a later failing example planted on c9, no commit of the history": {read: 188, refused: 387},
			"carry with m2's failure and a later pass of m2":                             {read: 188, refused: 387},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 188, refused: 387},
			"carry with its failing example planted by no step":                          {read: 188, refused: 387},
			"earlier-text":           {read: 103, refused: 197},
			"one-commit-two-answers": {read: 208, refused: 302},
			"production-and-graph":   {read: 81, refused: 125},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                      {read: 81, refused: 125},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                                   {read: 81, refused: 125},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                     {read: 81, refused: 125},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                               {read: 81, refused: 125},
			"production-and-graph with a pass and a caught failing example of the test of m2 before the failure of the test of c1": {read: 81, refused: 125},
			"production-and-graph with its program check of production as a program check of the graph":                            {read: 81, refused: 125},
			"production-and-graph with its program check of production as an agent of production":                                  {read: 68, refused: 114},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":       {read: 81, refused: 125},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":                    {read: 81, refused: 125},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production":      {read: 81, refused: 125},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                                {read: 81, refused: 125},
			"refusal":    {read: 188, refused: 387},
			"silent-leg": {read: 124, refused: 221},
			"silent-leg with its matrix dropped at m2": {read: 124, refused: 221},
			"wait-cycle": {read: 150, refused: 200},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 192, refused: 246},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 150, refused: 200},
		},
		"left out": {
			"carry": {read: 17, refused: 43},
			"carry with a later failing example planted by no step":                      {read: 17, refused: 43},
			"carry with a later failing example planted on c9, no commit of the history": {read: 17, refused: 43},
			"carry with m2's failure and a later pass of m2":                             {read: 17, refused: 43},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 17, refused: 43},
			"carry with its failing example planted by no step":                          {read: 17, refused: 43},
			"earlier-text":           {read: 14, refused: 32},
			"one-commit-two-answers": {read: 44, refused: 78},
			"production-and-graph":   {read: 14, refused: 38},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                      {read: 14, refused: 38},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                                   {read: 14, refused: 38},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                     {read: 14, refused: 38},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                               {read: 14, refused: 38},
			"production-and-graph with a pass and a caught failing example of the test of m2 before the failure of the test of c1": {read: 14, refused: 38},
			"production-and-graph with its program check of production as a program check of the graph":                            {read: 14, refused: 38},
			"production-and-graph with its program check of production as an agent of production":                                  {read: 13, refused: 37},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":       {read: 14, refused: 38},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":                    {read: 14, refused: 38},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production":      {read: 14, refused: 38},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                                {read: 14, refused: 38},
			"refusal":    {read: 17, refused: 43},
			"silent-leg": {read: 17, refused: 43},
			"silent-leg with its matrix dropped at m2": {read: 17, refused: 43},
			"wait-cycle": {read: 28, refused: 54},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 34, refused: 66},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 28, refused: 54},
		},
		"refusals": {
			"carry": {read: 14, refused: 0},
			"carry with a later failing example planted by no step":                      {read: 16, refused: 0},
			"carry with a later failing example planted on c9, no commit of the history": {read: 18, refused: 0},
			"carry with m2's failure and a later pass of m2":                             {read: 16, refused: 0},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 16, refused: 0},
			"carry with its failing example planted by no step":                          {read: 12, refused: 0},
			"earlier-text":           {read: 5, refused: 0},
			"one-commit-two-answers": {read: 20, refused: 0},
			"production-and-graph":   {read: 4, refused: 0},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                      {read: 3, refused: 0},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                                   {read: 3, refused: 0},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                     {read: 3, refused: 0},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                               {read: 3, refused: 0},
			"production-and-graph with a pass and a caught failing example of the test of m2 before the failure of the test of c1": {read: 4, refused: 0},
			"production-and-graph with its program check of production as a program check of the graph":                            {read: 4, refused: 0},
			"production-and-graph with its program check of production as an agent of production":                                  {read: 4, refused: 0},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":       {read: 4, refused: 0},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":                    {read: 4, refused: 0},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production":      {read: 4, refused: 0},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                                {read: 4, refused: 0},
			"refusal":    {read: 13, refused: 0},
			"silent-leg": {read: 10, refused: 0},
			"silent-leg with its matrix dropped at m2": {read: 10, refused: 0},
			"wait-cycle": {read: 18, refused: 0},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 24, refused: 0},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 18, refused: 0},
		},
	}
}

// sweepFloor is the floor of one sweep of one base: the inputs that the reader and the derivation
// read, as the base or as another state, and the inputs that they refused (sweepCount).
type sweepFloor struct{ read, refused int }

// markReadingsFloor is the count of readings of a member left out that
// TestAMemberLeftOutOfAFileIsReadByItsMark read when the floor was written.
const markReadingsFloor = 82

// atFloor fails the test unless, of the inputs of the base name that the sweep judged (n), the reader
// read at least the floor's count and refused at least the floor's count (sweepFloors).
func atFloor(t *testing.T, sweep, name string, n sweepCount) {
	t.Helper()
	floor, held := sweepFloors()[sweep][name]
	if !held || n.read < floor.read || n.refused < floor.refused {
		t.Errorf("the %s sweep judged %d inputs of %s that the reader read and %d that it refused, and its floor is %d read and %d refused (held %t; sweepFloors)",
			sweep, n.read, name, n.refused, floor.read, floor.refused, held)
	}
}

// floorsCover fails the test unless the sweep has a floor of inputs for each base of bases and for
// no other (sweepFloors).
func floorsCover(t *testing.T, sweep string, bases map[string]fstest.MapFS) {
	t.Helper()
	if got, want := slices.Sorted(maps.Keys(sweepFloors()[sweep])), slices.Sorted(maps.Keys(bases)); !slices.Equal(got, want) {
		t.Errorf("the %s sweep has floors of inputs for the bases %q, and the sweep reads the bases %q (sweepFloors)", sweep, got, want)
	}
}
