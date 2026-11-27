#!/usr/bin/env bash
# packages/envoy/infra/scripts/operator-token-minters.sh
#
# Who, person or machine, can come to hold the NATS auth service's operator identity standing.
#
# The broker's `operator` entry of BROKER_NATS_SERVICES names one Kubernetes ServiceAccount
# (envoy-operator/envoy-operator by default). NATS takes that account's token, and the account acts,
# through Workload Identity, as one Google service account, whose token reaches an agent only as the
# administrator-tagged broker secret ENVOY_OPERATOR, released when a person approves; the secrets
# broker mints that token, so the broker's own ServiceAccount (--broker-workload) is guarded as the
# operator's is. So holding the operator's power takes one of: minting a guarded ServiceAccount's
# token, running or entering a pod that runs as it, becoming a principal that can, minting the Google
# service account's token, making a namespace and ServiceAccount of the same names on another
# cluster of the project's workload pool, or registering a cluster to the pool's fleet. Nobody holds
# that identity standing, a person included (INFRA-840's R16, dispatch://INFRA-840/spec#b-r-operator-minters):
# a principal holds it standing when it can come to hold it without a second person approving that
# use at that time. This check lists every principal, person or machine, that holds such a right,
# each a finding with the right and where it was granted, and is the precondition checked before
# the operator entry is applied, and again before go-live.
#
# It runs with the gcloud and kubectl sign-ins of the shell it runs in: an identity
# already held, never a person's administrator credential. It requests no credential, impersonates
# no one, and writes nothing but its own work directory and --report. It never reads a Kubernetes
# Secret. Its --secret-render inputs supply only the declared namespace, name, type and
# service-account-name annotation of each Secret; an invalid render is UNKNOWN, and so is one that
# holds no JSON document or declares no Secret.
#
# The steps, in order; each prints PASS, a FINDING per principal and right, or UNKNOWN naming why;
# the RBAC steps print a CREDIT per grant whose use on the broker's pods its pod fence refuses, or
# NO CREDIT naming why not, and an ALLOWANCE per grant or chain that reaches the fence's residual
# (below, "The broker's pod fence"):
#
#   permission names  Each permission name the IAM steps ask about is in the predefined role that
#                     grants it (`gcloud iam roles describe`): every container.* name in
#                     roles/container.admin's includedPermissions, the Google service account ones
#                     in roles/iam.serviceAccountTokenCreator, ...KeyAdmin and ...User, the hold
#                     writers' in roles/cloudsql.admin, roles/secretmanager.secretAccessor and
#                     roles/storage.objectViewer, the fleet's in roles/gkehub.admin, and Deployment
#                     Manager's in roles/deploymentmanager.editor. A name the role does not hold would
#                     find no holder, which reads as clean, so it reads UNKNOWN instead (exit 2).
#   code: RBAC        What code declares, first: each --rbac-render file (the JSON render of the
#                     program that declares the cluster's Roles and bindings, a List, an array or a
#                     stream of objects) is read as the RBAC authorizer reads it, and each binding
#                     that gives a right below to a subject other than a system namespace's
#                     ServiceAccount, a system component or the operator's own Google service
#                     account, a person included, is a finding. A rule limited by resourceNames on
#                     any right the live steps ask (the list below, the guard rights, the
#                     certificate chain's and the admission
#                     rights) is a finding naming the names: impersonating the group system:masters
#                     by name is cluster-admin, RBAC lets one who may bind a role by name write a
#                     binding to it, and an update of one webhook configuration or a patch of one
#                     Deployment by name is the unnamed right's path. RBAC never limits a
#                     deletecollection by name, and a create limited by name answers no create;
#                     but a patch that finds no object creates it (server-side apply), authorized
#                     as a create of that name, as is an update of an RBAC object, so a create
#                     limited by name is a finding for a subject that may also patch (or there
#                     update) that name, by any of its bindings. A binding to a
#                     role the render does not hold (other than the
#                     built-in cluster-admin, which gives every right, and view, which gives none)
#                     or to an aggregated ClusterRole is UNKNOWN: its rights are not in the render.
#   code: IAM         Each --iam-preview file (`pulumi preview --json --show-sames` of a program that
#                     declares IAM) is read for its organization, folder, project, service account,
#                     secret and bucket bindings, judged as the live IAM steps judge them, with each
#                     role's permissions from the preview's own custom roles or `gcloud iam roles
#                     describe`. A binding the preview does not know all of (a value computed at
#                     apply) and a resource type there that can grant on those resources but that
#                     this check does not read are UNKNOWN. A group the code names is a finding as
#                     the group, as in the live steps, which also judge each member they expand.
#   live: clusters    `gcloud container clusters list` of --project, which must hold --cluster. The
#                     operator's cluster is --cluster in --location, or, without --location, the
#                     list's one cluster of that name; a name the list holds more than once with no
#                     --location is UNKNOWN, and no cluster of that name is read as the operator's or
#                     as another. A list gcloud says is incomplete (a zone that did not respond) is
#                     UNKNOWN: a cluster there is neither read nor counted. With --pool-project
#                     naming another project, that project's own clusters are listed too, and read
#                     as other clusters of the pool; a refused or incomplete list is UNKNOWN.
#   live: audited     With --audited-cluster, the clusters the auditor was given credentials for,
#     clusters        each PROJECT/LOCATION/NAME: each GKE cluster of the pool (the project's list,
#                     the pool project's, and each GKE member of the fleet) none names is a finding
#                     naming it; a named cluster the pool does not hold, and a pool list not read
#                     whole, are UNKNOWN. Without the flag the step does not run.
#   live: GKE IAM     GKE authorizes a Google identity's Kubernetes call through IAM beside RBAC, so
#                     Policy Analyzer's holders of the container.* permission of each right, on the
#                     project, and of resourcemanager.projects.setIamPolicy and iam.roles.update
#                     there, by which a holder grants itself any of them. It runs before the RBAC
#                     steps, which ask who may impersonate each user and Google service account it
#                     lists, by name: GKE's IAM authorizer judges an impersonated user by its own IAM.
#   live: Deploy-     Policy Analyzer's holders, on the project, of deploymentmanager.deployments.create
#     ment Manager    and update, judged as the GKE IAM step judges its own: Deployment Manager creates
#                     a deployment's resources with the credentials of the project's Google APIs
#                     service agent (<number>@cloudservices.gserviceaccount.com), not the caller's,
#                     and that agent holds roles/editor on the project, so either permission acts as
#                     roles/editor there. Each finding names that route.
#   live: minters     Policy Analyzer's holders, on --gsa or above it, of
#                     iam.serviceAccounts.getAccessToken, getOpenIdToken, signJwt, signBlob, actAs,
#                     implicitDelegation, iam.serviceAccountKeys.create and setIamPolicy. Each is a
#                     finding, a person included, but the Workload Identity member
#                     serviceAccount:<pool>.svc.id.goog[<namespace>/<service account>], which is the
#                     witness: the answer must list it.
#                     The broker's roles/iam.serviceAccountTokenCreator on that account, by which it
#                     mints the secret ENVOY_OPERATOR's token, is a finding too, as its account or as
#                     its Workload Identity principal: the broker is a job that can mint with no second
#                     person approving that use at that time, and no automated job holds the operator
#                     identity (INFRA-840's R16). It is an open route until R16's grant door exists
#                     (INFRA-840's check c-operator-grant-door, plan task t-e-55), and its finding
#                     says so. The broker account's own minters are checked the same way, its Workload
#                     Identity member (--broker-workload) the witness and the one principal allowed:
#                     that member is the broker being itself, not a path to the operator's account.
#   impersonation     Every IAM step analyses with --analyze-service-account-impersonation: who can
#                     mint a holder's token, sign as it or act as it holds what the holder holds, so
#                     the outermost principal of each such chain, through any number of service
#                     accounts, is judged as a holder, its finding written as the chain
#                     (serviceAccount:a -> impersonates serviceAccount:b (<role>) -> <b's role>).
#                     The design's own links are no finding: a Workload Identity member acting as
#                     the account it is bound to. The broker minting --gsa's token is no such link.
#   live: RBAC        On the operator's cluster, one SubjectAccessReview per principal and right,
#                     the principals being every ServiceAccount outside the system namespaces
#                     (kube-system, gke-*, gmp-*), as on every other cluster of the pool, with the
#                     groups the API server gives it (system:serviceaccounts,
#                     system:serviceaccounts:<namespace>, system:authenticated), Kubernetes' own
#                     groups each alone (below), the Google service account each is bound to (its
#                     iam.gke.io/gcp-service-account annotation) with system:authenticated, the
#                     cluster's binding subjects (below), and anyone unauthenticated
#                     (system:anonymous with system:unauthenticated), always. The
#                     rights are the list below for each guarded pair; create, update and patch on
#                     each custom resource the API server's discovery (`kubectl api-resources
#                     --namespaced --verbs=create`) lists, in each guarded pair's namespace: a
#                     custom resource's controller may make a pod from it, as an agent sandbox does,
#                     a patch that finds no object creates it, and an update changes what its
#                     controller makes; and create, update and patch, cluster-wide, of each
#                     admissionregistration.k8s.io resource its discovery lists (`kubectl
#                     api-resources --api-group=...`): a mutating webhook or admission policy can
#                     add a container to the operator's next pod, and a validating one can block or
#                     reshape what it applies. A discovery that cannot be read whole, or whose
#                     admission list lacks a webhook configuration, which every API server serves,
#                     is UNKNOWN. The writes of a binding or a role are asked in each namespace
#                     that holds a RoleBinding too (binding-rights.jq): bind and escalate roles,
#                     bind clusterroles, and patch and update roles and rolebindings, since binding
#                     a role or a ClusterRole there grants what it holds there, that namespace's
#                     Secrets among them; every other cluster of the pool is asked them too. RBAC
#                     allows a request against a rule with resourceNames only when the request
#                     names one, so a rule limited by name never answers a review that names
#                     nothing: every cluster's Roles and
#                     ClusterRoles (`kubectl get roles,clusterroles -A`) are read against its bindings
#                     as the code step reads a render: a rule whose resourceNames limit a right asked
#                     is a finding naming the names for its subject, a person included, but a
#                     system component, a system namespace's ServiceAccount and the operator's own
#                     account (an approval
#                     of certificate signing requests by name, whose name the requester picks, joins
#                     the certificate chain). A refused list is UNKNOWN, and so is one that lacks
#                     either of its two kinds' bootstrap objects, labelled
#                     kubernetes.io/bootstrapping=rbac-defaults, which every cluster has: the
#                     ClusterRole cluster-admin and the Role
#                     kube-system/extension-apiserver-authentication-reader. A
#                     review without the groups would miss every grant made to a group. Its witness
#                     is a review for the group the built-in cluster-admin binding names,
#                     system:masters, which must be in the answer and allowed: an API server whose
#                     answers cannot say yes proves nothing by saying no. Every answer read with
#                     `kubectl -o json` (the reviews, each list, a webhook's Service) is held to its
#                     shape before it is read, so one sent empty, as `{}` or as an error page with
#                     exit 0 is UNKNOWN, never a list of nothing. Each
#                     --agent-namespace pattern that matches no namespace holding a ServiceAccount
#                     is UNKNOWN, so a mistyped one cannot drop its agents in silence. Then who can
#                     become a ServiceAccount with a finding is a finding naming the chain, followed
#                     in turn up to 5 steps: in its namespace, by any namespaced right of the list
#                     below (a pod or a workload controller runs as it; exec, attach, an ephemeral
#                     container or a pod patch enters one; a rolebinding, bind or escalate grants
#                     those; a token Secret is read) or create, update or patch of a custom resource
#                     there, and by its name, create serviceaccounts/token or impersonate; each of
#                     those rights is read in the cluster's live roles too, so one limited by name
#                     (a patch of one Deployment sets its serviceAccountName) is a finding naming
#                     the chain; a read of them that fails is UNKNOWN. A system namespace's
#                     ServiceAccount is reviewed too, never a finding itself, but when it is allowed
#                     a right asked, outright or by name in a live role, who can become it is
#                     followed the same way. A grant whose use on the broker's pods its pod fence
#                     refuses, here and in the code step, is a credit instead of a finding when the
#                     fence is credited, and who can become its holder is followed for the residual
#                     its use elsewhere reaches (below, "The broker's pod fence").
#   live: Workload    The Workload Identity member names the workload pool (<pool>.svc.id.goog,
#     Identity        --pool-project, by default --project), the namespace and the ServiceAccount,
#                     never the cluster, so every cluster that uses the pool can act as the Google
#                     service account a pair is bound to. The guarded pairs are the operator's
#                     (--namespace/--service-account), the broker's (--broker-workload) and each
#                     --guarded <namespace>/<service account>, such as a route password reader
#                     reached the same way. On every cluster of the pool (the operator's, every other
#                     cluster of the project, and every member cluster of the pool project's fleet)
#                     no principal the step reviews may create or patch a namespace cluster-wide,
#                     or a ServiceAccount or a pod in a guarded pair's namespace (a patch that finds
#                     no object creates it), nor hold an escalation
#                     right by which it grants itself one: impersonate users, groups and
#                     serviceaccounts, escalate and bind clusterroles, patch and update
#                     clusterroles, and create, patch and update clusterrolebindings, cluster-wide;
#                     bind and escalate roles, patch and update roles, and create, patch and update
#                     rolebindings in a guarded pair's namespace, and the writes of a binding or a
#                     role in each namespace that holds a RoleBinding; nor write a webhook or
#                     admission policy (the RBAC step's admission rights, from each cluster's own
#                     discovery). Each finding's right names the pairs it guards. On
#                     every cluster but the operator's, a guarded pair's namespace that exists is
#                     no finding by itself: it brings the operator's cluster's rights to that
#                     cluster's review too (RIGHTS_TABLE and the declared token Secrets), each holder
#                     a finding naming it with the cluster's label, and a REVIEWED line says so; on
#                     a cluster that is not GKE Autopilot it is UNKNOWN, for a privileged pod on a
#                     node reads its token there, a node escape no review sees. The principals are
#                     that cluster's ServiceAccounts outside its
#                     system namespaces (kube-system, gke-*, gmp-*), with their groups, and its
#                     binding subjects (below); on the operator's cluster they are the RBAC step's.
#                     A project with the operator's cluster alone says so. The fleet's memberships
#                     are read from the GKE Hub API (projects/<pool>/locations/-/memberships, every
#                     page) with the running identity's access token, not through `gcloud container
#                     fleet memberships list`, which drops the answer's `unreachable` locations: a
#                     listing that could not reach a location reads complete there. A refused
#                     listing, one with an unreachable location, and one that lists no membership
#                     (which cannot be told from one that saw nothing) are UNKNOWN, unless --fleet
#                     none says the pool project has no fleet: then an empty listing passes and each
#                     membership listed is a finding naming it. Each fleet scope (the API's
#                     projects/<pool>/locations/-/scopes, every page; `gcloud container fleet scopes
#                     list` asks locations/global alone) is UNKNOWN too: its namespaces and RBAC
#                     role bindings reach every member cluster bound to it, the operator's namespace
#                     and member included, and this check does not analyse them; a refused listing
#                     is UNKNOWN. A member that is a GKE cluster is read
#                     through `gcloud container clusters get-credentials` in its own project, any
#                     other through `gcloud container fleet memberships get-credentials` (the
#                     Connect gateway); such a member is UNKNOWN besides, since its own cloud's
#                     authorizers and token issuer may grant beside the RBAC read here, unless
#                     --rbac-only-member names it. The operator's cluster, and a cluster the project
#                     step read, are not read twice. GKE authorizes IAM beside RBAC, so for each GKE
#                     member in a project other than --project, Policy Analyzer's holders on that
#                     project of
#                     each guard right's container.* permission (container.namespaces.create,
#                     serviceAccounts.create and pods.create; clusters.impersonate, clusterRoles.bind
#                     and escalate, clusterRoleBindings.create and update, roles.bind and escalate,
#                     roleBindings.create and update, nodes.proxy, mutatingWebhookConfigurations and
#                     validatingWebhookConfigurations create and update) and of the IAM writers
#                     (setIamPolicy, iam.roles.update) are judged as the GKE IAM step judges its own
#                     (--project's are in that step). Each such project, and the pool project, is
#                     read before its clusters, which also ask who may impersonate, by name, each
#                     user and Google service account it lists.
#   live: minters of  For each Google service account an RBAC finding names (on a cluster, a fleet
#     an RBAC         member or in the renders), Policy Analyzer's holders on it of the minters
#     finding's       step's permissions, judged as that step judges --gsa's: who can mint its token,
#     account         sign as it or act as it holds what the finding gives it, so once the finding is
#                     accepted, an agent that can become the account is still reported.
#   live: minters of  For each Google service account whose credited grant reaches the broker's pod
#     a credited      fence's residual and which no RBAC finding names, the same holders, each an
#     grant's         ALLOWANCE naming the chain to the account and the residual where the step above
#     account         would find it. Each Workload Identity member of the operator's pool either step
#                     lists is followed on every cluster read, a chain to a found account's member a
#                     finding and one to a residual account's an ALLOWANCE; a chain can name another
#                     Google account, so both steps run until a pass names none, the found accounts
#                     settling before any residual one is read.
#   live: fleet       Policy Analyzer's holders on the pool project of gkehub.memberships.create and
#     registration    update, of gkehub.namespaces, rbacrolebindings and membershipbindings create
#                     and update, of the scopes' create, update and setIamPolicy, and of the
#                     features' create and update, each judged as in the GKE IAM step: a cluster
#                     registered to the pool's fleet shares the pool's Workload Identity, so its
#                     ServiceAccount envoy-operator/envoy-operator is the operator's member there,
#                     and a scope's namespace and RBAC role binding reach every member bound to it.
#   live: IAM on      For each folder and organization above --project, the pool project and each
#     ancestors       fleet member's project (`gcloud projects get-ancestors`), Policy Analyzer's
#                     holders of its setIamPolicy, and of the organization's iam.roles.update: each
#                     can grant itself any role below it. An answer that does not hold the project
#                     itself, as a real one always does, is UNKNOWN.
#   live: hold        Who can write the writer hold's tables beside the broker's routes, each holder a
#     writers         finding: the holders of cloudsql.users.create or update on --sql-instance, of
#                     get on the Secret --db-secret (a SubjectAccessReview for each principal of the
#                     RBAC step, and container.secrets.get through IAM), of storage.objects.get on the
#                     bucket of --broker-state, and of secretmanager.versions.access or setIamPolicy
#                     on --signing-secret, which the operator's Google service account alone may read
#                     and must be listed on.
#   live: render      Who could have written the renders and previews the code steps read: the
#     writers         holders of storage.objects.create, delete or update, or storage.buckets
#                     setIamPolicy or update, on --renders-bucket. --renders-admin is allowed and must
#                     be listed; each --render-writer EMAIL=OBJECT is allowed only under the condition
#                     resource.name == "projects/_/buckets/<bucket>/objects/<OBJECT>". Any other
#                     holder is a finding. Renders given without the bucket or the admin are UNKNOWN;
#                     with no render given the step does not run.
#   admin tag         `gcloud secrets describe <--secrets-prefix><slug of --secret> --format=json`, the
#                     broker's secret as Secret Manager holds it: its annotations, the broker's tags,
#                     must hold "admin": "true" and "tier": "human" (the operator's credential is an
#                     administrator's, released only when a person approves and never decided by a
#                     model judge). No such secret is a finding too. The read needs
#                     secretmanager.secrets.get alone, and no broker.
#
# Binding subjects. The identities a cluster's RBAC names are principals of that cluster too: GKE
# authenticates a Google account as a User of its email, so `grade-ci@<p>.iam.gserviceaccount.com`
# bound to cluster-admin on a member of the pool's fleet can make the operator's namespace and
# ServiceAccount there. On the operator's cluster and on each other cluster of the pool, every User
# subject of its ClusterRoleBindings and RoleBindings (`kubectl get clusterrolebindings,rolebindings
# -A`) is reviewed with system:authenticated (system:anonymous with system:unauthenticated), but a
# system component and the operator's own Google service account: a person (an email outside
# gserviceaccount.com, as GKE names a Google account), a Google service account and a User that is
# no email (a bare name such as deployer-bot, a principal:// name) alike, each a finding for each
# right it holds. Nobody holds the operator identity standing, a person included (INFRA-840's R16),
# so no User is passed over for the shape of its name.
# A ServiceAccount a binding names is reviewed whether it exists or not: whoever can make it and a
# pod in its namespace takes the binding's rights. On every cluster a ServiceAccount list that holds
# none outside the system namespaces is UNKNOWN, since every cluster has default/default. A refused
# list of bindings is UNKNOWN, naming the permission, and so is a list without either of its two
# kinds' witnesses, each labelled kubernetes.io/bootstrapping=rbac-defaults, which every cluster has:
# the ClusterRoleBinding cluster-admin and the RoleBinding
# kube-system/system::extension-apiserver-authentication-reader. Without its RoleBindings the list
# would hide every right given in a namespace outside the guarded ones.
#
# Group and User subjects. Kubernetes' own groups take their members from Kubernetes, whatever
# Google Groups for RBAC says: system:authenticated, system:serviceaccounts and
# system:serviceaccounts:<namespace> (for each namespace of a reviewed ServiceAccount, and each one
# a binding names) are reviewed alone on every cluster, and a right one holds is a finding, since it
# reaches every member. system:masters takes its members from client certificates, which no review
# can enumerate, and every review of it is allowed by the bootstrap cluster-admin ClusterRoleBinding
# (matched by its name, its roleRef ClusterRole cluster-admin and its label
# kubernetes.io/bootstrapping=rbac-defaults): every other binding that names it is a finding, and so
# is a render's. Any other Group is judged by where the cluster takes its groups from: on a GKE
# cluster with Google Groups for RBAC off (authenticatorGroupsConfig, read from the cluster list or
# `gcloud container clusters describe`) the API server gives no request such a group, so it is no
# finding; with it on, the cluster is UNKNOWN, a Group subject bound or not, since a Google group's
# members are neither listed nor judged here; on a fleet member that is no GKE cluster, read through
# the Connect gateway, its users and groups come from its own authenticator, so a Group subject bound
# there is UNKNOWN (a User subject bound there is reviewed, as every User subject is); where whether
# it is on cannot be read, a Group subject bound there is UNKNOWN. The code step reads no cluster's
# setting and judges another Group as off; the live step of the cluster says UNKNOWN when it is on.
# A system: User or Group other than these is a Kubernetes component and allowed by its name:
# system:-prefixed Users but system:anonymous and system:serviceaccount:<namespace>:<name> of a
# namespace outside the system ones. A name is not who holds it, so the three ways a principal comes
# to present such a name are each read and reported as the holder instead: a client certificate the
# cluster signs for any user (the certificate signing chain), impersonate users cluster-wide (an
# escalation right on every cluster), and a node escape on a cluster that is not Autopilot.
#
# Admission power outside the admission objects (admission-backends.sh), asked on every cluster of
# the pool as the other rights are: for each mutating or validating webhook whose namespaceSelector
# can match a guarded namespace (read against kubernetes.io/metadata.name alone, which no one can
# change), or whose rules can match a cluster-scoped object, which the API server applies no
# namespaceSelector to (a rule's resource counts as cluster-scoped when it is `*`, when
# `api-resources --namespaced=false` lists it as cluster-scoped in a group the rule names, or when
# the namespaced discovery lists it in none of them), in
# the namespace of the Service it calls every way to run or enter a pod that a guarded
# namespace is asked (RIGHTS_TABLE's namespaced rows, jobs, cronjobs and replicationcontrollers,
# exec, attach and ephemeral containers included, and create, update and patch of each custom
# resource), since a pod there that carries the Service's labels joins it and can mount its TLS
# Secret, and the writes to the Service, its Endpoints, EndpointSlices and Secrets, and to its
# ConfigMaps, from which an injector reads the template it adds; a webhook that calls a URL outside
# the cluster is UNKNOWN, and so is one whose Service is of type ExternalName or has no selector
# (its endpoints can point anywhere), or cannot be read. Create, update and patch of each
# cluster-scoped resource of a policy engine (a CRD of kyverno.io, gatekeeper.sh, kubewarden.io or a
# subgroup of one, or a group whose name holds
# "policy" or "policies"), as Kyverno's ClusterPolicy and MutatingPolicy or Gatekeeper's Assign
# mutate pods. The writes to each parameter object an admission policy's binding names (paramRef,
# of the policy's paramKind, resolved through the API server's discovery): create, and update and
# patch by its name, unnamed for a paramRef.selector, in each guarded namespace for a namespaced
# kind whose paramRef names no namespace (the admitted object's is read then). And on the operator's
# cluster, GKE Autopilot's workload allowlists: an installed WorkloadAllowlist admits the privileged
# workloads it names, so it is UNKNOWN, and create, update and patch of AllowlistSynchronizers,
# which install them, are asked. Backup for GKE installed on a cluster (a CRD of gkebackup.gke.io) is
# UNKNOWN: a restore, chosen in Google Cloud by a holder of its restore rights, recreates a backup's
# workloads and RBAC there. A refused list of webhook configurations, CRDs, admission policies or
# allowlists, a cluster-scoped discovery that could not be read whole, and a parameter kind the
# discovery does not resolve, is UNKNOWN.
#
# Cluster settings outside RBAC. Legacy ABAC authorizes requests by its own policies beside RBAC, and
# the identity service for GKE takes users and groups from an outside identity provider, so no
# review sees who either lets in: on each GKE cluster of the pool, read from the cluster list
# (legacyAbac, identityServiceConfig) or `gcloud container clusters describe`, either one on is
# UNKNOWN on that cluster's step, and so is a describe that was refused. container.clusters.update,
# by which a holder turns legacy ABAC on, is asked through IAM beside the rights' permissions. The
# operator's cluster must be Autopilot (autopilot.enabled in the cluster list): GKE Standard runs a
# privileged or hostPath pod, which on the operator's node reads the kubelet's credentials and the
# operator's ServiceAccount token, a node escape no review sees, so Standard is UNKNOWN. On another
# GKE cluster of the pool, read Standard by `gcloud container clusters describe` (a refused describe
# is UNKNOWN), the same pod reads every token on its node, so a system namespace's ServiceAccount
# that runs a pod on a node is no longer allowed: it is reviewed for every right that cluster's
# review asks and create serviceaccounts/token for each guarded pair, and read in that cluster's
# roles for each rule that gives one by name, each allowed a finding naming it (the certificate
# chain only whole), an evaluation error UNKNOWN, and a list of pods that holds no pod UNKNOWN
# (node-escape.sh).
#
# In every IAM step a `user:` member, a person, is judged as every other holder is, a finding for
# each checked permission it holds; a group is a finding as the group, whatever its members, since
# whoever is added to it holds what it holds with no grant, and the members of a group the analysis
# expands are judged besides, each by name (one it does not expand says so); the Google service agents the deployment
# needs are allowed: GKE's (service-<number>@container-engine-robot), GKE Hub's
# (service-<number>@gcp-sa-gkehub) and Compute Engine's (service-<number>@compute-system) of a
# project the check reads (--project, the pool project and each fleet member's project, each number
# read from its project), and each --service-agent; every other member is a finding, such an agent
# of another project (named with its number: it acts for whoever owns that project) and another
# product's service-<number>@gcp-sa-* agent included. A Policy Analyzer
# answer that lists no holder, or that was not fully explored, is UNKNOWN: a resource always has
# holders (its project's owners at least), so an answer listing none saw nothing. The analysis runs
# over the
# project's top ancestor (`gcloud projects get-ancestors`), or --analysis-scope: a scope at the
# project does not see a grant made on its folder or its organization.
#
# The rights, in each guarded pair's namespace unless cluster-wide: create serviceaccounts/token of
# the pair's ServiceAccount; impersonate that ServiceAccount, and users and groups cluster-wide;
# escalate and bind roles and clusterroles, patch and update roles and clusterroles, create, patch
# and update rolebindings and clusterrolebindings;
# create, and get (which authorizes an exec, attach or port forward over WebSocket on API servers
# before 1.35), pods/exec, pods/attach and pods/portforward; create, patch and update pods, and patch
# and update pods/ephemeralcontainers; create, patch and update replicationcontrollers, deployments,
# replicasets, statefulsets, daemonsets, jobs and cronjobs, each of which runs a pod as the
# ServiceAccount; create and get nodes/proxy cluster-wide, whose kubelet API enters any pod on the
# node; get, list, watch and create secrets (a Secret of type kubernetes.io/service-account-token
# naming a ServiceAccount is filled with its token), and on the operator's cluster get, by name, of
# each guarded pair's token Secret, those the Secret renders declare and those its ServiceAccount
# lists in its `secrets` (no Secret is read); and, cluster-wide on every
# cluster of the pool, create, update
# and patch of each admissionregistration.k8s.io resource its discovery lists, and the certificate
# signing chain: a certificate signed for kubernetes.io/kube-apiserver-client authenticates as any
# user and groups its request names but system:masters, and GKE cannot turn the certificates API
# off, so a principal allowed update certificatesigningrequests/approval and approve on signers
# (kubernetes.io/kube-apiserver-client or kubernetes.io/*, by name) is one finding, naming create
# certificatesigningrequests too when it is allowed; create alone signs nothing. The code step does
# not read the chain. roles/container.admin names no permission for the admission policies (read
# with `gcloud iam roles describe` on 2026-11-21), so IAM is asked the webhook configurations'
# alone, and container.certificateSigningRequests.approve for the chain.
#
# The broker's pod fence (operator-token-minters/broker-fence.sh). RBAC allows a request that
# admission can still refuse, and the program the renders come from refuses, on every pod of the
# broker's in dispatch and to everyone, exec, attach and port forward (broker-pod-connect) and any
# change but metadata (broker-pod-update), beside broker-pod-create and the two workload policies,
# each bound Deny with failurePolicy Fail in dispatch alone. When dispatch/envoy-broker is a guarded
# pair (--broker-workload or a --guarded), the check credits that fence as reviewed and nothing else
# (operator-token-minters/broker-fence.json holds each policy's and binding's canonical spec, or, for
# the two workload policies, its sha256): the code step reads it in the renders, and the RBAC step
# reads the operator's cluster's ValidatingAdmissionPolicies and bindings (less what the API server
# and GKE fill in), its pods in dispatch and its webhooks. Only when the renders and the cluster each
# hold the five policies and bindings with the reviewed specs; no pod there named otherwise than
# envoy-broker-* runs as envoy-broker or takes --db-secret, a guarded pair's token Secret (declared,
# or listed by its ServiceAccount), a Secret a broker pod takes, or a Secret no --secret-render
# declares (the check reads no Secret, so it may hold the broker's token); at least one
# envoy-broker-* pod runs as envoy-broker; and no webhook of
# the cluster's configurations, whatever its selectors and rules, calls a Service in dispatch, does a
# grant of create or get on pods/exec, pods/attach or pods/portforward in dispatch, outright or by
# name, declared or live, or as a way to become envoy-broker, print a CREDIT line naming its use on
# the broker's pods instead of a finding; in the guarded rights, only while dispatch/envoy-broker is
# the one guarded pair in dispatch, since entry into another pod there reaches the other pair. Its
# use on dispatch's other pods, Dispatch's own, whose database login can write the writer hold's
# tables, the fence does not refuse: that is broker-fence.json's residual, tracked by INFRA-4604, and
# each such grant (not a way to become envoy-broker) prints an ALLOWANCE line naming it, and so does
# each chain to its holder: who can become a ServiceAccount, or mint, sign as or act as a Google
# service account, whose credited grant carries the residual, and so on along the chain, Google and
# Kubernetes alike, read to a fixed point, where it reaches nothing more; a chain that also reaches
# an account with a finding is a finding. Every other right stays a finding: patch and update of
# pods in dispatch (broker-pod-update admits a change of any pod's metadata, and a pod given the
# broker's label app=envoy-broker takes the broker's Services' traffic) and of
# pods/ephemeralcontainers (on a pod not the broker's an ephemeral container takes any Secret of the
# namespace), create pods in dispatch (so does a new pod), every Google IAM right and every hold
# writer; so does whoever can
# write the fence: its create, update and patch are admission rights, and its delete and
# deletecollection are asked while it is credited. A fence missing, held otherwise, or beside a pod
# or a webhook backend as above credits nothing and prints NO CREDIT naming why; one whose policies,
# pods or webhooks cannot be read, or are answered as no list, credits nothing and is UNKNOWN.
#
# Exit 0: no finding and no UNKNOWN (a CREDIT or ALLOWANCE line is neither). Exit 1: at least one
# finding, each line naming the principal and the right (UNKNOWN lines may stand beside them). Exit
# 2: no finding, and at least one UNKNOWN, each naming its step and why, such as the permission the
# running identity lacks; also a usage error, a copy of the script missing a part or a jq file of
# operator-token-minters/, or holding one empty (checked before any is sourced: an empty file
# sources as a clean no-op and an empty jq program is `.`), a step file that ran and reported
# nothing or never ran (the step census, output.sh), and a run that stops before its end (a
# command failing under set -e), each of which prints the UNKNOWN `run` and writes it to --report (a
# usage error writes none). --report is deleted when the run starts, so a report is this run's or
# none. --report <file> writes {"run_at": "<RFC 3339>", "findings": [{"principal", "right", "role",
# "resource", "permissions", "granted_at"}], "credited": [{"principal", "right", "resource",
# "granted_at", "policy", "declared", "live"}], "residuals": [{"principal", "right", "resource",
# "granted_at", "residual", "tracked_by"}], "unknown": [{"step", "reason"}], "allowances": [{"flag",
# "value"}], "steps": [{"step", "state"}]} beside the text, run_at being when the run began.
# `steps` names each step file in the order it runs and its census state: reported, quiet: <why>
# (a step that says why it prints nothing), silent or not run (each an UNKNOWN). `credited` holds each grant whose use
# on the broker's pods its pod fence refuses: principal and right as its CREDIT line prints them,
# resource and granted_at as a finding's (the grant, kept as evidence), the policy that refuses that
# use (its binding has its name), declared the sha256 of the renders' fence and live the same of the
# cluster's with the generation and resourceVersion of that policy and binding, so a reader sees what
# was trusted. `residuals` holds each ALLOWANCE line, a credited grant's use on the namespace's other
# pods or a chain to its holder, with the residual's text and the issue that tracks it: the fence's
# residual alone, never the run's flags, which `allowances` names. Each is [] when nothing is
# credited. `allowances` names each allowance the run was given (each --service-agent, each
# --rbac-only-member, and --fleet none), each of which lets a principal or a reading pass, so a
# reader holds each one to what was accepted; it is [] when there is none. `right` is the sentence
# the text prints; the rest are its parts, for a reader that matches a finding against an accepted
# entry by principal and role:
#   role         the IAM role (roles/container.admin), for an impersonation chain the role of its
#                outermost link; for an RBAC finding the right's label (create
#                serviceaccounts/token envoy-operator in envoy-operator); for the namespace and the
#                secret's tags, what is wrong (namespace envoy-operator, admin, tier, secret);
#   resource     what the right is on: the analysed resource's full name,
#                `cluster:<project>/<location>/<name>` on every cluster, the operator's included
#                (its location `-` while live: clusters has not read one),
#                `membership:<pool>/<location>/<id>`, or `secret:<name>`;
#   permissions  the IAM permissions the role grants among those asked, for a chain the holder's;
#                for an RBAC finding the right's label; empty for the namespace and the secret's tags;
#   granted_at   the resource the IAM binding is made on, for a chain its outermost link's; for an
#                RBAC finding the API server's reason (`RBAC: allowed by ClusterRoleBinding "x" of
#                ...`) or, from code, the binding; empty where there is none.
#
# The fixture testdata/operator-token-minters-report.json, beside this script, is the --report of a
# copy of this script whose broker-fence.json holds operator-token-minters.test.sh's example workload
# policies by their digests, run in that test's broker pod fence's world, with run_at fixed: an IAM
# finding, RBAC findings of a Google service account bound on a fleet member for each of three
# guarded pairs (the operator's, the broker's and a --guarded one), the fence's credits and
# residuals, and an UNKNOWN. The writer hold's preflight, which reads --report, tests its reader
# against it, and the test fails when the file differs from a fresh run
# (OPERATOR_MINTERS_WRITE_FIXTURE=1 rewrites it).
#
#   bash packages/envoy/infra/scripts/operator-token-minters.sh --project <id> --cluster <name> \
#     --gsa <email> --agent-namespace <ns|glob>... --broker-account <email> \
#     --broker-workload <ns>/<sa> --sql-instance <name> --db-secret <ns>/<name> \
#     --broker-state gs://<bucket>[/<path>] --signing-secret <name> \
#     --rbac-render <file>... --secret-render <file>... --iam-preview <file>... \
#     --renders-bucket gs://<bucket> --renders-admin <email> --render-writer <email>=<object>... \
#     [--guarded <ns>/<sa>]... [--report <file>]
#
# The controls run against scratch targets: --namespace envoy-operator-control --gsa
# envoy-operator-control@<project>.iam.gserviceaccount.com --secret ENVOY_OPERATOR_CONTROL.
# operator-token-minters.test.sh runs every plant and control against a fake kubectl, gcloud and
# curl. This file holds the run's flags, its exit and its report; each step is a part
# in operator-token-minters/ beside it, sourced in the order above, and their large jq programs
# are .jq files there. Needs bash 4.4, jq, and for the live steps gcloud, kubectl and curl.
set -euo pipefail

