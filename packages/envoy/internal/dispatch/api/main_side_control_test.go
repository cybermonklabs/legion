package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// Scratch control, never merged. This commit sits on main d57c7a79, which has no package access;
// main 8ce41950 has it (#4674). A vet of this commit alone cannot compile this file, and a vet of
// this commit merged into main 8ce41950 can: so the two exits tell whether the gate built main's
// side of the merge.
func TestMainSideControl(t *testing.T) { _ = access.PrivateProjectsHeader }
