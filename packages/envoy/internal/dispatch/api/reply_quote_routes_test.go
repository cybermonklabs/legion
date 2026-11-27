package api

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// replyBlockMembers are the request members that name the block of a post a reply answers:
// reply_to_block on the comment routes, in_reply_to_block on the message routes.
var replyBlockMembers = []string{"reply_to_block", "in_reply_to_block"}

// A reply to a block takes the words it quotes beside the block it names, quote and occurrence,
// as model.AnchorInput holds them for a document anchor. The walk reads every route's request
// type as its Shape declares it, so a route that comes to name a block to reply to without
// taking its quote fails here, and the routes it finds are held to the six the spec names, so a
// seventh fails until the spec names it too.
func TestEveryRouteThatNamesABlockToReplyToTakesItsQuote(t *testing.T) {
	t.Parallel()
	want := []string{
		"POST /api/v1/agents/{session_id}/messages",
		"POST /api/v1/artifacts/{id}/comments",
		"POST /api/v1/comments/{id}/reply",
		"POST /api/v1/issues/{key}/comments",
		"POST /api/v1/issues/{key}/messages",
		"POST /api/v1/messages/{id}/reply",
	}
	var found []string
	for _, declared := range routeRegistry {
		route, request := declared.Method+" "+declared.Pattern, declared.Shape.Request
		if request == nil {
			continue
		}
		names := false
		for _, problem := range replyQuoteProblems(reflect.TypeOf(request), &names, map[reflect.Type]bool{}) {
			t.Errorf("%s: %s", route, problem)
		}
		if names {
			found = append(found, route)
		}
	}
	slices.Sort(found)
	if !slices.Equal(found, want) {
		t.Errorf("the routes whose request names a block to reply to are\n  %s\nwant the spec's six\n  %s",
			strings.Join(found, "\n  "), strings.Join(want, "\n  "))
	}
}

// replyQuoteProblems walks ty and every struct it holds, through pointers, slices, arrays and
// maps, and answers, for each struct holding a member of replyBlockMembers, every one of quote
// (*string) and occurrence (*int) the same struct lacks or takes in another type. It sets
// *names when it meets such a struct.
func replyQuoteProblems(ty reflect.Type, names *bool, seen map[reflect.Type]bool) []string {
	for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Array || ty.Kind() == reflect.Map {
		ty = ty.Elem()
	}
	if ty.Kind() != reflect.Struct || seen[ty] {
		return nil
	}
	seen[ty] = true
	members := map[string]reflect.Type{}
	collectDeclaredMembers(ty, func(name string, field reflect.Type) { members[name] = field })
	var problems []string
	for _, block := range replyBlockMembers {
		if _, ok := members[block]; !ok {
			continue
		}
		*names = true
		for _, beside := range []struct {
			name string
			want reflect.Type
		}{
			{"quote", reflect.TypeFor[*string]()},
			{"occurrence", reflect.TypeFor[*int]()},
		} {
			got, ok := members[beside.name]
			switch {
			case !ok:
				problems = append(problems, ty.String()+" names "+block+" and takes no "+beside.name+" ("+beside.want.String()+")")
			case got != beside.want:
				problems = append(problems, ty.String()+" names "+block+" and takes "+beside.name+" as "+got.String()+", want "+beside.want.String())
			}
		}
	}
	for _, member := range slices.Sorted(maps.Keys(members)) {
		problems = append(problems, replyQuoteProblems(members[member], names, seen)...)
	}
	return problems
}