usage() {
  cat <<'USAGE'
usage: operator-token-minters.sh [flags]
  --project ID               the project that runs the operator's cluster
  --cluster NAME             the operator's cluster, one of the project's
  --location LOCATION        the operator's cluster's location (a region or a zone); needed when the
                             project holds more than one cluster named --cluster
  --pool-project ID          the project whose workload pool (ID.svc.id.goog) the operator's and
                             the broker's ServiceAccounts are in, and whose fleet's member clusters
                             share it (default --project)
  --fleet none               the pool project has no fleet: an empty membership listing passes, and
                             each membership listed is a finding (without it, an empty listing is
                             UNKNOWN, since it cannot be told from one that saw nothing)
  --rbac-only-member ID      a member of the pool project's fleet that is no GKE cluster and whose
                             cloud grants nothing beside its RBAC; repeatable (without it, such a
                             member is UNKNOWN)
  --service-agent EMAIL      another Google service agent the deployment needs, allowed in the IAM
                             steps and written into --report; EMAIL is
                             service-<number>@gcp-sa-<product>.iam.gserviceaccount.com and nothing
                             else (GKE's, GKE Hub's and Compute Engine's agents of the projects
                             read are allowed without it); repeatable
  --namespace NS             the operator's namespace (default envoy-operator)
  --service-account NAME     the operator's Kubernetes ServiceAccount (default envoy-operator)
  --guarded NS/NAME          one more namespace and ServiceAccount the pool's Workload Identity binds
                             to a Google service account, guarded as the operator's pair is, which is
                             always guarded, as the broker's (--broker-workload) is when it is named;
                             repeatable
  --gsa EMAIL                the Google service account the operator's ServiceAccount acts as
  --agent-namespace NS|GLOB  a namespace whose ServiceAccounts agents run as, or a glob of such
                             names with * and ?; repeatable. Every ServiceAccount outside the system
                             namespaces is reviewed; these name the groups reviewed alone, and one
                             must hold a ServiceAccount
  --broker-account EMAIL     the secrets broker's Google service account, which mints the
                             operator's token for the secret ENVOY_OPERATOR
  --broker-workload NS/NAME  the broker's Kubernetes ServiceAccount, bound to --broker-account
  --sql-instance NAME        the Cloud SQL instance of the broker's database, in --project
  --db-secret NS/NAME        the Secret holding the broker's database login
  --broker-state gs://BUCKET[/PATH]  where the broker program's Pulumi state is kept
  --signing-secret NAME      the Secret Manager secret of the writer hold's signing key
  --secret NAME              the broker secret of the operator's credential (default ENVOY_OPERATOR)
  --secrets-prefix projects/NUMBER/secrets/PREFIX-
                             the broker's secrets_prefix setting: --secret is <PREFIX>-<its slug> there
  --rbac-render FILE         a JSON render of the cluster's RBAC program; repeatable
  --secret-render FILE       a Kubernetes-object JSON render of a Secret program; repeatable
  --renders-bucket URL       the renders bucket, gs://BUCKET; its writers are read when renders are given
  --renders-project ID       the bucket's project, whose own Google service agents the writers step accepts
  --renders-admin EMAIL      the Google service account that applies the bucket: allowed every
                             permission the writers step asks, and its witness
  --render-writer EMAIL=OBJECT  a writer allowed to write OBJECT alone, under the condition
                             resource.name == "projects/_/buckets/BUCKET/objects/OBJECT"; repeatable
  --iam-preview FILE         `pulumi preview --json --show-sames` of an IAM program; repeatable
  --analysis-scope organizations/N|folders/N|projects/ID
                             Policy Analyzer's scope (default: the project's top ancestor)
  --credentials-arg ARG      one gcloud flag, --name or --name=value, passed to each
                             `gcloud container clusters get-credentials`; repeatable
  --report FILE              also write the result as JSON
  --audited-cluster PROJECT/LOCATION/NAME
                             a cluster the auditor reads; each GKE cluster of the pool must be one
