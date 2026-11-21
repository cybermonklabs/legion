// Package fixture calls nats.go's package NewInbox under another import name, which
// TestNoPackageNewInbox's walk must name.
package fixture

import natsgo "github.com/nats-io/nats.go"

// Inbox is a subject every _INBOX.> subscriber reads.
func Inbox() string {
	return natsgo.NewInbox()
}
