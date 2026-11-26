// Package agentloop runs a model and the tools it calls: the model loop of the secrets broker
// (internal/cipher), and the only import of Grafana's AI SDK for Go in this repository. It is
// the engine and nothing more. It keeps no conversation store, carries no tools of its own and
// routes nothing: a caller hands it an endpoint, a model, the messages, the tools the model may
// call and the bounds of the run, and reads back the reply, every step and what the run cost.
//
// Nothing here comes from the environment: neither a base URL nor a key, and none of the
// organization, the project and the custom headers a vendor client would otherwise read for
// itself. Every endpoint and every credential arrives in the request.
//
// # Tool call numbering
//
// ToolCall.Index is one-based. Request.FirstToolIndex is the number of tool calls an earlier run
// of the same work already made, so the first call of this run is FirstToolIndex + 1. The calls
// of a step are numbered in the order the model asked for them, and run one at a time, under one
// mutex. A caller can therefore key an idempotent write on the index and re-run the work without
// that write happening twice.
//
// # The auth header is fixed per Kind
//
// Endpoint.Token returns a token and the kind decides where it goes: X-Api-Key for
// anthropic-messages; Authorization, as a bearer token, for openai-responses, openai-chat and
// systemone; and for azure-anthropic the token is handed to the Azure provider's own credential.
// There is no Endpoint field for an auth scheme. The token is read again for every HTTP attempt,
// the retry included, except on azure-anthropic: there the provider's credential middleware sets
// the header above this package's HTTP client, so the token is read once per model call and the
// retry below the middleware resends the first attempt's token. A redirect is never followed, in
// a run or in a decision, so the token reaches the endpoint the caller named and no other
// server: the client's redirect check fails the call, above the transport and its retry, with
// an error naming the redirecting status and the host the Location points at, so the run ends
// in FaultProviderError. The Detail of that fault carries more than the host: Go's own
// *url.Error wraps the check's error and names the Location in full, query included, as the
// endpoint sent it. A caller that logs a Detail logs whatever the endpoint it named put there.
//
// # The retry
//
// Both of the SDK's retry layers are off, and this package retries once itself: on a transport
// error, a 429 or a 5xx, decided from the response status before any of the body is read, and
// only while Bounds.Time is left. A stream that breaks after its first byte is FaultProviderError
// and is never retried, so a caller's OnText never sees the same text twice.
//
// # The provider's own chunks
//
// Step.Native is the output items an openai-responses call sent, each byte for byte as its
// stream carried it, on every step a run returns, a run that ended on a fault or an error
// included, and a Chat Completions refusal is what an openai-chat call says in place of
// a reply. Both are read off the SDK's stream of raw provider chunks, which the run asks for on
// those two protocols and no other. The HTTP transport keeps no response body of its own: it is
// the token, the endpoint's query parameters, the Final schema's place and order (see "The
// structured reply") and the retry, and nothing else.
//
// # Cache marks
//
// The package places them by default, where the provider wants them: anthropic-messages takes
// an ephemeral mark at the end of the history, one on the newest message, and, where the
// conversation is long enough for the provider to cache it, two more at the boundaries the
// run before it marked, which are the two the provider wrote its entries at: the history's
// last question and the message before it. That is four marks, which is all Anthropic allows,
// and a run of a conversation a caller grew by an exchange then reads what the run before it
// wrote. openai-responses takes an explicit prompt cache breakpoint at the end of the history;
// openai-chat takes none. A caller that wants none of them anywhere sets Request.NoCache,
// which takes off the marks this package places and nothing else: a provider's own caching is
// not the field's to turn off, openai-responses keeps the implicit breakpoint it puts after
// the newest message, and on openai-chat, which this package never marks, the field changes
// nothing at all.
//
// # The structured reply
//
// Request.Final is the JSON schema the last reply must be exactly one object of, and the package
// reads every reply back against the schema itself: a last reply that does not fit is
// FaultMalformed. The schema also rides to the provider, where the protocol says: in
// output_config.format on anthropic-messages and azure-anthropic, as the Responses API's text
// format on openai-responses, and on openai-chat as response_format.json_schema with the
// provider's strict flag set. On every call that carries it the schema therefore binds the reply
// as it is generated, and the package's own read of the reply is the second check, which an
// endpoint that speaks a vendor's protocol without being that vendor still needs. Every call of
// a run with a Final carries it but one that offers the model a tool on the Messages API, below.
//
// The schema reaches the provider with each of its objects' keys in the order Request.Final
// writes them. Under structured output the model writes its reply's keys in the schema's order,
// so that order is the order the model decides the reply's fields in. On a run of the Legion
// daemon's answer loop the reply form, declared kind, text, needs, reached the provider sorted
// as kind, needs, text, and its last call replayed 12 times in each order answered not_answered
// with an empty needs 10 times of 12 sorted and none of 12 in the declared order. The SDK keeps
// the order on openai-chat alone: its Anthropic and Responses adapters rebuild the schema as a
// map before they send it, and Go writes a map with its keys sorted. The run's HTTP transport
// therefore puts every object of the schema in each call's body back in Final's order, nested
// objects and array elements included, once per call and before the retry, so the retry sends
// the same bytes. A key the SDK added that Final lacks keeps its place after Final's own keys,
// every value but an object or an array keeps the bytes the SDK wrote, a key is written again
// without HTML escaping, and a body that carries no schema is sent byte for byte as it came.
//
// Every call of a run with a Final but those the next paragraph names carries its schema at the
// protocol's path. A call whose schema the transport cannot put back, because the body holds
// none there, holds one of another JSON kind than Final's, or is no JSON, or because Final is no
// JSON the transport can read, such as one that holds a key twice, goes out as the SDK wrote it
// and logs one warning to Request.Log naming the protocol and the reason, once for the call and
// not again on its retry.
//
// On the Messages API, anthropic-messages and azure-anthropic, a call that offers the model a
// tool carries no schema: the transport takes output_config.format out of its body, keeps the
// rest of output_config, the model's effort, and every other byte as the SDK wrote them, takes
// out an output_config that held the format alone, as the SDK writes none for a request with
// neither, and logs nothing. A body offers a tool where its tools array holds one and its
// tool_choice is absent or of another type than none; the SDK keeps a step's tools in its body
// whatever the step's tool choice and writes the last allowed call's as {"type":"none"}, so the
// tool choice is what tells that call from the others. The run's last call, which offers no
// tool, and every call of a run with no tools, such as the secrets broker's judge's one call,
// carry the schema in Final's order as above, and openai-responses and openai-chat carry it on
// every call. With the schema on every call, the Claude answers of the Legion daemon's answer
// loop that needed no tool still made tool calls up to its call bound: in two proof runs, 18 of
// 135 answers with no fault took all 8 of their calls. The requests of proof run 45683361567,
// replayed through the model gateway against claude-sonnet-5-5, one model call each and no tool
// run, three samples a request, replied on 19 of the 162 samples of the 54 requests where a reply
// was possible with the schema in output_config.format and on 100 of them without it, and the
// calls that did nothing fell from 56 to 18; on the 10 answers' first calls the replies rose from
// 3 of 30 to 21 of 30, and counted by answer the replies rose by 50 points (interval +36 to +65)
// and in all 10. The tool calls a question needed were made in every sample either way. Of the
// 121 replies made without the schema, 120 were exactly the reply object with an allowed kind,
// and one had a sentence of prose before the object. Nothing was measured on the OpenAI
// protocols, whose answers in those runs took 1 to 3 calls, so they keep the schema on every
// call.
//
// A reply on a call that offered a tool that is exactly one object of Final ends the run with
// that object, as on any call. One that is not is no answer and, while the run has time left, no
// fault: the run drops its step as it drops a cut call's (Step.Dropped, and "A reply cut for
// length"), sends none of the reply back, and makes its next call its last, whatever Bounds.Calls
// has left. That call is sent the messages the dropped call was sent, its cache marks falling
// where the dropped call's did; it offers no tool and carries the schema and Request.LastCall;
// and its reply is final: the object, the run's Value with no ask again whatever Request.Check
// would make of it, as on any last call, or FaultMalformed. Bounds.Calls counts the dropped call
// and Result.Usage its usage; the run drops such a reply once at most, since the call after it
// carries the schema, and counts it against neither Request.AskAgain nor
// Request.CallAgainWhenCut. A reply out of form on a call that carried the schema, the last call,
// any call of a run with no tools or on another protocol, or one with no time left after it,
// ends the run on FaultMalformed as before.
//
// A tool's input schema (Tool.Schema) is not put back: the same two adapters send its keys
// sorted, as that run's requests show, so a tool whose input has more than one property is
// offered them sorted, and the model writes a call's arguments in that order. A call's input is
// read by key, so no call fails for it.
//
// # The Final schema is held to the strict subset
//
// A provider enforces a schema by compiling it into the grammar it generates against, and both
// of them compile a documented subset of JSON Schema and no more. A schema outside that subset
// is not always refused on arrival: Anthropic's output_config.format takes one, enforces what
// it can of it and says nothing about the rest, so a constraint it cannot compile is a
// constraint the caller believes in and the model never sees. Measured against the Messages API
// on 2026-11-07, a schema whose one optional property had the type ["string","null"] rode in
// output_config.format and bound nothing, and the reply ran to the output token bound repeating
// itself; OpenAI's strict mode, which this package now asks for, would have answered the same
// schema with a 400 in the middle of the run. Run therefore checks Request.Final before its
// first model call and answers a schema that is not strict with an error naming the JSON path
// of the node at fault and what is wrong with it — agentloop: the request's final schema:
// properties.needs: optional property (not in required) — rather than making the call. The
// rule, which is the two providers' subsets intersected, reading
// https://platform.claude.com/docs/en/build-with-claude/structured-outputs#json-schema-limitations
// and https://developers.openai.com/api/docs/guides/structured-outputs#supported-schemas:
//
//   - The root is an object, and so is every node: each carries one type, named by a string;
//     a type array, such as ["string","null"], is a union and is refused.
//   - anyOf, oneOf, allOf, not, if, then and else are refused wherever they appear. OpenAI's
//     strict mode refuses all but anyOf outright and refuses an anyOf at the root; Anthropic
//     counts every anyOf and every type array against a cap of sixteen union parameters per
//     request, at exponential compilation cost.
//   - Every object carries additionalProperties: false and lists every one of its properties in
//     required. An optional property is the defect above; OpenAI's strict mode refuses one
//     outright, and Anthropic caps them at twenty-four per request.
//   - An array says what its items are. How many items it holds is not a Final schema's to say:
//     Anthropic enforces no array constraint but a minItems of 0 or 1, and the SDK takes
//     minItems out of the schema before the call either way.
//   - The keywords a node may carry are type, properties, required, additionalProperties, items,
//     enum, format, $ref, $defs, definitions, title and description. Anything else — minLength,
//     maximum, multipleOf, pattern, minItems, uniqueItems, const, a keyword a later draft adds —
//     is refused by name. A keyword the provider's grammar does not take does not fail the call:
//     the SDK's Anthropic provider strips every one of those out of the schema and writes it
//     into the node's description, which is prose to the model and binds nothing. An enum holds
//     plain values only, and a format is one of date, date-time, duration, email, hostname,
//     ipv4, ipv6, time and uuid, which is Anthropic's list without uri, which OpenAI's does not
//     carry.
//   - A $ref names a place in this schema, carries nothing but a title or a description beside
//     itself, and never leads back to a node the walk is already inside: Anthropic serves no
//     recursive schema and loads no external $ref.
//
// The check reaches every node: each property, an array's items, and every member of $defs and
// definitions, whether or not a $ref names it.
//
// # Asking again
//
// Request.Check is a caller's rule of the Final object that the strict subset cannot state, such
// as a property that must not be empty for one value of another, and the run holds a reply to
// it as a validate-and-ask-again loop does (Pydantic AI's output validators, Instructor's
// retries): a reply whose value the check refuses is answered, in the same conversation, with
// the check's own sentence as a user message, and the model is asked for the reply again, at
// most Request.AskAgain times. The run goes on from its own messages, which are the SDK's: the
// ones the ask before was given and every message its calls produced, the refused reply among
// them as the provider returned it. That is how the SDK carries a step's turn to the next call
// between two tool calls, so a reply's thinking and its signature, or its reasoning item, go
// back as they came.
//
// An ask again is no new run. Bounds.Calls counts every model call of the run, and the run's last
// call offers no tool, and carries Request.LastCall, whichever ask it falls in;
// Bounds.Time is the one deadline of the whole run, and no ask is made again once it has passed;
// Bounds.OutputTokens bounds each call. Tool calls are numbered on across the asks, Result.Steps
// holds every call in order with the sentence on the step it answered (Step.AskedAgain), and
// Result.Usage sums them all. The cache marks are placed as for any call: the history keeps its
// own, and the check's sentence takes the last of the four, as the newest message but for the
// LastCall sentence after it on a last call, which takes none. Where the asks or the bounds are
// spent, the call after a dropped reply out of form ("The structured reply") being the run's
// last, the run returns the last reply as it would with no check, its Value set and no fault: a
// check's refusal is never a fault of this package, and refusing that value is the caller's to
// do. A request with no Check sends exactly what it sent before Check existed.
//
// # The last call
//
// The last call Bounds.Calls allows offers no tool, and so does the call after a dropped reply out
// of form, which is then the run's last ("The structured reply"), so the model must answer on it,
// and a model that is not told so meets a call with no tool it can make. Request.LastCall is the
// caller's sentence telling it, which the package sends on that call alone, as one user message
// after everything else the call carries: after the tool results of the call before it, or, where
// the last call is an ask again, after the check's sentence, which the model then reads first and
// answers on the same call; and where Bounds.Calls is 1, after Input, on the run's one call. The
// sentence is the caller's instruction to the run, as the system prompt is, and no turn of the
// conversation: no earlier call carries it, no Step and nothing else of the Result holds it, and
// on the Anthropic kinds it takes no cache mark, so the call's marks, four at most, are where
// they would be without it. An empty LastCall sends nothing, and a run without it sends exactly
// what it sent before the field existed.
//
// # A reply cut for length
//
// A reply the provider cut for length is no answer, and a reply cut at Bounds.OutputTokens is
// most often a model running on inside its structured reply rather than one with more to say.
// Measured on the Legion daemon's answer loop on 2026-11-19: of 415 model calls, the longest
// reply the bound of 4000 did not cut used 567 output tokens, and the one reply it cut was a
// complete answer of some 350 tokens inside the form's string followed by some 3800 newline
// escapes; five replays of that request with the bound at 64000 ended between 167 and 487
// tokens. Sending such a reply back asks the model to go on from its runaway, so
// Request.CallAgainWhenCut has the run make the cut call again instead, at most that many
// times: the next call is sent the run's messages as they stood before the cut call, the SDK's
// response messages less the cut call's own, and nothing of the cut reply. It is a model call
// of the run like any other: Bounds.Calls counts it, it offers no tool and carries
// Request.LastCall where it is the last allowed call, it is made only while Bounds.Time is
// left, its cache marks fall where the cut call's did, and Request.Check reads its reply. The
// cut call stays in Result.Steps, marked Step.Dropped so a caller that writes the steps back
// into a conversation leaves it out, and its usage stays in Result.Usage. A cut past
// CallAgainWhenCut, or a cut with no model call or no time left, ends the run with
// FaultLength. A prompt past the model's context window, which Anthropic also ends as length,
// is the same prompt on the call again, so that run ends with FaultLength one call later. A
// request with a zero CallAgainWhenCut sends exactly what it sent before the field existed. A
// reply out of form on a call that carried no schema is dropped the same way, and is no cut
// CallAgainWhenCut counts ("The structured reply").
//
// # Bounds
//
// Bounds.Calls bounds model calls. A per-tool budget — how much work one tool will do before it
// refuses — is that tool's own Run logic, which this package does not count.
package agentloop
