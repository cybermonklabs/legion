package supervise

// suspensionQueued is whether a suspension of m's claim waits for the machine's lock.
func suspensionQueued(m *Machine) bool { return m.suspendsWaiting.Load() > 0 }
