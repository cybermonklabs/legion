/**
 * argMaxForceHolds() says whether this machine's kernel holds an exec to ARG_MAX under a 512 KiB
 * stack, the force lib/arg-max-force.sh puts a row under: an environment of two 100,000-byte
 * variables, each under the kernel's limit for one string, together past the 128 KiB that stack
 * allows, must make the exec of a program of no arguments fail. Linux holds it. gVisor (a Legion
 * pod's kernel, 4.19.0-gvisor) starts that program, so there the force cannot be put in force, and a
 * row under it proves nothing either way: the rows that need it are gated on it (hostGate's reason),
 * skipped there by this name outside CI and failed by name under GITHUB_ACTIONS.
 */
let holds: boolean | undefined;

export function argMaxForceHolds(): boolean {
  if (holds === undefined) {
    const probe = Bun.spawnSync(
      [
        "bash",
        "-c",
        'ulimit -s 512 && a=$(printf "%100000s" "") && PADDING1=$a PADDING2=$a "$(type -P true)"',
      ],
      { env: { PATH: process.env.PATH ?? "" }, stdout: "ignore", stderr: "ignore" }
    );
    holds = probe.exitCode !== 0;
  }
  return holds;
}
