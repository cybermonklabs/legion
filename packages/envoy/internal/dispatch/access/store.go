package access

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/principal"
)

// The refusals of the reads and writes below. Each is wrapped with what it names, so a caller
// matches it with errors.Is and shows its text.
var (
	ErrProjectNotFound       = errors.New("no project has this key")
	ErrGroupNotFound         = errors.New("no group has this name")
	ErrOwnerNotAllowed       = errors.New("an owner is a full member who has signed in")
	ErrGroupMemberRestricted = errors.New("a group holds full members only, and this person is a restricted member")
	ErrPrincipalInput        = errors.New("a person is named by their email, and a group by its name")
	ErrVisibilityInput       = errors.New("a project is public or private")
	ErrListFull              = fmt.Errorf("an access list names at most %d people and groups", MaxPrincipals)
	ErrGroupFull             = fmt.Errorf("a group holds at most %d members", MaxGroupMembers)
	ErrSlackChannelInput     = errors.New("a Slack channel is named by its id, such as C0123456789")
	ErrSlackChannelsFull     = fmt.Errorf("a project names at most %d Slack channels", MaxSlackChannels)
	ErrGroupsPerOwnerFull    = fmt.Errorf("an owner holds at most %d groups", model.MaxGroupsPerOwner)
	ErrSlackChannelShared    = errors.New("a Slack channel belongs to one private project")
)

// The bounds of an access list and of a group.
const (
	MaxPrincipals   = 500
	MaxGroupMembers = 1000
)

// The two kinds of principal an access list names.
const (
	PrincipalPerson = "person"
	PrincipalGroup  = "group"
)

// ChangedChannel is the Postgres channel each write here that can move what someone sees - a
// project's owner, visibility or access list, a group - notifies inside its own transaction, so
// every Dispatch process hears it once the write commits and none hears a write that did not. It
// carries no payload: a listener reads what it needs afresh. The document service closes on it each
// socket whose person no longer reads its room (docs: Service.recheckAccess). A change made without
// it, by SQL alone, is found on the document sockets' heartbeat instead (docs:
// Service.RunAccessRecheck).
const ChangedChannel = "dispatch_access_changed"

// announceChange notifies ChangedChannel in tx, which Postgres delivers if tx commits.
func announceChange(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select pg_notify($1, '')`, ChangedChannel); err != nil {
		return fmt.Errorf("announce the access change: %w", err)
	}
	return nil
}

// groupName is the form of a group's name, which access_groups' own check repeats.
var groupName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidGroupName reports whether name is the name of a group: lowercase letters, digits, dots,
// underscores and dashes, starting with a letter or digit, at most 63 long.
func ValidGroupName(name string) bool { return groupName.MatchString(name) }

// PersonPrincipal is the stored form of a person an access list or a group names: the email as
// typed, in its stored form (model.CanonicalLogin), refused as ErrPrincipalInput when it is not a
// person's email as Dispatch stores one (principal.CheckPersonLogin): no email, a control or bidi
// character, or a capital beyond A-Z, which names nobody who could sign in.
func PersonPrincipal(email string) (string, error) {
	login := model.CanonicalLogin(email)
	if err := principal.CheckPersonLogin(login); err != nil {
		return "", fmt.Errorf("%w: %w", ErrPrincipalInput, err)
	}
	return login, nil
}

// MemberKind is the member kind of the person login names, MemberNone when no people row holds
// them. Its error names the read that failed and never the login: every caller knows whom it
// asked, and a caller that logs the error would otherwise write a person's email
// (TestNoAccessErrorNamesAPerson).
func MemberKind(ctx context.Context, q Querier, login string) (model.MemberKind, error) {
	var kind string
	err := q.QueryRow(ctx, `select member_kind from people where email = $1`, model.CanonicalLogin(login)).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.MemberNone, nil
	}
	if err != nil {
		return model.MemberNone, fmt.Errorf("read a person's member kind: %w", err)
	}
	return model.MemberKind(kind), nil
}

// The lock order every access write takes, so no two of them wait on each other in a cycle: a
// group's name first (LockGroupName), then the project's owner lock (store.LockOwner), then the
// rows a write appends events on. A write of a group (PutGroup, DeleteGroup) takes the name and
// then the projects its delete sweeps; a write of a project's list that names a group takes the
// same name before the project (LockForAccessWrite), so a delete of that group and the write are
// ordered by the name: the delete waits for the write and then sweeps its row, or the write,
// seeing the delete committed, finds no group.

// LockGroupName takes, for the rest of tx, the lock that orders every write of group name.
func LockGroupName(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('access_group:' || $1, 0))`, name); err != nil {
		return fmt.Errorf("lock group %s: %w", name, err)
	}
	return nil
}

