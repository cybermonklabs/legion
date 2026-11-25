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

// AcceptedByName is every field a request type keeps for an older plugin's body alone: the rows
// of the numbered entries (frozenAcceptedByName) and then each entry's, in number order
// (contracts.go). The request gate reads it three ways (request_shapes_test.go). A recorded field
// that this daemon's request type no longer carries passes only with a row here whose LeftAt is
// above the record's contract. Each row's field must still be a field of the route's request
// type, so that the body the row is written for is read and not refused as an unknown member.
// And a field the type reads that the record of this daemon's own contract does not hold must
// have a row here, so a row deleted while its field stays in the type fails rather than losing
// its reason.
//
// Each Go field a row names is in the request type, so it is in the type's walk. The record of
// any contract at or above the row's LeftAt leaves the field out all the same, because that
// contract's request does not carry it (request_shapes_test.go's requestShapesAt).
func AcceptedByName() []AcceptedField { return registry().acceptedByName }

// frozenAcceptedByName is the rows of AcceptedByName that the numbered entries accepted.
var frozenAcceptedByName = []AcceptedField{
	// Contract 86 took `ready` out of HandoffCompleteRequest with READY and the daemon's merge.
	{Route: "POST /legion/v1/handoff/complete", Field: "ready", LeftAt: stateMachineContract,
		Ignoring: "a plugin from 46 to 85 sends ready on every handoff_complete: false, its default, " +
			"meant what a completion means now, so it is read and ignored; true armed the daemon's merge of the pull request, " +
			"which no daemon at contract 86 or later makes, so true is refused (READY_RETIRED)"},
}

// childRouteRemovedContract is the contract of version.go's entry for dispatch://INFRA-3178,
// which removed the child route of the operation that moved a child out of the workflow: the
// owning architect writes the child's status in Dispatch instead. frozenContracts names it.
const childRouteRemovedContract = 205

// RemovedRoutes is every route pattern that answers a recorded request with a refusal and reads
// no body, with the first contract whose plugin no longer sends that request. It holds the rows
// of the numbered entries (frozenRemovedRoutes) and then each entry's (contracts.go). The refusal
// tells an older caller what to do instead, and refuses no body as an unknown member.
//
// The request gate (request_shapes_test.go) resolves each recorded route through the server's mux.
// If the pattern reads no plugin request, the record passes only where this table names that
// pattern at a contract above the record's. Each pattern is held to routesReadingNoBody. The map
// is the registry's own, which no caller writes to.
func RemovedRoutes() map[string]int { return registry().removedRoutes }

// frozenRemovedRoutes is the rows of RemovedRoutes that the numbered entries removed.
var frozenRemovedRoutes = map[string]int{
	// The route recorded at 193, 199 and 202 was removed at 205 and now resolves to this pattern.
	"POST /legion/v1/children/{op}": childRouteRemovedContract,
}

// requestDecoded is told the pattern of the route and the Go type of every body decodeJSON
// decodes. It is nil in a daemon and set by one test of this package, which holds each pair to
// the table of the request types a plugin sends (request_shapes_test.go's pluginRequests): the
// request gate walks the types that table names, so a handler that decodes another type would
// leave the gate reading a type no route reads.
var requestDecoded func(pattern string, into reflect.Type)
