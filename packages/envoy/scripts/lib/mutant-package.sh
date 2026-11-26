# shellcheck shell=bash
# packages/envoy/scripts/lib/mutant-package.sh
#
# The package a mutant name changes. airlock-mutants.sh uses it to find, classify and test a
# patch; airlock-mutants-apply.test.sh uses the same list to hold every patch to applying.
# Sourced, never run.

# mutant_package answers proxyrules, proxy, policy or requests for a mutant name. Every name not
# prefixed by the latter three changes the airlock's Go or Lean model under proxyrules.
mutant_package() {
	case "$1" in
	proxy-*) printf '%s\n' proxy ;;
	policy-*) printf '%s\n' policy ;;
	requests-*) printf '%s\n' requests ;;
	*) printf '%s\n' proxyrules ;;
	esac
}

# mutant_packages writes every package that holds mutant patches, once and in the order the runner
# and its apply test use them.
mutant_packages() {
	printf '%s\n' proxyrules proxy policy requests
}
