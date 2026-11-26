//go:build !linux

package launcher

// boundHomeMounts answers no mount under dir: off Linux no role-init runs (roleinit_other.go), so
// nothing is bound back under a role's home.
func boundHomeMounts(dir string) (string, []string, error) { return dir, nil, nil }
