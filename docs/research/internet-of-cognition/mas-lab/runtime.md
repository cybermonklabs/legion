# mas-lab `runtime/`

Subject: the directory `runtime/` of the repository `ioc-project/mas-lab`.

Clone commit: `477441ddb82d1b07c76c36f1426d8a5be0b6194b`.

Clone path: `$IOC_CLONES/mas-lab`, where `IOC_CLONES` is `/legion/scratch/tmp/ioc-clones`.

Our tree: `/legion/workspaces/cybermonklabs/legion/infra-3134` at commit `9542c6fbbecd184ccee73523ab3defa39d6cf23a`.

Requirement: r3.

## What it is

`runtime/` is one Python package, `mas-lab-runtime`, version 0.2.0 [m-rt-package].

The package README calls it an embeddable kernel library with no CLI [m-rt-readme-library].

The package metadata declares a console script named `mas-runtime` [m-rt-package].

That script only prints a message and exits with status 2 [m-rt-cli-stub].

The directory holds 419 files at this commit [m-rt-file-count].

160 of them are test files under `runtime/tests/` [m-rt-test-count].

## What it runs

The kernel reads a closed set of ingress symbols [m-rt-ingress-symbols].

There are nine ingress kinds [m-rt-ingress-symbols].

The kernel writes a closed set of six egress symbols [m-rt-egress-symbols].

One of the six, `RaiseBoundaryError`, carries the refusal [m-rt-egress-symbols].

The kernel wraps each call to a model, a tool, a memory or a transport in an envelope [m-rt-envelope].

The envelope has an egress half and an ingress half [m-rt-envelope].

Governance runs at two points of that envelope: `GOVERNANCE_AUTHORIZE` before the call, and `GOVERNANCE_VALIDATE` after it [m-rt-envelope].

## What it enforces at a system boundary

Start point: `runtime/src/mas/runtime/schema/ingress.py`.

Each ingress model is a frozen Pydantic model [m-rt-ingress-symbols].

The union is discriminated on the field `kind` [m-rt-ingress-symbols].

The field `correlation_id` of `EngineIoReturn` must be 1 or more [m-rt-ingress-symbols].

The next step of the path is `boundary/ingress_validate.py` [m-rt-ingress-valid].

`ingress_governance_valid` refuses a response kind outside a set of four [m-rt-ingress-valid].

It refuses a next step outside a set of five [m-rt-ingress-valid].

It refuses a correlation id that is not one of the in-flight ids [m-rt-ingress-valid].

Where no call is in flight, it refuses a correlation id that is not the pending id [m-rt-ingress-valid].

Where no call is pending, it refuses a correlation id that is not greater than the last one [m-rt-ingress-valid].

A refusal here gives the code `INGRESS_DENIED` and is recoverable [m-rt-ingress-denied].

After that check the kernel runs the ingress governance chain [m-rt-ingress-chain].

The chain is first-match [m-rt-ingress-chain].

A chain with no plugin falls back to one profile value [m-rt-ingress-chain].

The profile acts only on a response kind of `ERROR` [m-rt-ingress-profile].

The governance answer is one of ten actions [m-rt-gov-actions].

The kernel maps seven of those actions to a different boundary result [m-rt-ingress-actions].

The egress half runs the same way before the call [m-rt-egress-gate].

A `BLOCK` of a tool call does not stop the turn [m-rt-block-feeds-model].

It writes the policy reason as a synthetic tool observation and schedules another model call [m-rt-block-feeds-model].

The code on `RaiseBoundaryError` is a plain string [m-rt-code-open-set].

24 distinct code strings appear in the source [m-rt-code-count].

The control plane has its own refusals [m-rt-control-refusals].

A control caller holds a capability that names its sessions and its methods [m-rt-capability].

A narrowed capability cannot gain a method the parent did not hold [m-rt-capability].

A queue change carries a revision, and a stale revision is refused [m-rt-queue-cas].

### What the enforcement does not cover

The kernel and the governance package name no credential, no bearer token and no authentication [m-rt-no-authn].

The kernel and the governance package name no idempotency key and no dedupe key [m-rt-no-idempotency].

The kernel names no precondition, no entity tag and no revision check [m-rt-no-precondition].

Call ceilings are off unless a manifest sets them [m-rt-budget-off].

The whole governance half can be switched off by one configuration flag [m-rt-gov-switch].

With the shipped defaults, the egress profile blocks nothing [m-rt-default-allow].

A failure inside an envelope machine is logged at debug level and the envelope continues [m-rt-fail-open].

## Our side

Our three surfaces are these.

`packages/envoy/internal/dispatch/api/handler_errors.go`, function `writeHandlerError`: the one place a write's error becomes a refusal code and an HTTP status [m-rt-our-dispatch-refusals].

140 distinct refusal codes are written in the non-test files of that directory [m-rt-our-refusal-count].