USAGE
}
usage_error() {
  echo "operator-token-minters: $1" >&2
  usage >&2
  exit 2
}

# A run's report is this run's or none: an older one would be read as this run's if the run stopped
# before writing its own, so it goes before anything, a usage error included, can stop the run. A
# --report this run cannot delete is a usage error naming the flag, never the file.
for ((arg = 1; arg <= $#; arg++)); do
  case "${!arg}" in
    --report)
      next=$((arg + 1))
      [ "$next" -gt "$#" ] || rm -f -- "${!next}" 2>/dev/null || usage_error "--report names a file this run cannot delete"
      ;;
    --report=*)
      report_arg="${!arg}"
      rm -f -- "${report_arg#--report=}" 2>/dev/null || usage_error "--report names a file this run cannot delete"
      ;;
  esac
done
PROJECT="" CLUSTER="" LOCATION="" POOL_PROJECT="" NAMESPACE=envoy-operator KSA=envoy-operator GSA="" BROKER="" BROKER_WORKLOAD=""
SQL_INSTANCE="" DB_SECRET="" BROKER_STATE="" SIGNING_SECRET="" SECRET=ENVOY_OPERATOR ANALYSIS_SCOPE="" REPORT="" FLEET="" RENDERS_BUCKET="" RENDERS_ADMIN=""
RENDERS_PROJECT="" SECRETS_PREFIX=""
AGENT_NAMESPACES=() RBAC_RENDERS=() SECRET_RENDERS=() IAM_PREVIEWS=() CREDENTIALS_ARGS=() GUARDED_FLAGS=() RBAC_ONLY_MEMBERS=() SERVICE_AGENTS=() RENDER_WRITERS=()
AUDITED_CLUSTERS=()
# A usage error names the flag, or an argument's place, and never the text given: a value pasted in
# the wrong place may be a credential. Each flag that names a thing is held to that thing's form
# below, before any step runs, and refuses a value shaped as a credential its issuer marks
# (credential_prefix), so neither reaches a command line, output line or report. A value of the
# flag's form that is not so shaped is used, and may be printed, as given: a short ghp_abc is. A
# file flag's file is named by the flag and its place (--rbac-render entry 2).
total_args=$#
while [ "$#" -gt 0 ]; do
  place=$((total_args - $# + 1))
  flag="$1"
  shift
  case "$flag" in
    -h | --help) usage; exit 0 ;;
    --*=*) set -- "${flag#*=}" "$@"; flag="${flag%%=*}" ;;
  esac
  case "$flag" in
    --project | --cluster | --location | --pool-project | --namespace | --service-account | --gsa | --agent-namespace | --broker-account | \
      --broker-workload | --sql-instance | --db-secret | --broker-state | --signing-secret | --secret | --renders-bucket | --renders-admin | \
      --renders-project | --secrets-prefix | \
      --rbac-render | --secret-render | --iam-preview | --analysis-scope | --credentials-arg | --report | --guarded | --fleet | --rbac-only-member | \
      --service-agent | --render-writer | --audited-cluster)
      [ "$#" -gt 0 ] || usage_error "$flag needs a value"
      value="$1"
      shift
      ;;
    *)
      if [[ "$flag" =~ ^--[a-z][a-z0-9-]*$ ]]; then
        usage_error "unknown flag $flag"
      fi
      usage_error "argument $place is no flag this check takes"
      ;;
  esac
  case "$flag" in
    --project) PROJECT="$value" ;;
    --cluster) CLUSTER="$value" ;;
    --location) LOCATION="$value" ;;
    --pool-project) POOL_PROJECT="$value" ;;
    --namespace) NAMESPACE="$value" ;;
    --service-account) KSA="$value" ;;
    --gsa) GSA="$value" ;;
    --agent-namespace) AGENT_NAMESPACES+=("$value") ;;
    --broker-account) BROKER="$value" ;;
    --broker-workload) BROKER_WORKLOAD="$value" ;;
    --sql-instance) SQL_INSTANCE="$value" ;;
    --db-secret) DB_SECRET="$value" ;;
    --broker-state) BROKER_STATE="$value" ;;
    --signing-secret) SIGNING_SECRET="$value" ;;
    --secret) SECRET="$value" ;;
    --renders-bucket) RENDERS_BUCKET="$value" ;;
    --renders-admin) RENDERS_ADMIN="$value" ;;
    --renders-project) RENDERS_PROJECT="$value" ;;
    --secrets-prefix) SECRETS_PREFIX="$value" ;;
    --rbac-render) RBAC_RENDERS+=("$value") ;;
    --secret-render) SECRET_RENDERS+=("$value") ;;
    --iam-preview) IAM_PREVIEWS+=("$value") ;;
    --analysis-scope) ANALYSIS_SCOPE="$value" ;;
    --credentials-arg) CREDENTIALS_ARGS+=("$value") ;;
    --report) REPORT="$value" ;;
    --guarded) GUARDED_FLAGS+=("$value") ;;
    --fleet) FLEET="$value" ;;
    --rbac-only-member) RBAC_ONLY_MEMBERS+=("$value") ;;
    --service-agent) SERVICE_AGENTS+=("$value") ;;
    --render-writer) RENDER_WRITERS+=("$value") ;;
    --audited-cluster) AUDITED_CLUSTERS+=("$value") ;;
  esac
