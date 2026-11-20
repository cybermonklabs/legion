package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// mappingRow is one repo_projects row as this test compares the table: its stored name, its
// project, whether it is its project's default, and its renamed_to, "" for none.
type mappingRow struct {
	repo, project string
	isDefault     bool
	renamedTo     string
}

// storedMappings is what the table holds before each request, by name in byte order, as
// mappingTable reads it: `acme/..`, `acme/.git` and `acme/`, names a mapping may have been stored
// under before the settings route held a mapping's path to GitHub's names, which the route's
// canonical reading of a path trims into one another or cannot match as they are; `oldco/tools`,
// an ordinary name whose renamed_to that rule refuses; `oldco/..`, a refused renamed_to on a
// mapping whose own name the strict PUT refuses as well; `Zeta/..`, a name that reading
// lower-cases into another; `Zeta/`, an empty name as `acme/` is, with an owner that reading
// lower-cases; `/a/b`, `/acme` and `acme//`, names only direct SQL stores, whose owner is empty
// at the first `/` and whose path, where there is one, splits them at another; and
// `acme/widgets`, which no warning names.
var storedMappings = []mappingRow{
	{repo: "/a/b", project: "CIR"},
	{repo: "/acme", project: "CIR"},
	{repo: "Zeta/", project: "CIR"},
	{repo: "Zeta/..", project: "CIR"},
	{repo: "acme/", project: "CIR"},
	{repo: "acme/..", project: "CIR"},
	{repo: "acme/.git", project: "CIR"},
	{repo: "acme//", project: "CIR"},
	{repo: "acme/widgets", project: "CIR", isDefault: true},
	{repo: "oldco/..", project: "CIR", renamedTo: "newco/...git"},
	{repo: "oldco/tools", project: "CIR", renamedTo: "acme/...git"},
}

