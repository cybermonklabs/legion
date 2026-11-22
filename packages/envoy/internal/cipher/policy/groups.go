package policy

import (
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// GroupOwned reports whether the set serves name as a secret a group owns. Who is a member of
// the group rests on the group reader's last read, which is not in Version, so a caller re-checks
// every grant of such a name on every use rather than only once Version moves.
func (s *Set) GroupOwned(name string) bool {
	return s.Secrets[name].ownership == ownerGroup
}

// GroupNames is every group a served secret names, as its owner or in its requesters, by bare
// address, each once and sorted: what a full load hands the group reader to read (Current's
// reload).
func (s *Set) GroupNames() []string {
	var names []string
	for _, secret := range s.Secrets {
		if group, ok := record.GroupOf(secret.Owner); ok && secret.ownership == ownerGroup {
			names = append(names, group)
		}
		_, _, listed := SplitRequesters(secret.Requesters)
		for _, entry := range listed {
			if group, ok := record.GroupOf(entry); ok {
				names = append(names, group)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Groups answers the group reader the set's decisions ask (Loader.Groups), and nil for a set no
// Current published, which admits nobody to a group.
func (s *Set) Groups() record.Groups {
	return s.groups
}