`packages/envoy/internal/dispatch/api/project_scope.go`: the project and session scope refusals a credential meets [m-rt-our-scope-refusals].

`packages/daemon/internal/api/handoff.go`, functions `handoffComplete`, `handoffRecord`, `implementationPullRequest` and `checkReport`: the completion refusals [m-rt-our-handoff-refusals].

19 distinct completion refusal codes are written in that one file [m-rt-our-handoff-count].

`packages/envoy/internal/contracts/generated.go`, methods `Envelope.Validate` and `ValidateWire`: the generated envelope contract check [m-rt-our-contract-validate].

The same generated file holds the refusal codes and the bounds as constants [m-rt-our-generated-consts].

## Question 3: what its boundary enforcement does that our refusals do not

### Where their enforcement is wider

Their boundary gate runs before the side effect, not only after a bad request [m-rt-egress-gate].

A tool call is authorized at a chokepoint, and the chokepoint can hold it for a human [m-rt-egress-gate].

We have no equivalent human gate on any of the three surfaces [m-rt-our-no-hitl].

Their refusal algebra has ten verbs, not one [m-rt-gov-actions].

Three of the ten are not a refusal at all: `RETRY`, `MODIFY` and `SKIP` change the call and let it proceed [m-rt-egress-gate].

Our refusals have one verb: the write does not happen and a code says why [m-rt-our-dispatch-refusals].

Their `BLOCK` of a tool call returns the reason to the model as an observation, so the agent can choose again [m-rt-block-feeds-model].

Our refusal returns the reason to the caller, and no part of our code feeds a refusal back into a model turn [m-rt-our-handoff-refusals].

Their enforcement is pluggable at run time: a chain of policy objects decides each call [m-rt-ingress-chain].

Our refusals are compiled in, and a new rule is a new branch in Go [m-rt-our-dispatch-refusals].

### Where ours is wider

Ours covers identity; theirs does not [m-rt-no-authn].

A write to Dispatch meets a credential, a project scope and a session scope before it meets a schema [m-rt-our-scope-refusals].

Ours covers repeat requests; theirs does not [m-rt-no-idempotency].

A keyed write that is sent twice meets one of five idempotency codes [m-rt-our-generated-consts].

Ours covers concurrent change; theirs does not [m-rt-no-precondition].

A document write that lost its ground meets `PRECONDITION_FAILED` or `EDIT_LOST_TO_CONCURRENT_CHANGE` [m-rt-our-dispatch-refusals].

Ours covers facts outside the process: a GitHub pull request, a Dispatch handoff record, a run generation and a role assignment [m-rt-our-handoff-refusals].

Their enforcement reads only the kernel's own state [m-rt-ingress-valid].

Ours has a larger named surface: 140 codes against their 24 [m-rt-our-refusal-count] [m-rt-code-count].

Our codes are generated from one source, so the server and the client read one string [m-rt-our-generated-consts].

Their code strings are free text on the model and are not held to a set [m-rt-code-open-set].

Ours refuses by default; theirs allows by default [m-rt-default-allow].

Ours cannot be switched off by a flag; theirs can [m-rt-gov-switch].

Ours fails closed, because an error becomes a 500 and the write does not land [m-rt-our-dispatch-refusals].

Theirs fails open at the envelope, because a machine that raises is logged and skipped [m-rt-fail-open].

### Where the two are the same thing in different words

Their correlation-id check and our idempotency and precondition checks do one job: refuse an answer that does not match the state the server holds [m-rt-ingress-valid] [m-rt-our-generated-consts].

Their `INGRESS_DENIED` and our `PRECONDITION_FAILED` both say "this reply is not for the request I am holding" [m-rt-ingress-denied] [m-rt-our-dispatch-refusals].

Their frozen Pydantic ingress models and our `Envelope.Validate` do one job: refuse a message whose required fields are absent or whose enumerated field holds an unknown word [m-rt-ingress-symbols] [m-rt-our-contract-validate].

Their `ControlCapability.allows` and our project and session scope checks do one job: hold a caller to the sessions its token names [m-rt-capability] [m-rt-our-scope-refusals].

Their `QueueConflict` on a stale revision and our `PRECONDITION_FAILED` on a stale token are the same compare-and-set refusal [m-rt-queue-cas] [m-rt-our-dispatch-refusals].

Their `BudgetTracker` ceilings and our generated bounds, such as `MaxBroadcastRecipients`, are both a count that stops work [m-rt-budget-off] [m-rt-our-generated-consts].

Their `handoff`-like completion parser is not a parallel: `verification_status` only reads a word out of text and refuses nothing [m-rt-completion-parse].

## What I could not settle

I could not run their code [m-rt-no-execution].

The pod has no `pydantic`, and the install failed, so no behaviour row below measures their kernel at run time [m-rt-no-execution].

Every claim about their behaviour is therefore a reading of the source, marked `project-code`, and not a measurement.
