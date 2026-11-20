# shellcheck shell=bash disable=SC2034  # the sourcing script uses these variables
# The POSIX sh programs a live proof runs in an issue pod's `roles` container to reach one role.
# Sourced by stage4b-sandbox-tree.sh, share-volumes-kind.sh and claude-backend-sandbox.sh.
#
# An issue pod has one container, `roles`, whose PID 1 is the launcher of every role; each role's
# generation runs in Linux user, mount, PID and IPC namespaces of its own, whose init is that
# generation's `legion launcher role-init --role <role> …` (internal/launcher/roleinit.go), which
# binds the role's own gh, private, state and agent-secrets directories at the fixed paths under
# /var/run/legion, covers every role's directory of the roles directory, and runs the role's
# `legion worker-shim` (with its Oh My Pi) in a nested user namespace as the image's user.
# role_enter_sh is the POSIX sh, run in the `roles` container as `sh -c "$role_enter_sh" role-enter
# ROLE COMMAND…`, that runs COMMAND as ROLE's agent sees the pod: it finds ROLE's role-init by its
# argv (`launcher role-init`, then `--role ROLE` before the shim's `--`), enters that init's user,
# mount, PID and IPC namespaces (the container's user owns them, so nsenter needs no capability the
# container lacks), finds the role's worker-shim there (the process whose parent is the namespaces'
# PID 1 and whose argv[1] is worker-shim), and enters the shim's own user namespace, the agent's,
# uid 1000. COMMAND then sees the role's /proc (its role-init as PID 1, its shim and agent, nothing
# of another role), its /tmp and its gh bundle; stdin and the exit status pass through. gVisor
# refuses a PID namespace entered through the shim's user namespace, so the two hops are in that
# order (measured in a gVisor pod of the worker image, 2026-10-24). No role-init of ROLE, or two of
# them (a generation still ending as the next starts), is exit 1 naming why, as a wait expects.
# Every program here reads `${proc_root:-/proc}`: the container's own /proc, unless a test points
# proc_root at a fake tree, since a test scanning the host's /proc would find the worker-shim and
# Oh My Pi of any Legion role running on that host.
# role_shim_sh is role_enter_sh's second hop, run inside the role-init's namespaces as
# `sh -c "$role_shim_sh" role-shim ROLE COMMAND…`: it finds the role's worker-shim (the process
# whose parent is the namespaces' PID 1 and whose argv[1] is worker-shim, and no later argv word)
# and runs COMMAND in the shim's user namespace. No shim, or two, is exit 1 naming why. It holds no
# single quote, since role_enter_sh carries it single-quoted.
# shellcheck disable=SC2016  # expanded by the pod's shell
role_shim_sh='
role=$1
shift
shim=
for d in "${proc_root:-/proc}"/[0-9]*; do
  [ "$(sed -n "s/^PPid:[[:space:]]*//p" "$d/status" 2>/dev/null)" = 1 ] || continue
  [ "$(tr "\0" "\n" 2>/dev/null <"$d/cmdline" | sed -n 2p)" = worker-shim ] || continue
  if [ -n "$shim" ]; then
    echo "two worker-shims run in the namespaces of $role: $shim and ${d##*/}" >&2
    exit 1
  fi
  shim=${d##*/}
done
if [ -z "$shim" ]; then
  echo "no worker-shim runs in the namespaces of $role" >&2
  exit 1
fi
exec nsenter -t "$shim" -U --preserve-credentials -- "$@"
'
# shellcheck disable=SC2016  # expanded by the pod's shell
role_enter_sh='
role=$1
shift
init=
for d in "${proc_root:-/proc}"/[0-9]*; do
  tr "\0" "\n" 2>/dev/null <"$d/cmdline" | awk -v role="$role" "
    NR == 2 && \$0 != \"launcher\" { bad = 1; exit }
    NR == 3 && \$0 != \"role-init\" { bad = 1; exit }
    \$0 == \"--\" { exit }
    after && \$0 == role { found = 1 }
    { after = (\$0 == \"--role\") }
    END { exit bad || !found }" || continue
  if [ -n "$init" ]; then
    echo "two role-init processes of $role run in this container: ${init} and ${d##*/}" >&2
    exit 1
  fi
  init=${d##*/}
done
if [ -z "$init" ]; then
  echo "no role-init of $role runs in this container" >&2
  exit 1
fi
exec nsenter -t "$init" -U -m -p -i --preserve-credentials -- sh -c '"'$role_shim_sh'"' role-shim "$role" "$@"
'
# role_kill_sh, run in the `roles` container as `sh -c "$role_kill_sh" role-kill INIT`, sends
# SIGKILL to the process group of the worker-shim role-init INIT runs: the shim is the child of
# role-init whose argv[1] is worker-shim (as role_enter_sh finds it; role-init reaps every orphan
# of its namespace, so a process the agent detached is a child of it too and is skipped), started
# in a process group of its own (Setpgid, internal/launcher/roleinit_linux.go runRoleShim), and Oh
# My Pi, the shim's child, is in that group. The container's user owns the role's user namespaces,
# so it may signal them. No shim, two of them, or a shim that is not the leader of its own group
# is refused rather than signalled, since that group would be someone else's. It reads INIT's own
# children from INIT's own /proc entry (every thread's task/<tid>/children, as proc_children in
# lib/process-tree.sh reads them, since a Go role-init forks from whichever thread runs the call),
# and no other process's entry: a test on a devbox, whose /proc holds 22,000 to 30,000 processes,
# ran a read of every PPid past its 120 s bound at a load of 542. Only where /proc lists no children
# file (a sandbox's procfs may not, or a kernel built without CONFIG_PROC_CHILDREN) does it read
# every process's PPid, in one grep (-s: a process that ends between the listing and the read is no
# child): there the /proc is the pod's own, which holds the pod's processes alone. It reads
# `${proc_root:-/proc}`, as every program here does, and sends its signal with `${role_kill:-kill}`:
# a test of it over a fake tree names a stand-in kill there, since a group the fake tree names is a
# real group's number on the host (kubectl exec hands the pod's sh neither variable, so the pod
# always reads its /proc and runs kill).
# shellcheck disable=SC2016  # expanded by the pod's shell
role_kill_sh='
init=$1 shim= proc=${proc_root:-/proc} listed= children=
for list in "$proc/$init/task/"*/children; do
  [ -e "$list" ] || continue
  listed=1
  children="$children $(cat "$list" 2>/dev/null)"
done
[ -n "$listed" ] || children=$(cd "$proc" && grep -s -H "^PPid:" [0-9]*/status | awk -F "[/:\t ]+" -v init="$init" "\$4 == init { print \$1 }")
for c in $children; do
  [ "$(tr "\0" "\n" 2>/dev/null <"$proc/$c/cmdline" | sed -n 2p)" = worker-shim ] || continue
  [ -z "$shim" ] || { echo "role-init $init runs two worker-shims: $shim and $c" >&2; exit 1; }
  shim=$c
done
[ -n "$shim" ] || { echo "role-init $init runs no worker-shim" >&2; exit 1; }
group=$(sed "s/.*) //" "$proc/$shim/stat" | cut -d" " -f3)
[ "$group" = "$shim" ] || { echo "the shim $shim of role-init $init is in process group $group, not its own" >&2; exit 1; }
# no-wait: the last act; the caller waits, bounded, for the daemon to see the generation end.
"${role_kill:-kill}" -s KILL -- "-$group"'
