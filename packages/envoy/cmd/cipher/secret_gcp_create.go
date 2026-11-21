// packages/envoy/cmd/cipher/secret_gcp_create.go
//
// The GCP side of `secret create` and its recovery: the empty secret it makes, labelled with a
// value of its own, before the value is read (gcpNewSecret), the add of that value, and every
// cleanup that settles what a failed or lost call left - reading the secret back, listing its
// versions, deleting only an empty secret this create made - with the commands it prints for
// settling it by hand, signed in as the create was. The sign-in and the other forms are in
// secret_gcp.go.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// createLabel is the label create sets on the secret it makes, to a value of its own
// (newCreateToken), so that a secret it finds after a CreateSecret whose answer was lost is its own
// exactly when that label holds its value: one that held the name before, or that another create of
// the name made in between, carries no label or another's. It is a label and not an annotation,
// since the policy's tags are the annotations. The broker reads it for one thing only: a secret
// that carries it and holds no version is a create waiting for its value (secrets.Listing.Created
// and policy.ReasonPendingValue). It stays on the secret once its value is added, since removing it
// would cost a create secretmanager.secrets.update.
const createLabel = secrets.CreateLabel

// newCreateToken is a value for createLabel: 128 random bits as 32 lowercase hex digits, which a
// Secret Manager label value admits. crypto/rand.Read never returns an error.
func newCreateToken() string {
	var token [16]byte
	_, _ = rand.Read(token[:])
	return hex.EncodeToString(token[:])
}

// prepareCreate builds the state, including the ownership label, before the form installs its
// signal cleanup. begin then makes the secret, so a signal during CreateSecret waits for its
// answer and cleans up as it does during the later store step.
func (b *gcpBackend) prepareCreate(slug string, tags map[string]string) newSecret {
	id := b.id(slug)
	return &gcpNewSecret{b: b, name: id, shown: b.settings.words(id), secretID: b.secretID(slug), display: b.displayName(id), tags: tags, token: newCreateToken()}
}

// addPermission is the permission store needs on the secret begin makes.
const addPermission = "secretmanager.versions.add"

// begin makes the secret, empty and labelled with this create's token, under storeCallDeadline,
// then asks IAM whether this sign-in may add its value (addPermission), so a sign-in that may
// create but not add is told before it is asked for the value: begin then discards the empty
// secret through discard and answers that refusal. Where IAM cannot be asked, begin goes on, and
// store's add is the check. A CreateSecret whose answer leaves open whether the store made the
// secret is settled by settleLostCreate.
func (n *gcpNewSecret) begin(ctx context.Context) (discardOutcome, error) {
	b := n.b
	callCtx, cancel := withStoreDeadline(ctx)
	_, err := b.client.CreateSecret(callCtx, &secretmanagerpb.CreateSecretRequest{
		Parent:   b.parent(),
		SecretId: n.secretID,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{
						CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{
							KmsKeyName: b.settings.KMSKeyName,
						},
					},
				},
			},
			Annotations: n.tags,
			Labels:      map[string]string{createLabel: n.token},
		},
	})
	cancel()
	if err != nil {
		if gcpDefiniteRefusal(err) {
			return madeNothing, err
		}
		outcome, left := n.settleLostCreate(ctx)
		return outcome, fmt.Errorf("%w; %s", err, left)
	}
	if b.mayNotAdd(ctx, n.name) {
		refusal := fmt.Errorf("%s may create %s but not add its value: IAM grants it no %s on the secret, so no value was asked for; %s", b.identity(), n.shown, addPermission, b.chooseAnother())
		outcome, left := n.discard(ctx)
		if left != nil {
			return outcome, fmt.Errorf("%w; %w", refusal, left)
		}
		return outcome, refusal
	}
	return madeNothing, nil
}

// mayNotAdd reports whether IAM answers, under storeCallDeadline, that this sign-in does not hold
// addPermission on the secret named id. A failed ask answers false: the add is then the check.
func (b *gcpBackend) mayNotAdd(ctx context.Context, id string) bool {
	ctx, cancel := withStoreDeadline(ctx)
	defer cancel()
	held, err := b.client.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: id, Permissions: []string{addPermission}})
	return err == nil && !slices.Contains(held.GetPermissions(), addPermission)
}

