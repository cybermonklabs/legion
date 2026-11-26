package policy

import (
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// SetNow makes now the clock c dates its rereads by and expires them against.
func SetNow(c *Current, now func() time.Time) {
	c.mu <- struct{}{}
	defer c.unlock()
	c.now = now
}

// ForgetLegacyTypeTagLines forgets every secret id this process has written LegacyTypeTagMessage
// for, so a test that counts those lines counts its own whatever ran before it in the process.
func ForgetLegacyTypeTagLines() { legacyTypesLogged.Clear() }

// NewSetWithGroups is NewSet carrying groups as its group reader, as a set Current publishes
// carries the loader's, for a test that decides on a set it built itself.
func NewSetWithGroups(secrets map[string]Secret, groups record.Directory) *Set {
	set := NewSet(secrets)
	set.groups = groups
	return set
}
