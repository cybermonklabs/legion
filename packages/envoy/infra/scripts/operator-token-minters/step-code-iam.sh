# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-code-iam.sh: the step "code: IAM", and the project number it and the
# signing key's check read. Sourced by operator-token-minters.sh, whose header says what each step
# checks.
# --- code: IAM ---

# covers CHECK KIND ID PROJECT: whether a binding the code declares on KIND ID (with the project its
# resource names, for a secret) reaches the check's resource. The renders bucket's project is no
# flag, so a grant on that project is the live step's alone; one on the bucket is read here.
covers() {
  local check="$1" kind="$2" id="$3" in_project="$4"
  case "$kind" in organization | folder) return 0 ;; esac
  case "$check:$kind" in
    gke:project | dm:project | sql:project | dbsecret:project | state:project | signing:project) [ "${id#projects/}" = "$PROJECT" ] ;;
    gsa:project) [ "${id#projects/}" = "$GSA_PROJECT" ] ;;
    broker:project) [ "${id#projects/}" = "$BROKER_PROJECT" ] ;;
    fleet:project) [ "${id#projects/}" = "$POOL_PROJECT" ] ;;
    gsa:serviceaccount) [ "${id##*/}" = "$GSA" ] ;;
    broker:serviceaccount) [ "${id##*/}" = "$BROKER" ] ;;
    signing:secret)
      [ "${id##*/}" = "$SIGNING_SECRET" ] || return 1
      case "$id" in projects/*) in_project="${id#projects/}" in_project="${in_project%%/*}" ;; esac
      [ -z "$in_project" ] || [ "$in_project" = "$PROJECT" ] || [ "$in_project" = "${PROJECT_NUMBER:-}" ]
      ;;
    state:bucket) [ "${id#b/}" = "$BUCKET" ] ;;
    renders:bucket) [ "${id#b/}" = "$RENDERS_BUCKET_NAME" ] ;;
    *) return 1 ;;
  esac
}
resource_of() { if [ "$1" = signing ]; then echo "//secretmanager.googleapis.com/projects/${PROJECT_NUMBER:-$PROJECT}/secrets/$SIGNING_SECRET"; else echo "${CHECK_RESOURCE[$1]}"; fi; }

# The project number, which Secret Manager's resource names use.
PROJECT_NUMBER="" PROJECT_NUMBER_ERROR=""
if [ -z "$PROJECT" ]; then
  PROJECT_NUMBER_ERROR="no --project"
elif ! have gcloud; then
  PROJECT_NUMBER_ERROR="gcloud is not on PATH"
elif ! gcloud projects describe "$PROJECT" --format=json --quiet >"$WORK/project.json" 2>"$WORK/project.err"; then
  PROJECT_NUMBER_ERROR="gcloud projects describe $PROJECT was refused (it needs resourcemanager.projects.get): $(first_line "$WORK/project.err")"
else
  PROJECT_NUMBER="$(jq -r '.projectNumber // ""' "$WORK/project.json" 2>/dev/null || true)"
  [ -n "$PROJECT_NUMBER" ] || PROJECT_NUMBER_ERROR="gcloud projects describe $PROJECT answered no projectNumber"
fi
CHECK_RESOURCE[signing]="$(resource_of signing)"
# The renders bucket's project number, whose service agents the renders check accepts (checks.sh).
# A number that cannot be read leaves those agents unknown, so the check is UNKNOWN, not clean.
if [ -z "${CHECK_MISSING[renders]:-}" ]; then
  if ! have gcloud ||
    ! gcloud projects describe "$RENDERS_PROJECT" --format=json --quiet >"$WORK/renders-project.json" 2>"$WORK/renders-project.err" ||
    ! RENDERS_PROJECT_NUMBER="$(jq -er '.projectNumber | select(type == "string" and test("^[0-9]+$"))' "$WORK/renders-project.json" 2>/dev/null)"; then
    RENDERS_PROJECT_NUMBER=""
    CHECK_MISSING[renders]="the number of the renders bucket's project $RENDERS_PROJECT cannot be read (gcloud projects describe needs resourcemanager.projects.get there): $(first_line "$WORK/renders-project.err" 2>/dev/null)"
  fi
fi

step="code: IAM"
if [ "${#IAM_PREVIEWS[@]}" -eq 0 ]; then
  unknown "$step" "no --iam-preview: what the IAM programs declare is not read"
else
  mark
  : >"$WORK/preview.rec"
  # Each preview is named by its flag and place (--iam-preview entry 2), never its file, and jq
  # reads it from its stdin, so an error it prints names no file either.
  n=0
  for file in "${IAM_PREVIEWS[@]}"; do
    n=$((n + 1))
    if [ ! -r "$file" ] || [ -d "$file" ]; then
      unknown "$step" "--iam-preview entry $n cannot be read"
      continue
    fi
    if ! jq -r --arg file "--iam-preview entry $n" -L "$LIB" -f "$LIB/code-iam.jq" <"$file" >>"$WORK/preview.rec" 2>"$WORK/preview.err"; then
      unknown "$step" "--iam-preview entry $n is not the JSON pulumi preview --json prints: $(first_line "$WORK/preview.err")"
    fi
  done
  # The previews' own custom roles, so a role the code declares is read as the code declares it.
  while IFS="$US" read -r -u 3 what name permissions _; do
    [ "$what" = role ] || continue
    role_file "$name"
    printf '%s\n' "${permissions//,/$'\n'}" | sed '/^$/d' | sort -u >"$ROLE_FILE"
  done 3<"$WORK/preview.rec"
  judged=0
  while IFS="$US" read -r -u 3 what a b c d e f g; do
    case "$what" in
      empty) unknown "$step" "$a lists no resource, so it shows nothing" ;;
      nosames) unknown "$step" "$a was made without --show-sames, so it leaves out every binding it does not change" ;;
      unknown) unknown "$step" "$a: the preview does not know its $b (it is computed when the program applies), so what it grants is unknown" ;;
      unread) unknown "$step" "$a: the check does not read $b, which can grant on the resources these checks are about" ;;
      binding)
        # a: kind, b: id, c: the secret's project, d: role, e: member, f: urn, g: condition
        for check in "${CHECKS[@]}"; do
          [ -z "${CHECK_MISSING[$check]:-}" ] || continue
          covers "$check" "$a" "$b" "$c" || continue
          if ! role_perms "$d"; then
            unknown "$step" "$f grants $d, whose permissions are unknown: $ROLE_ERROR"
            continue
          fi
          role_granted "$check"
          granted="$GRANTED"
          [ -n "$granted" ] || continue
          judged=$((judged + 1))
          attached="$a:$b"
          [ "$a" != serviceaccount ] || attached="//iam.googleapis.com/projects/-/serviceAccounts/${b##*/}"
          iam_verdict "$check" "$e" "$d" "$attached" code "$g"
          [ "$VERDICT" != finding ] ||
            finding "$e" "$(held_text "$check" "$granted") on $(resource_of "$check") through $d, declared by $f$NOTE" \
              "$d" "$(resource_of "$check")" "$(held_list "$check" "$granted")" "$attached"
        done
        ;;
    esac
  done 3<"$WORK/preview.rec"
  if unchanged; then
    pass "$step" "the previews grant the checked permissions to no one but Google's service agents and the accounts allowed by name, no person and no group among them ($judged grants read)"
  fi
fi
