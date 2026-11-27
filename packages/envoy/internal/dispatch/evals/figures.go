package evals

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"iter"
	"maps"
	"math"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// The stored figures (dispatch://INFRA-2916, k8; migration 20261120072042) are the eval list's
// folds of the ranges the Evals tab offers, kept ahead of the read, so a paint of an offered range
// reads its figures by key where it would read every day row of the range and fold them
// (DayWindowIn, SeriesValuesOf). k8 measured the first paint at 84 days with a compare range on the
// farm's slowest node: 95.6 ms of database time over the day rows, 12.2 ms over the stored rows.
//
//   - eval_range_figures holds, for each project, cut (figureCut: '' for the project's own rows, or
//     a repository), range (one of FigureLengths and its end day, exclusive) and eval, the
//     statistics FoldStatistics answers for the eval's listed series over the range's rows of the
//     cut, those rows in the folds' one order (sortWindowRows) with a repository cut's zero parts
//     (cutZeros, withZeros), each series' via-repository count, and the oldest updated_at of the
//     rows (the read's freshness). A range that holds no listed row of the eval in the cut has no
//     row: its fold is the fold of no parts.
//   - eval_day_figures holds, for each project, cut, day and eval that holds a listed row of the
//     eval in the cut, the day's figures of each headline series that holds a part that day, a
//     repository cut's zero part among them (the per-day array SeriesValuesOf answers), and the
//     stamp of the rows it folded (rowsStamp).
//
// Each is kept for every cut a read folds: the project's own rows (repo '', the all-repositories
// day rows: its own and those of the owner keys it reads under the published defaults, owners.go),
// which a read that names no repository folds, the first paint of the Evals tab k8 measured; and
// each repository's, which a read that names that one repository folds (DayWindowIn with it, R16's
// header filter): the rows of the repository the project reads, its own and its owner keys', and,
// of the repository it publishes as its default, its own rows stored under
// legionfacts.DefaultRepo (repoCut), which belong to that cut alone and to none where the project
// publishes no default (cutOf). No cut of legionfacts.DefaultRepo itself is stored: no read names
// it. At INFRA's size the fold of the day rows of one repository read 35,297 rows at 84 days with a
// compare range, 75 to 175 ms on the farm, past R8's 150 ms (the eval list's 84-day read bound),
// where its stored cut answered in at most 17 ms at 04de13fa. Each is kept for the rows of each
// deploy repository apart, as the day rows are (deploy_repos.go), and for each project's union of
// all its currently recorded deploy repositories under deploy_repo `*` (unionDeployRepo): a fold
// of the evals that read the deploy repository over the rows of DeployRepos.Of(project), which
// ReadStoredFiguresIn looks up for every deploy-keyed eval, whether the project reads one deploy
// repository or several (a project with two GitHub sources, or a GitHub source and an events
// source).
// A range exists for every length of FigureLengths and every end day from the day after the
// eval's first row through FigureLead days after today: a compare range ends on any day before the
// window's, and a page held open keeps its compare range while its window rolls on.
//
// Every write keeps one rule: a stored figure is a fold of day rows, so it moves only with them.
// A write that folds its moved days inline folds them in each cut they reach (refoldFigures):
// a row of a repository reaches the cut of that
// repository of every project that reads its key, a row stored under legionfacts.DefaultRepo the
// cut of its project's published default, and a row of repo '' the project's own; in each, the
// day's own row and every range whose span holds the day (D < E <= D+n), up to the last end day
// the project's figures are built through (figureScope), each upserted only where its figures
// moved and deleted where it holds no row any more. A day's rows moved where their stamp is not
// the one its stored day row recorded, or that row was folded under another fold key (foldKeyOf).
// So a write that moved nothing, such as a new build's rebuild of rows whose values stood, reads
// two short index ranges and writes nothing. A readings write whose days and series pass the most
// one transaction folds (inlineFoldDaySeries) folds of them the first paint of each cut its rows
// reach and marks those cuts stale, naming its eval, for the refold below (foldWrittenPaints).
// A settings rewrite marks the cuts its changed and deleted rows reach for the same refold
// (markMovedCuts), naming the evals that moved, and folds the first paints of the cuts whose
// marks name only computed evals in its commit (foldMarkedFirstPaints, paintReached).
//
// A move of a project's default repository moves no day row, and moves what some of its cuts
// fold: the old and the new default's (the rows stored under legionfacts.DefaultRepo), and, of
// each owner key whose reading by the project it gains, loses or reads otherwise through the
// repository, the cut of the key's repository and the project's own (""). At INFRA's size the
// two defaults' cuts hold about 290,000 range rows, and on the farm at ten times INFRA's rate,
// Postgres just restarted, the fold of the own cut alone over every day of its delivery evals took
// 3.45 s: no write folds a cut's history again inside R14's 2 s for a move to reach every view. So
// the move's own commit (publishDefaults) marks each cut it moves stale (eval_figure_cuts_stale,
// with a generation each mark moves), the own cut among them, naming the evals whose rows it moved
// there, read from the stored rows (staleMarksOf): at INFRA, 6 of its 40 evals hold rows under
// legionfacts.DefaultRepo. Where a mark names only computed evals, the same commit folds their
// first paint (firstPaintOf, foldMarkedFirstPaints), so that paint reads the stored figures from
// the commit on. The refresher's pass folds the rest of them again,
// every range and day, newest end day first, batch after batch, each batch a short transaction
// (refoldCut, one function a transaction calls), and clears its mark in the transaction that
// reaches the cut's first end day unless a later move marked it again (refoldStaleCuts); a mark
// that owes a first paint, a new UTC day's, a rename's (store.markRenamedCuts), or one whose
// commit folded none, is folded first in a batch of its own. The pass that publishes the move
// or commits the settings rewrite begins the refold. A pass stops it for an owed wake, whose pass goes
// on with it, and a boot folds the first paint of every mark that owes one before it serves
// (FoldFirstPaintsAtBoot). Until a mark's first paint is folded, a read of the stale cut folds the
// day rows of the evals its mark names, which answer the same bytes; from then on it reads their
// stored figures where every stored row it reads is one the mark's generation has folded again, of
// its first paint or of the end days its refold has done, and folds their day rows otherwise
// (figuresThrough). It reads every other eval's stored figures throughout, which fold rows no move
// moved, and every other cut stays served.
//
// The writers are the refresher's writes of a project's day (writeProjectDay), its unowned-p0 row
// (WriteUnownedP0Day), its logged zero fill (fillLoggedZeros) and its rewrite of a settings change
// (rewriteSettings), and the readings write (WriteReadings); the walks fold the days they wrote at
// the end of their batch (refresh.foldWalked), the refresher's pass folds the ranges of each new
// end day (refresh.walkFigures) and again the days of the changes a write left unfolded
// (refresh.figureChanges), whose read of the change log also marks stale each cut that holds a
// stored figure of an eval a new version gave another fold key (markMovedKeys), which a register
// write of a logged eval marks in its own transaction as well (markMovedKey), and a rebuild folds
// every figure of its projects aside and flips the ones that moved (rebuild.go). Every build
// that holds this code writes them, as every build writes the day rows, and each row records the
// build that folded it (built_by): a read serves a stored row whichever build folded it, as it
// serves a day row whichever build built it, and a release whose folds differ rewrites, in its
// rebuild's flip, the rows an older build folded whose figures its folds move, and no other
// (figures_refresher.go says what a read sees meanwhile).

// FigureLengths is every range length the figures are stored for, ascending: each preset of the
// Evals tab (7, 14, 28 and 84 days) and each one day longer. A read from midnight to midnight
// covers L days ending today; the live view reads a preset from L days before the moment it
// reads to that moment, which covers L+1 days ending tomorrow at any moment but a midnight (the
// read's days, api/evals.go's evalDaysOf).
var FigureLengths = []int{7, 8, 14, 15, 28, 29, 84, 85}

// figureSpan is the longest of FigureLengths: no stored range holds a day more than figureSpan
// days before its end.
var figureSpan = slices.Max(FigureLengths)

// FigureLead is how many days after today the last stored end day lies, the day a project's
// figures stand built through once its walk is done (figuresThrough). Just after a midnight the
// live view's window ends the new tomorrow, two days after the day before's today, and a read can
// come before the pass of the new day has written the ranges ending then, so the pass of the day
// before writes them already: their last day holds no row yet, and a fold divides by a range's
// days, not by its rows.
const FigureLead = 2

// foldKeyOf is the fingerprint of what entry's definition folds its figures by (fold_key): the
// eval's id and source, and each listed series' name, fold and role, in name order: the series the
// folds read (listedAndHeadline), an unlisted one folding into no figure. A register write that
// moves one of them, or lists or unlists a series, moves it, and a read whose eval no longer has
// the key a stored row carries reads the day rows instead. The build is no part of it: two builds
// whose folds agree fold one figure, which neither then writes again.
func foldKeyOf(entry Entry) int64 {
	hash := fnv.New64a()
	write := func(value string) {
		var length [binary.MaxVarintLen64]byte
		hash.Write(length[:binary.PutUvarint(length[:], uint64(len(value)))])
		hash.Write([]byte(value))
	}
	write(entry.Definition.EvalID)
	write(entry.Definition.Source)
	series := slices.SortedFunc(slices.Values(entry.Series), func(a, b Series) int { return strings.Compare(a.Series, b.Series) })
	for _, one := range series {
		if !one.Listed {
			continue
		}
		write(one.Series)
		write(one.Agg)
		write(one.Role)
	}
	return int64(hash.Sum64())
}

// listedAndHeadline is entry's listed series, and those of them that are headline series, in
// entry's order: the series a range's figures and a day's figures fold (SeriesValuesOf).
func listedAndHeadline(entry Entry) (listed, headline []Series) {
	for _, series := range entry.Series {
		if !series.Listed {
			continue
		}
		listed = append(listed, series)
		if series.Role == RoleHeadline {
			headline = append(headline, series)
		}
	}
	return listed, headline
}

// encodeFigures is a fold's map and its via-repository counts as a figures column holds them:
// the count of series, then each series by name (its name, and its statistics: none, or their
// count and each by name, its name and either no value or the 64 bits of its value), then the
// count of series whose via-repository count is not 0 and each by name with its count. Every
// figure is its float's 64 bits, so the fold is stored bit for bit, -0 included.
func encodeFigures(folded map[string]Statistics, via map[string]int) []byte {
	b := make([]byte, 0, 256)
	appendString := func(value string) {
		b = binary.AppendUvarint(b, uint64(len(value)))
		b = append(b, value...)
	}
	names := slices.Sorted(maps.Keys(folded))
	b = binary.AppendUvarint(b, uint64(len(names)))
	for _, name := range names {
		appendString(name)
		stats := folded[name]
		if stats == nil {
			b = append(b, 0)
			continue
		}
		b = append(b, 1)
		statistics := slices.Sorted(maps.Keys(stats))
		b = binary.AppendUvarint(b, uint64(len(statistics)))
		for _, statistic := range statistics {
			appendString(statistic)
			if value := stats[statistic]; value == nil {
				b = append(b, 0)
			} else {
				b = append(b, 1)
				b = binary.LittleEndian.AppendUint64(b, math.Float64bits(*value))
			}
		}
	}
	var counted []string
	for name, count := range via {
		if count != 0 {
			counted = append(counted, name)
		}
	}
	slices.Sort(counted)
	b = binary.AppendUvarint(b, uint64(len(counted)))
	for _, name := range counted {
		appendString(name)
		b = binary.AppendVarint(b, int64(via[name]))
	}
	return b
}

// errFiguresEncoding is a figures column encodeFigures did not write.
var errFiguresEncoding = errors.New("evals: a stored figures column is not one encodeFigures writes")

// figuresDecoder reads a figures column from its front.
type figuresDecoder struct {
	b   []byte
	err error
}

func (d *figuresDecoder) uvarint() uint64 {
	value, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err, d.b = errFiguresEncoding, nil
		return 0
	}
	d.b = d.b[n:]
	return value
}

func (d *figuresDecoder) varint() int64 {
	value, n := binary.Varint(d.b)
	if n <= 0 {
		d.err, d.b = errFiguresEncoding, nil
		return 0
	}
	d.b = d.b[n:]
	return value
}

func (d *figuresDecoder) flag() bool {
	if len(d.b) == 0 {
		d.err = errFiguresEncoding
		return false
	}
	set := d.b[0] == 1
	d.b = d.b[1:]
	return set
}

func (d *figuresDecoder) text() string {
	n := d.uvarint()
	if uint64(len(d.b)) < n {
		d.err, d.b = errFiguresEncoding, nil
		return ""
	}
	value := string(d.b[:n])
	d.b = d.b[n:]
	return value
}

