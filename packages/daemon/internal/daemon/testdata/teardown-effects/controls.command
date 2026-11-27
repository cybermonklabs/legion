Captured 2026-11-27T06:30:58Z from the real daemon through the real Oh My Pi Legion tool, in a public build Job of kind proof (its controls step, exit 0), from commit 1098e27eaf18b355b7a8b883360e10807507d9b8 with the staged patch of lane D's controls (SHA-256 cae231be673d96aec8fdac307f914381cbdc4169d7dc7437b0cf7353d8d07966). The evidence archive's SHA-256 is 4c5c3f0b08b6fa54f108f0ac9a6149d31fee49f081c669a50eb351535f90813f. No model was called.

TEARDOWN_EFFECTS_EVIDENCE_DIR=<dir> bash scripts/e2e/teardown-effects.sh --controls --cases packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json

controls/ holds the four trace directories of <dir> as the run wrote them: P05/good, P05/bad, P10/good and P10/bad, each with the trace's replay.json and effects.json under the trace's own id. The run's other files (its logs, daemon-answers.json and the effect records of the completion made before the push) are not kept here.
