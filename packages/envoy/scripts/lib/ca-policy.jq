# packages/envoy/scripts/lib/ca-policy.jq
# The ca stand's policy a profiles file gives, as POST <ca URL>/policy takes it: the names of the
# file's bound hosts, a "*." pattern as its domain, each once, sorted. dev-airlock.sh applies it
# before the brokers start and proxy-chain-check.sh before a roll, as a deploy applies the pool's
# issuance policy before it rolls the brokers.
#
# The bound hosts are those boundHosts (cmd/cipherd/main.go) asks each intermediate to permit,
# and checkBounds holds it to exactly them: each kind's hosts, and each dpop block's hosts, which
# stay under profiles[].proxy as an object. A profile whose proxy is a kind's name (a string)
# binds no host of its own, and a file may hold no kinds or no profiles at all.
{names: ([(.kinds[]?.hosts[]?), (.profiles[]?.proxy | objects | .hosts[]?)] | map(sub("^\\*\\."; "")) | unique)}