func (d *figuresDecoder) float() float64 {
	if len(d.b) < 8 {
		d.err, d.b = errFiguresEncoding, nil
		return 0
	}
	value := math.Float64frombits(binary.LittleEndian.Uint64(d.b))
	d.b = d.b[8:]
	return value
}

// decodeFigures is the fold's map and the via-repository counts encodeFigures wrote into b.
func decodeFigures(b []byte) (map[string]Statistics, map[string]int, error) {
	d := figuresDecoder{b: b}
	count := d.uvarint()
	folded := map[string]Statistics{}
	for range count {
		if d.err != nil {
			break
		}
		name := d.text()
		if !d.flag() {
			folded[name] = nil
			continue
		}
		statistics := d.uvarint()
		stats := Statistics{}
		for range statistics {
			if d.err != nil {
				break
			}
			statistic := d.text()
			if !d.flag() {
				stats[statistic] = nil
				continue
			}
			value := d.float()
			stats[statistic] = &value
		}
		folded[name] = stats
	}
	via := map[string]int{}
	for range d.uvarint() {
		if d.err != nil {
			break
		}
		name := d.text()
		via[name] = int(d.varint())
	}
	if d.err == nil && len(d.b) > 0 {
		d.err = errFiguresEncoding
	}
	if d.err != nil {
		return nil, nil, d.err
	}
	return folded, via, nil
}

// stampedRow is what a day's stamp reads of one of its rows: its series, its updated_at and its
// via_repository as the project reads it (viaAsRead).
type stampedRow struct {
	series    string
	updatedAt time.Time
	via       int
}

// rowsStamp is the fingerprint of one eval's listed rows of one cut, deploy repository and day
// (eval_day_figures.rows_stamp): each row's series, its updated_at to the microsecond Postgres
// keeps and its via_repository as the project reads it, in series order and then in that of the
// moments and of the counts, which it sorts rows into: a project's day can hold a row of one
// series of its own and one of an owner key's (owners.go), a cut of its default repository one of
// that repository and one stored under legionfacts.DefaultRepo, and two rows one transaction wrote
// share a moment. Every write that moves a row's value moves its updated_at (upsertDays,
// writeDays, relistDays), a row deleteStaleDays deletes leaves the day's rows, and one that moves
// nothing, such as a new build's rebuild of rows whose values stood, leaves it; and a publishing
// of the default repositories that moves which project reads an owner key's rows, or whether it
// reads them through the repository, moves the rows a project's day holds or the via_repository
// it reads of them (publishDefaults, which marks each cut it moves stale and folds its first
// paint). So the stamp moves exactly when what the day folds does, whichever build
// wrote the rows.
func rowsStamp(rows []stampedRow) int64 {
	slices.SortFunc(rows, func(a, b stampedRow) int {
		return cmp.Or(strings.Compare(a.series, b.series), a.updatedAt.Compare(b.updatedAt), cmp.Compare(a.via, b.via))
	})
	hash := fnv.New64a()
	var word [binary.MaxVarintLen64]byte
	for _, row := range rows {
		hash.Write(word[:binary.PutUvarint(word[:], uint64(len(row.series)))])
		hash.Write([]byte(row.series))
		hash.Write(binary.LittleEndian.AppendUint64(word[:0], uint64(row.updatedAt.UnixMicro())))
		hash.Write(word[:binary.PutUvarint(word[:], uint64(row.via))])
	}
	return int64(hash.Sum64())
}

// figureGroup is one eval's listed day rows of one cut and deploy repository, in the folds' order
// (sortWindowRows), and what folds them: the stored figures of every range and day of the rows are
// folded from it. parts is the rows' parts in the same order and, in a repository's cut, the zero
// part of each series of the eval each day holds no row of after that day's (withZeros): the parts
// every fold of the group reads, as SeriesValuesOf folds them.
type figureGroup struct {
	entry            Entry
	listed, headline []Series
	key              int64
	rows             []WindowRow
	parts            []measures.DayPart
}

// newFigureGroup is the group of rows, one eval's listed rows of one cut and deploy repository as
// DayWindowIn reads them, whose figures fold under key. It sorts rows into the one order
// SeriesValuesOf folds its inputs in too (sortWindowRows), so a range's figures are the bits a read
// folds whatever order either read answered the rows in. Each series whose every row holds samples
// a sort keeps bit for bit (SortableSamples) has each row's samples sorted in place, once, so every
// range of the series reads its ranks across them (measures.MergedSamples, which finds them
// sorted); a series with a -0 or a NaN keeps its rows' samples in the order they were stored, which
// the merge and sort of each range reads as a read of the rows does, in the same order. A
// repository cut's zero parts (cutZeros) are added once, here, for every range and day of the
// group: each is a part of one day alone, so the parts of a range's days are those a read of that
// range folds.
func newFigureGroup(entry Entry, key int64, rows []WindowRow) *figureGroup {
	sortWindowRows(rows)
	group := &figureGroup{entry: entry, key: key, rows: rows}
	group.listed, group.headline = listedAndHeadline(entry)
	sortable := map[string]bool{}
	for _, row := range rows {
		held, seen := sortable[row.Part.Series]
		sortable[row.Part.Series] = (held || !seen) && measures.SortableSamples(row.Part.Samples)
	}
	parts := make([]measures.DayPart, len(rows))
	for index, row := range rows {
		if sortable[row.Part.Series] {
			slices.Sort(row.Part.Samples)
		}
		parts[index] = row.Part
	}
	group.parts = withZeros(parts, cutZeros(entry.Definition, rows))
	return group
}

// span is the index range of the group's rows of the days [from, to).
func (g *figureGroup) span(from, to time.Time) (int, int) {
	first := sort.Search(len(g.rows), func(k int) bool { return !g.rows[k].Part.Day.Before(from) })
	last := sort.Search(len(g.rows), func(k int) bool { return !g.rows[k].Part.Day.Before(to) })
	return first, last
}

// partsIn is the group's parts of the days [from, to), which a fold of those days reads.
func (g *figureGroup) partsIn(from, to time.Time) []measures.DayPart {
	first := sort.Search(len(g.parts), func(k int) bool { return !g.parts[k].Day.Before(from) })
	last := sort.Search(len(g.parts), func(k int) bool { return !g.parts[k].Day.Before(to) })
	return g.parts[first:last]
}

// rangeOf is the n days ending at end, exclusive.
func rangeOf(n int, end time.Time) Range {
	return Range{From: end.AddDate(0, 0, -n), To: end}
}

// rangeFigure is the stored row of the n days ending at end: its figures and the oldest updated_at
// of the rows it folds, and false where the range holds no row of the group.
func (g *figureGroup) rangeFigure(n int, end time.Time) ([]byte, time.Time, bool) {
	r := rangeOf(n, end)
	first, last := g.span(r.From, r.To)
	if first == last {
		return nil, time.Time{}, false
	}
	folded := FoldStatistics(g.entry.Definition, g.listed, g.partsIn(r.From, r.To), r)
	via := map[string]int{}
	oldest := g.rows[first].UpdatedAt
	for index := first; index < last; index++ {
		row := &g.rows[index]
		via[row.Part.Series] += row.Part.ViaRepository
		if row.UpdatedAt.Before(oldest) {
			oldest = row.UpdatedAt
		}
	}
	return encodeFigures(folded, via), oldest, true
}

// dayFigure is the stored day row of day: the figures of each headline series that holds a part
// that day, each folded over the day's parts alone as SeriesValuesOf folds a day of its per-day
// array (foldDay), and the stamp of the day's rows; false where the day holds no row of the group.
func (g *figureGroup) dayFigure(day time.Time) ([]byte, int64, bool) {
	next := day.AddDate(0, 0, 1)
	first, last := g.span(day, next)
	if first == last {
		return nil, 0, false
	}
	stamped := make([]stampedRow, 0, last-first)
	for _, row := range g.rows[first:last] {
		stamped = append(stamped, stampedRow{series: row.Part.Series, updatedAt: row.UpdatedAt, via: row.Part.ViaRepository})
	}
	return encodeFigures(foldDay(g.entry.Definition, g.headline, g.partsIn(day, next), day), nil), rowsStamp(stamped), true
}

// foldEnds folds the group's stored figures of each end day from first through last, of evalID in
// cut, as the figures walk folds them: the range of each of FigureLengths that ends on it, each
// into ranged, and the day before it, into daily, each where it holds a row of the group
// (figureEnds.keys of endsThrough).
func (g *figureGroup) foldEnds(evalID string, cut figureCut, first, last time.Time, ranged func(rangeFigureRow), daily func(dayFigureRow)) {
	endsThrough(first, last).keys(evalID, cut, func(key rangeFigureKey) {
		if figures, oldest, held := g.rangeFigure(key.days, key.end); held {
			ranged(rangeFigureRow{rangeFigureKey: key, key: g.key, figures: figures, oldest: oldest})
		}
	}, func(key dayFigureKey) {
		if figures, stamp, held := g.dayFigure(key.day); held {
			daily(dayFigureRow{dayFigureKey: key, key: g.key, figures: figures, stamp: stamp})
		}
	})
}

// figureEnds is what one fold of a project's stored figures folds of each eval's rows of each cut
// it reads (buildFigureEnds, figureGroup.foldEnds): the ranges of every length of FigureLengths
// that end on each of its end days, from first through last, every one of them where picked is nil
// and only those picked names, in day order, where it is not; and the day row of each day of days.
// A batch of the figures walk or of a stale cut's refold folds each end day of a run of them and
// the day before each (endsThrough), and a stale cut's first-paint batch the end days and days a
// first paint reads (firstPaintOf).
type figureEnds struct {
	first, last time.Time
	picked      []time.Time
	days        Range
}

// endsThrough is every end day from first through last, and the day before each.
func endsThrough(first, last time.Time) figureEnds {
	return figureEnds{first: first, last: last, days: Range{From: first.AddDate(0, 0, -1), To: last}}
}

// endDays is f's end days, in day order.
func (f figureEnds) endDays() iter.Seq[time.Time] {
	return func(yield func(time.Time) bool) {
		if f.picked != nil {
			for _, end := range f.picked {
				if !yield(end) {
					return
				}
			}
			return
		}
		for end := f.first; !end.After(f.last); end = end.AddDate(0, 0, 1) {
			if !yield(end) {
				return
			}
		}
	}
}

// holds reports whether f folds the ranges that end on end.
func (f figureEnds) holds(end time.Time) bool {
	if f.picked != nil {
		_, found := slices.BinarySearchFunc(f.picked, end, time.Time.Compare)
		return found
	}
	return !end.Before(f.first) && !end.After(f.last)
}

// keys calls ranged with the key of each range f names of evalID in cut, for each of its end days
// in day order the range of each of FigureLengths that ends on it, and then daily with the key of
// each of its days, in day order: what a fold of f folds of one eval's group of rows
// (figureGroup.foldEnds, figurePlan.addEnds).
func (f figureEnds) keys(evalID string, cut figureCut, ranged func(rangeFigureKey), daily func(dayFigureKey)) {
	for end := range f.endDays() {
		for _, n := range FigureLengths {
			ranged(rangeFigureKey{repo: cut.repo, days: n, end: end, eval: evalID, deployRepo: cut.deployRepo})
		}
	}
	for day := f.days.From; day.Before(f.days.To); day = day.AddDate(0, 0, 1) {
		daily(dayFigureKey{repo: cut.repo, day: day, eval: evalID, deployRepo: cut.deployRepo})
	}
}

// rows is the days whose rows f's folds read: every day one of its ranges spans, and its days.
func (f figureEnds) rows() Range {
	read := Range{From: f.first.AddDate(0, 0, -figureSpan), To: f.last}
	if f.days.From.Before(read.From) {
		read.From = f.days.From
	}
	if f.days.To.After(read.To) {
		read.To = f.days.To
	}
	return read
}

// figureScope is what one write of a project's stored figures writes under: the build that folds
// them, which each row it writes records (built_by), and through, the last end day any range of the
// project's figures has been folded through: the end day eval_figures_built says they are built
// through, or the farthest any build's figures walk has come (refresh.walkFigures), whichever is
// later. Every range ending on or before it may be stored, whichever build folded it, so every
// write keeps each of them true to its rows; no range ending after it is written, nor the day row
// of a day on or after it: those are the walks' to fold, from the rows that stand when they do.
type figureScope struct {
	build   string
	project string
	through time.Time
}

