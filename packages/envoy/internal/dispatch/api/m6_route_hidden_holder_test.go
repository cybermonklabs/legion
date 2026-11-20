package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestM6ARouteNamesNoRoleOrHolderItsReaderMayNotSee: a route reaches a reader only as far as the
// reader sees what it names (seedHiddenHolders). To alice, who does not see PRIV:
//
//   - role:legion-priv-architect, a role of PRIV by its name (contracts.RoleProject, as
//     access.RoleClause reads it), answers as no route at all, whoever holds it, in the issue read,
//     the listing and the ask read;
//   - role:reviewer, whose holder session-priv she may not see, answers as role:idle, which no
//     session holds: no_holder with no holder, never live;
//   - ?route_status=live lists neither.
//
// carol, who sees PRIV, reads both live with their holders.
//
// Control: leaveOutRoute keeping a reach whose holder is hidden and a role of a hidden project;
// alice then reads role:legion-priv-architect live and role:reviewer live, and each row fails.
func TestM6ARouteNamesNoRoleOrHolderItsReaderMayNotSee(t *testing.T) {
	seed := seedHiddenHolders(t, testServerOptions{})
	reads := func(key string) []string {
		return []string{"/api/v1/issues/" + key, "/api/v1/asks/" + seed.asks[key]}
	}

	t.Run("a role of a project the reader does not see", func(t *testing.T) {
		for _, target := range reads(seed.toPrivateRole) {
			got, body := seed.routeOf(t, callerAlice, target)
			if got != "null null null null" {
				t.Errorf("alice's GET %s reads the route as %s, want no route: role:%s is a role of PRIV", target, got, privateProjectRole)
			}
			if strings.Contains(body, privateProjectRole) {
				t.Errorf("alice's GET %s names role:%s: %s", target, privateProjectRole, body)
			}
			if got, _ := seed.routeOf(t, callerCarol, target); got != "role:"+privateProjectRole+" live "+sessionOfPublic+" null" {
				t.Errorf("carol's GET %s reads the route as %s, want it live with session-pub", target, got)
			}
		}
	})

	t.Run("a role whose holder the reader may not see", func(t *testing.T) {
		for index, target := range reads(seed.toHiddenHolder) {
			got, body := seed.routeOf(t, callerAlice, target)
			nobodys, _ := seed.routeOf(t, callerAlice, reads(seed.toUnheldRole)[index])
			if want := strings.Replace(nobodys, "role:"+unheldRole, "role:"+hiddenHolderRole, 1); got != want || want != "role:"+hiddenHolderRole+" no_holder null null" {
				t.Errorf("alice's GET %s reads the route as %s, want %s, as she reads role:%s, which no session holds (%s)", target, got, want, unheldRole, nobodys)
			}
			if strings.Contains(body, sessionOfPrivate) {
				t.Errorf("alice's GET %s names %s: %s", target, sessionOfPrivate, body)
			}
			if got, _ := seed.routeOf(t, callerCarol, target); got != "role:"+hiddenHolderRole+" live "+sessionOfPrivate+" null" {
				t.Errorf("carol's GET %s reads the route as %s, want it live with session-priv", target, got)
			}
		}
	})

	listed := func(caller privateCaller, query string) map[string]string {
		t.Helper()
		var rows []struct {
			Key               string  `json:"key"`
			Route             *string `json:"route"`
			RouteStatus       *string `json:"route_status"`
			RouteHolder       *string `json:"route_holder"`
			RouteStatusReason *string `json:"route_status_reason"`
		}
		response := seed.mustRequest(t, caller, http.MethodGet, "/api/v1/issues?project="+seedPublic+query, nil, http.StatusOK)
		if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode %s's listing: %v", caller, err)
		}
		routes := map[string]string{}
		for _, row := range rows {
			routes[row.Key] = reachText(row.Route, row.RouteStatus) + " " + reachText(row.RouteHolder, row.RouteStatusReason)
		}
		return routes
	}

	t.Run("the listing", func(t *testing.T) {
		routes := listed(callerAlice, "")
		for key, want := range map[string]string{
			seed.toPrivateRole:  "null null null null",
			seed.toHiddenHolder: "role:" + hiddenHolderRole + " no_holder null null",
			seed.toUnheldRole:   "role:" + unheldRole + " no_holder null null",
		} {
			if routes[key] != want {
				t.Errorf("alice's listing reads %s's route as %s, want %s", key, routes[key], want)
			}
		}
		routes = listed(callerCarol, "")
		for key, want := range map[string]string{
			seed.toPrivateRole:  "role:" + privateProjectRole + " live " + sessionOfPublic + " null",
			seed.toHiddenHolder: "role:" + hiddenHolderRole + " live " + sessionOfPrivate + " null",
		} {
			if routes[key] != want {
				t.Errorf("carol's listing reads %s's route as %s, want %s", key, routes[key], want)
			}
		}
	})

	t.Run("the route_status filter", func(t *testing.T) {
		for _, test := range []struct {
			caller privateCaller
			status string
			want   []string
		}{
			{callerAlice, "live", []string{}},
			{callerAlice, "no_holder", []string{seed.toHiddenHolder, seed.toUnheldRole}},
			{callerCarol, "live", []string{seed.toPrivateRole, seed.toHiddenHolder}},
		} {
			got := slices.Sorted(maps.Keys(listed(test.caller, "&route_status="+test.status)))
			want := slices.Sorted(slices.Values(test.want))
			if !slices.Equal(got, want) {
				t.Errorf("%s's ?route_status=%s lists %v, want %v", test.caller, test.status, got, want)
			}
		}
	})
}
