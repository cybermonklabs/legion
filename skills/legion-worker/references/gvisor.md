# What gVisor changes in your pod

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
- Set the stack with `ulimit -S -s <n>` first, the hard limit only where it reads `unlimited`, then read both back and refuse a mismatch: `unlimited` gives 8 MiB under gVisor, which caps the stack at 128 MiB.
- Compare durations within one pod only, and label every figure taken in a gVisor pod a gVisor figure.
