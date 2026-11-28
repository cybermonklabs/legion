package gcp

import (
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
)

// HeldForForTest is gcpHeldFor, how long Read trusts a metadata verdict, for the tests outside the
// package that time it.
const HeldForForTest = gcpHeldFor

// MetadataAttemptsForTest is gcpMetadataAttempts, how many times a metadata call is made before an
// UNAVAILABLE answer is returned, for the tests outside the package that count them.
const MetadataAttemptsForTest = gcpMetadataAttempts

// SetClockForTest has s's held verdicts (Read's metadata cache) read the time from now.
func SetClockForTest(s Store, now func() time.Time) { s.held.now = now }

// RecordMetadataPausesForTest has client's metadata calls retry as NewClient has them retry
// (metadataRetry), with each retry's pause handed to record rather than waited out: a test reads
// every pause the store would wait out, and waits none. It comes after NewClient's own retry in
// each call's options, and the last retry option of a call is the one gax uses.
func RecordMetadataPausesForTest(client *secretmanager.Client, record func(time.Duration)) {
	retry := metadataRetry(func(pause time.Duration) time.Duration {
		record(pause)
		return 0
	})
	client.CallOptions.ListSecrets = append(client.CallOptions.ListSecrets, retry)
	client.CallOptions.GetSecret = append(client.CallOptions.GetSecret, retry)
	client.CallOptions.GetSecretVersion = append(client.CallOptions.GetSecretVersion, retry)
}
