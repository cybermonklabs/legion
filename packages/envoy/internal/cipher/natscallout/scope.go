package natscallout

import "github.com/sjawhar/envoy/natsgrant"

// scopeGrant is grant, entry's kind's grant, held to the projects entry may reach. An all-projects
// entry takes the grant whole. Any other entry would have to be held to the projects the broker's
// own exchange list gives its account, which this service does not read, so it refuses that
// entry's connection by name rather than admit it to every project.
func scopeGrant(entry ServiceEntry, grant natsgrant.Permissions) (natsgrant.Permissions, error) {
	if entry.AllProjects {
		return grant, nil
	}
	return natsgrant.Permissions{}, refuse(labelHeldToProjects, "held to exchange projects, which this broker cannot read: service "+entry.name())
}