// readFigureScope reads, in q, how far project's stored figures are folded (figureScope), for a
// write of them by build. ok is false where no build's figures walk of the project has begun and
// none has built them, and a write of the day rows then writes no figure: the walk, once it
// begins, folds them from the rows as they then stand.
func readFigureScope(ctx context.Context, q delivery.Querier, build, project string) (figureScope, bool, error) {
	var through *time.Time
	if err := q.QueryRow(ctx, `
		select greatest(
			(select through from eval_figures_built where project_key = $2),
			(select max(next_day) - 1 from eval_days_walks where family = $1 and project_key = $2))
	`, walkFigures, project).Scan(&through); err != nil {
		return figureScope{}, false, fmt.Errorf("read how far the stored figures of %s are folded: %w", project, err)
	}
	if through == nil {
		return figureScope{}, false, nil
	}
	return figureScope{build: build, project: project, through: measures.Midnight(*through)}, true, nil
}

// figureScopeReader answers a read, in q, of each project's figureScope for a write by build
// (readFigureScope) that reads each project's once: its first call for a project reads it, and each
// later call answers what that read answered, nil where the project's figures are folded nowhere.
func figureScopeReader(ctx context.Context, q delivery.Querier, build string) func(project string) (*figureScope, error) {
	scopes := map[string]*figureScope{}
	return func(project string) (*figureScope, error) {
		if scope, read := scopes[project]; read {
			return scope, nil
		}
		folded, ok, err := readFigureScope(ctx, q, build, project)
		if err != nil {
			return nil, err
		}
		var scope *figureScope
		if ok {
			scope = &folded
		}
		scopes[project] = scope
		return scope, nil
	}
}

// readEntries reads, in q, project's evals as Current reads each, those of ids where ids is not
// nil: the newest definition of each and the newest version of each of its series (entriesOf).
// An explicit list of ids is read in chunks bounded by statementBytes.
func readEntries(ctx context.Context, q delivery.Querier, project string, ids []string) ([]Entry, error) {
	read := func(filter string, args ...any) ([]Definition, error) {
		rows, err := q.Query(ctx, `select `+definitionColumns+` from evals_current where `+filter, args...)
		if err != nil {
			return nil, fmt.Errorf("read the evals of %s: %w", project, err)
		}
		found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Definition, error) { return scanDefinition(row) })
		if err != nil {
			return nil, fmt.Errorf("read the evals of %s: %w", project, err)
		}
		return found, nil
	}
	var definitions []Definition
	if ids == nil {
		var err error
		if definitions, err = read(`project_key = $1`, project); err != nil {
			return nil, err
		}
	} else {
		ids = slices.Compact(slices.Sorted(slices.Values(ids)))
		for chunk := range statementChunks(ids, paramBytes(1, project), textBytes) {
			found, err := read(`project_key = $1 and eval_id = any($2)`, project, chunk)
			if err != nil {
				return nil, err
			}
			definitions = append(definitions, found...)
		}
	}
	slices.SortFunc(definitions, func(a, b Definition) int { return strings.Compare(a.Slug, b.Slug) })
	return entriesOf(ctx, q, definitions, project)
}

// daySpan is the days first to last of one eval, both included, whose figures a write may have
// moved.
type daySpan struct {
	first, last time.Time
}

// widen is s widened to hold day; the zero span holds day alone.
func (s daySpan) widen(day time.Time) daySpan {
	if s.first.IsZero() || day.Before(s.first) {
		s.first = day
	}
	if s.last.IsZero() || day.After(s.last) {
		s.last = day
	}
	return s
}

// daySpans is, by eval id, the days a write wrote of each eval, or may have moved, as runs of days
// (daySpan) in day order, no two runs of one eval overlapping or touching (add). A write of a
// months-old day and of today holds those two days, where one span from the first to the last
// would have its fold read the stamps and the stored rows of every day between, in the write's
// transaction (movedFigureDays; check k29 of dispatch://INFRA-2916).
type daySpans map[string][]daySpan

// add adds the days first to last of evalID to s, joined with each run of them they overlap or
// touch.
func (s daySpans) add(evalID string, first, last time.Time) {
	runs := s[evalID]
	before, after := first.AddDate(0, 0, -1), last.AddDate(0, 0, 1)
	// The runs from the first that ends on or after the day before first, to the first that
	// begins after the day after last, are those the days join.
	from := sort.Search(len(runs), func(k int) bool { return !runs[k].last.Before(before) })
	to := from
	for to < len(runs) && !runs[to].first.After(after) {
		to++
	}
	joined := daySpan{first: first, last: last}
	if from < to {
		if runs[from].first.Before(first) {
			joined.first = runs[from].first
		}
		if runs[to-1].last.After(last) {
			joined.last = runs[to-1].last
		}
	}
	s[evalID] = slices.Replace(runs, from, to, joined)
}

// sets is s as the sets of spans movedFigureDays takes, each one span of each eval it names
// (spansOf): the k-th holds the k-th run of each eval that has more than k.
func (s daySpans) sets() []map[string]daySpan {
	var sets []map[string]daySpan
	for evalID, runs := range s {
		for k, run := range runs {
			if k == len(sets) {
				sets = append(sets, map[string]daySpan{})
			}
			sets[k][evalID] = run
		}
	}
	return sets
}

// writtenSpans is what a write wrote of the day rows whose stored figures it folds again in its
// own transaction (foldFiguresInTx): by the key whose rows it wrote (a project, or an owner key),
// the days of each of that key's evals (daySpans).
type writtenSpans map[string]daySpans

// add adds the days first to last of key's eval evalID to w (daySpans.add).
func (w writtenSpans) add(key, evalID string, first, last time.Time) {
	if w[key] == nil {
		w[key] = daySpans{}
	}
	w[key].add(evalID, first, last)
}

// sets is w as the sets of spans foldFiguresInTx takes, by key: the k-th holds each key's k-th set
// (daySpans.sets).
func (w writtenSpans) sets() []map[string]map[string]daySpan {
	var sets []map[string]map[string]daySpan
	for key, spans := range w {
		for k, set := range spans.sets() {
			if k == len(sets) {
				sets = append(sets, map[string]map[string]daySpan{})
			}
			sets[k][key] = set
		}
	}
	return sets
}

// figureCut is one fold a read of a project's stored figures looks up: its rows of repo, the empty
// string for the project's own rows (repo "") and a repository's name for the rows a read that
// names that one repository folds (cutOf), of one deploy repository ("" for the evals no deploy
// repository moves, a repository's name or events repository for its own rows, or unionDeployRepo
// "*" for the fold of all the project's currently recorded deploy repositories; deploy_repos.go).
type figureCut struct {
	repo, deployRepo string
}

// compareFigureCuts orders cuts by repository and then by deploy repository, each by its bytes.
func compareFigureCuts(a, b figureCut) int {
	return cmp.Or(strings.Compare(a.repo, b.repo), strings.Compare(a.deployRepo, b.deployRepo))
}

// repos is the repositories a read of the cut names (DayWindowIn's repos): none for the project's
// own rows, else the cut's one.
func (c figureCut) repos() []string {
	if c.repo == "" {
		return nil
	}
	return []string{c.repo}
}

// scanFigureCut reads one cut of a read that answers a repository and then a deploy repository
// (readFigureCuts, and the cuts of a rebuild's staged rows in stageFigures). It scans in a
// statement of its own and then returns the cut: of a return that names the cut beside the call
// that fills it, the Go spec leaves open whether the cut is read before or after the call, so such
// a return may answer the cut unfilled.
func scanFigureCut(row pgx.CollectableRow) (figureCut, error) {
	var cut figureCut
	err := row.Scan(&cut.repo, &cut.deployRepo)
	return cut, err
}

// cutOf is, in SQL, the repository of the cut whose stored figures fold a day row (alias) of an
// owner key or of the project the expression project names, as that project reads it: the row's
// repo, the empty string for its all-repositories rows and a repository's name for that
// repository's; and for a row stored under legionfacts.DefaultRepo (a Legion row of an issue whose
// chain names no repository) the repository the project publishes as its default
// (eval_project_repos) where the row is the project's own, and null for an owner key's such row and
// where the project publishes none, which no cut folds. It is repoCut's rule read the other way: a
// read that names the one repository R folds exactly the rows whose cut is R, so no cut of
// legionfacts.DefaultRepo itself is stored, and a move of the default moves those rows' cut.
func cutOf(alias, project string) string {
	return `case when ` + alias + `.repo <> '` + legionfacts.DefaultRepo + `' then ` + alias + `.repo when ` + alias + `.project_key = ` + project +
		` then (select p.repo from eval_project_repos p where p.project_key = ` + project + `) end`
}

// figureGroupKey is one eval's rows of one cut.
type figureGroupKey struct {
	eval string
	figureCut
}

// evalSpan is one run of days of one eval that movedFigureDays reads (spansOf): the eval's id, the
// writer whose rows its fold reads (Definition.writer), and the days.
type evalSpan struct {
	eval, writer string
	daySpan
}