// LockForAccessWrite takes, in the order above, the locks a write of project key's access takes:
// the group's name when the write names a group (empty when it names none), then the project's
// owner lock, the advisory lock every writer of a project takes (store.LockOwner), and never a
// row lock on projects (TestNoForUpdateOnOwnerTables). It answers the project's owner as tx reads
// it in the statement after that lock, nil for a project with none, and ErrProjectNotFound for an
// unknown key. AddPrincipal and RemovePrincipal of a group are sound only after it.
func LockForAccessWrite(ctx context.Context, tx pgx.Tx, key, group string) (*string, error) {
	if group != "" {
		if err := LockGroupName(ctx, tx, group); err != nil {
			return nil, err
		}
	}
	if err := lockProject(ctx, tx, key); err != nil {
		return nil, err
	}
	var owner *string
	err := tx.QueryRow(ctx, `select owner from projects where key = $1`, key).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("read the owner of project %s: %w", key, err)
	}
	return owner, nil
}

// lockProject takes project key's owner lock for the rest of tx; taking it again in tx returns at
// once. The read it guards is a statement of its own after it (store.LockOwner). Its failure is
// the store's, which names the project ("lock project <key>: …"), so it is not wrapped again, but
// for a wait past the transaction's lock_timeout, which is ErrLockTimeout naming LockOwnerRow.
func lockProject(ctx context.Context, tx pgx.Tx, key string) error {
	return lockTimedOut(LockOwnerRow, store.LockOwner(ctx, tx, store.ProjectOwner(key)))
}

// ReadProject answers who may see project key and who changes that, as viewer reads it: viewer is
// the visibility argument every read binds (refs.VisibilityArg), and a project it does not see
// answers ErrProjectNotFound, exactly as a key no project holds.
func ReadProject(ctx context.Context, q Querier, key string, viewer any) (model.ProjectAccess, error) {
	return readProject(ctx, q, key, `p.key = $1 and `+ProjectClause("p.key", "$2"), key, viewer)
}

// readProjectWritten is ReadProject for an access write's own answer: it runs inside the write,
// after the owner's check under the project's lock, and answers what the write left whoever may
// see the project now, so a writer who gave the project away is still told what they wrote.
func readProjectWritten(ctx context.Context, q Querier, key string) (model.ProjectAccess, error) {
	return readProject(ctx, q, key, `p.key = $1`, key)
}

// readProject reads the access of the one project where names, with args, or ErrProjectNotFound.
func readProject(ctx context.Context, q Querier, key, where string, args ...any) (model.ProjectAccess, error) {
	var access model.ProjectAccess
	err := q.QueryRow(ctx, `
		select p.key, coalesce(p.owner, ''), p.visibility,
			array(select principal from project_access where project_key = p.key and principal_kind = 'person' order by principal collate "C"),
			array(select principal from project_access where project_key = p.key and principal_kind = 'group' order by principal collate "C"),
			p.slack_channels, p.slack_channels_changed_at,
			array(select listed.principal from project_access listed
				where listed.project_key = p.key and listed.principal_kind = 'person'
					and not exists (select 1 from people where email = listed.principal)
				order by listed.principal collate "C")
		from projects p
		where `+where, args...).Scan(&access.Project, &access.Owner, &access.Visibility, &access.People, &access.Groups,
		&access.SlackChannels, &access.SlackChannelsChangedAt, &access.NotSignedIn)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProjectAccess{}, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	if err != nil {
		return model.ProjectAccess{}, fmt.Errorf("read the access of project %s: %w", key, err)
	}
	return access, nil
}

// OwnerAllowed answers owner's stored form, the canonical login, when owner may own a project or a
// group: a full member who has signed in. Anyone else - a restricted member, or a login nobody has
// signed in with - is ErrOwnerNotAllowed. Every write that names an owner holds it to this: a
// transfer (SetOwner), a group's write (PutGroup) and a create that takes its default assignee as
// owner (api.createProject).
func OwnerAllowed(ctx context.Context, q Querier, owner string) (string, error) {
	login := model.CanonicalLogin(owner)
	kind, err := MemberKind(ctx, q, login)
	if err != nil {
		return "", err
	}
	if kind != model.MemberFull {
		return "", OwnerNotAllowed(login)
	}
	return login, nil
}