// settleLostCreate answers what a CreateSecret whose answer leaves open whether the store acted
// left in the store, and settles it. It reads the secret back: none shown means the store did not
// show it then, though a create it has not finished may still land; one whose createLabel is not
// this create's token is another write's - one that held the name before, or another create's -
// and is left as it is; one carrying the token goes through discard, which deletes it only where it
// holds no version. A read that fails names the token and the gcloud commands that show a secret's
// labels and clear it. Each call runs under storeCallDeadline.
func (n *gcpNewSecret) settleLostCreate(ctx context.Context) (discardOutcome, string) {
	lost := "the store's answer leaves open whether it made " + n.shown
	getCtx, cancel := withStoreDeadline(ctx)
	secret, err := n.b.client.GetSecret(getCtx, &secretmanagerpb.GetSecretRequest{Name: n.name})
	cancel()
	switch {
	case status.Code(err) == codes.NotFound:
		return keptUnknown, fmt.Sprintf("%s, and reading it back did not find it then, but a create the store has not finished may still land: `%s` shows whether it is there, and one whose %s label is %s, with `versions: none`, is this create's empty secret, which %s clears",
			lost, n.agentSecrets("secret", "show", n.display), createLabel, n.token, n.clearCommand())
	case err != nil:
		return keptUnknown, fmt.Sprintf("%s, and reading it back failed, so whether this create made it is unknown: %s shows its labels, and one whose %s label is %s is this create's, and with no version it is an empty secret, which %s clears. Reading it failed: %v",
			lost, n.secretCommand("describe"), createLabel, n.token, n.clearCommand(), err)
	case secret.GetLabels()[createLabel] != n.token:
		return keptOthers, lost + "; reading it back finds a secret this create did not make (its " + createLabel + " label is not this create's), so it is left as it is"
	}
	outcome, left := n.discardRead(ctx, secret, lost+"; reading it back finds it made by this create")
	if left != nil {
		return outcome, left.Error()
	}
	return outcome, lost + "; reading it back finds it, made by this create and empty, so it is deleted again and nothing stays"
}

// countVersions is how many versions the secret named id holds, listed under storeCallDeadline.
func (b *gcpBackend) countVersions(ctx context.Context, id string) (int, error) {
	ctx, cancel := withStoreDeadline(ctx)
	defer cancel()
	versions, err := b.versionsOf(ctx, id)
	return len(versions), err
}

// deleteOutright deletes the observed secret outright, under storeCallDeadline, conditional on
// the etag read before listing its versions, so a replacement cannot be deleted by name alone.
func (b *gcpBackend) deleteOutright(ctx context.Context, id, etag string) error {
	ctx, cancel := withStoreDeadline(ctx)
	defer cancel()
	return b.client.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: id, Etag: etag})
}

// gcloud is a gcloud command line for printing, in backticks, pinned to the sign-in this command
// used where gcloud can be told it (gcloudPin), and followed by that sign-in's name, so a person
// runs it as the identity that made what it settles.
func (b *gcpBackend) gcloud(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellWord(arg)
	}
	return fmt.Sprintf("`%sgcloud %s` (as %s)", b.gcloudPin(), strings.Join(quoted, " "), b.identity())
}