done
# credential_prefix: the shape of a credential whose issuer marks it, its prefix and the characters
# after it, as the daemon's redaction reads one (packages/daemon/internal/capabilities/
# capabilities.go, credentialRules, whose JWT rule this leaves out): a GitHub token (ghp_, gho_,
# ghu_, ghs_, ghr_, github_pat_), a Google OAuth access token (ya29.), a Slack token (xoxb- and the
# like), and an OpenAI, Dispatch, Datadog, GitLab or npm token (sk-, dsp_, ddsat_, ddpat_, ddapp_,
# glpat-, npm_), each with 20 characters or more after its prefix; a Google API key, AIza and 35 or
# more; and an AWS access key ID, AKIA or ASIA and exactly 16. A name flag's form admits some: a ghp_
# token, an AIza key and a github_pat_ token are each of --signing-secret's form, and an AWS access
# key ID of --secret's. A value with one of these prefixes and fewer characters is not refused.
credential_prefix='^((gh[pousr]_|github_pat_)[A-Za-z0-9._-]{20,}|(sk-|dsp_|ddsat_|ddpat_|ddapp_|ya29\.|xox[a-z]-|glpat-|npm_)[A-Za-z0-9_-]{20,}|AIza[A-Za-z0-9_-]{35,}|(AKIA|ASIA)[A-Z0-9]{16}([^A-Za-z0-9_]|$))'
# refuse_form FLAG PATTERN FORM VALUE...: a usage error naming FLAG and the FORM it takes, never the
# value, unless each VALUE is empty (the flag not given), or of PATTERN's form and not shaped as a
# credential its issuer marks (credential_prefix).
refuse_form() {
  local flag="$1" pattern="$2" form="$3" which="its value" value
  shift 3
  [ "$#" -le 1 ] || which="one of its values"
  for value in "$@"; do
    [ -z "$value" ] || [[ "$value" =~ $pattern ]] || usage_error "$flag is $form; $which is not"
    [[ ! "$value" =~ $credential_prefix ]] || usage_error "$flag is $form; $which is shaped as a credential its issuer marks"
  done
}
# A project ID, as Resource Manager allows it: 6 to 30 lowercase letters, digits and hyphens,
# starting with a letter, after a domain and a colon for a domain-scoped project.
project_id='([a-z][-.a-z0-9]*[a-z0-9]:)?[a-z][-a-z0-9]{4,28}[a-z0-9]'
k8s_label='^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'
account_email='^[a-z0-9][-a-z0-9]*@[a-z0-9][-.:a-z0-9]*\.gserviceaccount\.com$'
refuse_form --project "^$project_id\$" "a Google Cloud project ID" "$PROJECT"
refuse_form --pool-project "^$project_id\$" "a Google Cloud project ID" "$POOL_PROJECT"
refuse_form --cluster '^[a-z]([-a-z0-9]{0,38}[a-z0-9])?$' "a GKE cluster name" "$CLUSTER"
refuse_form --location '^[a-z]+-[a-z]+[0-9]+(-[a-z])?$' "a Google Cloud region or zone" "$LOCATION"
refuse_form --namespace "$k8s_label" "a Kubernetes namespace name" "$NAMESPACE"
refuse_form --service-account '^[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?$' "a Kubernetes ServiceAccount name" "$KSA"
refuse_form --gsa "$account_email" "a Google service account's email" "$GSA"
refuse_form --broker-account "$account_email" "a Google service account's email" "$BROKER"
refuse_form --agent-namespace '^[-*?a-z0-9]{1,63}$' "a Kubernetes namespace name, or a glob of one with * and ?" \
  ${AGENT_NAMESPACES[@]+"${AGENT_NAMESPACES[@]}"}
