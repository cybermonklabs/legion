# Whole outputs of operator-token-minters.sh check runs, for want_recorded (INFRA-6357)

Each file is the output one check run gave the harness's `run_only` in `$output` (stdout and
stderr, as `$(...)` keeps it), at cybermonklabs/legion `98caff1b0a6465bef6fc4c6a9354d643dcdb4ffd`,
captured on 2026-11-28 with the pod's temporary directory `/legion/scratch/tmp/` written `/tmp/`
(the only change; no FINDING, UNKNOWN or last line holds it).

Capture command (check-replay.sh version 4, dispatch://INFRA-6357/artifact/check-replay-sh, patches a
copy of the scripts so that each check run writes a record `<out>/rows-<file>.<n>`):

    d=$(mktemp -d)
    bash -c 'eval "$(sed -n "/^REPLAY_WRAP_BLOCK=/,/^export REPLAY_WRAP_BLOCK/p" check-replay.sh)"
      source <(sed -n "/^patch_harness() {/,/^}/p" check-replay.sh); patch_harness "$1" "$2"' \
      _ packages/envoy/infra/scripts "$d/s"
    mkdir -p "$d/out"
    REPLAY_OUT="$d/out" REPLAY_FAKE_CALLS= OPERATOR_MINTERS_TEST_SLOTS=1 \
      OPERATOR_MINTERS_TEST_ROWS="a u h ba al b fb" bash "$d/s/operator-token-minters.test.sh"
    # the two of rows (bc) and (co), in a second out dir of the same patched copy:
    mkdir -p "$d/out2"
    REPLAY_OUT="$d/out2" REPLAY_FAKE_CALLS= OPERATOR_MINTERS_TEST_SLOTS=1 \
      OPERATOR_MINTERS_TEST_ROWS="bc co" bash "$d/s/operator-token-minters.test.sh"
    # each fixture: awk '/^--- output$/{o=1;next} /^--- report$/{o=0} o' "$d/out/<record>" | sed 's#/legion/scratch/tmp/#/tmp/#g'

| File | Record | Row | FINDING lines | UNKNOWN lines | Last line |
| --- | --- | --- | --- | --- | --- |
| exit0-row-a.txt | rows-run.0001 | (a) | 0 | 0 | `operator-token-minters: no finding and nothing unknown: exit 0` |
| exit1-one-finding-row-u.txt | rows-rbac.0002 | (u) | 1 | 0 | `operator-token-minters: 1 finding(s), 0 unknown: exit 1` |
| exit1-53-findings-row-b.txt | rows-rbac.0001 | (b) | 53 | 0 | `operator-token-minters: 53 finding(s), 0 unknown: exit 1` |
| exit1-findings-and-unknowns-row-fb.txt | rows-fence.0008 | (fb), its eighth item | 6 | 2 | `operator-token-minters: 6 finding(s), 2 unknown: exit 1` |
| exit2-one-unknown-row-h.txt | rows-run.0002 | (h) | 0 | 1 | `operator-token-minters: no finding, 1 unknown: exit 2` |
| exit2-ten-unknowns-row-ba.txt | rows-run.0005 | (ba), `ancestors` | 0 | 10 | `operator-token-minters: no finding, 10 unknown: exit 2` |
| stopped-before-end-row-al.txt | rows-run.0003 | (al) | 0 | 1 | `operator-token-minters: stopped before its end, 1 unknown: exit 2` (the crash report, not the exit block) |
| usage-error-row-al.txt | rows-run.0004 | (al) | 0 | 0 | the usage text: no summary line at all |
| no-step-ran-row-bc.txt | rows-run.0001 of out2 | (bc), `output.sh` | 0 | 1 | `operator-token-minters: no step ran, 1 unknown: exit 2` (a copy missing a part: `operator-token-minters.sh:713-723`, not the exit block) |
| report-unwritable-row-co.txt | rows-run.0029 of out2 | (co), `report-unwritable` (pasted_row: the row holds stdout, then stderr) | 0 | 1 | `operator-token-minters: --report cannot be written` (`output.sh:59`, after the crash report's `stopped before its end, 1 unknown: exit 2`) |

From dispatch://INFRA-6357/artifact/minters-summary-fixtures-tar-gz version 2; row (df) of `operator-token-minters/test-rows-run.sh` reads them.