// gcloudPin is the shell assignments that make gcloud sign in as this command did, each
// shell-quoted, by the source the credential came from: CLOUDSDK_ACTIVE_CONFIG_NAME for the
// configuration CLOUDSDK_ACTIVE_CONFIG_NAME named, the variables that chose a gcloud configuration or login as they
// were set, CLOUDSDK_CORE_ACCOUNT for the active gcloud account's login, and
// CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE for the key file GOOGLE_APPLICATION_CREDENTIALS named, as
// the absolute path of the file this sign-in read (credentialsFile), as signInPrefix writes it;
// "" for a token CLOUDSDK_AUTH_ACCESS_TOKEN lent, which gcloud reads from the same environment,
// and for credentials gcloud cannot be pointed at (the metadata server's, or the well-known
// file's), which the name after the command (gcloud) says to use.
func (b *gcpBackend) gcloudPin() string {
	switch b.source {
	case sourceChosenGcloud:
		return b.signInPrefix()
	case sourceActiveGcloud:
		return "CLOUDSDK_CORE_ACCOUNT=" + shellWord(b.gcloudAccount) + " "
	case sourceCredentialsFile:
		return "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=" + shellWord(b.credentialsFile) + " "
	}
	return ""
}

// agentSecrets is an agent-secrets command line for printing, with the sign-in prefix that makes
// it sign in as this command did (signInPrefix, pipeCommand).
func (b *gcpBackend) agentSecrets(words ...string) string {
	return pipeCommand(b.signInPrefix(), words...)
}

// gcpNewSecret is the empty secret create made: name its whole resource name, secretID the id
// under the project, display the NAME a session asks for it as, token its createLabel value.
// name and secretID open with the broker's secrets_prefix, text of the broker's settings answer, so
// a line prints the name as shown, and the id as secretCommand does: as the broker's address lets
// its answer print (Settings.words). mayHoldValue and deleted are what unsettled reads while a call
// of begin, store or discard may be in flight: mayHoldValue is set from the moment store begins the
// add until a refusal the store answered before acting on it, and from the moment discard finds a
// version; deleted once the delete is done. An add whose answer was lost leaves mayHoldValue set,
// and discard then never deletes the secret: the add may still land.
type gcpNewSecret struct {
	b                              *gcpBackend
	name, shown, secretID, display string
	token                          string
	tags                           map[string]string
	mayHoldValue, deleted          atomic.Bool
}

// store adds value as the secret's first version, under storeCallDeadline.
func (n *gcpNewSecret) store(ctx context.Context, value string) error {
	n.mayHoldValue.Store(true)
	ctx, cancel := withStoreDeadline(ctx)
	defer cancel()
	err := n.b.addVersion(ctx, n.name, value)
	if err != nil && gcpDefiniteRefusal(err) {
		n.mayHoldValue.Store(false)
	}
	return err
}

// discard reads the secret back and goes on through discardRead, which deletes it only where it is
// this create's, empty, unchanged, and no add of this create's may still land. A secret already gone
// is deleted, for this create.
func (n *gcpNewSecret) discard(ctx context.Context) (discardOutcome, error) {
	getCtx, cancel := withStoreDeadline(ctx)
	secret, err := n.b.client.GetSecret(getCtx, &secretmanagerpb.GetSecretRequest{Name: n.name})
	cancel()
	switch {
	case status.Code(err) == codes.NotFound:
		n.deleted.Store(true)
		return deletedEmpty, nil
	case err != nil:
		return keptUnknown, fmt.Errorf("reading %s before cleanup failed, so it is left as it is: %s. Reading it failed: %w", n.shown, n.unsettled(), err)
	}
	return n.discardRead(ctx, secret, "")
}

