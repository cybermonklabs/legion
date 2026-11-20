# Contracts

`@legion/contracts` holds the shapes Legion's TypeScript and Go code share:
the Envoy envelope and subjects, the Dispatch specs the `dispatch` command is built from, Dispatch's HTTP shapes, the Legion daemon's wire shapes, and the limits both languages enforce.
It has no release of its own; its consumers bundle it.
`README.md` has the commands, the files generated from this package, and the GitHub and Slack subject hierarchy.

## Rules

- Never edit a generated file: change its source and regenerate it.

- A limit that both the server and a client enforce is one exported constant here, which `scripts/gen-go.ts` emits into Go as `contracts.<Name>`.
  Never write the number a second time.

- A Dispatch shape goes in the `src/dispatch-api-*.ts` module of its area.
  Callers import `src/dispatch-api.ts`, which re-exports every module, and never name a module by path.

- `src/legion-api.ts` mirrors the daemon's Go wire shapes by hand.
  Import it as `@legion/contracts/legion-api`, never from the barrel, so a bundle that does not speak to the daemon does not carry it.

- A delivery mode goes into `DELIVERY_CAPABILITIES` before any server, adapter or UI accepts it.

- Declare a new wire field in every nested `z.object` that reads it: a zod object drops the keys it does not declare.

- Dispatch, its clients and the plugins ship separately: prefer backward-compatible additions when you extend the envelope.

- Build each Dispatch spec's fields through `zodSchemaApi(hostZod)`, and validate a call with `dispatchToolSchema(spec, zodSchemaApi(hostZod))`:
  the host's Zod can be Oh My Pi's `pi.zod` facade, which differs from zod v4.

- Change a Dispatch spec's `example` with its argument shape: the command's help and every refusal of its input print it.

- A change to `SEARCH_QUERY_MAX`, to `PROJECT_KEY_PATTERN`, or to the parameters `dispatch search` sends redoes the URL budget in `SEARCH_QUERY_MAX`'s doc comment (`src/dispatch-tools.ts`).
