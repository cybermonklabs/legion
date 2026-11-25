# The contracts filter's gate: what job contracts reads.
GATE = gate("contracts", "contracts", reads=reads(
    ("packages/contracts/**", f"{at('contracts', 'Typecheck contracts')}, test (src/, scripts/gen-go.test.ts) and build in packages/contracts"),
    ("skills/dispatch/SKILL.md", "packages/contracts/src/dispatch-tools.test.ts's dispatchSkillHeadings reads skills/dispatch/SKILL.md headings"),
    ("skills/dispatch-writing-specs/SKILL.md", "packages/contracts/src/dispatch-tools.test.ts:1565 existsSync(skills/dispatch-writing-specs/SKILL.md)"),
    (f"{ENVOY}/internal/dispatch/text/testdata/dispatch-href-references.json", "packages/contracts/src/dispatch-href.test.ts:51-55"),
    (f"{ENVOY}/internal/dispatch/text/testdata/dispatch-text-references.json", "packages/contracts/src/dispatch-text-references.test.ts:10-14"),
    (f"{ENVOY}/internal/dispatch/text/testdata/dispatch-block-id-encodings.json", "packages/contracts/src/dispatch-block-ref.test.ts:98-102 reads the Go side's own table by relative path and asserts set equality against it, so the two halves are one corpus"),
    (f"{DAEMON}/internal/intake/testdata/dispatch/server-started.json", "packages/contracts/src/dispatch-api.test.ts:370 (the new URL at :366-369)"),
    (f"{ENVOY}/internal/dispatch/pmdoc/testdata/block-pointer-values.json", "packages/contracts/src/dispatch-block-ref.test.ts:129-133 reads pmdoc's own JSON copy of the block pointer table by relative path"),
    (f"{ENVOY}/internal/dispatch/pmdoc/testdata/typed-block-id-spellings.json", "packages/contracts/src/dispatch-block-ref.test.ts:146-150 reads pmdoc's own JSON copy of the typed block id spellings by relative path"),
    (f"{ENVOY}/internal/contracts/generated.go", "packages/contracts/scripts/gen-go.test.ts:63 reads it for the envelope aliases, :81 reads every file of generated (:20-26, this one :7), and gen-go.ts's outputs list holds it (--check compares the committed file)"),
    (f"{ENVOY}/internal/contracts/dispatch_tools_generated.go", "packages/contracts/src/dispatch-tools.test.ts:1701 reads it for the decision-block rule and :1823 for every tool's Min and Max bounds, packages/contracts/scripts/gen-go.test.ts:81 reads every file of generated (:20-26, this one :9-12), and gen-go.ts's outputs list holds it (--check compares the committed file)"),
    (f"{ENVOY}/listenerwire/envelope_generated.go", "packages/contracts/scripts/gen-go.test.ts:53 reads it for the envelope, :81 reads every file of generated (:20-26, this one :8), and gen-go.ts's outputs list holds it (the envelope's Go mirror, generated from schemas/envelope.schema.json)"),
    (f"{ENVOY}/internal/dispatch/pmdoc/schema_generated.go", "packages/contracts/scripts/gen-go.test.ts:81 reads every file of generated (:20-26, this one :13-16) and gen-go.ts's outputs list (the block schema's shape, generated from schemas/block-schema.schema.json)"),
    (TEST_FIXTURES_SRC, "packages/contracts/src/*.test.ts import @legion/test-fixtures"),
) + bun_job("contracts"))
