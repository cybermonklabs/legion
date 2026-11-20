package natscallout

import "github.com/sjawhar/envoy/natsgrant"

// scopeGrant is grant, entry's kind's grant, held to the projects entry may reach. An all-projects
// entry takes the grant whole. An edge listener's entry lists its projects, and natsgrant holds
// no grant of an edge listener's to hold to them, so its connection is refused by name. Any other
// entry would have to be held to the projects the broker's own exchange list gives its account
// and the repositories Dispatch maps them to, which this service does not read, so it refuses
// that entry's connection by name rather than admit it to every project.
func scopeGrant(entry ServiceEntry, grant natsgrant.Permissions) (natsgrant.Permissions, error) {
	if entry.Kind == KindEdgeListener {
		return natsgrant.Permissions{}, refuse(labelHeldToProjects, "held to the projects its entry lists, for which this broker has no edge listener's grant: service "+entry.name())
	}
	if entry.AllProjects {
		return grant, nil
	}
	return natsgrant.Permissions{}, refuse(labelHeldToProjects, "held to exchange projects, which this broker cannot read: service "+entry.name())
}
