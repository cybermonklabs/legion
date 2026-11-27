package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/legion/board",
	Auth:        authAny,
	Description: "The Legion board of the project its path names, from what the sampler recorded of that project's daemon and the stored per-issue Legion facts: twelve columns (queue, design, planning, implementing, testing, reviewing, accepting, retro, merging, awaiting_children, awaiting_person, held), each card naming its issue's key, title, priority, phase and when that phase began, the slot it holds and what it uses it for (slot_use), the roles in a turn now, the issues a blocker link holds it on, what it waits on from a person, why it waits and since when (waiting) and its place in the daemon's waiting line (waiting_place), its tree root and the children of its tree that have not started; an issues map carrying, by issue key, the stored facts of every open issue with Legion phase work and of every issue the board shows (each card's issue and every issue of a tree a card stands in), a closed issue being in the map only through its tree; and a header with the slots in use, the slot cap, the daemon's own number of P0 holders (p0_holders, which hold their slots outside the cap; null, never 0, where the daemon's newest recorded state gives none), the queue's length, the last read of the daemon with its failure, and how fresh the stored facts it serves are. 404 LEGION_NOT_CONFIGURED when the project names no daemon, 400 INVALID_PROJECT for a key that is not a project key, 404 PROJECT_NOT_FOUND for a project that does not exist.",
	Handler:     (*server).getLegionBoard,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: legionBoardResponse{},
	},
})
