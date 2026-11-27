package leakguard

// PushNamesReceivePackVar is the environment variable the pod's git wrapper
// (packages/daemon/docker/git) exports to 1 before it runs the real git for a push whose command
// line can name its own receive-pack program: any argument after the subcommand matching `--<P>` or
// `--<P>=<value>` where `<P>` is a non-empty prefix of `receive-pack` or of `exec`. git hands its
// environment to the pre-push hook, where Guard.StaysLocal treats the variable being set — with any
// value, empty included — as a push that cannot be cleared as local. The wrapper never unsets it,
// so a caller that sets it by hand only makes a push stricter.
const PushNamesReceivePackVar = "LEGION_PUSH_NAMES_RECEIVE_PACK"
