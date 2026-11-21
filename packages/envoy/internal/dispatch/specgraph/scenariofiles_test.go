package specgraph

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// scenarioFS is each file of the scenario at dir, by its path in the scenario.
func scenarioFS(t *testing.T, dir string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		fsys[filepath.ToSlash(rel)] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

// treeEdit is a writer of a JSON file of a sweep or of a base (editTree): it gives the file's new
// tree from tree, the file's tree decoded for it alone, which it may build the new tree in.
type treeEdit func(tree any) any

// editTree changes the JSON file name of fsys by the writer that build gives, a writer of a sweep
// or of a base, which takes the file's tree, decoded for it alone, its own to build the new tree
// in, and gives the new one; and reads the file back by its name, with each other file as a copy of
// fsys taken before the call holds it (cloneFiles, filesRead), so that a writer that writes another
// file too fails by name, as does one that changes a value of the package where it is built or
// where it is called (writerCalled). The value that the writer writes is the caller's to read back,
// against what the caller states before the call, and a writer of a sweep is the function that a
// function of the package gives (refusalAt, resultsWithout), so that it can name no variable of
// the sweep.
func editTree(t *testing.T, fsys fstest.MapFS, name string, build func() treeEdit) {
	t.Helper()
	before := cloneFiles(fsys)
	var data []byte
	writerCalled(t, name, build, func(edit treeEdit) { data = encodeTree(t, edit(fileTree(t, fsys, name))) })
	fsys[name] = &fstest.MapFile{Data: data}
	filesRead(t, before, fsys, map[string][]byte{name: data}, nil)
}

// filesRead fails the test unless fsys, which a writer of a sweep made from a copy of base, holds
// what the sweep means it to hold: each file of written with its bytes, or no such file where they
// are nil, which takes the file out; each JSON file of changed, as encoding/json/v2 decodes it into
// any, as its meant tree (treeChange.meant), which the caller builds before the writer runs,
// compared whole; each other file of base with the base's bytes; no other file; and each file its
// bytes alone, with no mode, time or sys, as a file of a scenario is. So a writer that writes under
// another file name, writes another value, writes it at another place or under another key, changes
// another place or another file too, or makes a file a directory or a link, fails by name.
func filesRead(t *testing.T, base, fsys fstest.MapFS, written map[string][]byte, changed map[string]meantTree) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(fsys)) {
		switch f := fsys[name]; {
		case f == nil:
			t.Fatalf("the scenario that the sweep wrote holds %s as no file (nil)", name)
		case f.Mode != 0 || !f.ModTime.IsZero() || f.Sys != nil:
			t.Fatalf("the scenario that the sweep wrote holds %s with the mode %v, the time %v and the sys %v, where a file of a scenario is its bytes alone", name, f.Mode, f.ModTime, f.Sys)
		}
	}
	meant := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(written)) {
		meant[name] = true
		file, held := fsys[name]
		switch data := written[name]; {
		case data == nil && held:
			t.Fatalf("the sweep took %s out, and the scenario holds it", name)
		case data != nil && !held:
			t.Fatalf("the sweep wrote %s, and the scenario holds no file of that name", name)
		case data != nil && !bytes.Equal(file.Data, data):
			t.Fatalf("the sweep wrote %d bytes to %s, and the scenario holds %d other bytes there", len(data), name, len(file.Data))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(changed)) {
		meant[name] = true
		want := changed[name]
		file, held := fsys[name]
		if !held {
			t.Fatalf("the sweep changed %s at %q, and the scenario holds no file of that name", name, want.at)
		}
		var got any
		if err := json.Unmarshal(file.Data, &got); err != nil {
			t.Fatalf("the sweep changed %s at %q, and the file is no JSON: %v", name, want.at, err)
		}
		if at, differ := treeDiff(got, want.tree, nil); differ {
			x, xHeld := treeAt(got, at)
			w, wHeld := treeAt(want.tree, at)
			t.Fatalf("the sweep changed %s at %q, and the file differs from that change at %q: it holds %s there (held %t), where it should hold %s (held %t)",
				name, want.at, treePointer(at), encodeTree(t, x), xHeld, encodeTree(t, w), wHeld)
		}
	}
	names := slices.Sorted(maps.Keys(meant))
	for _, name := range slices.Sorted(maps.Keys(fsys)) {
		if meant[name] {
			continue
		}
		if was, held := base[name]; !held || !bytes.Equal(fsys[name].Data, was.Data) {
			t.Fatalf("the sweep wrote %q, and the scenario holds %s, which the base does not hold as it is (held %t)", names, name, held)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(base)) {
		if _, held := fsys[name]; !meant[name] && !held {
			t.Fatalf("the sweep wrote %q, and the scenario no longer holds %s", names, name)
		}
	}
}

// treeChange is one change of a JSON tree as a sweep means it, which the sweep gives apart from the
// writer that makes it, for the read back of the write (treeChange.meant, filesRead): at place, the
// value value, null for nil; or, with out, the member or element taken out; or, with renamed, the
// member under the name that value holds.
type treeChange struct {
	place        []string
	value        any
	out, renamed bool
}

// meantTree is a JSON file as a sweep means a write to leave it (treeChange.meant): the tree, and the
// JSON pointer of the place of the change, for the lines of a failure.
type meantTree struct {
	at   string
	tree any
}

// meant is the JSON file name of base with the change c (on), and the pointer of c's place. It holds
// no object of c: the tree is decoded anew from the base's bytes, c's value is copied through its
// JSON text (a renamed key is a string), and its place is kept as its pointer, so a writer that is
// handed c's value or place, and changes it after meant returns, changes no part of what its write
// is read back against (changedFile).
func (c treeChange) meant(t *testing.T, base fstest.MapFS, name string) meantTree {
	t.Helper()
	return meantTree{at: treePointer(c.place), tree: c.on(t, fileTree(t, base, name))}
}

// on is the JSON tree v, which the caller decoded for it alone, with the change c made in place, its
// value as encoding/json/v2 decodes it into any (a number as a float64). It shares no code of the
// package with the writers of the sweeps (treeWith, treeChanged and treeRenamed, which copy the tree
// on the path that they change), so that a writer that also writes at another place fails the read
// back.
func (c treeChange) on(t *testing.T, v any) any {
	t.Helper()
	c.value = jsonCopy(t, c.value)
	if len(c.place) == 0 && !c.out && !c.renamed {
		return c.value
	}
	if len(c.place) > 0 {
		parent, _ := treeAt(v, c.place[:len(c.place)-1])
		last := c.place[len(c.place)-1]
		switch p := parent.(type) {
		case map[string]any:
			x, held := p[last]
			switch {
			case !c.out && !c.renamed:
				p[last] = c.value
				return v
			case held:
				delete(p, last)
				if c.renamed {
					p[c.value.(string)] = x
				}
				return v
			}
		case []any:
			if i, err := strconv.Atoi(last); err == nil && i >= 0 && i < len(p) && !c.renamed {
				if !c.out {
					p[i] = c.value
					return v
				}
				return treeChange{place: c.place[:len(c.place)-1], value: slices.Delete(p, i, i+1)}.on(t, v)
			}
		}
	}
	t.Fatalf("the sweep means a change at %q, where the base holds no member or element", treePointer(c.place))
	return nil
}

// treeDiff is the first place, by the order of the names, where the JSON trees a and b differ (a
// member that one holds and the other does not, a list of another length, a value of another type
// or another value), and whether they differ.
func treeDiff(a, b any, at []string) ([]string, bool) {
	switch x := a.(type) {
	case map[string]any:
		y, object := b.(map[string]any)
		if !object {
			return at, true
		}
		names := slices.Collect(maps.Keys(x))
		for name := range y {
			if _, held := x[name]; !held {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		for _, name := range names {
			xv, inA := x[name]
			yv, inB := y[name]
			place := append(slices.Clone(at), name)
			if !inA || !inB {
				return place, true
			}
			if p, differ := treeDiff(xv, yv, place); differ {
				return p, true
			}
		}
		return nil, false
	case []any:
		y, list := b.([]any)
		if !list || len(x) != len(y) {
			return at, true
		}
		for i := range x {
			if p, differ := treeDiff(x[i], y[i], append(slices.Clone(at), strconv.Itoa(i))); differ {
				return p, true
			}
		}
		return nil, false
	}
	return at, !reflect.DeepEqual(a, b)
}

// copied is a copy of c that shares no object with c: its place cloned, and its value as jsonCopy
// gives it.
func (c treeChange) copied(t *testing.T) treeChange {
	t.Helper()
	c.place, c.value = slices.Clone(c.place), jsonCopy(t, c.value)
	return c
}

// treeWriter is a writer of a sweep's JSON file (changedFile): it gives the file's new tree from
// tree, the file's tree, and c, the change that the sweep means. Each is the writer's own: tree is
// decoded for it alone, and it may build its result in it; c is a copy of the sweep's change
// (treeChange.copied), which it may read and may not change. Each writer of a sweep is a function
// of the package, not a closure of the sweep, so that it can name no variable of the sweep.
type treeWriter func(tree any, c treeChange) any

// nullAt, leftOutAt, renamedAt and valueAt are the writers of the sweeps (treeWriter): the tree with
// null at c's place (the whole file null at no place), with the member or the element at c's place
// taken out, with the member at c's place under the name that c's value holds, and with c's value at
// c's place.
func nullAt(tree any, c treeChange) any    { return treeChanged(tree, c.place, false) }
func leftOutAt(tree any, c treeChange) any { return treeChanged(tree, c.place, true) }
func renamedAt(tree any, c treeChange) any { return treeRenamed(tree, c.place, c.value.(string)) }
func valueAt(tree any, c treeChange) any   { return treeWith(tree, c.place, c.value) }

// changedFile is a copy of base with the JSON file name written as the tree that the writer that
// build gives, a writer of a sweep, gives, read back against want, the change that the sweep means
// (filesRead). Each object that the writer is handed is its own: before it runs, changedFile builds
// the meant tree from want and from a decode of base of its own (treeChange.meant), and it hands
// the writer a tree of the file decoded for it alone and a copy of want (treeChange.copied). After
// the writer returns, it compares that copy, and want itself, with a copy of want taken before the
// call, as decoded values, so that a difference names its place (changeKept).
// So a writer that writes another value or at another place fails by the read back, and one that
// changes the value or the place that it is handed, before or after a right write, or a value of the
// package, where it is built or where it is called (writerCalled), fails by name too; and no later
// input of the sweep reads what a writer changed.
func changedFile(t *testing.T, base fstest.MapFS, name string, want treeChange, build func() treeWriter) fstest.MapFS {
	t.Helper()
	meant := want.meant(t, base, name)
	handed, before := want.copied(t), want.copied(t)
	var data []byte
	writerCalled(t, name, build, func(write treeWriter) { data = encodeTree(t, write(fileTree(t, base, name), handed)) })
	changeKept(t, name, meant.at, "the writer's copy of the change", handed, before)
	changeKept(t, name, meant.at, "the sweep's own change", want, before)
	fsys := maps.Clone(base)
	fsys[name] = &fstest.MapFile{Data: data}
	filesRead(t, base, fsys, nil, map[string]meantTree{name: meant})
	return fsys
}

// changeKept fails the test unless c, the copy of a change that changedFile handed a writer, or the
// sweep's own change, whose words give, holds after the writer ran what before, the copy that
// changedFile took before the call, holds: the same place, and the same value as encoding/json/v2
// decodes it (jsonCopy). A string cannot change through a copy of it; a place, or a value that is a
// list or an object, can.
func changeKept(t *testing.T, name, at, whose string, c, before treeChange) {
	t.Helper()
	if !slices.Equal(c.place, before.place) {
		t.Fatalf("the sweep changed %s at %q, and %s names the place %q after the writer ran, where it named %q", name, at, whose, treePointer(c.place), treePointer(before.place))
	}
	value := jsonCopy(t, c.value)
	if p, differ := treeDiff(value, before.value, nil); differ {
		x, _ := treeAt(value, p)
		w, _ := treeAt(before.value, p)
		t.Fatalf("the sweep changed %s at %q, and %s holds %s at %q of its value after the writer ran, where it held %s", name, at, whose, encodeTree(t, x), treePointer(p), encodeTree(t, w))
	}
}

// cloneFiles is a copy of fsys that shares no object with fsys: each file a MapFile of its own, with
// its bytes copied.
func cloneFiles(fsys fstest.MapFS) fstest.MapFS {
	c := make(fstest.MapFS, len(fsys))
	for name, file := range fsys {
		f := *file
		f.Data = slices.Clone(file.Data)
		c[name] = &f
	}
	return c
}

// fileWriter is a writer of a sweep's files (filesWritten): it changes the files that it is handed,
// a copy of the base of its own.
type fileWriter func(fsys fstest.MapFS)

// filesWritten is base as the writer that build gives, a writer of a sweep's files, leaves it, read
// back against written, the bytes of each file that the sweep means the writer to write, or nil to
// take it out, which the caller builds from base before the call (filesRead). The writer is handed
// a copy of base of its own (cloneFiles), which it changes, and the read back takes each other file
// from another copy of base, taken before the call, so that a writer that writes another file, or
// writes through a file that it is handed, fails by name, as does one that changes a value of the
// package where it is built or where it is called (writerCalled). The input that filesWritten gives
// is a third copy, taken after the read back, which no writer holds: so a writer that keeps the
// files that it was handed, and changes them in a later call, changes no input of the sweep. Each
// writer is the function that a function of the package gives (takenOut, textWritten,
// attributeOutOfFiles), of its own copies of what it writes, so that it can name no variable of the
// sweep.
func filesWritten(t *testing.T, base fstest.MapFS, written map[string][]byte, build func() fileWriter) fstest.MapFS {
	t.Helper()
	before, handed := cloneFiles(base), cloneFiles(base)
	writerCalled(t, fmt.Sprintf("the files %q", slices.Sorted(maps.Keys(written))), build, func(write fileWriter) { write(handed) })
	filesRead(t, before, handed, written, nil)
	return cloneFiles(handed)
}

// takenOut is the writer of the tree sweep that takes the file name out of the files that it is
// handed.
func takenOut(name string) fileWriter {
	return func(fsys fstest.MapFS) { delete(fsys, name) }
}

// fileTree is the JSON tree of the file name of fsys, decoded anew for its caller.
func fileTree(t *testing.T, fsys fstest.MapFS, name string) any {
	t.Helper()
	var tree any
	if err := json.Unmarshal(fsys[name].Data, &tree); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return tree
}

// placeMatches is whether the place at is the place pattern, whose "*" stands for any one name.
func placeMatches(pattern, at []string) bool {
	if len(pattern) != len(at) {
		return false
	}
	for i, name := range pattern {
		if name != "*" && name != at[i] {
			return false
		}
	}
	return true
}

// treeChanged is a copy of the JSON tree v with the value at place null, or, with leftOut, taken
// out of its object or its list.
func treeChanged(v any, place []string, leftOut bool) any {
	if len(place) == 0 {
		return nil
	}
	switch v := v.(type) {
	case map[string]any:
		c := maps.Clone(v)
		if len(place) == 1 && leftOut {
			delete(c, place[0])
		} else {
			c[place[0]] = treeChanged(v[place[0]], place[1:], leftOut)
		}
		return c
	case []any:
		i, _ := strconv.Atoi(place[0])
		if len(place) == 1 && leftOut {
			return slices.Delete(slices.Clone(v), i, i+1)
		}
		c := slices.Clone(v)
		c[i] = treeChanged(v[i], place[1:], leftOut)
		return c
	}
	panic(fmt.Sprintf("no value at %q", place))
}

// treeWith is a copy of the JSON tree v with x at place.
func treeWith(v any, place []string, x any) any {
	if len(place) == 0 {
		return x
	}
	switch v := v.(type) {
	case map[string]any:
		c := maps.Clone(v)
		c[place[0]] = treeWith(v[place[0]], place[1:], x)
		return c
	case []any:
		i, _ := strconv.Atoi(place[0])
		c := slices.Clone(v)
		c[i] = treeWith(v[i], place[1:], x)
		return c
	}
	panic(fmt.Sprintf("no value at %q", place))
}

// treeAt is the value of the JSON tree v at place, and whether v holds one there.
func treeAt(v any, place []string) (any, bool) {
	for _, name := range place {
		switch o := v.(type) {
		case map[string]any:
			x, held := o[name]
			if !held {
				return nil, false
			}
			v = x
		case []any:
			i, err := strconv.Atoi(name)
			if err != nil || i < 0 || i >= len(o) {
				return nil, false
			}
			v = o[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// treeRenamed is a copy of the JSON tree v with the member at place, the last name of place, under
// the name renamed in its object.
func treeRenamed(v any, place []string, renamed string) any {
	object := v
	for _, name := range place[:len(place)-1] {
		switch o := object.(type) {
		case map[string]any:
			object = o[name]
		case []any:
			i, _ := strconv.Atoi(name)
			object = o[i]
		}
	}
	c := maps.Clone(object.(map[string]any))
	c[renamed] = c[place[len(place)-1]]
	delete(c, place[len(place)-1])
	return treeWith(v, place[:len(place)-1], c)
}

// treePointer is the JSON pointer of place.
func treePointer(place []string) string {
	var b strings.Builder
	for _, name := range place {
		b.WriteString("/" + pointerToken.Replace(name))
	}
	return b.String()
}

// encodeTree is the JSON text of the tree v, with the members of each object in the order of their
// names, so that one tree has one text (inputKey).
func encodeTree(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// encodeTree writes one tree as one text, whatever the order in which Go gives the keys of a map,
// and the distinct-input check rests on it (inputKey): each of ten encodes of an object of 26
// members, decoded from a text that names them from z to a, is the text that names them from a to
// z. An encode in the order in which Go gives the keys of the map writes that text only by chance.
func TestEncodeTreeWritesOneTreeAsOneText(t *testing.T) {
	t.Parallel()
	var backward, forward []string
	for c := 'z'; c >= 'a'; c-- {
		backward = append(backward, fmt.Sprintf("%q:%d", string(c), c))
	}
	for c := 'a'; c <= 'z'; c++ {
		forward = append(forward, fmt.Sprintf("%q:%d", string(c), c))
	}
	want := "{" + strings.Join(forward, ",") + "}"
	for i := range 10 {
		var tree any
		if err := json.Unmarshal([]byte("{"+strings.Join(backward, ",")+"}"), &tree); err != nil {
			t.Fatal(err)
		}
		if got := string(encodeTree(t, tree)); got != want {
			t.Fatalf("encode %d of one tree is %s, where it should be %s", i+1, got, want)
		}
	}
}

// jsonCopy is v as encoding/json/v2 decodes its JSON text into any: a copy that shares no object
// with v, with a number as a float64.
func jsonCopy(t *testing.T, v any) any {
	t.Helper()
	var x any
	if err := json.Unmarshal(encodeTree(t, v), &x); err != nil {
		t.Fatal(err)
	}
	return x
}
