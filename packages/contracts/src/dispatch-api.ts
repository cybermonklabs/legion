/**
 * The Dispatch HTTP and event-bus contract. This file is the entry every caller imports
 * (`@legion/contracts` re-exports it whole); nothing outside the package names a module of the
 * split by path.
 *
 * The wire shapes are generated from the Go route table through `schemas/dispatch-openapi.json`:
 * `dispatch-api.generated.ts` is the one definition of every HTTP request and response body of
 * that table, and this file re-exports it whole. What the route table does not hold is written
 * by hand, one module per area:
 *   - the Dispatch event payloads and the event envelope, which travel the message bus;
 *   - the `GET /ws/live` socket's frames, a route the table does not carry, held against Go by
 *     `fixtures/live/` and the server's `delivery_live_diff_test.go`;
 *   - the closed string enums the OpenAPI document widens to `string`, with the `const` arrays
 *     they are read from and the limits and header names `scripts/gen-go.ts` renders into Go;
 *   - the readings the dashboard takes of a generated row (`AskWithTrees` and the rest), and the
 *     helpers and client-side composites no route answers (`Advised`, `IssueRead`,
 *     `ArtifactVersionContent`, `CreateArtifactInput`, …).
 *
 * An event payload is read two ways, and one rule names the two schemas. The tolerant reading,
 * what a reader of the bus makes of a frame, carries the plain name - `CommentEventPayloadSchema`
 * - because that is the name this package has always exported and every reader of the bus
 * imports: JetStream retains a frame for 72 hours, so a frame written before a field existed must
 * still parse, and a name that quietly became strict would refuse it. The payload exactly as
 * Dispatch writes it carries the same name under the prefix `Written`, so each reading is spelled
 * once: `WrittenCommentEventPayloadSchema` is the `z.strictObject`, with `export type X =
 * z.output<typeof WrittenXSchema>` beside it, in the area the payload belongs to.
 *
 * A shape read only one way keeps its plain name, whether it is written only
 * (`AskAnsweredEventPayloadSchema`, which no reader of the bus parses on its own) or read off a
 * frame only. The one shape that is read more ways than it is written is the actor: three
 * readings of it (`InboundActorSchema`, an author; `InboundWriterSchema`, a writer whose extra
 * fields ride along; `InboundBySchema`, a writer whose id a frame may omit) cannot all be
 * `ActorSchema`, so each keeps the `Inbound` prefix this package already spells
 * `InboundDispatchEvent` with, and `ActorSchema` stays the actor Dispatch writes.
 *
 * A tolerant schema is derived from the exact one and differs in four ways and no more: it keeps
 * only the keys that reading keeps, makes optional the fields a retained frame may omit, keeps
 * unknown fields only where today's reading does, and leaves open the strings a frame may carry a
 * value of that this release does not know (`kind`, `urgency`, an actor's `kind`, a writer's
 * `id`).
 */
export * from "./dispatch-api.generated";
export * from "./dispatch-api-asks";
export * from "./dispatch-api-comments";
export * from "./dispatch-api-core";
export * from "./dispatch-api-delivery";
export * from "./dispatch-api-documents";
export * from "./dispatch-api-events";
export * from "./dispatch-api-graph";
export * from "./dispatch-api-inbox";
export * from "./dispatch-api-issues";
export * from "./dispatch-api-limits";
export * from "./dispatch-api-live";
export * from "./dispatch-api-messages";
export * from "./dispatch-api-refs";
export * from "./dispatch-api-requests";
export * from "./dispatch-api-search";
export * from "./dispatch-api-sessions";
export * from "./dispatch-api-targeted";
export * from "./dispatch-api-user";
