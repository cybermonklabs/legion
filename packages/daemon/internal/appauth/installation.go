package appauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Installations answers whether a GitHub App is installed on a repository. The daemon records an
// issue on a repository only once both role Apps are installed on it.
//
// InstalledOn is GitHub's answer wherever GitHub holds the repository now, a redirect followed;
// InstalledUnder is its answer for the name asked alone, a name GitHub redirects answering that
// no App is installed under it. The daemon reads each name a configured rename holds by the
// second, since a name GitHub redirects is no repository of its own (ghrepo.Renames), and every
// other name by the first.
type Installations interface {
	InstalledOn(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error)
	InstalledUnder(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error)
}

var _ Installations = (*Manager)(nil)

// NotInstalledOn is the sentence naming missing, the role Apps GitHub answered are not installed
// on repo, in the words the daemon refuses an admission with and legion state shows: "the review
// App is not installed on acme/gadgets", or "the implement and review Apps are not installed on
// acme/gadgets" for both (AppsAre). missing holds one role at least.
func NotInstalledOn(missing []AppRole, repo string) string {
	return AppsAre(missing, "not installed on "+repo)
}

// AppsAre is the sentence saying of roles, each a role App, what predicate says of them: "the
// review App is <predicate>", or "the implement and review Apps are <predicate>" for both. It
// builds NotInstalledOn's sentence and the daemon's rename pass's of the Apps still installed on a
// configured pair's old name ("still installed on sjawhar/legion"). roles holds one role at least.
func AppsAre(roles []AppRole, predicate string) string {
	if len(roles) == 1 {
		return "the " + string(roles[0]) + " App is " + predicate
	}
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, string(role))
	}
	return "the " + strings.Join(names, " and ") + " Apps are " + predicate
}

// Logins answers each GitHub App's bot login under the App's own JWT, which needs no installation
// on any repository: the review App's is the login the workflow judges a push's pusher and a
// review's author by, known even while the daemon serves no repository. An App on token_command
// has no JWT: its login is read under a token of any installation its command releases.
type Logins interface {
	AppLogin(ctx context.Context, role AppRole) (string, error)
}

var _ Logins = (*Manager)(nil)

// AppLogin is role's App's bot login, "<slug>[bot]", read from GET /app under the App's JWT, or in
// token mode from GraphQL's viewer under a token of any installation, checked against the
// configured app_id (tokenIdentity).
func (m *Manager) AppLogin(ctx context.Context, role AppRole) (string, error) {
	app, err := m.app(role)
	if err != nil {
		return "", err
	}
	if app.TokenCommand != "" {
		installation, err := m.anyInstallation(ctx, role, app)
		if err != nil {
			return "", fmt.Errorf("read the %s App's login: %w", role, err)
		}
		identity, err := m.tokenIdentity(ctx, role, app.AppID, installation.token)
		if err != nil {
			return "", fmt.Errorf("read the %s App's login: %w", role, err)
		}
		return identity.Name, nil
	}
	jwt, err := m.generateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return "", err
	}
	slug, err := m.appSlug(ctx, jwt)
	if err != nil {
		return "", fmt.Errorf("read the %s App's login: %w", role, err)
	}
	return slug + "[bot]", nil
}

// InstalledOn reads GET /repos/{owner}/{repo}/installation under role's App JWT: GitHub answers
// the installation that covers the repository, 2xx, or 404 when the App has none there. An owner's
// installation may cover only some of its repositories, so the read is per repository. A redirect
// is followed, as the Manager's client follows it (net/http keeps the App JWT on a redirect within
// one host), so a name GitHub moved is answered for the repository where it is now. Any other
// answer is an error naming the repository and the App, a TransientError when it is GitHub's
// trouble. In token mode it reads the owner's installation's repositories instead
// (installedFromCommand).
func (m *Manager) InstalledOn(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error) {
	return m.installedOn(ctx, role, repo, false)
}

// InstalledUnder is InstalledOn of repo's own name, read without following a redirect. GitHub
// answers a moved repository's API path with a 301 to where the repository is now (GET
// /repos/sjawhar/legion answered 301 to /repositories/1146927990 on 2026-11-19), and a read that
// follows it reads the installation that covers the repository there, so the name it moved away
// from would answer installed. Here a 3xx is the answer instead: no App is installed under that
// name. In token mode it is InstalledOn, whose read of the owner's own installation's repositories
// follows no name to another (installedFromCommand).
func (m *Manager) InstalledUnder(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error) {
	return m.installedOn(ctx, role, repo, true)
}

// installedOn is InstalledOn, and with own set InstalledUnder: the read made through the Manager's
// client that follows no redirect (noRedirect), a 3xx answering not installed.
func (m *Manager) installedOn(ctx context.Context, role AppRole, repo ghrepo.Repository, own bool) (bool, error) {
	app, err := m.app(role)
	if err != nil {
		return false, err
	}
	if app.TokenCommand != "" {
		return m.installedFromCommand(ctx, role, repo)
	}
	jwt, err := m.generateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.baseURL+"/repos/"+url.PathEscape(repo.Owner())+"/"+url.PathEscape(repo.Name())+"/installation", nil)
	if err != nil {
		return false, err
	}
	m.appHeaders(request, jwt)
	client := m.client
	if own {
		client = m.noRedirect
	}
	response, err := client.Do(request)
	if err != nil {
		return false, &TransientError{Err: fmt.Errorf("read whether the %s App is installed on %s: %w", role, repo, err)}
	}
	body := refusalBody(response)
	switch {
	case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices:
		return true, nil
	case response.StatusCode == http.StatusNotFound:
		return false, nil
	case own && response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest:
		return false, nil
	default:
		return false, transient(response, body, fmt.Errorf("read whether the %s App is installed on %s failed (%d)", role, repo, response.StatusCode))
	}
}