// discardRead deletes secret, as discard read it, only where it carries this create's token, holds
// no version, no add of this create's may still land (mayHoldValue), and its etag is unchanged at
// the delete; otherwise it leaves it, saying why. Even when this form never added a value, another
// writer may have filled or replaced the secret while its prompt was open. why, when set, opens
// each refusal. Each call runs under storeCallDeadline. The one case it cannot see: a version
// another writer adds between the list and the delete, which the delete then takes with the secret.
func (n *gcpNewSecret) discardRead(ctx context.Context, secret *secretmanagerpb.Secret, why string) (discardOutcome, error) {
	if why != "" {
		why += "; "
	}
	if secret.GetLabels()[createLabel] != n.token {
		return keptOthers, fmt.Errorf("%sanother writer now holds %s (its %s label is not this create's), so it is left as it is: `%s` shows it", why, n.shown, createLabel, n.agentSecrets("secret", "show", n.display))
	}
	addLost := n.mayHoldValue.Load()
	versions, err := n.b.countVersions(ctx, n.name)
	switch {
	case err != nil:
		return keptUnknown, fmt.Errorf("%slisting the secret's versions failed, so it stays: whether %s holds the value is unknown. `%s` settles it, under a sign-in that may list its versions: `versions: none` is an empty secret, which %s clears, and a version is the value stored. Listing the versions failed: %w",
			why, n.shown, n.agentSecrets("secret", "show", n.display), n.clearCommand(), err)
	case versions > 0:
		n.mayHoldValue.Store(true)
		return keptMayHoldValue, fmt.Errorf("%s%s holds %d version(s) now, so it stays: the value may have been stored by this create or another writer. Check it with `%s`, and run `%s` to store it again",
			why, n.shown, versions, n.agentSecrets("secret", "show", n.display), n.agentSecrets("secret", "set", n.display))
	case addLost:
		return keptMayHoldValue, fmt.Errorf("%sthe store's answer to the add leaves open whether it stored the value, and %s showed no version when listed, but a late add may still land, so it stays: `%s` shows whether the value is there, `versions: none` being an empty secret, which %s clears",
			why, n.shown, n.agentSecrets("secret", "show", n.display), n.clearCommand())
	}
	if err := n.b.deleteOutright(ctx, n.name, secret.GetEtag()); err != nil {
		switch {
		case status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.Aborted:
			return keptOthers, fmt.Errorf("%s%s changed before cleanup could delete it, so it is left as it is: `%s` shows it. Deleting it failed: %w", why, n.shown, n.agentSecrets("secret", "show", n.display), err)
		case gcpDefiniteRefusal(err):
			return keptEmpty, fmt.Errorf("%san empty secret named %s stays in the store: clear it with %s. Deleting it failed: %w", why, n.shown, n.clearCommand(), err)
		}
		return n.settleLostDelete(ctx, why, err)
	}
	n.deleted.Store(true)
	return deletedEmpty, nil
}

// settleLostDelete answers what a DeleteSecret whose answer, deleteErr, leaves open whether the
// store acted (no gcpDefiniteRefusal: a lost answer, a deadline, an outage) left, by reading the
// secret back under storeCallDeadline: none found is the delete done; another writer's is left as
// it is; and a read that fails leaves it unknown, so the store may still hold it. This create's
// secret still there has its versions listed again, since another writer may have added one
// between the cleanup's list and the delete: with none it stays, empty, though a delete the store
// has not finished may yet land; with one it may hold a value, so it stays with no command that
// deletes it (keptMayHoldValue, which the broker rereads); and a list that fails names the delete
// only for a secret shown with no version. It says the secret stays only where the store showed it.
func (n *gcpNewSecret) settleLostDelete(ctx context.Context, why string, deleteErr error) (discardOutcome, error) {
	lost := why + "the store's answer to the delete leaves open whether it deleted " + n.shown
	getCtx, cancel := withStoreDeadline(ctx)
	secret, err := n.b.client.GetSecret(getCtx, &secretmanagerpb.GetSecretRequest{Name: n.name})
	cancel()
	switch {
	case status.Code(err) == codes.NotFound:
		n.deleted.Store(true)
		return deletedEmpty, nil
	case err != nil:
		return keptUnknown, fmt.Errorf("%s, and reading it back failed, so the store may still hold it: `%s` says whether it is there, `versions: none` being this create's empty secret, which %s clears. Deleting it failed: %w; reading it back failed: %v",
			lost, n.agentSecrets("secret", "show", n.display), n.clearCommand(), deleteErr, err)
	case secret.GetLabels()[createLabel] != n.token:
		return keptOthers, fmt.Errorf("%s, and reading it back finds a secret this create did not make (its %s label is not this create's), so it is left as it is: `%s` shows it. Deleting it failed: %w",
			lost, createLabel, n.agentSecrets("secret", "show", n.display), deleteErr)
	}
	versions, err := n.b.countVersions(ctx, n.name)
	switch {
	case err != nil:
		return keptUnknown, fmt.Errorf("%s, and reading it back still finds it, but listing its versions failed, so whether it holds a value is unknown: `%s` says, and only with `versions: none` is it this create's empty secret, which %s then clears. Deleting it failed: %w; listing the versions failed: %v",
			lost, n.agentSecrets("secret", "show", n.display), n.clearCommand(), deleteErr, err)
	case versions > 0:
		n.mayHoldValue.Store(true)
		return keptMayHoldValue, fmt.Errorf("%s, and reading it back finds it, and it holds %d version(s) now, so it stays: another writer may have stored a value. Check it with `%s`, and run `%s` to store yours. Deleting it failed: %w",
			lost, versions, n.agentSecrets("secret", "show", n.display), n.agentSecrets("secret", "set", n.display), deleteErr)
	}
	return keptEmpty, fmt.Errorf("%s, and reading it back still finds it, with no version, so this create's empty secret stays in the store unless a delete the store has not finished lands: `%s` says whether it is there, and %s clears it. Deleting it failed: %w",
		lost, n.agentSecrets("secret", "show", n.display), n.clearCommand(), deleteErr)
}

