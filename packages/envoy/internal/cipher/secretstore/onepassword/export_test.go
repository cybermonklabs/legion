package onepassword

import "time"

// SetReadLag makes s trust its kept items for d after its last List, in place of ReadLag: the
// contract's mutant of a store whose reads trust their kept items longer than it documents.
func SetReadLag(s *Store, d time.Duration) { s.readLag = d }
