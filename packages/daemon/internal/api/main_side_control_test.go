package api

import "testing"

// Scratch control, never merged. This commit sits on main ac1c9a33, which has no
// ContractOrderStamped; main 8b6b6b44 has it (#5399). A vet of this commit alone cannot compile
// this file, and a vet of it merged into main 8b6b6b44 can: so the two exits tell whether the
// gate built main's side of the merge.
func TestMainSideControl(t *testing.T) { _ = ContractOrderStamped() }
