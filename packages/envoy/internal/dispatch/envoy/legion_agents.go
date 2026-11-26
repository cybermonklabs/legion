package envoy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/sjawhar/envoy/listenerwire"
)

// Agent returns the Legion agent registry's entry for one session (listenerwire.LegionAgentAnswer,
// whose Live is never read as "that agent has stopped"), or ErrNotFound when the registry holds
// none: a 404, or an id the listener refuses (a 400 or 413), under which no entry can be recorded.
// Every other status, a body that does not decode and a transport failure are ErrUnavailable
// carrying the listener's reason: a read the registry did not answer says nothing about whether
// the session was recorded.
func (c *Client) Agent(ctx context.Context, sessionID string) (listenerwire.LegionAgentAnswer, error) {
	route := "/v1/legion/agents/sessions/" + url.PathEscape(sessionID)
	response, err := c.get(ctx, route, "")
	if err != nil {
		return listenerwire.LegionAgentAnswer{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return listenerwire.LegionAgentAnswer{}, ErrNotFound
	default:
		return listenerwire.LegionAgentAnswer{}, unavailableGetStatus(response, route)
	}
	var agent listenerwire.LegionAgentAnswer
	if err := json.NewDecoder(response.Body).Decode(&agent); err != nil {
		return listenerwire.LegionAgentAnswer{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	return agent, nil
}

// RoleAgents returns every session the Legion agent registry holds for a role token, newest
// first, the first being the role's current holder; none when it holds none. Any answer but a 200
// is ErrUnavailable.
func (c *Client) RoleAgents(ctx context.Context, roleToken string) ([]listenerwire.LegionAgentAnswer, error) {
	return c.legionAgents(ctx, "/v1/legion/agents/roles/"+url.PathEscape(roleToken), "")
}

// IssueAgents returns every session the Legion agent registry holds for an issue, by role in
// workflow order (architect, planner, implementer, tester, reviewer, merger), then newest first;
// none when it holds none. Any answer but a 200 is ErrUnavailable.
func (c *Client) IssueAgents(ctx context.Context, issueKey string) ([]listenerwire.LegionAgentAnswer, error) {
	return c.legionAgents(ctx, "/v1/legion/agents", "?issue="+url.QueryEscape(issueKey))
}

func (c *Client) legionAgents(ctx context.Context, route, query string) ([]listenerwire.LegionAgentAnswer, error) {
	response, err := c.get(ctx, route, query)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, unavailableGetStatus(response, route)
	}
	var body listenerwire.LegionAgentList
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	if body.Entries == nil {
		body.Entries = []listenerwire.LegionAgentAnswer{}
	}
	return body.Entries, nil
}
