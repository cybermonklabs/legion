# packages/envoy/scripts/lib/ca-policy.jq
# The ca stand's policy a profiles file gives, as POST <ca URL>/policy takes it: the names of the
# file's bound hosts, a "*." pattern as its domain, each once, sorted. dev-airlock.sh applies it
# before the brokers start and proxy-chain-check.sh before a roll, as a deploy applies the pool's
# issuance policy before it rolls the brokers.
{names: ([.kinds[].hosts[]? | sub("^\\*\\."; "")] | unique)}
