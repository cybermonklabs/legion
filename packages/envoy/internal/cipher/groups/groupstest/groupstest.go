// Package groupstest is a stand-in for the broker's group reader (groups.Reader) in the tests of
// the packages that ask it who is a member: each group's members are what a test sets, a group
// is fresh until a test marks it stale or forgets it, and each Want call is recorded. It imports
// no broker package, so record's own tests can use it.
package groupstest

import (
	"slices"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/fold"
)

// Directory answers the methods of record.Directory from what a test set. A group it was never
// told of, and one marked stale, admits nobody, is not fresh, and is in no login's GroupsOf.
type Directory struct {
	mu sync.Mutex
	// groups is each group a test set, by bare address, with its members by canonical login.
	groups map[string]map[string]struct{}
	// stale is each group a test marked stale.
	stale map[string]bool
	wants [][]string
}

// New answers a Directory that knows of no group.
func New() *Directory {
	return &Directory{groups: map[string]map[string]struct{}{}, stale: map[string]bool{}}
}

// canonical is record.CanonicalLogin's form, which this package cannot import.
func canonical(login string) string { return fold.ASCII(strings.TrimSpace(login)) }

// Set makes group fresh with exactly members.
func (d *Directory) Set(group string, members ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	set := make(map[string]struct{}, len(members))
	for _, m := range members {
		set[canonical(m)] = struct{}{}
	}
	d.groups[group] = set
	delete(d.stale, group)
}

// Drop takes login out of group, which stays fresh.
func (d *Directory) Drop(group, login string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.groups[group], canonical(login))
}

// MarkStale makes group stale: its last good read is past the two-hour cut-off, so it admits
// nobody and is not fresh, though its members are kept for a later Set to replace.
func (d *Directory) MarkStale(group string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stale[group] = true
}

// Wants answers each Want call's names, in the order they came.
func (d *Directory) Wants() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]string, len(d.wants))
	for i, w := range d.wants {
		out[i] = slices.Clone(w)
	}
	return out
}

// fresh is Fresh with d.mu held.
func (d *Directory) fresh(group string) bool {
	_, known := d.groups[group]
	return known && !d.stale[group]
}

// Membership answers whether group's members, as a test last set them, list login, whatever the
// group's freshness, and whether the group is fresh: set and not marked stale since.
func (d *Directory) Membership(group, login string) (listed, fresh bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, listed = d.groups[group][canonical(login)]
	return listed, d.fresh(group)
}

// GroupsOf answers every fresh group that lists login, sorted.
func (d *Directory) GroupsOf(login string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	login = canonical(login)
	var names []string
	for name, members := range d.groups {
		if _, ok := members[login]; ok && d.fresh(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Want records names. It reads nothing: a group is what the test sets.
func (d *Directory) Want(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wants = append(d.wants, slices.Clone(names))
}