refuse_form --sql-instance '^[a-z]([-a-z0-9]{0,96}[a-z0-9])?$' "a Cloud SQL instance name" "$SQL_INSTANCE"
refuse_form --signing-secret '^[-_A-Za-z0-9]{1,255}$' "a Secret Manager secret ID" "$SIGNING_SECRET"
# The broker's secret-name rule (internal/cipher/policy/names.go).
refuse_form --secret '^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$' "a broker secret name, uppercase letters, digits and single underscores starting with a letter" "$SECRET"
refuse_form --rbac-only-member "$k8s_label" "a fleet membership ID" ${RBAC_ONLY_MEMBERS[@]+"${RBAC_ONLY_MEMBERS[@]}"}
refuse_form --credentials-arg '^--[a-z][-a-z0-9]*(=[^[:space:]]+)?$' "one gcloud flag, --name or --name=value" \
  ${CREDENTIALS_ARGS[@]+"${CREDENTIALS_ARGS[@]}"}
refuse_form --analysis-scope "^(organizations/[0-9]+|folders/[0-9]+|projects/([0-9]+|$project_id))\$" \
  "organizations/N, folders/N or projects/ID" "$ANALYSIS_SCOPE"
[ -n "$POOL_PROJECT" ] || POOL_PROJECT="$PROJECT"
case "$FLEET" in "" | none) ;; *) usage_error "--fleet takes none and nothing else" ;; esac
# --service-agent allows a Google-managed service agent alone, of the gcp-sa-<product> form: any other
# account, which a principal of the project can act as, would turn a minter into a pass.
for agent in ${SERVICE_AGENTS[@]+"${SERVICE_AGENTS[@]}"}; do
  [[ "$agent" =~ ^service-[0-9]+@gcp-sa-[a-z0-9-]+\.iam\.gserviceaccount\.com$ ]] ||
    usage_error "--service-agent takes a Google service agent, service-<number>@gcp-sa-<product>.iam.gserviceaccount.com; one of its values is not one"