// SetOwner gives project key to owner, a full member who has signed in, at any time, whoever owned
// it before. It appends project.access_changed in tx, whose principal is the person the project was
// taken from, when that is another: a reader that held the project's rows through its ownership
// alone learns from it that it no longer reads them (api: eventGapSQL).
func SetOwner(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, owner string, by model.Actor) (model.ProjectAccess, model.Event, error) {
	login, err := OwnerAllowed(ctx, tx, owner)
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	// The previous owner is read under the project's owner lock, the lock updateProject takes
	// before the row, so it is the owner the write replaces.
	if err := lockProject(ctx, tx, key); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	var previous string
	if err := tx.QueryRow(ctx, `select coalesce(owner, '') from projects where key = $1`, key).Scan(&previous); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("read the owner of project %s: %w", key, err)
	}
	if err := updateProject(ctx, tx, key, `update projects set owner = $2 where key = $1`, login); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	if previous == login {
		previous = ""
	}
	return appendProjectChange(ctx, tx, ev, key, model.AccessOwnerChanged, previous, by)
}

// OwnerNotAllowed is the refusal of login as an owner: one who is no full member who has signed in,
// or, to a caller that may not see them, a person it is answered for exactly as for such a one.
func OwnerNotAllowed(login string) error {
	return fmt.Errorf("%w: %s is not one", ErrOwnerNotAllowed, model.CanonicalLogin(login))
}

// SetVisibility makes project key public or private. It appends project.access_changed in tx.
// Whether the deployment lets a project be private at all is the caller's to decide first. A switch
// to public clears the project's full switch (full_switch_at), so a later private stretch is judged
// afresh (RecordFullSwitch).
func SetVisibility(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, visibility string, by model.Actor) (model.ProjectAccess, model.Event, error) {
	if visibility != model.ProjectPublic && visibility != model.ProjectPrivate {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("%w, not %q", ErrVisibilityInput, visibility)
	}
	if err := updateProject(ctx, tx, key, `
		update projects set visibility = $2,
			full_switch_at = case when $2::text = 'public' then null else full_switch_at end
		where key = $1
	`, visibility); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	return appendProjectChange(ctx, tx, ev, key, model.AccessVisibilityChanged, "", by)
}

