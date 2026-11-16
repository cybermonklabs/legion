package policy

import "time"

// SetNow makes now the clock c dates its rereads by and expires them against.
func SetNow(c *Current, now func() time.Time) {
	c.mu <- struct{}{}
	defer c.unlock()
	c.now = now
}

// ForgetLegacyTypeTagLines forgets every secret id this process has written LegacyTypeTagMessage
// for, so a test that counts those lines counts its own whatever ran before it in the process.
func ForgetLegacyTypeTagLines() { legacyTypesLogged.Clear() }
