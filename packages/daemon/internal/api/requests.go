package api

// This file is the request half of the gate that answers.go and shaping.go are the answer half
// of. An answer is served down to the contract its caller reads. A request is not shaped at all:
// readBody decodes every plugin body strictly and refuses a member its Go type does not name, so
// a field a later contract takes out of a request type refuses every older plugin that still
// sends it. The records under `packages/contracts/fixtures/daemon-api/request-shapes.json` hold
// the field paths of every request a plugin sends, one record per route from this daemon's own
// contract on, and request_shapes_test.go fails a later request type that no longer reads a
// recorded field, unless the field is a row of AcceptedByName below, and a later daemon that
// reads no body at a recorded route, unless the route is a row of RemovedRoutes below.

import "reflect"

// AcceptedField is one field of a plugin's request body that a contract took out of its request
// type and that this daemon still reads by name, because a plugin below that contract sends it.
// Ignoring the field gives the call the meaning that plugin meant. Where accepting a value would
// change what the call means, the route refuses that value with a sentence of its own, and the
// row's Ignoring says so.
type AcceptedField struct {
	// Route is the route that reads the field, as `METHOD /path`.
	Route string
	// Field is the field's path in the request body, written as a key path of an answer is
	// written (contract_shapes_test.go's keyPathRule).
	Field string
	// LeftAt is the first contract whose request type does not carry the field.
	LeftAt int
	// Ignoring is what ignoring the field means: why a body that carries it is read as the call
	// its plugin meant, and which value, if any, is refused.
	Ignoring string
}

// AcceptedByName is every field a request type keeps for an older plugin's body alone. The
// request gate reads it three ways (request_shapes_test.go). A recorded field that this daemon's
// request type no longer carries passes only with a row here whose LeftAt is above the record's
// contract. Each row's field must still be a field of the route's request type, so that the
// body the row is written for is read and not refused as an unknown member. And a field the
// type reads that the record of this daemon's own contract does not hold must have a row here,
// so a row deleted while its field stays in the type fails rather than losing its reason.
//
// Each Go field a row names is in the request type, so it is in the type's walk. The record of
// any contract at or above the row's LeftAt leaves the field out all the same, because that
// contract's request does not carry it (request_shapes_test.go's requestShapesAt).
var AcceptedByName = []AcceptedField{
	// Contract 86 took `ready` out of HandoffCompleteRequest with READY and the daemon's merge.
	{Route: "POST /legion/v1/handoff/complete", Field: "ready", LeftAt: stateMachineContract,
		Ignoring: "a plugin from 46 to 85 sends ready on every handoff_complete: false, its default, " +
			"meant what a completion means now, so it is read and ignored; true armed the daemon's merge of the pull request, " +
			"which no daemon at contract 86 or later makes, so true is refused (READY_RETIRED)"},
}

// childRouteRemovedContract is version.go's 205, the contract that removed the child route
// removedChildRoute now answers: the owning architect writes the child's status in Dispatch
// instead. SupportedContracts names it, so a renumber of that entry at a forward merge moves this
// constant and the list together, beside DaemonAPIVersion while 205 is this daemon's own contract.
const childRouteRemovedContract = 205

// RemovedRoute is a route a plugin of an earlier contract sends a body to and that a later
// contract took away from the plugin. The daemon still registers the route and answers every
// call with a refusal of its own that says what the caller does instead, so a plugin below LeftAt
// is told why its call did nothing, where an unregistered route would answer "no route". The
// route reads no body, so no body it is sent is refused as an unknown member.
type RemovedRoute struct {
	// Route is the route, as `METHOD /path`.
	Route string
	// LeftAt is the first contract whose plugin does not call the route.
	LeftAt int
	// Answer is what the route answers every call with.
	Answer string
}

// RemovedRoutes is every route a recorded contract's plugin sends a body to that this daemon
// reads no body at any more. The request gate reads it (request_shapes_test.go). A record of a
// route that no row of pluginRequests reads passes only with a row here whose LeftAt is above the
// record's contract, and so the record of this daemon's own contract never holds the route. Each
// row's route is registered and reads no body (routesReadingNoBody), and no row of pluginRequests
// names it.
var RemovedRoutes = []RemovedRoute{
	{Route: "POST /legion/v1/children/park", LeftAt: childRouteRemovedContract,
		Answer: "410 ROUTE_REMOVED to every call, with the status the owning architect writes in Dispatch in its place " +
			"and the rules of that write (removedChildRoute); the route applies no fact and writes nothing to Dispatch"},
}

// requestDecoded is told the pattern of the route and the Go type of every body decodeJSON
// decodes. It is nil in a daemon and set by one test of this package, which holds each pair to
// the table of the request types a plugin sends (request_shapes_test.go's pluginRequests): the
// request gate walks the types that table names, so a handler that decodes another type would
// leave the gate reading a type no route reads.
var requestDecoded func(pattern string, into reflect.Type)