// RecordFullSwitch records, in tx, that project key went private by the full switch: the owner's
// switch through the API, which proves provisioning, completeness, the Slack channels and the hubs'
// admission. full_switch_at becomes statement_timestamp(). The caller records it before the
// hub-admission proof, its last step, so a switch whose proof fails rolls it back and records
// nothing. A project private with no full switch recorded went private some other way.
func RecordFullSwitch(ctx context.Context, tx pgx.Tx, key string) error {
	if err := lockProject(ctx, tx, key); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update projects set full_switch_at = statement_timestamp() where key = $1`, key); err != nil {
		return fmt.Errorf("record the full switch of project %s: %w", key, lockTimedOut(LockOwnerRow, err))
	}
	return nil
}

// FullSwitchRecorded reports whether project key is private by a full switch already, as tx reads it
// after the project's owner lock: a switch to private of such a project changes nothing.
func FullSwitchRecorded(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	var recorded bool
	err := tx.QueryRow(ctx, `select visibility = 'private' and full_switch_at is not null from projects where key = $1`, key).Scan(&recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	if err != nil {
		return false, fmt.Errorf("read the full switch of project %s: %w", key, err)
	}
	return recorded, nil
}

// ReadWrittenProject answers project key's access as a write left it in q, for a write of its owner
// that changed nothing, whose answer is still the project's access.
func ReadWrittenProject(ctx context.Context, q Querier, key string) (model.ProjectAccess, error) {
	return readProjectWritten(ctx, q, key)
}

// MaxSlackChannels is how many Slack channels a project names.
const MaxSlackChannels = 16

// slackChannel is the form of a Slack channel id, at most 21 characters, which the column's own
// check repeats.
var slackChannel = regexp.MustCompile(`^[CG][A-Z0-9]{8,20}$`)

// SetSlackChannels replaces the Slack channel ids project key's credentials may post to, sorted
// and with each id named once. The secrets broker reads a private project's Slack channels to
// release the Legion Slack bot token to that project's credentials for those channels alone (R14),
// so only the project's owner writes them: the caller makes that check first, under the project's
// owner lock (LockForAccessWrite). A list different from the one stored, compared sorted, sets
// slack_channels_changed_at in the same update to statement_timestamp(), that update's own start:
// it comes after the project's owner lock and the channel lock are held, so it never precedes a
// lock wait, and two writes of one project are stamped in the order they wrote. The transaction's
// now() would be its start, before those waits. The same list leaves the time.
//
// A Slack channel belongs to one private project. So every call takes the project's owner lock,
// then the channel lock (lockSlackChannels), before it reads anything, and a private project's
// write naming a channel another private project lists is refused with ErrSlackChannelShared,
// which names that project only to a viewer who sees it (refuseSharedSlackChannel); a public
// project's write is never refused for it. by is the person the write records, and viewer what
// the request's own caller sees, as the API resolved it for the request (readerOf), whom the
// refusal is written for. It appends project.access_changed in tx.
func SetSlackChannels(ctx context.Context, tx pgx.Tx, ev *events.Broker, key string, channels []string, by model.Actor, viewer *model.Viewer) (model.ProjectAccess, model.Event, error) {
	named := make([]string, 0, len(channels))
	for _, channel := range channels {
		id := strings.TrimSpace(channel)
		if !slackChannel.MatchString(id) {
			return model.ProjectAccess{}, model.Event{}, fmt.Errorf("%w: %q", ErrSlackChannelInput, id)
		}
		named = append(named, id)
	}
	slices.Sort(named)
	named = slices.Compact(named)
	if len(named) > MaxSlackChannels {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("%w: %d given", ErrSlackChannelsFull, len(named))
	}
	// The project's owner lock and then the channel lock, in the order every access write takes its
	// locks: a caller that took the first already (LockForAccessWrite) takes it again at once.
	if err := lockProject(ctx, tx, key); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	if err := lockSlackChannels(ctx, tx); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	var visibility string
	err := tx.QueryRow(ctx, `select visibility from projects where key = $1`, key).Scan(&visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("read the visibility of project %s: %w", key, err)
	}
	if visibility == model.ProjectPrivate {
		if err := refuseSharedSlackChannel(ctx, tx, key, named, viewer); err != nil {
			return model.ProjectAccess{}, model.Event{}, err
		}
	}
	if err := updateProject(ctx, tx, key, `
		update projects set slack_channels = $2,
			slack_channels_changed_at = case
				when array(select stored from unnest(slack_channels) stored order by stored collate "C") = $2::text[]
					then slack_channels_changed_at
				else statement_timestamp()
			end
		where key = $1
	`, named); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	return appendProjectChange(ctx, tx, ev, key, model.AccessSlackChannelsChanged, "", by)
}

// RefuseSharedSlackChannels is a switch of project key to private's check of its own Slack channels,
// as tx reads them after every other write of the switch: under the channel lock, taken after the
// project's owner lock in the order every access write takes them, it answers ErrSlackChannelShared
// while another private project lists one of them, naming that project only to viewer when viewer
// sees it (refuseSharedSlackChannel), and nil otherwise.
func RefuseSharedSlackChannels(ctx context.Context, tx pgx.Tx, key string, viewer *model.Viewer) error {
	if err := lockProject(ctx, tx, key); err != nil {
		return err
	}
	if err := lockSlackChannels(ctx, tx); err != nil {
		return err
	}
	var channels []string
	err := tx.QueryRow(ctx, `select slack_channels from projects where key = $1`, key).Scan(&channels)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	if err != nil {
		return fmt.Errorf("read the Slack channels of project %s: %w", key, err)
	}
	if len(channels) == 0 {
		return nil
	}
	return refuseSharedSlackChannel(ctx, tx, key, channels, viewer)
}

// StartSlackChannelsTime starts project key's Slack channels' time at a switch to private, when the
// project names any: slack_channels_changed_at, the one moment its channels became a private
// project's, which the secrets broker's settle time counts from as it does from a change of the
// list. A project that names none keeps its time. The time is statement_timestamp(), the update's
// own start, after the owner lock, as SetSlackChannels stamps it, and not the transaction's start.
func StartSlackChannelsTime(ctx context.Context, tx pgx.Tx, key string) error {
	if err := lockProject(ctx, tx, key); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		update projects set slack_channels_changed_at = statement_timestamp()
		where key = $1 and cardinality(slack_channels) > 0
	`, key); err != nil {
		return fmt.Errorf("start the Slack channels' time of project %s: %w", key, lockTimedOut(LockOwnerRow, err))
	}
	return nil
}

// lockSlackChannels takes, for the rest of tx, the one lock every write of a project's Slack
// channels takes before it reads any project's list, so two writes naming one channel for two
// private projects cannot both read the other's list as it was and both commit. It comes after the
// project's owner lock (lockProject, which LockForAccessWrite also takes) in the order every access
// write takes its locks, and a project's row is written only under that project's owner lock. A
// wait past the transaction's lock_timeout is ErrLockTimeout naming LockSlackChannel.
func lockSlackChannels(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-slack-channel'), 0)`); err != nil {
		return fmt.Errorf("lock the Slack channels: %w", lockTimedOut(LockSlackChannel, err))
	}
	return nil
}

