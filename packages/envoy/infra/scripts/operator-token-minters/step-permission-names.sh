# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-permission-names.sh: the step "permission names". Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- permission names ---
step="permission names"
mark
checked_names=("${CONTAINER_PERMISSIONS[@]}" "${GSA_PERMISSIONS[@]}" "${SQL_PERMISSIONS[@]}" "${FLEET_PERMISSIONS[@]}"
  "${IAM_WRITE_PERMISSIONS[@]}" secretmanager.versions.access storage.objects.get)
declare -A roles_used=() role_holds=()
for permission in "${checked_names[@]}"; do
  granting_role "$permission"
  role="$GRANTING_ROLE"
  if [ -z "${roles_used[$role]:-}" ]; then
    roles_used[$role]="read"
    if role_perms "$role"; then
      while IFS= read -r held; do role_holds["$role $held"]=1; done <"$ROLE_FILE"
    else
      roles_used[$role]=failed
      unknown "$step" "$ROLE_ERROR"
    fi
  fi
  [ "${roles_used[$role]}" != failed ] || continue
  [ -n "${role_holds["$role $permission"]:-}" ] ||
    unknown "$step" "$permission is not in $role's includedPermissions, so no check that names it can find a holder"
done
if unchanged; then
  pass "$step" "each of the ${#checked_names[@]} permission names the IAM steps ask about is in the role that grants it ($(printf '%s\n' "${!roles_used[@]}" | sort | paste -sd ' ' | sed 's/ /, /g'))"
fi
