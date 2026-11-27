package api

import (
	"regexp"
)

// Test helpers the S3c-documents tests read; the rest of this file is S3c-issues's.

// timestampsAndIDs matches what differs between two runs of one seed: an RFC 3339 time and a uuid.
var timestampsAndIDs = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