// unsettled says what the store may hold should the form end now: nothing once the empty secret
// is deleted; the secret, empty or holding the value, once the add has begun and nothing has shown
// it empty, which `secret show` tells apart; else the empty secret, or another writer's secret of
// that name. Each names the command that clears the empty secret.
func (n *gcpNewSecret) unsettled() string {
	switch {
	case n.deleted.Load():
		return fmt.Sprintf("the empty secret %s is deleted, so nothing stays in the store", n.shown)
	case n.mayHoldValue.Load():
		return fmt.Sprintf("%s may stay in the store, empty or holding the value: `%s` says which, `versions: none` being an empty secret, which %s clears, and a version the value stored",
			n.shown, n.agentSecrets("secret", "show", n.display), n.clearCommand())
	}
	return fmt.Sprintf("the empty secret %s may stay in the store, or another writer may now hold that name. %s shows its labels: only one whose %s label is %s is this create's, and only with `versions: none` from `%s` is it empty and safe to clear with %s",
		n.shown, n.secretCommand("describe"), createLabel, n.token, n.agentSecrets("secret", "show", n.display), n.clearCommand())
}

// clearCommand is the gcloud command that deletes the secret outright, as gcloud prints it.
func (n *gcpNewSecret) clearCommand() string { return n.secretCommand("delete") }

// secretCommand is the gcloud command that runs `gcloud secrets verb` on this create's secret, as
// gcloud prints it (gcpBackend.gcloud). The secret's id and its project are text of the broker's
// settings, so each prints as the broker's address lets its answer print (Settings.words): beside
// an address that is not clean the command names neither.
func (n *gcpNewSecret) secretCommand(verb string) string {
	words := n.b.settings.words
	return n.b.gcloud("secrets", verb, words(n.secretID), "--project", words(n.b.settings.GCPProject))
}

// agentSecrets is an agent-secrets command line signing in as this create did.
func (n *gcpNewSecret) agentSecrets(words ...string) string { return n.b.agentSecrets(words...) }

// madeBeforeValue is true: begin makes the empty secret, which waits in the store for its value.
func (*gcpNewSecret) madeBeforeValue() bool { return true }

// gcpDefiniteRefusal reports whether err is an answer Secret Manager gives before it acts on a
// call: the request refused as malformed, unauthorized, out of quota or against the secret's state.
// Every other answer - a lost one (Unknown, DeadlineExceeded, Canceled, Unavailable), a fault
// (Internal, DataLoss, Aborted), an error with no gRPC status at all - may follow a call the store
// carried out. It reads a gRPC status alone, so it is GCP's: an AWS error has none and answers
// false here whatever it says (awsDefiniteRefusal reads those).
func gcpDefiniteRefusal(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.PermissionDenied, codes.Unauthenticated,
		codes.ResourceExhausted, codes.FailedPrecondition, codes.OutOfRange, codes.Unimplemented:
		return true
	}
	return false
}