// refuseSharedSlackChannel answers ErrSlackChannelShared when a private project other than key
// lists one of channels, and nil when none does. Sound only under lockSlackChannels. The refusal
// names that project only when viewer sees it (Sees, or All), the viewer the request resolved
// before its transaction opened, so nothing is resolved here and no person's own sight stands in
// for the credential's: an agent credential that lists no project sees only public ones, so it
// never reads a private project's name here, whoever its person is. A nil viewer names nothing.
func refuseSharedSlackChannel(ctx context.Context, tx pgx.Tx, key string, channels []string, viewer *model.Viewer) error {
	channel, other, err := SharedSlackChannel(ctx, tx, key, channels)
	if err != nil || channel == "" {
		return err
	}
	refusal := slackChannelShared{channel: channel}
	if viewer != nil && (viewer.All || slices.Contains(viewer.Sees, other)) {
		refusal.other = other
	}
	return refusal
}

// slackChannelShared is ErrSlackChannelShared for one channel: naming other, the private project
// that lists it, or, with other empty, no project, as a caller who does not see it reads it.
type slackChannelShared struct{ channel, other string }

func (refusal slackChannelShared) Error() string {
	if refusal.other == "" {
		return refusal.channel + " is listed by a private project you cannot see"
	}
	return refusal.channel + " is listed by " + refusal.other + ", a private project; " + ErrSlackChannelShared.Error()
}

func (slackChannelShared) Unwrap() error { return ErrSlackChannelShared }

// SharedSlackChannel answers the first of channels, by id, that a private project other than key
// lists, and that project, the first by key when several do; "" for both when none does.
func SharedSlackChannel(ctx context.Context, q Querier, key string, channels []string) (channel, other string, err error) {
	err = q.QueryRow(ctx, `
		select key, c from projects, unnest(slack_channels) c
		where visibility = 'private' and key <> $1 and c = any($2)
		order by c, key limit 1
	`, key, channels).Scan(&other, &channel)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read the private projects listing the Slack channels of project %s: %w", key, err)
	}
	return channel, other, nil
}