// The request the CI reconcile's WARN names beside a refused mapped name (mappingRepairs), sent
// through the real settings route, removes or mends the one mapping the WARN is about and touches
// no other row. The route's DELETE removes only the mapping stored under its path's own spelling,
// the segments as the mux unescapes them; its PUT reads a path as parseSourceRepo does, through
// refs.ParseExternalRepo, which lower-cases the name and trims one trailing `.git` off it, and
// through the strict rule as well (repoProjectPath). So `acme/.git` is removed by its own name,
// `acme/..` and `Zeta/..` by theirs with the dots percent-encoded, `/a/b` by its own name split at
// its second `/`, the first `/` written `%2F`, the renamed_to of `oldco/tools` is cleared by that
// mapping's PUT, and the renamed_to of `oldco/..`, whose own name the PUT refuses, is ended by
// that mapping's removal. Each request runs against the whole table, so a request that reached a
// neighbour shows as that neighbour gone.
//
// `acme/`, `Zeta/`, `/acme` and `acme//`, which no path spells, are removed by no request: the
// WARN names none and says the mapping is removed by SQL. The DELETE of `acme/` or `Zeta/` with
// one `.git` more, which the route's reading reads as `acme/` (and as `zeta/`, lower-cased),
// removes nothing, `acme/.git` among them where no mapping is stored under that spelling; and the
// DELETE of the path that would spell `/acme` or `acme//` at one of its `/`, an empty segment or
// a segment of `/` alone, reaches no wildcard of the route.
//
// The negative controls: mappingRemoval naming, for a mapping no path spells, the DELETE of the
// path the route's reading reads as it, as it did, names
// `DELETE /api/v1/settings/repo-projects/acme/.git` for `acme/`; the DELETE reaching, where its
// spelling names no mapping, the row that reading names, as it did, removes `acme/` for the
// DELETE of `acme/.git` where `acme/.git` is not stored; spelledPath splitting a name at its first
// `/` alone, as it did, names no request for `/a/b`; and spelledPath taking that first split
// whatever its halves, names `DELETE /api/v1/settings/repo-projects//acme` for `/acme`.
func TestTheRequestAMappingWarningNamesReachesThatMappingThroughTheSettingsRoute(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": "CIR", "name": "CI reconcile"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	repairs := MappingRepairs(database.Pool)

	for _, tc := range []struct {
		name string
		// row is the name the pass refuses and names in its WARN: a mapping's canonical name,
		// which is its renamed_to where it names one.
		row string
		// field is the WARN's field that names the request; removed is the mapping that
		// request removes, and mended the mapping whose renamed_to it clears.
		field, removed, mended string
	}{
		{name: "a mapping stored under a dot segment", row: "acme/..", field: "removed_by", removed: "acme/.."},
		{name: "a mapping stored under a name ending in .git", row: "acme/.git", field: "removed_by", removed: "acme/.git"},
		{name: "a mapping stored under a name the reading lower-cases", row: "Zeta/..", field: "removed_by", removed: "Zeta/.."},
		{name: "a mapping stored under a name whose owner is empty at its first slash", row: "/a/b", field: "removed_by", removed: "/a/b"},
		{name: "a refused renamed_to of an ordinary mapping", row: "acme/...git", field: "mended_by", mended: "oldco/tools"},
		{name: "a refused renamed_to of a mapping the PUT refuses", row: "newco/...git", field: "removed_by", removed: "oldco/.."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedMappings(t, database)
			field, named := namedRepair(t, repairs(ctx, tc.row))
			method, rest, _ := strings.Cut(named, " ")
			target, _, _ := strings.Cut(rest, " ")
			if !strings.HasPrefix(target, repoProjectsPath) {
				t.Fatalf("the WARN names %s %q, which is no request of the settings route", field, named)
			}
			var body any
			if method == http.MethodPut {
				// The PUT the WARN names takes the mapping's project and a renamed_to GitHub
				// allows, or "" to clear it; every mapping here is in CIR.
				body = map[string]any{"project": "CIR", "renamed_to": ""}
			}
			if response := dispatchRequest(t, handler, method, target, body, "alice"); response.Code/100 != 2 {
				t.Errorf("%s answered %d %s", named, response.Code, response.Body.String())
			}
			want := slices.Clone(storedMappings)
			want = slices.DeleteFunc(want, func(row mappingRow) bool { return row.repo == tc.removed })
			for i := range want {
				if want[i].repo == tc.mended {
					want[i].renamedTo = ""
				}
			}
			if got := mappingTable(t, database); !slices.Equal(got, want) {
				t.Errorf("after %s the table holds\n%+v\nwant\n%+v", named, got, want)
			}
			if field != tc.field {
				t.Errorf("the WARN names %q under %s, want it under %s", named, field, tc.field)
			}
		})
	}

	for _, tc := range []struct {
		name string
		// row is the mapping no path spells; named is what the WARN names for it.
		row, named string
		// unstored is a mapping taken out of the table first, "" for none.
		unstored string
		// targets are the DELETEs nearest to row, none of which reaches it: for an empty name,
		// row's name with one `.git` more, which the route's reading reads as row or,
		// lower-cased, as another name, and which the route answers 204 for a mapping it does
		// not hold (routed); for every other row, the path that would spell it at each of its
		// `/`, which no wildcard of the route matches, so the route never answers it.
		targets []string
		routed  bool
	}{
		{
			name: "a mapping stored under an empty name", row: "acme/", unstored: "acme/.git",
			named:   `no request reaches the mapping stored under "acme/": remove it by SQL`,
			targets: []string{"/api/v1/settings/repo-projects/acme/.git"}, routed: true,
		},
		{
			name: "a mapping stored under an empty name and an owner the reading lower-cases", row: "Zeta/",
			named:   `no request reaches the mapping stored under "Zeta/": remove it by SQL`,
			targets: []string{"/api/v1/settings/repo-projects/Zeta/.git", "/api/v1/settings/repo-projects/zeta/.git"}, routed: true,
		},
		{
			name: "a mapping stored under a name whose only slash begins it", row: "/acme",
			named:   `no request reaches the mapping stored under "/acme": remove it by SQL`,
			targets: []string{"/api/v1/settings/repo-projects//acme"},
		},
		{
			name: "a mapping stored under a name each of whose slashes leaves nothing or a slash on one side", row: "acme//",
			named:   `no request reaches the mapping stored under "acme//": remove it by SQL`,
			targets: []string{"/api/v1/settings/repo-projects/acme/%2F", "/api/v1/settings/repo-projects/acme%2F/"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedMappings(t, database)
			if _, err := database.Pool.Exec(ctx, `delete from repo_projects where repo = $1`, tc.unstored); err != nil {
				t.Fatalf("take %q out of the table: %v", tc.unstored, err)
			}
			want := slices.DeleteFunc(slices.Clone(storedMappings), func(row mappingRow) bool { return row.repo == tc.unstored })
			if field, named := namedRepair(t, repairs(ctx, tc.row)); field != "removed_by" || named != tc.named {
				t.Errorf("the WARN names %s %q, want removed_by %q", field, named, tc.named)
			}
			for _, target := range tc.targets {
				response := dispatchRequest(t, handler, http.MethodDelete, target, nil, "alice")
				switch {
				case tc.routed && response.Code != http.StatusNoContent:
					t.Errorf("DELETE %s answered %d %s, want 204 for a mapping it does not hold", target, response.Code, response.Body.String())
				case !tc.routed && response.Code/100 == 2:
					t.Errorf("DELETE %s answered %d %s, want no route of the settings route to answer it", target, response.Code, response.Body.String())
				}
				if got := mappingTable(t, database); !slices.Equal(got, want) {
					t.Errorf("DELETE %s changed the table to\n%+v\nwant\n%+v", target, got, want)
				}
			}
		})
	}
}

