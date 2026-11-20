# shellcheck shell=bash
# shellcheck disable=SC2154 # check-omp-offline.test.sh initializes the shared harness variables.

# Cases of the helpers themselves: a Go OMPOffline or a TypeScript ompOffline that stopped naming
# HTTPS_PROXY fails, and so does an environment helper (OMPEnviron, ompEnv) that stopped calling
# its own. Sourced by check-omp-offline.test.sh, whose fixtures, run_check, named and counters they
# use; not run on its own.

echo "case: a helper that stopped naming HTTPS_PROXY fails"
root=$(fixture helper_bare)
put "$root" packages/daemon/internal/testbin/omp.go <<'EOF'
package testbin

import "testing"

func OMPEnviron(t *testing.T, home string) []string {
	return []string{"HOME=" + home, OMPOffline(t)}
}

func OMPOffline(t *testing.T) string {
	return "PATH=/usr/bin"
}
EOF
put "$root" packages/pi-shared/test/omp-harness.ts <<'EOF'
function ompEnv(home: string): Record<string, string> {
  return { HOME: home };
}

export function spawnRpc(binary: string, home: string) {
  return Bun.spawn([binary, "--mode", "rpc"], {
    env: { ...ompEnv(home) },
  });
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the Go helper" "$(contains "$out" 'omp.go:9: no Go OMPOffline that names HTTPS_PROXY')"
check "names the environment helper that calls no ompOffline" "$(contains "$out" 'omp-harness.ts:1: ompEnv does not call ompOffline')"

echo "case: a TypeScript ompOffline that stopped naming HTTPS_PROXY fails, and so does a Go OMPEnviron that calls no OMPOffline"
root=$(fixture helper_unnamed)
put "$root" packages/pi-shared/test/omp-offline.ts <<'EOF'
export function ompOffline(cleanup: (() => Promise<void>)[]): { readonly HOME: string } {
  return { HOME: "/tmp/home" };
}
EOF
put "$root" packages/daemon/internal/testbin/omp.go <<'EOF'
package testbin

import "testing"

func OMPEnviron(t *testing.T, home string) []string {
	return []string{"HOME=" + home}
}

func OMPOffline(t *testing.T) string {
	return "HTTPS_PROXY=http://127.0.0.1:1"
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the TypeScript helper" "$(contains "$out" 'omp-offline.ts:1: no TypeScript ompOffline that names HTTPS_PROXY')"
check "names the Go environment helper that calls no OMPOffline" "$(contains "$out" 'omp.go:5: OMPEnviron does not call OMPOffline')"
