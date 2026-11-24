package api

// What this package's external tests (package api_test) read of its own tests and of its
// unexported state route: composed_state_test.go composes the state fixtures from a seeded
// database, by the daemon's own calls, with the settings files the goldens here load and the read
// the state route makes beside its source (server.go's projectState, lowestPluginContract).
var (
	// UpdateGolden is this package's `-update` flag.
	UpdateGolden = updateGolden
	// SettingsFixtureFile and MinimalSettingsFile are the two legion.yaml files the goldens load,
	// and LoadSettingsFile loads one as the daemon does.
	SettingsFixtureFile = settingsFixtureFile
	MinimalSettingsFile = minimalSettingsFile
	LoadSettingsFile    = loadSettingsFile
	// GoldenPath is the path of a daemon-api fixture.
	GoldenPath = goldenPath
	// LowestPluginContract is the state route's own read of the snapshot's claims.
	LowestPluginContract = lowestPluginContract
	// GoldenPause is the pause the pause routes' goldens answer, which the last-pause fixture's
	// seed writes through the store's own writers.
	GoldenPause = goldenPause
)

// ComposedStatePath and ComposedStateRewrite are the populated state's composed file and the two
// commands that write it and the fixtures read from it, in that order.
const (
	ComposedStatePath    = composedStatePath
	ComposedStateRewrite = composedStateRewrite
)
