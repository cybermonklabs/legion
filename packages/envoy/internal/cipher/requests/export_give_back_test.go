package requests

// WaitGiveBacks waits for every give-back the machine started in the background (giveBack), so a
// test reads what they did at GitHub only once they are done. It exists in tests alone.
func (m *Machine) WaitGiveBacks() { m.givingBack.Wait() }
