package pgguard

// ForgetLoggedRefusalsForTest forgets every calling stack that has logged a refusal
// (loggedSites), so the next refusal of each logs its line again. It is for tests alone: a test
// that counts refusal lines calls it before it counts, so a test the binary runs again (-count)
// logs its own stacks again rather than finding them already seen.
func ForgetLoggedRefusalsForTest() {
	loggedSites.Lock()
	clear(loggedSites.seen)
	loggedSites.Unlock()
}