// seedMappings makes the table hold storedMappings and nothing else, in project CIR.
func seedMappings(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `delete from repo_projects`); err != nil {
		t.Fatalf("empty repo_projects: %v", err)
	}
	for _, row := range storedMappings {
		if _, err := database.Pool.Exec(ctx, `
			insert into repo_projects (repo, project, created_by, is_default, renamed_to)
			values ($1, $2, '{"kind":"system","id":"test"}'::jsonb, $3, nullif($4, ''))
		`, row.repo, row.project, row.isDefault, row.renamedTo); err != nil {
			t.Fatalf("store the mapping %q: %v", row.repo, err)
		}
	}
	if got := mappingTable(t, database); !slices.Equal(got, storedMappings) {
		t.Fatalf("the seeded table holds %+v, want %+v", got, storedMappings)
	}
}

// mappingTable is every row of repo_projects, by name in byte order.
func mappingTable(t *testing.T, database *store.Store) []mappingRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select repo, project, is_default, coalesce(renamed_to, '') from repo_projects`)
	if err != nil {
		t.Fatalf("read repo_projects: %v", err)
	}
	defer rows.Close()
	var table []mappingRow
	for rows.Next() {
		var row mappingRow
		if err := rows.Scan(&row.repo, &row.project, &row.isDefault, &row.renamedTo); err != nil {
			t.Fatalf("scan a mapping: %v", err)
		}
		table = append(table, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read repo_projects: %v", err)
	}
	slices.SortFunc(table, func(a, b mappingRow) int { return cmp.Compare(a.repo, b.repo) })
	return table
}

// namedRepair is the one field and the one repair a WARN's repairs name, failing t on anything
// else: an error, no field, two fields, or a field naming the repairs of two mappings.
func namedRepair(t *testing.T, repairs []any) (field, named string) {
	t.Helper()
	if len(repairs) != 2 {
		t.Fatalf("the WARN names %v, want one field", repairs)
	}
	field, _ = repairs[0].(string)
	named, _ = repairs[1].(string)
	if (field != "removed_by" && field != "mended_by") || named == "" || strings.Count(named, "repo-projects/") > 1 {
		t.Fatalf("the WARN names %v, want the repair of one mapping under removed_by or mended_by", repairs)
	}
	return field, named
}