// movedFigureDays reads, in q, which days of spans (each by eval id, each eval one of entries)
// hold figures that no longer fold their rows: each eval, cut and day whose listed rows' stamp
// (rowsStamp, of the rows of the cut, the project's and its owner keys', each with its
// via_repository as the project reads it, and each row in the cut cutOf names) is not the one the
// day's stored row of the cut recorded, which holds rows and no stored row or a stored row and no
// rows, or whose stored row was folded under another fold key than the eval's definition now has
// (a register write). For an eval keyed by the deploy repository (KeyedByDeployRepo), each row
// whose deploy repository is among the project's currently recorded deploy repositories
// (DeployReposIn) is also stamped into the project's union cut (`deploy_repo` `*`, unionDeployRepo),
// beside its own deploy repository's cut. Which build folded a stored row is no part of the test:
// a figure whose rows stand is the fold every build of these folds answers. A day on or after
// scope.through is the figures walks', and none of it is read. The days of an eval are those any
// of spans names, each read once (spansOf), and none between two spans of it that none names.
//
// Each of its two reads is one range of an index for each of those spans, every cut's rows in it:
// eval_days_stamps (project_key, eval_id, writer, day) of the rows' stamps, one for each key the
// project reads (eval_project_owners), and eval_day_figures_eval_day of the stored day rows. Each
// lateral ends in `offset 0`, which keeps Postgres from pulling it up into a join: pulled up, the
// stamps' read took the estimate of the view's widest arm, refused a nested loop into eval_days and
// read every listed row of every project (181 to 304 ms on the farm to answer the 75 rows of one
// closed issue's day, dispatch://INFRA-2916, k29 at 4dbab72a), and the stored rows' read hashed the
// spans against every stored day row of the project. The spans go in chunks of at most
// statementBytes (statementChunks), each read by both statements.
func movedFigureDays(ctx context.Context, q delivery.Querier, scope figureScope, entries map[string]Entry, spans ...map[string]daySpan) (map[figureGroupKey]map[time.Time]bool, error) {
	var named []evalSpan
	hasKeyed := false
	for _, evalID := range spannedEvals(spans) {
		entry, held := entries[evalID]
		if !held {
			continue
		}
		if KeyedByDeployRepo(entry.Definition) {
			hasKeyed = true
		}
		for _, span := range spansOf(spans, evalID, scope.through.AddDate(0, 0, -1)) {
			named = append(named, evalSpan{eval: evalID, writer: entry.Definition.writer(), daySpan: span})
		}
	}
	moved := map[figureGroupKey]map[time.Time]bool{}
	if len(named) == 0 {
		return moved, nil
	}
	var deployRepos *DeployRepos
	if hasKeyed {
		var err error
		if deployRepos, err = DeployReposIn(ctx, q); err != nil {
			return nil, err
		}
	}
	type dayKey struct {
		group figureGroupKey
		day   time.Time
	}
	type storedDay struct{ key, stamp int64 }
	stamped, stored := map[dayKey][]stampedRow{}, map[dayKey]storedDay{}
	var key dayKey
	var row stampedRow
	var day storedDay
	size := func(span *evalSpan) int { return 4*4 + len(span.eval) + len(span.writer) + 2*8 }
	for chunk := range statementChunks(named, paramBytes(4, scope.project), size) {
		evalIDs, writers := make([]string, len(chunk)), make([]string, len(chunk))
		firsts, lasts := make([]time.Time, len(chunk)), make([]time.Time, len(chunk))
		for index, span := range chunk {
			evalIDs[index], writers[index], firsts[index], lasts[index] = span.eval, span.writer, span.first, span.last
		}
		rows, err := q.Query(ctx, `
			select given.eval_id, d.deploy_repo, cut.repo, d.day, d.series, d.updated_at, `+viaAsRead("d.via_repository")+`
			from unnest($2::text[], $3::text[], $4::date[], $5::date[]) as given(eval_id, writer, first_day, last_day)
			cross join eval_project_owners o
			cross join lateral (
				select d.project_key, d.deploy_repo, d.repo, d.day, d.series, d.updated_at, d.via_repository
				from eval_days d
				where d.project_key = o.owner and d.eval_id = `+evalIDAs("o.owner", "given.eval_id", "$1")+`
					and d.writer = given.writer and d.day >= given.first_day and d.day <= given.last_day and d.listed
				offset 0
			) d
			cross join lateral (select `+cutOf("d", "$1")+` as repo) cut
			where o.project_key = $1 and cut.repo is not null
		`, scope.project, evalIDs, writers, firsts, lasts)
		if err != nil {
			return nil, fmt.Errorf("read the stamps of %s's day rows: %w", scope.project, err)
		}
		if _, err := pgx.ForEachRow(rows, []any{&key.group.eval, &key.group.deployRepo, &key.group.repo, &key.day, &row.series, &row.updatedAt, &row.via}, func() error {
			key.day = key.day.UTC()
			stamped[key] = append(stamped[key], row)
			if deployRepos.hasRepo(scope.project, key.group.deployRepo) && KeyedByDeployRepo(entries[key.group.eval].Definition) {
				union := key
				union.group.deployRepo = unionDeployRepo
				stamped[union] = append(stamped[union], row)
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the stamps of %s's day rows: %w", scope.project, err)
		}
		rows, err = q.Query(ctx, `
			select f.eval_id, f.deploy_repo, f.repo, f.day, f.fold_key, f.rows_stamp
			from unnest($2::text[], $3::date[], $4::date[]) as given(eval_id, first_day, last_day)
			cross join lateral (
				select f.eval_id, f.deploy_repo, f.repo, f.day, f.fold_key, f.rows_stamp
				from eval_day_figures f
				where f.project_key = $1 and f.eval_id = given.eval_id and f.day >= given.first_day and f.day <= given.last_day
				offset 0
			) f
		`, scope.project, evalIDs, firsts, lasts)
		if err != nil {
			return nil, fmt.Errorf("read the stored day figures of %s: %w", scope.project, err)
		}
		if _, err := pgx.ForEachRow(rows, []any{&key.group.eval, &key.group.deployRepo, &key.group.repo, &key.day, &day.key, &day.stamp}, func() error {
			key.day = key.day.UTC()
			stored[key] = day
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the stored day figures of %s: %w", scope.project, err)
		}
	}
	keys := map[string]int64{}
	note := func(at dayKey) {
		if moved[at.group] == nil {
			moved[at.group] = map[time.Time]bool{}
		}
		moved[at.group][at.day] = true
	}
	for at, held := range stamped {
		if _, done := keys[at.group.eval]; !done {
			keys[at.group.eval] = foldKeyOf(entries[at.group.eval])
		}
		if figure, found := stored[at]; !found || figure.key != keys[at.group.eval] || figure.stamp != rowsStamp(held) {
			note(at)
		}
	}
	for at := range stored {
		if _, found := stamped[at]; !found {
			note(at)
		}
	}
	return moved, nil
}

// spannedEvals is every eval id one of spans names, in byte order.
func spannedEvals(spans []map[string]daySpan) []string {
	ids := map[string]bool{}
	for _, one := range spans {
		for evalID := range one {
			ids[evalID] = true
		}
	}
	return slices.Sorted(maps.Keys(ids))
}

// spansOf is the spans of evalID that spans name, each cut to end by last, in day order, with those
// that overlap or touch joined into one: every day one of them names, once, and no day between two
// of them that none names. A span cut to end before it begins names no day, and is none of them.
func spansOf(spans []map[string]daySpan, evalID string, last time.Time) []daySpan {
	var named []daySpan
	for _, one := range spans {
		span, held := one[evalID]
		if !held {
			continue
		}
		if span.last.After(last) {
			span.last = last
		}
		if !span.first.After(span.last) {
			named = append(named, span)
		}
	}
	slices.SortFunc(named, func(a, b daySpan) int { return a.first.Compare(b.first) })
	joined := named[:0]
	for _, span := range named {
		if n := len(joined); n > 0 && !span.first.After(joined[n-1].last.AddDate(0, 0, 1)) {
			if span.last.After(joined[n-1].last) {
				joined[n-1].last = span.last
			}
			continue
		}
		joined = append(joined, span)
	}
	return joined
}

// figureWrites is what one fold writes of a project's stored figures: the range and day rows to
// upsert where they moved, and the ranges and days that hold no row any more, whose stored rows
// it deletes.
type figureWrites struct {
	ranges     []rangeFigureRow
	goneRanges []rangeFigureKey
	days       []dayFigureRow
	goneDays   []dayFigureKey
}

// rangeFigureKey is one range row's key within its project; rangeFigureRow the row.
type rangeFigureKey struct {
	repo       string
	days       int
	end        time.Time
	eval       string
	deployRepo string
}

type rangeFigureRow struct {
	rangeFigureKey
	key     int64
	figures []byte
	oldest  time.Time
}

// dayFigureKey is one day row's key within its project; dayFigureRow the row.
type dayFigureKey struct {
	repo       string
	day        time.Time
	eval       string
	deployRepo string
}

type dayFigureRow struct {
	dayFigureKey
	key     int64
	figures []byte
	stamp   int64
}

// movedSpans is, by eval, the span of the days moved names of the eval in any cut: the days a
// write's refold reads the eval's rows around (readFoldGroups).
func movedSpans(moved map[figureGroupKey]map[time.Time]bool) map[string]daySpan {
	spans := map[string]daySpan{}
	for group, days := range moved {
		for day := range days {
			spans[group.eval] = spans[group.eval].widen(day)
		}
	}
	return spans
}

// figureFoldWorkers is the usual limit on goroutines folding ranges and days in one transaction.
// The settings rewrite folds its first paints on more (withFoldWorkers), since its views wait for
// its commit. Neither limit exceeds the CPUs the process may use.
const figureFoldWorkers = 4

// figurePlan is what one write of a project's stored figures folds of the days it moved, every row
// of it read before any is folded (readFigurePlan): each range and day of an eval's group of rows
// to fold, the ranges in the byte order of their keys and the days in theirs (sortByKey), and the
// ranges and days of the evals that hold no row the reads span, whose stored rows the write
// deletes. buildFigureEnds folds each cut it reads in one too, its ranges and days in the order it
// adds them (addEnds) and no gone range or day: it deletes the stored rows no fold of it built.
type figurePlan struct {
	ranges     []rangeTarget
	days       []dayTarget
	goneRanges []rangeFigureKey
	goneDays   []dayFigureKey
}

// rangeTarget is one range of group's rows to fold and, once folded (figurePlan.fold), its row,
// held false where the range holds no row of the group; dayTarget is one day's.
type rangeTarget struct {
	rangeFigureRow
	group *figureGroup
	held  bool
}

type dayTarget struct {
	dayFigureRow
	group *figureGroup
	held  bool
}

// readFigurePlan reads, in q, what a write of the stored figures of each day moved names (by eval
// and cut) folds, with every range whose span holds one, up to scope.through: one read of the
// moved evals' rows of every cut over every day those ranges span (readFoldGroups), and each range
// and day to fold once, however many moved days it holds (add).
func readFigurePlan(ctx context.Context, q delivery.Querier, scope figureScope, entries map[string]Entry, moved map[figureGroupKey]map[time.Time]bool) (figurePlan, error) {
	var plan figurePlan
	groups, err := readFoldGroups(ctx, q, scope, entries, movedSpans(moved))
	if err != nil {
		return figurePlan{}, err
	}
	for key, days := range moved {
		plan.add(groups[key], key, slices.SortedFunc(maps.Keys(days), time.Time.Compare), scope.through)
	}
	plan.sortByKey()
	return plan, nil
}

// readFoldGroups reads, in q, the listed rows of each eval spans names (each by eval id, an eval of
// entries) of every cut of scope's project, over the days the ranges that hold a day of its span
// span, up to scope.through: the rows readFigureGroups reads of each cut, the same columns as
// DayWindowIn answers them with no compare range and at most one repository named, and answers
// each eval's group of each cut and deploy repository (figureGroupKey), none for a cut the eval
// holds no row of there. A row's cut is the one cutOf names, so a row of the project's own stored
// under legionfacts.DefaultRepo is its published default's, as a read of that repository reads it
// (repoCut); the rows of a deploy repository are of its cut where the eval is keyed by it
// (KeyedByDeployRepo) and of none otherwise. Each eval and key the project reads
// (eval_project_owners) is one range of eval_days_stamps (project, eval, writer, day), as
// movedFigureDays reads their stamps, where a read of a cut reads every listed row of the cut's
// days through eval_days_window (project, repo, day), every eval's: a production rule's refold read
// 10,792 rows of two cuts in 184 to 274 ms on the farm at ten times INFRA's size
// (dispatch://INFRA-2916). The evals go in statements of at most statementBytes (statementChunks),
// each with the pairs (eval, series) whose samples its folds read (readsSamples).
func readFoldGroups(ctx context.Context, q delivery.Querier, scope figureScope, entries map[string]Entry, spans map[string]daySpan) (map[figureGroupKey]*figureGroup, error) {
	type given struct {
		entry       Entry
		first, last time.Time
	}
	var named []given
	hasKeyed := false
	for _, evalID := range slices.Sorted(maps.Keys(spans)) {
		entry, held := entries[evalID]
		if !held {
			continue
		}
		if KeyedByDeployRepo(entry.Definition) {
			hasKeyed = true
		}
		span := spans[evalID]
		first, last := span.first.AddDate(0, 0, 1-figureSpan), span.last.AddDate(0, 0, figureSpan-1)
		if !last.Before(scope.through) {
			last = scope.through.AddDate(0, 0, -1)
		}
		if !last.Before(first) {
			named = append(named, given{entry: entry, first: first, last: last})
		}
	}
	var deployRepos *DeployRepos
	if hasKeyed {
		var err error
		if deployRepos, err = DeployReposIn(ctx, q); err != nil {
			return nil, err
		}
	}
	multiDeploy := len(deployRepos.Of(scope.project)) > 1
	byGroup := map[figureGroupKey][]WindowRow{}
	size := func(one *given) int {
		return 4*4 + len(one.entry.Definition.EvalID) + len(one.entry.Definition.writer()) + 2*8 + windowEntryBytes(&one.entry, false)
	}
	for chunk := range statementChunks(named, paramBytes(6, scope.project), size) {
		evalIDs, writers := make([]string, len(chunk)), make([]string, len(chunk))
		firsts, lasts := make([]time.Time, len(chunk)), make([]time.Time, len(chunk))
		var sampledEvals, sampledSeries []string
		for index, one := range chunk {
			evalIDs[index], writers[index], firsts[index], lasts[index] = one.entry.Definition.EvalID, one.entry.Definition.writer(), one.first, one.last
			for _, series := range one.entry.Series {
				if readsSamples(one.entry.Definition, series) {
					sampledEvals, sampledSeries = append(sampledEvals, one.entry.Definition.EvalID), append(sampledSeries, series.Series)
				}
			}
		}
		rows, err := q.Query(ctx, `
			select given.eval_id, d.deploy_repo, cut.repo, d.series, d.day, d.n, d.sum, d.num, d.den, d.max,
				case when (given.eval_id, d.series) in (select * from unnest($6::text[], $7::text[])) then d.samples end,
				d.last_value, d.last_at, nullif(d.counts, '{}'), `+viaAsRead("d.via_repository")+`, d.updated_at, o.owner, d.repo
			from unnest($2::text[], $3::text[], $4::date[], $5::date[]) as given(eval_id, writer, first_day, last_day)
			cross join eval_project_owners o
			cross join lateral (
				select d.project_key, d.deploy_repo, d.repo, d.series, d.day, d.n, d.sum, d.num, d.den, d.max, d.samples,
					d.last_value, d.last_at, d.counts, d.via_repository, d.updated_at
				from eval_days d
				where d.project_key = o.owner and d.eval_id = `+evalIDAs("o.owner", "given.eval_id", "$1")+`
					and d.writer = given.writer and d.day >= given.first_day and d.day <= given.last_day and d.listed
				offset 0
			) d
			cross join lateral (select `+cutOf("d", "$1")+` as repo) cut
			where o.project_key = $1 and cut.repo is not null
		`, scope.project, evalIDs, writers, firsts, lasts, sampledEvals, sampledSeries)
		if err != nil {
			return nil, fmt.Errorf("read the day rows of %s's moved figures: %w", scope.project, err)
		}
		read := 0
		var key figureGroupKey
		var row WindowRow
		part := &row.Part
		if _, err := pgx.ForEachRow(rows, []any{&key.eval, &key.deployRepo, &key.repo, &part.Series, &part.Day, &part.N, &part.Sum, &part.Num,
			&part.Den, &part.Max, &part.Samples, &part.LastValue, &part.LastAt, &part.Counts, &part.ViaRepository, &row.UpdatedAt, &row.owner, &row.repo}, func() error {
			row.Range, row.EvalID, row.UpdatedAt = RangeWindow, key.eval, row.UpdatedAt.UTC()
			utcPart(part)
			byGroup[key] = append(byGroup[key], row)
			if deployRepos.hasRepo(scope.project, key.deployRepo) && KeyedByDeployRepo(entries[key.eval].Definition) {
				union := key
				union.deployRepo = unionDeployRepo
				unionRow := row
				if multiDeploy {
					unionRow.deployRepo = key.deployRepo
				}
				byGroup[union] = append(byGroup[union], unionRow)
			}
			row, read = WindowRow{}, read+1
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the day rows of %s's moved figures: %w", scope.project, err)
		}
		if times := figureTimesIn(ctx); times != nil {
			times.reads++
			times.rows += read
		}
	}
	groups := make(map[figureGroupKey]*figureGroup, len(byGroup))
	for key, held := range byGroup {
		entry := entries[key.eval]
		if KeyedByDeployRepo(entry.Definition) == (key.deployRepo != "") {
			groups[key] = newFigureGroup(entry, foldKeyOf(entry), held)
		}
	}
	return groups, nil
}

// add adds to p the stored row of each of days, days of group's rows of key in day order, and of
// each range of those rows whose span holds one of days and that ends by through, each once, or
// its deletion where group is nil (the eval holds no row the read spans). The ranges of length n
// whose span holds a day end on each day from the day after it through n days after it, and add
// adds, for each length, the union of those runs of end days: it walks the days in order and adds
// each day's run from where the run of the day before stopped, where that is later than the run's
// first day. Every end day before it is one an earlier day's run added, since that run began no
// later, so each end day is added once and no set of the keys is kept. A map of every day's every
// (length, end day) pair took a write for each: 1.9 million writes for the 64,640 ranges of a
// settings rewrite's 200 days of 40 groups, whose plan, its sort included, took 350-365 ms on the
// farm where this takes 86-92 ms, the two timed in turn in one process
// (BenchmarkTheFigurePlanOfASettingsRewrite).
func (p *figurePlan) add(group *figureGroup, key figureGroupKey, days []time.Time, through time.Time) {
	for _, n := range FigureLengths {
		// next is where the run of the day before stopped: no end day before it is left to add.
		var next time.Time
		for _, day := range days {
			end, last := day.AddDate(0, 0, 1), day.AddDate(0, 0, n)
			if end.Before(next) {
				end = next
			}
			for ; !end.After(last) && !end.After(through); end = end.AddDate(0, 0, 1) {
				target := rangeFigureKey{repo: key.repo, days: n, end: end, eval: key.eval, deployRepo: key.deployRepo}
				if group == nil {
					p.goneRanges = append(p.goneRanges, target)
				} else {
					p.ranges = append(p.ranges, rangeTarget{rangeFigureRow: rangeFigureRow{rangeFigureKey: target, key: group.key}, group: group})
				}
			}
			next = end
		}
	}
	for _, day := range days {
		target := dayFigureKey{repo: key.repo, day: day, eval: key.eval, deployRepo: key.deployRepo}
		if group == nil {
			p.goneDays = append(p.goneDays, target)
		} else {
			p.days = append(p.days, dayTarget{dayFigureRow: dayFigureRow{dayFigureKey: target, key: group.key}, group: group})
		}
	}
}

// addEnds adds to p each range and day that ends names of group, the rows of evalID in cut
// (figureEnds.keys), to fold.
func (p *figurePlan) addEnds(group *figureGroup, evalID string, cut figureCut, ends figureEnds) {
	ends.keys(evalID, cut, func(key rangeFigureKey) {
		p.ranges = append(p.ranges, rangeTarget{rangeFigureRow: rangeFigureRow{rangeFigureKey: key, key: group.key}, group: group})
	}, func(key dayFigureKey) {
		p.days = append(p.days, dayTarget{dayFigureRow: dayFigureRow{dayFigureKey: key, key: group.key}, group: group})
	})
}

// sortByKey sorts p's ranges and days in the byte order of their keys, the order a write folds and
// sends them in.
func (p *figurePlan) sortByKey() {
	slices.SortFunc(p.ranges, func(a, b rangeTarget) int { return compareRangeKeys(a.rangeFigureKey, b.rangeFigureKey) })
	slices.SortFunc(p.days, func(a, b dayTarget) int { return compareDayFigureKeys(a.dayFigureKey, b.dayFigureKey) })
}

// fold folds the index-th of p's ranges and then of its days into its own target. The folds of one
// group, on as many goroutines as fold them, share it, which newFigureGroup sorted before any of
// them began, since none writes into its rows or parts: rangeFigure and dayFigure read them, and a
// day's stamp sorts a list of its own (rowsStamp); FoldStatistics' folds add the parts up
// (measures.FoldSeries) and merge their samples (measures.MergedSamples), which reads the parts'
// lists as they stand where each ascends and otherwise merges and sorts a list of its own, and a
// quantile selects its ranks across those lists (SortedSamples' Quantile, At and kth) with indexes
// of its own; and each fold writes only the statistics it makes (writable) and its own target.
func (p *figurePlan) fold(index int) {
	if index < len(p.ranges) {
		target := &p.ranges[index]
		target.figures, target.oldest, target.held = target.group.rangeFigure(target.days, target.end)
		return
	}
	target := &p.days[index-len(p.ranges)]
	target.figures, target.stamp, target.held = target.group.dayFigure(target.day)
}

// rangeChunkLeast is the fewest ranges a chunk of a write's ranges holds before the end of a
// (repository, length) prefix of their keys closes it (rangeChunks); statementRows still closes a
// chunk, and statementBytes still cuts the statements its upsert sends (rangeUpserts). A write of
// a few ranges, a reading's or the refresher's day, so folds them in one chunk and upserts them in
// one statement, rather than in a statement a prefix of a few rows each, each its own round trip in
// the write's transaction with no fold of a later chunk to overlap.
const rangeChunkLeast = 1024

// rangeChunks is where each chunk of ranges, in the byte order of their keys, ends that a write
// folds and then upserts while it folds the chunks after it (figurePlan.foldAndWrite): each chunk
// closed at the end of a (repository, length) prefix of the keys once it holds at least least
// ranges, or at limit ranges.
func rangeChunks(ranges []rangeTarget, least, limit int) []int {
	var ends []int
	for from, index := 0, 1; index <= len(ranges); index++ {
		if index == len(ranges) || index-from == limit ||
			index-from >= least && (ranges[index].repo != ranges[index-1].repo || ranges[index].days != ranges[index-1].days) {
			ends = append(ends, index)
			from = index
		}
	}
	return ends
}

// foldAndWrite folds p's ranges and days in the byte order of their keys on up to workers
// goroutines at once, and writes them in tx as writeFigures writes a whole fold, while they are
// folded: each chunk of the ranges (rangeChunks) is upserted once its folds are done, while the
// workers fold the chunks after it and then the days (foldInChunks), and once every range is
// upserted the gone ranges, the days and the gone days follow (writeAfterRanges). So the range
// upserts take the rows writeFigures would take, in its order, cut at other boundaries. The first
// error of a statement ends the folds, and is returned.
func (p *figurePlan) foldAndWrite(ctx context.Context, tx pgx.Tx, scope figureScope, workers int) error {
	ends := rangeChunks(p.ranges, rangeChunkLeast, statementRows)
	writes := figureWrites{goneRanges: p.goneRanges, goneDays: p.goneDays}
	held := make([]rangeFigureRow, 0, min(len(p.ranges), statementRows))
	from := 0
	if err := foldInChunks(len(p.ranges)+len(p.days), workers, p.fold, ends, func(chunk int) error {
		held = held[:0]
		for index := from; index < ends[chunk]; index++ {
			if target := &p.ranges[index]; target.held {
				held = append(held, target.rangeFigureRow)
			} else {
				writes.goneRanges = append(writes.goneRanges, target.rangeFigureKey)
			}
		}
		from = ends[chunk]
		return upsertRangeFigures(ctx, tx, scope, held)
	}); err != nil {
		return err
	}
	for index := range p.days {
		if target := &p.days[index]; target.held {
			writes.days = append(writes.days, target.dayFigureRow)
		} else {
			writes.goneDays = append(writes.goneDays, target.dayFigureKey)
		}
	}
	writes.sortByKey()
	return writeAfterRanges(ctx, tx, scope, writes)
}

// foldInChunks runs fold of each index below n on up to workers goroutines at once, each taking the
// next index as it finishes one, since ranges differ in length and their folds in cost, and calls
// send of each chunk on the calling goroutine, in order, once every fold of it is done (chunk c is
// the indexes from ends[c-1], or 0, to ends[c]; ends ascend to at most n), while the workers fold
// the indexes after it: a transaction's statements run one at a time on its one connection, and
// the folds of the next chunks run while it sends one. The first error of send stops it: no worker
// takes another index, and it answers that error once every worker has stopped. Otherwise it
// returns once every index is folded.
func foldInChunks(n, workers int, fold func(index int), ends []int, send func(chunk int) error) error {
	// left[c] is how many folds of chunk c are not done yet, and ready[c] is closed once none is.
	left := make([]atomic.Int64, len(ends))
	ready := make([]chan struct{}, len(ends))
	from := 0
	for c, end := range ends {
		left[c].Store(int64(end - from))
		ready[c] = make(chan struct{})
		if end == from {
			close(ready[c])
		}
		from = end
	}
	var next atomic.Int64
	var stopped atomic.Bool
	var folding sync.WaitGroup
	for range max(min(workers, n), 1) {
		folding.Go(func() {
			for index := int(next.Add(1) - 1); index < n && !stopped.Load(); index = int(next.Add(1) - 1) {
				fold(index)
				if c := sort.SearchInts(ends, index+1); c < len(ends) && left[c].Add(-1) == 0 {
					close(ready[c])
				}
			}
		})
	}
	var err error
	for c := range ends {
		<-ready[c]
		if err = send(c); err != nil {
			stopped.Store(true)
			break
		}
	}
	folding.Wait()
	return err
}

// readFigureGroups reads, in q, the listed rows of cut (its repository the empty string for the
// project's own rows, its deploy repository the empty string for the evals no deploy repository
// moves, a single deploy repository's name or events repository for its own rows, or unionDeployRepo
// `*` for all of the project's currently recorded deploy repositories; cutDeployRepos) of each eval
// of evalIDs over r, as DayWindowIn reads them with no repository named, or with the cut's one
// repository, and answers each eval's group, none for an eval with no row there. An eval whose rows
// the cut's deploy repository does not key (KeyedByDeployRepo) is none of its evals. An eval none
// of whose series is listed any more, whose rows a register write has not relisted yet, still has a
// group of the rows DayWindowIn reads, whose figures fold no series: so a day that holds a listed
// row always holds its stored row, the one a read of the stamps compares (movedFigureDays).
func readFigureGroups(ctx context.Context, q delivery.Querier, scope figureScope, entries map[string]Entry, evalIDs []string, cut figureCut, r Range) (map[string]*figureGroup, error) {
	groups := map[string]*figureGroup{}
	var read []Entry
	for _, evalID := range evalIDs {
		if entry, held := entries[evalID]; held && KeyedByDeployRepo(entry.Definition) == (cut.deployRepo != "") {
			read = append(read, entry)
		}
	}
	if len(read) == 0 || !r.From.Before(r.To) {
		return groups, nil
	}
	deployRepos, err := cutDeployRepos(ctx, q, scope.project, cut.deployRepo)
	if err != nil {
		return nil, err
	}
	rows, err := DayWindowIn(ctx, q, []string{scope.project}, read, deployRepos, cut.repos(), r, nil)
	if err != nil {
		return nil, err
	}
	if times := figureTimesIn(ctx); times != nil {
		times.reads++
		times.rows += len(rows)
	}
	byEval := map[string][]WindowRow{}
	for _, row := range rows {
		byEval[row.EvalID] = append(byEval[row.EvalID], row)
	}
	for evalID, held := range byEval {
		entry := entries[evalID]
		groups[evalID] = newFigureGroup(entry, foldKeyOf(entry), held)
	}
	return groups, nil
}

// refoldFigures folds again, in tx, the stored figures of every day of spans (each by eval id: the
// days a write moved, or may have) whose rows moved since their figures were folded
// (movedFigureDays), and of every range that holds one, and writes them where they moved. It reads
// every row it folds first (readFigurePlan), since the transaction's one connection runs one
// statement at a time, and then folds them on up to figureFoldWorkers goroutines and writes them
// as they are folded (figurePlan.foldAndWrite): the folds of the later ranges run while the
// earlier ones are written, so the fold's time and the writes' time overlap, where a production
// rule's rewrite on the farm folded for 280-350 ms and then wrote for 400-480 ms
// (dispatch://INFRA-2916, phase H). entries is the project's evals by id, every eval spans names
// among them.
func refoldFigures(ctx context.Context, tx pgx.Tx, scope figureScope, entries map[string]Entry, spans ...map[string]daySpan) error {
	_, err := refoldMoved(ctx, tx, scope, entries, spans...)
	return err
}

// refoldMoved is refoldFigures, and answers, by eval, the span of the days whose figures it folded
// again, none where no day's rows had moved.
func refoldMoved(ctx context.Context, tx pgx.Tx, scope figureScope, entries map[string]Entry, spans ...map[string]daySpan) (map[string]daySpan, error) {
	moved, err := movedFigureDays(ctx, tx, scope, entries, spans...)
	if err != nil || len(moved) == 0 {
		return nil, err
	}
	plan, err := readFigurePlan(ctx, tx, scope, entries, moved)
	if err != nil {
		return nil, err
	}
	if err := plan.foldAndWrite(ctx, tx, scope, min(runtime.GOMAXPROCS(0), figureFoldWorkers)); err != nil {
		return nil, err
	}
	folded := map[string]daySpan{}
	for group, days := range moved {
		for day := range days {
			folded[group.eval] = folded[group.eval].widen(day)
		}
	}
	return folded, nil
}

// figureTimes counts the statements that read day rows for a fold of stored figures and the
// rows those reads return (readFoldGroups, readFigureGroups), when the context carries a count.
type figureTimes struct {
	reads, rows int
}

type figureTimesKey struct{}

// withFigureTimes is ctx carrying times, which the folds made under it add to.
func withFigureTimes(ctx context.Context, times *figureTimes) context.Context {
	return context.WithValue(ctx, figureTimesKey{}, times)
}

// figureTimesIn is the figureTimes ctx carries, nil for none.
func figureTimesIn(ctx context.Context) *figureTimes {
	times, _ := ctx.Value(figureTimesKey{}).(*figureTimes)
	return times
}

type foldWorkersKey struct{}

// withFoldWorkers is ctx carrying workers, the most goroutines a fold made under it folds on in
// place of figureFoldWorkers: the settings rewrite's (rewriteSettings), which every view it moves
// waits for.
func withFoldWorkers(ctx context.Context, workers int) context.Context {
	return context.WithValue(ctx, foldWorkersKey{}, workers)
}

// foldWorkersIn is the worker limit for a fold of end days (buildFigureEnds), no larger than
// the CPUs the process may use.
func foldWorkersIn(ctx context.Context) int {
	workers, ok := ctx.Value(foldWorkersKey{}).(int)
	if !ok {
		workers = figureFoldWorkers
	}
	return min(runtime.GOMAXPROCS(0), workers)
}

// foldFiguresInTx folds the moved figures of a day-row write that folds its moved days inline,
// after its day rows, in the write's own transaction. Each of written is, by the key it wrote
// (a project, or an owner key), the spans of the days it wrote, by that key's eval ids, one span
// each, so a write of days that do not touch passes a set for each run of them (writtenSpans.sets).
// An owner key's rows (owners.go) have no figures of their own: they are folded into the figures of
// each project whose read folds them (readersOf, one read of every owner key written), each over
// the same days of its eval of the same slug (asReader). Each project is then folded once,
// whatever number of the keys it reads the write wrote (refoldProject): where any of its figures
// are folded, the figures of the days whose rows moved of every span its own write and those keys'
// name, and of every range that holds one. The spans are kept as they were written (spansOf):
// their hull would read the stamps and the stored rows of the days between two of them, which no
// write named, and a write of a months-old day and of today would read every day between in its
// transaction. Folds of the same rows in one transaction answer alike, so one fold over every span
// stores what a fold after each write of them would, and the folds after the first found nothing
// moved.
func foldFiguresInTx(ctx context.Context, tx pgx.Tx, build string, written ...map[string]map[string]daySpan) error {
	owned := map[string]bool{}
	for _, set := range written {
		for key, spans := range set {
			if isOwner(key) && len(spans) > 0 {
				owned[key] = true
			}
		}
	}
	readers, err := readersOf(ctx, tx, slices.Sorted(maps.Keys(owned)))
	if err != nil {
		return err
	}
	folds := map[string][]map[string]daySpan{}
	for _, set := range written {
		for _, key := range slices.Sorted(maps.Keys(set)) {
			spans := set[key]
			switch {
			case len(spans) == 0:
			case !isOwner(key):
				folds[key] = append(folds[key], spans)
			default:
				for _, reader := range readers[key] {
					moved := make(map[string]daySpan, len(spans))
					for evalID, span := range spans {
						moved[asReader(key, reader, evalID)] = span
					}
					folds[reader] = append(folds[reader], moved)
				}
			}
		}
	}
	for _, project := range slices.Sorted(maps.Keys(folds)) {
		if _, _, err := refoldProject(ctx, tx, build, project, folds[project]...); err != nil {
			return err
		}
	}
	return nil
}

// refoldProject folds again, in tx, by build, project's stored figures of the days of spans (each
// by eval id) whose rows moved, where any of its figures are folded (readFigureScope), over its
// evals that spans name (readEntries), and of every range that holds one (refoldMoved), in every
// cut the rows reach, a cut a move marked stale among them: folded is, by eval, the span of the
// days whose figures it folded again, and ok is false where none of the project's figures are
// folded, which it then folds none of.
func refoldProject(ctx context.Context, tx pgx.Tx, build, project string, spans ...map[string]daySpan) (map[string]daySpan, bool, error) {
	scope, ok, err := readFigureScope(ctx, tx, build, project)
	if err != nil || !ok {
		return nil, false, err
	}
	entries, err := readEntries(ctx, tx, project, spannedEvals(spans))
	if err != nil {
		return nil, false, err
	}
	folded, err := refoldMoved(ctx, tx, scope, entriesByID(entries), spans...)
	return folded, true, err
}

// entriesByID is entries by eval id.
func entriesByID(entries []Entry) map[string]Entry {
	byID := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		byID[entry.Definition.EvalID] = entry
	}
	return byID
}

// statementRows is the most rows one statement of a write sends (statementChunks): its arrays are
// its parameters, and a whole build's batch of a large project holds tens of thousands of rows.
const statementRows = 5000

// statementBytes is the most bytes of parameters one statement of a write sends, as
// statementChunks bounds them. On the farm (dispatch://INFRA-2916, Job infra2916-k8hso1), 9 of 24
// passes over TCP stalled about 200 ms on a statement whose Bind and Execute messages held 0.8-1.0
// MB, in the pod's loopback TCP (gVisor), while Postgres sat in ClientRead reading it; none of 24
// passes over the unix socket did. A row larger than the bound alone is sent whole, in a statement
// of its own (statementChunks), so the routes bound what one row may hold: the series of an eval
// (MaxEvalSeries) and the values of a day row (MaxDaySamples), each the most whose row, at the
// largest keys the routes take, is written in one statement within the bound.
const statementBytes = 256 << 10

// MaxEvalSeries is the most series one eval holds, listed or not (CodeSeriesLimit): the most whose
// stored range row, with every key, the build and each series' name at their largest, the range
// upsert sends in one statement within statementBytes (rangeUpserts). The range row is the widest
// row a series makes: a day's figures fold the headline series alone, with no via-repository
// entry. A register write or a readings write that would leave a logged eval holding more is
// refused (Register, registerMissing), so no statement that writes a stored row of the eval passes
// the bound. It is the bound less what the statement holds besides its series' entries
// (figureRowReserve), over the most one series adds (seriesFigureBytes): (262,144 - 2,337) / 1,106
// = 234; with 235 series the statement would take 262,247 bytes.
//
// The entries count each series' via-repository entry, which a logged eval's rows never carry
// (writeDays sets none), and without which the bound would admit 446 series. A figures write reads
// the rows it folds (readFigureGroups through DayWindowIn, and readFoldGroups) with one pair (eval,
// series) for each series whose fold reads its samples, 1,043 bytes at the largest keys, fewer
// than a series' 1,106 here, so that read's statement stays within the bound at this count too
// (244,062 bytes of pairs for one eval, beside the one repository of a cut's read), where at 446
// series it would not.
const MaxEvalSeries = (statementBytes - figureRowReserve) / seriesFigureBytes

// seriesFigureBytes is the most bytes one series adds to a figures column (encodeFigures): its name
// of at most maxKeyBytes after a 2-byte length, its flag and its count of statistics, a byte each,
// and the statistics of the widest fold, a quantile's n, sum, median, p90 and max (aggStatistics),
// each its name after a 1-byte length, a flag and 8 bytes of value: 11 + 13 + 16 + 13 + 13 = 66;
// then its via-repository entry, the name again after its 2-byte length and a varint of at most
// binary.MaxVarintLen64 bytes. 582 + 524 = 1,106.
const seriesFigureBytes = (2 + maxKeyBytes + 1 + 1 + 66) + (2 + maxKeyBytes + binary.MaxVarintLen64)

// figureRowReserve is the most bytes the range upsert of one stored range row takes besides its
// series' entries (rangeUpserts): the statement's own, paramBytes(8, project, build), 4 bytes of
// length before each of its 10 parameters, an array header for each of its 8 arrays, a project key
// (maxProjectKeyBytes) and a build of at most maxKeyBytes (a 40-character commit, or a shorter name
// the process draws: cmd/dispatch's processBuild); the row's, upsertBytes less its figures, 4 bytes
// of length in each of the 8 arrays, 4 numbers, dates and times of at most 8 bytes, an eval id
// (maxEvalIDBytes), and a repository and a deploy repository of at most maxKeyBytes each; and the
// figures' two counts, of the series and of their via-repository entries, 2 bytes each at this
// many series. 722 + 1,611 + 4 = 2,337.
const figureRowReserve = 4*(8+2) + arrayHeaderBytes*8 + maxProjectKeyBytes + maxKeyBytes +
	8*4 + 4*8 + maxEvalIDBytes + 2*maxKeyBytes + 2*2

// MaxVersionBytes is the most bytes one version of an eval's definition (Definition), or of one of
// its series (Series), takes as JSON encodes it, as every answer that holds the version writes it
// (CodeVersionTooLarge). A write's version is judged once the fields the write leaves out are
// copied forward, under the eval's lock and before the write sends any statement that stores it
// (definitionTooLarge, seriesTooLarge), so no run of writes each within the bound leaves a version
// past it. It is what keeps a version beside the rest of a write's answer within MaxAnswerBytes: a
// register write answers its eval's definition beside the eval's series, and a readings write the
// definition beside its readings. A definition and six series at this bound take 229,376 of the
// answer's 258,048 bytes, so the register write's answer lists at least one series however many
// the eval holds, and the readings write's leaves about 225,000 bytes to its readings. The
// statement that stores a version sends the same texts and lists, unescaped, within a few hundred
// bytes of the version's JSON, far within statementBytes.
const MaxVersionBytes = 32 << 10

// statementChunks is rows cut into the chunks one statement each sends, in their order: each chunk
// closed before the row that would make it hold more than statementRows rows, or parameters of
// more than statementBytes, base being the most bytes the statement's parameters take with no row
// (paramBytes) and size the most a row adds (upsertBytes, deleteBytes, dayRowBytes). A row larger
// than that alone is a chunk of its own, which its statement sends whole.
func statementChunks[R any](rows []R, base int, size func(*R) int) iter.Seq[[]R] {
	return func(yield func([]R) bool) {
		from, bytes := 0, base
		for index := range rows {
			n := size(&rows[index])
			if index > from && (index-from == statementRows || bytes+n > statementBytes) {
				if !yield(rows[from:index]) {
					return
				}
				from, bytes = index, base
			}
			bytes += n
		}
		if from < len(rows) {
			yield(rows[from:])
		}
	}
}

// arrayHeaderBytes is what pgx's binary encoding of a one-dimensional array writes before its
// elements: the dimensions, the null flag, the element type, and the dimension's length and lower
// bound, 4 bytes each.
const arrayHeaderBytes = 20

// paramBytes is the most bytes the parameters of a statement take with no row in its arrays, of
// which it has arrays, beside scalars: 4 bytes of length before each parameter, each scalar's
// bytes, and each array's header.
func paramBytes(arrays int, scalars ...string) int {
	bytes := 4*(arrays+len(scalars)) + arrayHeaderBytes*arrays
	for _, scalar := range scalars {
		bytes += len(scalar)
	}
	return bytes
}

// upsertBytes and deleteBytes are the most bytes a row adds to the parameters of the statement
// that upserts it or deletes its key, as pgx encodes them in binary: in each array, 4 bytes of
// length and the element, each string and figures their bytes and each number and date at most 8.
func (r *rangeFigureRow) upsertBytes() int {
	return 8*4 + len(r.repo) + len(r.eval) + len(r.deployRepo) + len(r.figures) + 4*8
}

func (k *rangeFigureKey) deleteBytes() int {
	return 5*4 + len(k.repo) + len(k.eval) + len(k.deployRepo) + 2*8
}

func (r *dayFigureRow) upsertBytes() int {
	return 7*4 + len(r.repo) + len(r.eval) + len(r.deployRepo) + len(r.figures) + 3*8
}

func (k *dayFigureKey) deleteBytes() int {
	return 4*4 + len(k.repo) + len(k.eval) + len(k.deployRepo) + 8
}

// rangeUpserts is the arguments of each statement upsertRangeFigures sends of rows, each of a chunk
// of them (statementChunks).
func rangeUpserts(scope figureScope, rows []rangeFigureRow) iter.Seq[[]any] {
	return func(yield func([]any) bool) {
		for chunk := range statementChunks(rows, paramBytes(8, scope.project, scope.build), (*rangeFigureRow).upsertBytes) {
			repos, ns, ends, evalIDs := make([]string, len(chunk)), make([]int32, len(chunk)), make([]time.Time, len(chunk)), make([]string, len(chunk))
			deployRepos, keys, figures, oldest := make([]string, len(chunk)), make([]int64, len(chunk)), make([][]byte, len(chunk)), make([]time.Time, len(chunk))
			for index, row := range chunk {
				repos[index], ns[index], ends[index], evalIDs[index], deployRepos[index] = row.repo, int32(row.days), row.end, row.eval, row.deployRepo
				keys[index], figures[index], oldest[index] = row.key, row.figures, row.oldest
			}
			if !yield([]any{scope.project, repos, ns, ends, evalIDs, keys, figures, oldest, scope.build, deployRepos}) {
				return
			}
		}
	}
}

// rangeDeletes is the arguments of each statement writeAfterRanges sends of gone, the keys of
// range rows of project, each of a chunk of them (statementChunks).
func rangeDeletes(project string, gone []rangeFigureKey) iter.Seq[[]any] {
	return func(yield func([]any) bool) {
		for chunk := range statementChunks(gone, paramBytes(5, project), (*rangeFigureKey).deleteBytes) {
			repos, ns, ends, evalIDs := make([]string, len(chunk)), make([]int32, len(chunk)), make([]time.Time, len(chunk)), make([]string, len(chunk))
			deployRepos := make([]string, len(chunk))
			for index, row := range chunk {
				repos[index], ns[index], ends[index], evalIDs[index], deployRepos[index] = row.repo, int32(row.days), row.end, row.eval, row.deployRepo
			}
			if !yield([]any{project, repos, ns, ends, evalIDs, deployRepos}) {
				return
			}
		}
	}
}

// dayUpserts is the arguments of each statement writeAfterRanges sends of rows, each of a chunk of
// them (statementChunks).
func dayUpserts(scope figureScope, rows []dayFigureRow) iter.Seq[[]any] {
	return func(yield func([]any) bool) {
		for chunk := range statementChunks(rows, paramBytes(7, scope.project, scope.build), (*dayFigureRow).upsertBytes) {
			repos, days, evalIDs, deployRepos := make([]string, len(chunk)), make([]time.Time, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk))
			keys, figures, stamps := make([]int64, len(chunk)), make([][]byte, len(chunk)), make([]int64, len(chunk))
			for index, row := range chunk {
				repos[index], days[index], evalIDs[index], deployRepos[index] = row.repo, row.day, row.eval, row.deployRepo
				keys[index], figures[index], stamps[index] = row.key, row.figures, row.stamp
			}
			if !yield([]any{scope.project, repos, days, evalIDs, keys, figures, stamps, scope.build, deployRepos}) {
				return
			}
		}
	}
}

// dayDeletes is the arguments of each statement writeAfterRanges sends of gone, the keys of day
// rows of project, each of a chunk of them (statementChunks).
func dayDeletes(project string, gone []dayFigureKey) iter.Seq[[]any] {
	return func(yield func([]any) bool) {
		for chunk := range statementChunks(gone, paramBytes(4, project), (*dayFigureKey).deleteBytes) {
			repos, days, evalIDs, deployRepos := make([]string, len(chunk)), make([]time.Time, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk))
			for index, row := range chunk {
				repos[index], days[index], evalIDs[index], deployRepos[index] = row.repo, row.day, row.eval, row.deployRepo
			}
			if !yield([]any{project, repos, days, evalIDs, deployRepos}) {
				return
			}
		}
	}
}

// sortByKey sorts each of w's lists in the byte order of its rows' keys, the order their
// statements take them in (upsertRangeFigures, writeAfterRanges).
func (w *figureWrites) sortByKey() {
	slices.SortFunc(w.ranges, func(a, b rangeFigureRow) int { return compareRangeKeys(a.rangeFigureKey, b.rangeFigureKey) })
	slices.SortFunc(w.goneRanges, compareRangeKeys)
	slices.SortFunc(w.days, func(a, b dayFigureRow) int { return compareDayFigureKeys(a.dayFigureKey, b.dayFigureKey) })
	slices.SortFunc(w.goneDays, compareDayFigureKeys)
}

// writeFigures writes w's rows of scope's project's stored figures in tx, each list sorted by its
// keys: the range rows (upsertRangeFigures), then the gone ranges, the day rows and the gone days
// (writeAfterRanges).
func writeFigures(ctx context.Context, tx pgx.Tx, scope figureScope, w figureWrites) error {
	w.sortByKey()
	if err := upsertRangeFigures(ctx, tx, scope, w.ranges); err != nil {
		return err
	}
	return writeAfterRanges(ctx, tx, scope, w)
}

// upsertRangeFigures upserts rows, range rows of scope's project's stored figures in the byte order
// of their keys, in tx, a statement for each chunk of them (rangeUpserts), each row recording
// scope.build as the build that folded it and written only where its fold key, figures or oldest
// updated_at moved: so a fold that moved nothing writes nothing, and a row another build folded to
// the same figures keeps its built_by, as the build that folded those figures. Every statement
// takes its rows in the byte order of their keys, as every writer of them does, so two writers of
// overlapping rows wait on each other rather than deadlock: rows come in that order
// (compareRangeKeys, whose order is the one `collate "C"` gives their keys:
// TestEachWritesGoKeyOrderIsPostgresCollateCOrder), and the statement takes them in the order it
// is sent them (`with ordinality`), which Postgres knows its unnest answers and so sorts nothing
// for.
func upsertRangeFigures(ctx context.Context, tx pgx.Tx, scope figureScope, rows []rangeFigureRow) error {
	for args := range rangeUpserts(scope, rows) {
		if _, err := tx.Exec(ctx, `
			insert into eval_range_figures (project_key, repo, days, end_day, eval_id, deploy_repo, fold_key, figures, oldest_updated_at, built_by)
			select $1, given.repo, given.days, given.end_day, given.eval_id, given.deploy_repo, given.fold_key, given.figures, given.oldest, $9
			from unnest($2::text[], $3::int[], $4::date[], $5::text[], $10::text[], $6::bigint[], $7::bytea[], $8::timestamptz[]) with ordinality
				as given(repo, days, end_day, eval_id, deploy_repo, fold_key, figures, oldest, sent)
			order by given.sent
			on conflict (project_key, repo, days, end_day, eval_id, deploy_repo) do update
			set fold_key = excluded.fold_key, figures = excluded.figures, oldest_updated_at = excluded.oldest_updated_at,
				built_by = excluded.built_by
			where (eval_range_figures.fold_key, eval_range_figures.figures, eval_range_figures.oldest_updated_at)
				is distinct from (excluded.fold_key, excluded.figures, excluded.oldest_updated_at)
		`, args...); err != nil {
			return fmt.Errorf("write the stored range figures of %s: %w", scope.project, err)
		}
	}
	return nil
}

// writeAfterRanges writes the rest of w once its range rows are written (upsertRangeFigures), in
// tx, each list in the byte order of its keys and a statement for each chunk of it (rangeDeletes,
// dayUpserts, dayDeletes): it deletes each gone range, upserts each day row as upsertRangeFigures
// upserts a range row, only where its fold key, figures or stamp moved and recording scope.build,
// and deletes each gone day. Every statement takes its rows in the byte order of their keys, the
// day upsert in the order it is sent them (compareDayFigureKeys), as upsertRangeFigures' does.
// Where it deletes a row of a union cut (unionDeployRepo) it first says tx folds union cuts
// (markUnionFigures), so the delete leaves the union cut's mark as it stands.
func writeAfterRanges(ctx context.Context, tx pgx.Tx, scope figureScope, w figureWrites) error {
	project := scope.project
	if slices.ContainsFunc(w.goneRanges, func(key rangeFigureKey) bool { return key.deployRepo == unionDeployRepo }) ||
		slices.ContainsFunc(w.goneDays, func(key dayFigureKey) bool { return key.deployRepo == unionDeployRepo }) {
		if err := markUnionFigures(ctx, tx); err != nil {
			return err
		}
	}
	for args := range rangeDeletes(project, w.goneRanges) {
		if _, err := tx.Exec(ctx, `
			delete from eval_range_figures f
			using unnest($2::text[], $3::int[], $4::date[], $5::text[], $6::text[]) as gone(repo, days, end_day, eval_id, deploy_repo)
			where f.project_key = $1 and f.repo = gone.repo and f.days = gone.days and f.end_day = gone.end_day
				and f.eval_id = gone.eval_id and f.deploy_repo = gone.deploy_repo
		`, args...); err != nil {
			return fmt.Errorf("delete the stored range figures of %s that hold no row: %w", project, err)
		}
	}
	for args := range dayUpserts(scope, w.days) {
		if _, err := tx.Exec(ctx, `
			insert into eval_day_figures (project_key, repo, day, eval_id, deploy_repo, fold_key, figures, rows_stamp, built_by)
			select $1, given.repo, given.day, given.eval_id, given.deploy_repo, given.fold_key, given.figures, given.rows_stamp, $8
			from unnest($2::text[], $3::date[], $4::text[], $9::text[], $5::bigint[], $6::bytea[], $7::bigint[]) with ordinality
				as given(repo, day, eval_id, deploy_repo, fold_key, figures, rows_stamp, sent)
			order by given.sent
			on conflict (project_key, repo, day, eval_id, deploy_repo) do update
			set fold_key = excluded.fold_key, figures = excluded.figures, rows_stamp = excluded.rows_stamp, built_by = excluded.built_by
			where (eval_day_figures.fold_key, eval_day_figures.figures, eval_day_figures.rows_stamp)
				is distinct from (excluded.fold_key, excluded.figures, excluded.rows_stamp)
		`, args...); err != nil {
			return fmt.Errorf("write the stored day figures of %s: %w", project, err)
		}
	}
	for args := range dayDeletes(project, w.goneDays) {
		if _, err := tx.Exec(ctx, `
			delete from eval_day_figures f
			using unnest($2::text[], $3::date[], $4::text[], $5::text[]) as gone(repo, day, eval_id, deploy_repo)
			where f.project_key = $1 and f.repo = gone.repo and f.day = gone.day and f.eval_id = gone.eval_id
				and f.deploy_repo = gone.deploy_repo
		`, args...); err != nil {
			return fmt.Errorf("delete the stored day figures of %s that hold no row: %w", project, err)
		}
	}
	return nil
}

// markUnionFigures says, in tx, that tx folds the union cuts (unionDeployRepo): the
// transaction-local setting dispatch.union_figures, which the figures tables' delete triggers read
// (migration 20261126125956), so a delete of union rows in tx leaves each project's mark of its
// union cut (eval_figures_built.union_through) as it stands. A build that folds no union cut
// deletes them with no such word, and its deletes take the marks back before them.
func markUnionFigures(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select set_config('dispatch.union_figures', 'on', true)`); err != nil {
		return fmt.Errorf("say the transaction folds the union cuts: %w", err)
	}
	return nil
}

// compareRangeKeys orders range rows as eval_range_figures' primary key does within a project,
// each string by its bytes: the order upsertRangeFigures' statement takes them in, as it is sent
// them.
func compareRangeKeys(a, b rangeFigureKey) int {
	return cmp.Or(strings.Compare(a.repo, b.repo), cmp.Compare(a.days, b.days), a.end.Compare(b.end),
		strings.Compare(a.eval, b.eval), strings.Compare(a.deployRepo, b.deployRepo))
}

// compareDayFigureKeys orders day rows as eval_day_figures' primary key does within a project,
// each string by its bytes: the order writeAfterRanges' day upsert takes them in, as it is sent
// them.
func compareDayFigureKeys(a, b dayFigureKey) int {
	return cmp.Or(strings.Compare(a.repo, b.repo), a.day.Compare(b.day), strings.Compare(a.eval, b.eval),
		strings.Compare(a.deployRepo, b.deployRepo))
}

// buildFigureEnds folds, in tx, what ends names of project's stored figures (figureEnds): every
// range that ends on one of its end days and the day row of each of its days, of each eval of
// entries in each cut the project's rows of the days those ranges and days span hold
// (readFigureCuts), or in the one cut of repo where repo is not nil (a stale cut's refold,
// refoldCut, and its first-paint batch, foldFirstPaint), and writes them where they moved
// (writeFigures): one read of the rows of each cut over those days. Every stored row of those end
// days and days, of the cuts it folds, that no row holds any more is deleted: one an earlier build
// folded, of an eval, a cut or a deploy repository that holds none now, or one a move of a default
// repository left in a cut that no longer holds its rows. Where every is set, entries being every
// eval of the project, every stored range row of those end days is read for it, whatever eval it
// is of; where it is not (the refold of the evals a stale mark names), only those of entries' evals
// are, so no stored row of another eval is written or deleted. It is a batch of the figures walk,
// the whole of a new end day's ranges once the walk stands at today, a batch of a stale cut's
// refold and a stale cut's first-paint batch.
//
// The stored day rows are read through eval_day_figures_eval_day, one range of the days for each
// eval, the project's evals in statements of at most statementBytes: no route bounds how many
// evals a project holds. So are entries' stored range rows where every is not set. The read of the
// stored range rows reads the end days of ends: every one from its first through its last, or,
// where it picks them, those it picks alone (a first paint's 24 of the 173 from its first to its
// last, firstPaintOf), and passes over those of an end day it does not fold.
//
// Each cut's ranges and days are folded once its rows are read, on up to foldWorkersIn(ctx)
// goroutines at once (foldInChunks of figurePlan.fold, whose folds of one group share it as a
// write's do), each into a target of its own, and gathered on the calling goroutine, which sends
// every statement of tx, one at a time: so the rows it upserts and deletes, and the order each
// statement takes them in, are those a fold on one goroutine gives. On one goroutine the fold was
// most of each first paint a move's commit folds: on the farm at INFRA's size, gaps of up to 500
// ms in which no statement took over 10 ms, in a commit whose first paints of three cuts took
// 0.8-1.7 s and left some views 2.02-2.17 s after the move, past R14's 2 s
// (dispatch://INFRA-2916). It writes nothing while it folds, as figurePlan.foldAndWrite does: its
// range upserts take the ranges in the byte order of their keys, in which the cuts of a repository
// interleave (a key orders by length and end day before eval and deploy repository), so they could
// begin only once a repository's last cut is folded, and a plan of every cut would hold all their
// rows at once, where it holds one cut's; and it deletes the stored rows no fold built, which it
// reads once every cut is folded.
func buildFigureEnds(ctx context.Context, tx pgx.Tx, scope figureScope, entries map[string]Entry, repo *string, every bool, ends figureEnds) error {
	read := ends.rows()
	cuts, err := readFigureCuts(ctx, tx, scope.project, read, repo)
	if err != nil {
		return err
	}
	evalIDs := slices.Sorted(maps.Keys(entries))
	workers := foldWorkersIn(ctx)
	var writes figureWrites
	var plan figurePlan
	built := map[rangeFigureKey]bool{}
	builtDays := map[dayFigureKey]bool{}
	for _, cut := range cuts {
		groups, err := readFigureGroups(ctx, tx, scope, entries, evalIDs, cut, read)
		if err != nil {
			return err
		}
		for _, evalID := range slices.Sorted(maps.Keys(groups)) {
			plan.addEnds(groups[evalID], evalID, cut, ends)
		}
		// No chunk is sent, so the fold answers no error.
		if err := foldInChunks(len(plan.ranges)+len(plan.days), workers, plan.fold, nil, nil); err != nil {
			return err
		}
		for index := range plan.ranges {
			if target := &plan.ranges[index]; target.held {
				writes.ranges = append(writes.ranges, target.rangeFigureRow)
				built[target.rangeFigureKey] = true
			}
		}
		for index := range plan.days {
			if target := &plan.days[index]; target.held {
				writes.days = append(writes.days, target.dayFigureRow)
				builtDays[target.dayFigureKey] = true
			}
		}
		// The next cut's targets reuse the lists, and none keeps this cut's groups and rows.
		clear(plan.ranges)
		clear(plan.days)
		plan.ranges, plan.days = plan.ranges[:0], plan.days[:0]
	}
	ofCut, rangeArgs, base := "", []any{scope.project, ends.first, ends.last}, paramBytes(1, scope.project)+2*(4+8)
	if repo != nil {
		ofCut, rangeArgs, base = ` and repo = $4`, append(rangeArgs, *repo), base+4+len(*repo)
	}
	ofEnds, rangeBase := "", base
	if ends.picked != nil {
		ofEnds, rangeArgs = fmt.Sprintf(` and end_day = any($%d::date[])`, len(rangeArgs)+1), append(rangeArgs, ends.picked)
		rangeBase += 4 + arrayHeaderBytes + (4+8)*len(ends.picked)
	}
	readRanges := func(ofEvals string, args []any) error {
		rows, err := tx.Query(ctx, `
			select repo, days, end_day, eval_id, deploy_repo from eval_range_figures where project_key = $1 and end_day >= $2 and end_day <= $3`+ofCut+ofEnds+ofEvals,
			args...)
		if err != nil {
			return fmt.Errorf("read %s's stored range figures ending %s to %s: %w", scope.project, ends.first.Format(time.DateOnly), ends.last.Format(time.DateOnly), err)
		}
		var stored rangeFigureKey
		var n int32
		if _, err := pgx.ForEachRow(rows, []any{&stored.repo, &n, &stored.end, &stored.eval, &stored.deployRepo}, func() error {
			stored.days, stored.end = int(n), stored.end.UTC()
			if !built[stored] && ends.holds(stored.end) {
				writes.goneRanges = append(writes.goneRanges, stored)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("read %s's stored range figures ending %s to %s: %w", scope.project, ends.first.Format(time.DateOnly), ends.last.Format(time.DateOnly), err)
		}
		return nil
	}
	if every {
		if err := readRanges("", rangeArgs); err != nil {
			return err
		}
	} else {
		ofEvals := fmt.Sprintf(` and eval_id = any($%d)`, len(rangeArgs)+1)
		for chunk := range statementChunks(evalIDs, rangeBase, textBytes) {
			if err := readRanges(ofEvals, append(slices.Clip(rangeArgs), chunk)); err != nil {
				return err
			}
		}
	}
	for chunk := range statementChunks(evalIDs, base, textBytes) {
		dayArgs := []any{scope.project, ends.days.From, ends.days.To, chunk}
		ofDayCut := ""
		if repo != nil {
			ofDayCut, dayArgs = ` and repo = $5`, append(dayArgs, *repo)
		}
		rows, err := tx.Query(ctx, `
			select repo, day, eval_id, deploy_repo from eval_day_figures where project_key = $1 and eval_id = any($4) and day >= $2 and day < $3`+ofDayCut,
			dayArgs...)
		if err != nil {
			return fmt.Errorf("read %s's stored day figures: %w", scope.project, err)
		}
		var storedDay dayFigureKey
		if _, err := pgx.ForEachRow(rows, []any{&storedDay.repo, &storedDay.day, &storedDay.eval, &storedDay.deployRepo}, func() error {
			storedDay.day = storedDay.day.UTC()
			if !builtDays[storedDay] {
				writes.goneDays = append(writes.goneDays, storedDay)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("read %s's stored day figures: %w", scope.project, err)
		}
	}
	return writeFigures(ctx, tx, scope, writes)
}

// readFigureCuts reads, in q, each cut project's listed day rows of the days of r hold, its own and
// those of the owner keys it reads under the published defaults (eval_project_owners, owners.go),
// each row in the cut cutOf names, in their order (compareFigureCuts); or, where repo is not nil,
// those of the one cut of repo (cutDeployReposQuery). For each repository cut that holds a row of
// any of the project's currently recorded deploy repositories (DeployReposIn), it also names the
// project's union cut (`deploy_repo` `*`, unionDeployRepo), whose fold reads all of the project's
// current deploy repositories together. The published default's cut is among them wherever a row
// stored under legionfacts.DefaultRepo is. One statement: for every cut, a walk of each owner's
// repositories through eval_days_window, one probe each, and the read of each one's deploy
// repositories from its rows of the days of r, one range of the index each, rather than every
// listed row of the project.
func readFigureCuts(ctx context.Context, q delivery.Querier, project string, r Range, repo *string) ([]figureCut, error) {
	var rows pgx.Rows
	var err error
	if repo == nil {
		rows, err = q.Query(ctx, `
			select distinct cut.repo, d.deploy_repo
			from eval_project_owners o
			cross join lateral (
				with recursive held(repo) as (
					(select e.repo from eval_days e where e.project_key = o.owner and e.listed order by e.repo limit 1)
					union all
					select (select e.repo from eval_days e where e.project_key = o.owner and e.listed and e.repo > held.repo order by e.repo limit 1)
					from held where held.repo is not null
				)
				select repo from held where repo is not null
			) owned
			join eval_days d on d.project_key = o.owner and d.listed and d.repo = owned.repo and d.day >= $2 and d.day < $3
			cross join lateral (select `+cutOf("d", "$1")+` as repo) cut
			where o.project_key = $1 and cut.repo is not null
		`, project, r.From, r.To)
	} else {
		rows, err = q.Query(ctx, cutDeployReposQuery, project, r.From, r.To, *repo)
	}
	if err != nil {
		return nil, fmt.Errorf("read the cuts of %s's day rows: %w", project, err)
	}
	cuts, err := pgx.CollectRows(rows, scanFigureCut)
	if err != nil {
		return nil, fmt.Errorf("read the cuts of %s's day rows: %w", project, err)
	}
	cuts, err = withUnionCuts(ctx, q, project, cuts)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(cuts, compareFigureCuts)
	return slices.Compact(cuts), nil
}

// withUnionCuts adds, for every repository in cuts that holds a row of one of project's currently
// recorded deploy repositories (DeployReposIn), the project's union cut (`deploy_repo` `*`).
func withUnionCuts(ctx context.Context, q delivery.Querier, project string, cuts []figureCut) ([]figureCut, error) {
	if !slices.ContainsFunc(cuts, func(cut figureCut) bool { return cut.deployRepo != "" && cut.deployRepo != unionDeployRepo }) {
		return cuts, nil
	}
	deployRepos, err := DeployReposIn(ctx, q)
	if err != nil {
		return nil, err
	}
	added := map[string]bool{}
	for _, cut := range cuts {
		if !added[cut.repo] && deployRepos.hasRepo(project, cut.deployRepo) {
			added[cut.repo] = true
			cuts = append(cuts, figureCut{repo: cut.repo, deployRepo: unionDeployRepo})
		}
	}
	return cuts, nil
}

// cutDeployReposQuery is readFigureCuts' read of one cut, of the repository $4, of project $1's
// listed day rows of the days $2 to $3, each a cut of $4 and of the deploy repository of a row of
// it, as the read of every cut answers those of $4 (cutOf): for each key the project reads
// (eval_project_owners), its own and each owner key's, one range of eval_days_window (project_key,
// repo, day) bounded by the key, $4 and the days; and, where the project publishes $4 as its
// default (eval_project_repos, read once), one range bounded by its own key,
// legionfacts.DefaultRepo and the days, of its own rows stored under that repository, which no
// owner key's row under it joins. The lateral ends in `offset 0`, which keeps Postgres from pulling
// it up into a join: pulled up, the generic plan a cached statement settles on read
// eval_days_window by the repository and the days alone, every project's rows of the repository,
// and joined the keys to them. So it reads the cut's rows of those days and no other row, the rows
// the batch's fold of the cut reads (readFigureGroups, DayWindowIn, 16.8 ms a call on the farm),
// where the statement before it, one join of every key's rows of those days bounded by the keys
// alone, the repository a filter over the two, read every listed row the keys hold of those days:
// a stale cut's refold at INFRA's size called it 205 times in 157 s, at 703 ms and about 233,000
// buffers a call, 144 s of the 157 (dispatch://INFRA-2916, Job infra2916-refoldstats-sd-kpj25, at
// b613db5e).
const cutDeployReposQuery = `
	select $4::text, d.deploy_repo
	from eval_project_owners o
	cross join lateral (
		select d.deploy_repo from eval_days d
		where d.project_key = o.owner and d.listed and d.repo = $4 and d.day >= $2 and d.day < $3
		offset 0
	) d
	where o.project_key = $1
	union
	select $4::text, d.deploy_repo from eval_days d
	where d.project_key = $1 and d.listed and d.repo = '` + legionfacts.DefaultRepo + `' and d.day >= $2 and d.day < $3
		and exists (select 1 from eval_project_repos p where p.project_key = $1 and p.repo = $4)
`
