package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The repositories block names a record kept on a configured pair's `from` under the pair's `to`
// exactly where the rename rule serves it there (ghrepo.Renames.Serves): GitHub answered the `to`
// served and the `from` not; GitHub answered neither name yet, which serves the pair as the
// operator configured it; and never where GitHub answered both names served, since each is then a
// repository of its own. The entry reads its Apps and its read error from the served name's
// answer, and shows Apps only once GitHub answered both reads.
func TestTheRepositoriesBlockServesAPairsOldNameWhereTheRenameRuleDoes(t *testing.T) {
	t.Parallel()
	renames := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	live, err := record.GitHubRepositoriesOf([]record.Issue{
		{Key: "LEGION-1", GitHubRepo: "sjawhar/legion"},
		{Key: "LEGION-2", GitHubRepo: "Acme/Widgets"},
		{Key: "LEGION-3", GitHubRepo: "acme/widgets"},
	})
	if err != nil {
		t.Fatal(err)
	}
	both := AppInstallationsView{Implement: true, Review: true}
	widgetsFailed := RepositoryAnswer{ReadError: "the implement App's installation on Acme/Widgets could not be read: GitHub answered 502"}
	for _, tc := range []struct {
		name    string
		answers map[string]RepositoryAnswer
		want    LiveRepositoryView
	}{
		{
			name: "the transfer answered",
			answers: map[string]RepositoryAnswer{
				"sjawhar/legion":       {Answered: true},
				"cybermonklabs/legion": {Answered: true, Apps: both},
			},
			want: LiveRepositoryView{Repo: "sjawhar/legion", ServedAs: "cybermonklabs/legion", Apps: &both},
		},
		{
			name: "no read of either name answered",
			answers: map[string]RepositoryAnswer{
				"cybermonklabs/legion": {Apps: AppInstallationsView{Implement: true}, ReadError: "GitHub answered 502"},
			},
			want: LiveRepositoryView{Repo: "sjawhar/legion", ServedAs: "cybermonklabs/legion", ReadError: "GitHub answered 502"},
		},
		{
			name: "both names served",
			answers: map[string]RepositoryAnswer{
				"sjawhar/legion":       {Answered: true, Apps: both},
				"cybermonklabs/legion": {Answered: true, Apps: both},
			},
			want: LiveRepositoryView{Repo: "sjawhar/legion", Apps: &both},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.answers["acme/widgets"] = widgetsFailed
			answer := func(name ghrepo.Repository) RepositoryAnswer { return tc.answers[strings.ToLower(name.String())] }
			view := RepositoriesViewOf(live, renames, answer, nil)
			want := []LiveRepositoryView{{Repo: "Acme/Widgets", ReadError: widgetsFailed.ReadError}, tc.want}
			if !reflect.DeepEqual(view.Live, want) {
				t.Fatalf("live = %+v\nwant %+v", view.Live, want)
			}
			if view.WithoutWebhookApp != nil || view.Caches != nil {
				t.Fatalf("withoutWebhookApp = %+v, caches = %+v, want neither", view.WithoutWebhookApp, view.Caches)
			}
		})
	}
}