done
# allowances_json: the JSON array of each allowance the run was given, {flag, value}: each
# --service-agent, each --rbac-only-member and --fleet none, which let a principal or a reading pass,
# --renders-admin and each --render-writer, which let an account write the renders, and each
# --credentials-arg, which chooses how each cluster is reached, so a reader of --report can hold
# each one to what was accepted.
allowances_json() {
  {
    for value in ${SERVICE_AGENTS[@]+"${SERVICE_AGENTS[@]}"}; do printf -- '--service-agent\t%s\n' "$value"; done
    for value in ${RBAC_ONLY_MEMBERS[@]+"${RBAC_ONLY_MEMBERS[@]}"}; do printf -- '--rbac-only-member\t%s\n' "$value"; done
    [ -z "${FLEET:-}" ] || printf -- '--fleet\t%s\n' "$FLEET"
    [ -z "${RENDERS_ADMIN:-}" ] || printf -- '--renders-admin\t%s\n' "$RENDERS_ADMIN"
    for value in ${RENDER_WRITERS[@]+"${RENDER_WRITERS[@]}"}; do printf -- '--render-writer\t%s\n' "$value"; done
    for value in ${CREDENTIALS_ARGS[@]+"${CREDENTIALS_ARGS[@]}"}; do printf -- '--credentials-arg\t%s\n' "$value"; done
  } | jq -R -s -c '[split("\n")[] | select(length > 0) | split("\t") | {flag: .[0], value: .[1]}]'
}
# The guarded pairs: the operator's first, then the broker's (--broker-workload), then each
# --guarded, each once.
GUARDED=("$NAMESPACE/$KSA")
pair_name='^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-.a-z0-9]*[a-z0-9])?$'
refuse_form --broker-workload "$pair_name" "<namespace>/<name>, each a Kubernetes name" "$BROKER_WORKLOAD"
refuse_form --guarded "$pair_name" "<namespace>/<serviceaccount>, each a Kubernetes name" ${GUARDED_FLAGS[@]+"${GUARDED_FLAGS[@]}"}
[ -z "$BROKER_WORKLOAD" ] || [ "$BROKER_WORKLOAD" = "${GUARDED[0]}" ] || GUARDED+=("$BROKER_WORKLOAD")
for pair in ${GUARDED_FLAGS[@]+"${GUARDED_FLAGS[@]}"}; do
  case " ${GUARDED[*]} " in *" $pair "*) ;; *) GUARDED+=("$pair") ;; esac