// PrivateProjects answers every private project's Slack channels and when they last changed, by
// key, read live: the list GET /api/v1/access/private-projects answers the secrets broker. It is
// [] when no project is private, never nil, since a reader takes anything but an array for the
// answer being down.
func PrivateProjects(ctx context.Context, q Querier) ([]model.PrivateProjectChannels, error) {
	rows, err := q.Query(ctx, `
		select key, slack_channels, slack_channels_changed_at from projects where visibility = 'private' order by key
	`)
	if err != nil {
		return nil, fmt.Errorf("list the private projects: %w", err)
	}
	projects, err := pgx.AppendRows(make([]model.PrivateProjectChannels, 0), rows, func(row pgx.CollectableRow) (model.PrivateProjectChannels, error) {
		var project model.PrivateProjectChannels
		err := row.Scan(&project.Project, &project.SlackChannels, &project.SlackChannelsChangedAt)
		return project, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the private projects: %w", err)
	}
	return projects, nil
}

// updateProject writes one column of project key's row after the project's owner lock, the order
// store.LockOwner's contract names: the lock and then the row. Written first, the row would be held
// while the append waits for the lock, and a holder of the lock that writes the row next, as an
// issue create does, would wait for it in turn (TestAProjectWriteTakesTheOwnerLockBeforeTheRow).
// A caller that holds the lock already, the routes after LockForAccessWrite, takes it again at once.
func updateProject(ctx context.Context, tx pgx.Tx, key, statement string, value any) error {
	if err := lockProject(ctx, tx, key); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, statement, key, value)
	if err != nil {
		return fmt.Errorf("write project %s: %w", key, lockTimedOut(LockOwnerRow, err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	return nil
}

// principalOf checks one principal and answers its stored form: a person's lowercase email, or a
// group's name.
func principalOf(kind, principal string) (string, error) {
	switch kind {
	case PrincipalPerson:
		return PersonPrincipal(principal)
	case PrincipalGroup:
		name := strings.TrimSpace(principal)
		if !ValidGroupName(name) {
			return "", fmt.Errorf("%w: %q is no group name", ErrPrincipalInput, name)
		}
		return name, nil
	}
	return "", fmt.Errorf("%w: a principal is a person or a group, not %q", ErrPrincipalInput, kind)
}

// AddPrincipal names a person or a group on project key's access list; a principal the list
// names already stays as it was. A group must exist. It appends project.access_changed in tx.
func AddPrincipal(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, kind, principal string, by model.Actor) (model.ProjectAccess, model.Event, error) {
	name, err := principalOf(kind, principal)
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	by = by.Recorded()
	if err := lockProject(ctx, tx, key); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	// One statement, after the project's lock, reads the project, the group and the list's size,
	// and writes the row only when all three allow it, so the answer says which refused.
	var project, group, listed bool
	var size int
	if err := tx.QueryRow(ctx, `
		with target as (
			select key from projects where key = $1
		), named_group as (
			select 1 from access_groups where $2 = 'group' and name = $3
		), size as (
			select count(*) as rows from project_access where project_key = $1
		), already as (
			select 1 from project_access where project_key = $1 and principal_kind = $2 and principal = $3
		), inserted as (
			insert into project_access (project_key, principal_kind, principal, added_by)
			select $1, $2, $3, $4
			where exists (select 1 from target)
				and ($2 = 'person' or exists (select 1 from named_group))
				and not exists (select 1 from already)
				and (select rows from size) < $5
			returning 1
		)
		select exists (select 1 from target), $2 = 'person' or exists (select 1 from named_group),
			exists (select 1 from already) or exists (select 1 from inserted), (select rows from size)
	`, key, kind, name, by, MaxPrincipals).Scan(&project, &group, &listed, &size); err != nil {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("list a %s on project %s: %w", kind, key, err)
	}
	switch {
	case !project:
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	case !group:
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("%w: %s", ErrGroupNotFound, name)
	case !listed:
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("%w: project %s names %d already", ErrListFull, key, size)
	}
	change := model.AccessPersonAdded
	if kind == PrincipalGroup {
		change = model.AccessGroupAdded
	}
	return appendProjectChange(ctx, tx, ev, key, change, name, by)
}

// RemovePrincipal takes a person or a group off project key's access list; one the list does not
// name leaves it as it was. It appends project.access_changed in tx.
func RemovePrincipal(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, kind, principal string, by model.Actor) (model.ProjectAccess, model.Event, error) {
	name, err := principalOf(kind, principal)
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	if err := lockProject(ctx, tx, key); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	var project bool
	if err := tx.QueryRow(ctx, `
		with target as (
			select key from projects where key = $1
		), removed as (
			delete from project_access
			where project_key = $1 and principal_kind = $2 and principal = $3 and exists (select 1 from target)
			returning 1
		)
		select exists (select 1 from target)
	`, key, kind, name).Scan(&project); err != nil {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("take a %s off project %s: %w", kind, key, err)
	}
	if !project {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	change := model.AccessPersonRemoved
	if kind == PrincipalGroup {
		change = model.AccessGroupRemoved
	}
	return appendProjectChange(ctx, tx, ev, key, change, name, by)
}

// appendProjectChange appends the event naming change on project key, records beside it what the
// change named (recordProjectChanges), and reads the project's access as tx left it, which the
// write answers. principal is the person or group a list change added or removed, the person an
// owner change took the project from (empty when it had none), and empty for any other change.
func appendProjectChange(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, change, principal string, by model.Actor) (model.ProjectAccess, model.Event, error) {
	access, err := readProjectWritten(ctx, tx, key)
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	event, err := ev.Append(ctx, tx, projectChangeEvent(key, change, by))
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, fmt.Errorf("append project.access_changed: %w", err)
	}
	if err := recordProjectChanges(ctx, tx, []int64{event.ID}, principal); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	if err := announceChange(ctx, tx); err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	return access, event, nil
}

// projectChangeEvent is the project.access_changed of one write of project key's access: the
// project and the change alone (model.ProjectAccessEventPayload).
func projectChangeEvent(key, change string, by model.Actor) model.Event {
	return model.Event{
		ProjectKey: &key, Type: "project.access_changed", Actor: by.Recorded(),
		Payload: model.ProjectAccessEventPayload{Project: key, Change: change},
	}
}

// recordProjectChanges writes, in tx, the server's own record of each project.access_changed of
// eventIDs (project_access_changes): principal, which the change named, and the owner and visibility
// of the event's project as tx left them. The payload names none of these, since every reader of the
// project's events reads it; a live resume reads this record to tell whether a change in its gap
// moved what its caller sees (api: lostSightSQL, gainedSightSQL), and no route serves it.
func recordProjectChanges(ctx context.Context, tx pgx.Tx, eventIDs []int64, principal string) error {
	if _, err := tx.Exec(ctx, `
		insert into project_access_changes (event_id, principal, owner, visibility)
		select e.id, $2, coalesce(p.owner, ''), p.visibility
		from events e join projects p on p.key = e.project_key
		where e.id = any($1)
	`, eventIDs, principal); err != nil {
		return fmt.Errorf("record the access changes %v: %w", eventIDs, err)
	}
	return nil
}

// groupMembersSQL selects group g's members and the members nobody has signed in as yet, each
// sorted.
const groupMembersSQL = `array(select person from access_group_members where group_name = g.name order by person collate "C"),
			array(select member.person from access_group_members member
				where member.group_name = g.name and not exists (select 1 from people where email = member.person)
				order by member.person collate "C")`

// ReadGroup answers one group, ErrGroupNotFound when no row holds its name.
func ReadGroup(ctx context.Context, q Querier, name string) (model.AccessGroup, error) {
	var group model.AccessGroup
	err := q.QueryRow(ctx, `
		select g.name, g.owner, `+groupMembersSQL+`
		from access_groups g
		where g.name = $1
	`, name).Scan(&group.Name, &group.Owner, &group.Members, &group.NotSignedIn)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.AccessGroup{}, fmt.Errorf("%w: %s", ErrGroupNotFound, name)
	}
	if err != nil {
		return model.AccessGroup{}, fmt.Errorf("read group %s: %w", name, err)
	}
	return group, nil
}

// ListGroups answers one page of the groups, in name order: at most limit of them whose names come
// after after ("" for the first page), and Next, the last name of the page, when another group
// follows it.
func ListGroups(ctx context.Context, q Querier, after string, limit int) (model.AccessGroupPage, error) {
	rows, err := q.Query(ctx, `
		select g.name, g.owner, `+groupMembersSQL+`
		from access_groups g
		where g.name collate "C" > $1
		order by g.name collate "C"
		limit $2
	`, after, limit+1)
	if err != nil {
		return model.AccessGroupPage{}, fmt.Errorf("list groups: %w", err)
	}
	groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.AccessGroup, error) {
		var group model.AccessGroup
		err := row.Scan(&group.Name, &group.Owner, &group.Members, &group.NotSignedIn)
		return group, err
	})
	if err != nil {
		return model.AccessGroupPage{}, fmt.Errorf("list groups: %w", err)
	}
	page := model.AccessGroupPage{Groups: groups}
	if len(groups) > limit {
		page.Groups = groups[:limit]
		page.Next = page.Groups[limit-1].Name
	}
	if page.Groups == nil {
		page.Groups = []model.AccessGroup{}
	}
	return page, nil
}

