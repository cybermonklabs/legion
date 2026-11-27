package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/repositories/{owner}/{repo}/app-installation", Auth: authAny, Versions: onlyV2,
		Description: "Whether Dispatch's GitHub App, which GitHub sends the repository's webhook events through, is " +
			"installed on the repository, read from GitHub on every call: {installed}, false when GitHub answers 404, " +
			"for a repository no mapping names too. 400 INVALID_REPOSITORY, before GitHub is asked anything, when " +
			"the path's owner or name has a character GitHub does not allow or the name is . or .. or empty or longer " +
			"than 100 characters (a trailing .git trimmed). 503 GITHUB_APP_UNCONFIGURED when Dispatch holds no App " +
			"key, 502 GITHUB_UNREADABLE naming the repository and GitHub's error on any other failure.",
		Handler:  s.getAppInstallation,
		Resource: "app-installation", Kind: kindSingleton,
		Shape:    routeShape{Status: http.StatusOK, Response: appInstallationResponse{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, repositoryPathInvalidRefusal, gitHubAppKeyMissingRefusal, gitHubReadFailedRefusal},
	}
})
