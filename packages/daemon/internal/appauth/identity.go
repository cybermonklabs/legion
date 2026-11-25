package appauth

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// AppRoleFor is the one role-to-App decision. Keep this switch exhaustive over claim.Role, plus
// the one non-claim role it also decides: the project controller, which acts as the review App
// (it writes no issue branch). Roles that can write an issue branch use implement; every
// verdict-only role, and the controller, uses review.
func AppRoleFor(role claim.Role) AppRole {
	switch role {
	case claim.RoleArchitect, claim.RolePlanner, claim.RoleTester, claim.RoleReviewer, claim.RoleController:
		return Review
	case claim.RoleImplementer, claim.RoleMerger:
		return Implement
	}
	panic(fmt.Sprintf("AppRoleFor: unsupported claim role %q", role))
}

// botSuffix is what GitHub puts after an App's slug to name its bot account: the App whose slug
// is `legion-reviewer` acts as the login `legion-reviewer[bot]`. It is written here once, and
// botLogin and slugOfBotLogin are the only places this package puts it on a slug or takes it
// off: a second spelling that drifted would read a login GitHub serves as no App's bot, and the
// package would act as an account that is not its App's.
const botSuffix = "[bot]"

// botLogin is the login of the bot account of the App whose slug is slug.
func botLogin(slug string) string { return slug + botSuffix }

// slugOfBotLogin is the slug of the App whose bot account is login, and whether login is a bot
// account's at all. A login that is not one comes back whole, with false.
func slugOfBotLogin(login string) (string, bool) { return strings.CutSuffix(login, botSuffix) }