// PutGroup makes group name, or writes it again: its owner, a full member, and exactly members,
// each a person who is not a restricted member. It appends access_group.changed in tx. Who may
// write the group is the caller's to decide first. A write that makes the group, or gives it to
// another owner, is refused with ErrGroupsPerOwnerFull when that owner holds
// model.MaxGroupsPerOwner groups already (roomForAGroup); a write that keeps the owner is not.
func PutGroup(ctx context.Context, tx pgx.Tx, ev *events.Broker, name, owner string, members []string, by model.Actor) (model.AccessGroup, model.Event, error) {
	if !ValidGroupName(name) {
		return model.AccessGroup{}, model.Event{}, fmt.Errorf("%w: %q is no group name", ErrPrincipalInput, name)
	}
	if err := LockGroupName(ctx, tx, name); err != nil {
		return model.AccessGroup{}, model.Event{}, err
	}
	ownerLogin, err := OwnerAllowed(ctx, tx, owner)
	if err != nil {
		return model.AccessGroup{}, model.Event{}, err
	}
	people := make([]string, 0, len(members))
	for _, member := range members {
		login, err := PersonPrincipal(member)
		if err != nil {
			return model.AccessGroup{}, model.Event{}, err
		}
		people = append(people, login)
	}
	slices.Sort(people)
	people = slices.Compact(people)
	if len(people) > MaxGroupMembers {
		return model.AccessGroup{}, model.Event{}, fmt.Errorf("%w: %d given", ErrGroupFull, len(people))
	}
	var restricted []string
	if err := tx.QueryRow(ctx, `
		select array(select email from people where email = any($1) and member_kind = 'restricted' order by email collate "C")
	`, people).Scan(&restricted); err != nil {
		return model.AccessGroup{}, model.Event{}, fmt.Errorf("read the member kinds of group %s: %w", name, err)
	}
	if len(restricted) > 0 {
		return model.AccessGroup{}, model.Event{}, fmt.Errorf("%w: %s", ErrGroupMemberRestricted, strings.Join(restricted, ", "))
	}
	var held string
	switch err := tx.QueryRow(ctx, `select owner from access_groups where name = $1`, name).Scan(&held); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return model.AccessGroup{}, model.Event{}, fmt.Errorf("read the owner of group %s: %w", name, err)
	}
	if held != ownerLogin {
		if err := roomForAGroup(ctx, tx, ownerLogin); err != nil {
			return model.AccessGroup{}, model.Event{}, err
		}
	}
	var created bool
	if err := tx.QueryRow(ctx, `
		insert into access_groups (name, owner) values ($1, $2)
		on conflict (name) do update set owner = excluded.owner
		returning xmax = 0
	`, name, ownerLogin).Scan(&created); err != nil {
		return model.AccessGroup{}, model.Event{}, fmt.Errorf("write group %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, `
		with gone as (
			delete from access_group_members where group_name = $1 and person <> all($2)
		)
		insert into access_group_members (group_name, person)
		select $1, person from unnest($2::text[]) as person
		on conflict do nothing
	`, name, people); err != nil {
		return model.AccessGroup{}, model.Event{}, fmt.Errorf("write the members of group %s: %w", name, err)
	}
	group, err := ReadGroup(ctx, tx, name)
	if err != nil {
		return model.AccessGroup{}, model.Event{}, err
	}
	change := model.AccessGroupUpdated
	if created {
		change = model.AccessGroupCreated
	}
	event, err := ev.Append(ctx, tx, groupChangeEvent(name, change, by))
	if err != nil {
		return model.AccessGroup{}, model.Event{}, fmt.Errorf("append access_group.changed: %w", err)
	}
	if err := announceChange(ctx, tx); err != nil {
		return model.AccessGroup{}, model.Event{}, err
	}
	return group, event, nil
}

// roomForAGroup answers nil when owner may hold one more group, and ErrGroupsPerOwnerFull when they
// hold model.MaxGroupsPerOwner already. It first takes, for the rest of tx, the owner's lock, so two
// writes that each give owner a group are ordered and the second counts the first's once it
// commits; the count is a statement of its own after that lock, so it reads what committed while
// the lock was waited for. Every write takes a group's name lock (LockGroupName) and then one owner
// lock, so no two writes wait on each other in a cycle. Its errors name no person: an error can
// reach a log line (TestNoAccessErrorNamesAPerson).
func roomForAGroup(ctx context.Context, tx pgx.Tx, owner string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-group-owner'), hashtext($1))`, owner); err != nil {
		return fmt.Errorf("lock the groups of the owner: %w", err)
	}
	var held int
	if err := tx.QueryRow(ctx, `select count(*) from access_groups where owner = $1`, owner).Scan(&held); err != nil {
		return fmt.Errorf("count the groups of the owner: %w", err)
	}
	if held >= model.MaxGroupsPerOwner {
		return fmt.Errorf("%w: the owner holds %d", ErrGroupsPerOwnerFull, held)
	}
	return nil
}

