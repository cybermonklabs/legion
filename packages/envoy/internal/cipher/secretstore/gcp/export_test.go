package gcp

import "time"

// HeldForForTest is gcpHeldFor, how long Read trusts a metadata verdict, for the tests outside the
// package that time it.
const HeldForForTest = gcpHeldFor

// MetadataAttemptsForTest is gcpMetadataAttempts, how many times a metadata call is made before an
// UNAVAILABLE answer is returned, for the tests outside the package that count them.
const MetadataAttemptsForTest = gcpMetadataAttempts

// SetClockForTest has s's held verdicts (Read's metadata cache) read the time from now.
func SetClockForTest(s Store, now func() time.Time) { s.held.now = now }