done
refuse_form --db-secret "$pair_name" "<namespace>/<name>, each a Kubernetes name" "$DB_SECRET"
refuse_form --broker-state '^gs://[a-z0-9][-_.a-z0-9]{1,220}[a-z0-9](/.*)?$' "gs://<bucket>[/<path>]" "$BROKER_STATE"
refuse_form --renders-bucket '^gs://[a-z0-9][-_.a-z0-9]{1,220}[a-z0-9]$' "gs://BUCKET" "$RENDERS_BUCKET"
refuse_form --renders-admin '^[a-z0-9][-a-z0-9]*@[a-z0-9][-.:a-z0-9]*\.gserviceaccount\.com$' "a Google service account email" "$RENDERS_ADMIN"
refuse_form --renders-project "^$project_id\$" "a Google Cloud project ID" "$RENDERS_PROJECT"
# The broker's own form of its secrets_prefix setting (internal/cipher/secrets/gcp.go, ValidGCPPrefix).
refuse_form --secrets-prefix '^projects/[0-9]+/secrets/[A-Za-z0-9_-]*-$' "projects/<number>/secrets/<prefix>-, the broker's secrets_prefix" "$SECRETS_PREFIX"
declare -A render_seen=()
for writer in ${RENDER_WRITERS[@]+"${RENDER_WRITERS[@]}"}; do
  [[ "$writer" =~ ^[a-z0-9][-a-z0-9]*@[a-z0-9][-.:a-z0-9]*\.gserviceaccount\.com=[a-z][-a-z0-9]*/[a-z][-a-z0-9]*$ ]] ||
    usage_error "--render-writer takes a Google service account email and one object, EMAIL=program/name"
  [ -z "${render_seen[${writer%%=*}]:-}" ] && [ -z "${render_seen[=${writer#*=}]:-}" ] ||
    usage_error "--render-writer names each writer and each object once"
  [ "${writer%%=*}" != "$RENDERS_ADMIN" ] || usage_error "--render-writer names the --renders-admin account"
  render_seen[${writer%%=*}]=1 render_seen[=${writer#*=}]=1
done
declare -A audited_seen=()
for audited in ${AUDITED_CLUSTERS[@]+"${AUDITED_CLUSTERS[@]}"}; do
  [[ "$audited" =~ ^[a-z][-a-z0-9]{4,28}[a-z0-9]/[a-z][-a-z0-9]*[a-z0-9]/[a-z]([-a-z0-9]{0,38}[a-z0-9])?$ ]] ||
    usage_error "--audited-cluster takes PROJECT/LOCATION/NAME, a GKE cluster's project ID, location and name"
  [ -z "${audited_seen[$audited]:-}" ] || usage_error "--audited-cluster names each cluster once"
  audited_seen[$audited]=1
done
command -v jq >/dev/null || usage_error "jq is not on PATH"

RUN_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/operator-token-minters.XXXXXX")"
# on_exit, the EXIT trap: a run that stops before its end (a command that failed under set -e)
# reads as UNKNOWN, never as clean or as a finding. crash_report, below, prints the UNKNOWN naming
# that, writes --report with it, and the run exits 2. The work directory goes either way.
FINISHED=false
on_exit() {
  local status=$?
  set +e
  if [ "$FINISHED" != true ] && declare -F crash_report >/dev/null; then
    crash_report "$status"
    status=2
  fi
  rm -rf "$WORK"
  exit "$status"
}
trap on_exit EXIT
mkdir -p "$WORK/kube" "$WORK/roles"

# The steps run in the header's order, each part in its own file of operator-token-minters/ beside
# this script, sourced here so that all of them share this run's state; a part's large jq programs
# are .jq files there, read with jq -f.
LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/operator-token-minters"
# STEP_FILES: the step files, in the order they run, each sourced between census_begin and
# census_end (output.sh), so one that reports nothing, or never runs, is an UNKNOWN naming it.
STEP_FILES=(step-permission-names.sh step-code-secrets.sh step-code-rbac.sh step-code-iam.sh step-clusters.sh step-gke-iam.sh
  step-rbac.sh step-workload-identity.sh step-audited-clusters.sh step-live-iam.sh step-admin-tag.sh)
# Every part and jq file must be readable and hold something before any is sourced: a missing one
# would stop the run before output.sh defines its report, or leave a step that needs it unread, and
# an empty one sources as a clean no-op under set -e (and an empty jq program is `.`), so a step
# would vanish from a run that exits 0.
missing=""
for part in output.sh rights.sh principals.sh checks.sh reviews.sh admission-backends.sh broker-fence.sh iam-analysis.sh node-escape.sh \
  "${STEP_FILES[@]}" analysis.jq become-rights.jq broker-fence.jq broker-fence.json \
  code-iam.jq code-rbac.jq custom.jq custom-rights.jq fleet.jq guard-rights.jq binding-rights.jq rec.jq review-answer.jq \
  review-request.jq webhook-backends.jq; do
  [ -r "$LIB/$part" ] && [ -s "$LIB/$part" ] || missing="${missing:+$missing, }$part"
done
if [ -n "$missing" ]; then
  reason="the parts $missing of $LIB cannot be read or are empty, so no step runs"
  echo "UNKNOWN run: $reason"
  [ -z "$REPORT" ] ||
    { jq -n --arg run_at "$RUN_AT" --arg reason "$reason" --argjson allowances "$(allowances_json)" '
      {run_at: $run_at, findings: [], credited: [], residuals: [], unknown: [{step: "run", reason: $reason}], allowances: $allowances,
       steps: [$ARGS.positional[] | {step: ., state: "not run"}]}' --args "${STEP_FILES[@]}" >"$REPORT"; } 2>/dev/null ||
    echo "operator-token-minters: --report cannot be written" >&2
  echo "operator-token-minters: no step ran, 1 unknown: exit 2"
  FINISHED=true
  exit 2
fi
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/output.sh
. "$LIB/output.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/rights.sh
. "$LIB/rights.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/principals.sh
. "$LIB/principals.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/checks.sh
. "$LIB/checks.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/reviews.sh
. "$LIB/reviews.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/admission-backends.sh
. "$LIB/admission-backends.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/broker-fence.sh
. "$LIB/broker-fence.sh"
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-permission-names.sh
. "$LIB/step-permission-names.sh"
census_end step-permission-names.sh
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-code-secrets.sh
. "$LIB/step-code-secrets.sh"
census_end step-code-secrets.sh
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-code-rbac.sh
. "$LIB/step-code-rbac.sh"
census_end step-code-rbac.sh
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-code-iam.sh
. "$LIB/step-code-iam.sh"
census_end step-code-iam.sh
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-clusters.sh
. "$LIB/step-clusters.sh"
census_end step-clusters.sh
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/iam-analysis.sh
. "$LIB/iam-analysis.sh"
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-gke-iam.sh
. "$LIB/step-gke-iam.sh"
census_end step-gke-iam.sh
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-rbac.sh
. "$LIB/step-rbac.sh"
census_end step-rbac.sh
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/node-escape.sh
. "$LIB/node-escape.sh"
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-workload-identity.sh
. "$LIB/step-workload-identity.sh"
census_end step-workload-identity.sh
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-audited-clusters.sh
. "$LIB/step-audited-clusters.sh"
census_end step-audited-clusters.sh
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-live-iam.sh
. "$LIB/step-live-iam.sh"
census_end step-live-iam.sh
census_begin
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-admin-tag.sh
. "$LIB/step-admin-tag.sh"
census_end step-admin-tag.sh

# --- The result. ---
census_close
findings="${#FINDINGS[@]}"
unknowns="${#UNKNOWNS[@]}"
write_report
FINISHED=true
if [ "$findings" -gt 0 ]; then
  echo "operator-token-minters: $findings finding(s), $unknowns unknown: exit 1"
  exit 1
fi
if [ "$unknowns" -gt 0 ]; then
  echo "operator-token-minters: no finding, $unknowns unknown: exit 2"
  exit 2
fi
echo "operator-token-minters: no finding and nothing unknown: exit 0"