func groupChangeEvent(name, change string, by model.Actor) model.Event {
	return model.Event{
		Type: "access_group.changed", Actor: by.Recorded(),
		Payload: model.AccessGroupEventPayload{Name: name, Change: change},
	}
}

// DeleteGroup deletes group name and takes it off every access list that named it, so a later
// group of the same name inherits no project. It takes the owner lock of each such project
// (store.LockOwners) before it appends there, so the record of each change it writes beside the
// event (recordProjectChanges: the project's owner and visibility) carries every write the lock
// ordered first, and appends one project.access_changed for each project, in key order, and then
// access_group.changed, all in tx. Who may delete the group is the caller's to decide first.
func DeleteGroup(ctx context.Context, tx pgx.Tx, ev *events.Broker, name string, by model.Actor) ([]model.Event, error) {
	if err := LockGroupName(ctx, tx, name); err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `delete from access_groups where name = $1`, name)
	if err != nil {
		return nil, fmt.Errorf("delete group %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("%w: %s", ErrGroupNotFound, name)
	}
	rows, err := tx.Query(ctx, `
		delete from project_access where principal_kind = 'group' and principal = $1 returning project_key
	`, name)
	if err != nil {
		return nil, fmt.Errorf("take group %s off its lists: %w", name, err)
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("take group %s off its lists: %w", name, err)
	}
	slices.Sort(projects)
	owners := make([]store.Owner, len(projects))
	for index, project := range projects {
		owners[index] = store.ProjectOwner(project)
	}
	if err := store.LockOwners(ctx, tx, owners...); err != nil {
		return nil, fmt.Errorf("lock the projects that listed group %s: %w", name, err)
	}
	changes := make([]model.Event, 0, len(projects)+1)
	for _, project := range projects {
		changes = append(changes, projectChangeEvent(project, model.AccessGroupRemoved, by))
	}
	changes = append(changes, groupChangeEvent(name, model.AccessGroupDeleted, by))
	appended := make([]model.Event, len(changes))
	if err := store.RunWrite(ctx, tx, func(batch *store.WriteBatch) error {
		for index, change := range changes {
			if err := ev.QueueAppend(batch, change, func(written model.Event) { appended[index] = written }); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("append the events of group %s's delete: %w", name, err)
	}
	projectEvents := make([]int64, 0, len(projects))
	for _, event := range appended[:len(projects)] {
		projectEvents = append(projectEvents, event.ID)
	}
	if err := recordProjectChanges(ctx, tx, projectEvents, name); err != nil {
		return nil, err
	}
	if err := announceChange(ctx, tx); err != nil {
		return nil, err
	}
	return appended, nil
}
