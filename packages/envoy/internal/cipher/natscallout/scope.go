package natscallout

import (
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/natsgrant"
)

// scopeGrant is grant, entry's kind's grant, held to the projects entry may reach. An all-projects
// entry takes the grant whole, and so does an edge listener's: its grant is already built for the
// projects its entry lists (serviceGrant, natsgrant.Edge), which name its role subjects and their
// exception lanes, and no other subject or key of it names a project yet, so its read is the whole
// bus. Any other entry would have to be held to the projects the broker's own exchange list gives
// its account and the repositories Dispatch maps them to, which this service does not read, so it
// refuses that entry's connection by name rather than admit it to every project.
func scopeGrant(entry config.NATSService, grant natsgrant.Permissions) (natsgrant.Permissions, error) {
	if entry.AllProjects || entry.Kind == config.NATSEdgeListener {
		return grant, nil
	}
	return natsgrant.Permissions{}, refuse(labelHeldToProjects, "held to exchange projects, which this broker cannot read: service "+entry.Name())
}
