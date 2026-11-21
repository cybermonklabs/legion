# What gVisor changes in your pod

Read it before you walk a path one component at a time, trust a tool's binary or set a stack limit in your pod, and before you report a duration measured there.

Issue pods run under gVisor unless the deployment's `runtime.kubernetes.sandbox` puts the issue in a GKE Sandbox microVM (`docs/kubernetes.md`, "Issue pods in microVMs"). These items are about a gVisor pod.

- Under gVisor `openat` with `O_DIRECTORY|O_NOFOLLOW` follows a link to a directory, and `openat2` can return `ENOSYS`. Open each path component with `O_PATH|O_NOFOLLOW|O_CLOEXEC` and read its kind with `fstat`.
- Read off a tool's binary whether it calls `openat2`:

  ```sh
  b=$(readlink -f "$(command -v <tool>)")
  n=$(nm -D --undefined-only "$b" | wc -l)
  bad=$(nm -D --undefined-only "$b" | grep -c openat2 || true)   # grep -c exits 1 on no match
  [ "$n" -gt 0 ] || { echo "no dynamic symbols: static binary or nm failed, use a tracer"; exit 2; }
  [ "$bad" -eq 0 ] || { echo "this binary calls openat2 and fails under gVisor"; exit 2; }
  ```

  An undefined `openat2` symbol proves the binary calls it. Its absence proves nothing: a call through `syscall(SYS_openat2, …)` leaves only `syscall` in the table. A total of zero is a static binary or a failed `nm`, not a clean result. To show a binary safe, trace it.
- Set the stack with `ulimit -S -s <n>` first, the hard limit only where it reads `unlimited`, then read both back and refuse a mismatch: `unlimited` gives 8 MiB under gVisor, which caps the stack at 128 MiB. 131072 is the number for an ordinary job; name a larger one when a tool you run polices its own limit and demands it:

  ```sh
  want=131072   # a tool with its own stack guard may demand more; use its minimum
  ulimit -S -s "$want" 2>/dev/null || { ulimit -H -s "$want" 2>/dev/null || true; ulimit -S -s "$want"; }
  { [ "$(ulimit -Hs)" = unlimited ] && ulimit -H -s "$want" 2>/dev/null; } || true
  soft=$(ulimit -Ss); hard=$(ulimit -Hs)
  [ "$soft" = "$want" ] || { echo "stack soft is $soft, want $want; refusing"; exit 2; }
  [ "$hard" != unlimited ] && [ "$hard" -ge "$want" ] || { echo "stack hard is $hard; refusing"; exit 2; }
  ```

  The soft limit must equal what you asked for, and the hard limit must be finite and no lower. Each part of that setter answers a failure, and a simpler form breaks one of them:
  - The soft limit is set first, because lowering the hard limit below a soft `unlimited` is `EINVAL`: a hard set made first fails there, a soft set chained after it with `&&` never runs, and the process keeps `unlimited` and gets 8 MiB.
  - The hard limit is set only when it reads `unlimited`, because setting it unconditionally lowers a higher finite hard limit the pod gave you, the headroom a tool with its own guard may need.
  - Keep the `|| true` around that line: where the hard limit is already finite its test is false, and a bare `&&` list whose test fails exits non-zero, so as a script's last line it exits 1 with everything correct.
  - The fallback covers a want above the current hard limit, where the soft set fails: it raises the hard limit, then sets the soft one.
  - Do not reduce it to a single `ulimit -s <n>`: whether that also sets the hard limit differs from shell to shell.

  The ceiling is 128 MiB whichever finite value you choose, and a larger soft limit costs rather than buys: glibc sizes a default thread stack from it, and a tool that creates a thread pool can fail to allocate one.
- Compare durations within one pod, never across pods: a pod lands on whatever node the autoscaler provisions, so two pods are two machines, two sets of neighbours and two moments. Run both arms in one pod, as two containers on one node started together, and a difference between them is a difference between the arms. Label every figure taken in a gVisor pod a gVisor figure. The gVisor slowdown is the cost of syscalls and no budget for a whole job: compute-bound work can run as fast or faster there.
