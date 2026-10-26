#!/usr/bin/env bash
# Tests for scripts/azure-setup.sh against a fake Azure CLI.
#
# Usage: scripts/test-azure-setup.sh    (VERBOSE=1 prints each run's output)
#
# A stand-in `az` on PATH answers the queries azure-setup.sh makes from a
# small state directory (does the account exist, is shared key access on,
# what lifecycle rules are there) and logs every call, so each case checks
# what the script would change in Azure without an Azure subscription.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/stradl-azuretest.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

FAILURES=0
pass() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  FAILURES=$((FAILURES + 1))
}

# --- fake az ------------------------------------------------------------------

mkdir -p "$WORK/bin"
cat >"$WORK/bin/az" <<'EOF'
#!/usr/bin/env bash
# Fake Azure CLI. State lives in $FAKE_AZ; every call is appended to
# $FAKE_AZ/calls.log, one line per call.
set -u
S="$FAKE_AZ"
state() { cat "$S/$1"; }
printf '%s\n' "$*" >>"$S/calls.log"

# Keep the JSON handed to management-policy for the test to parse.
prev=""
for a in "$@"; do
  case "$prev" in
    --policy) printf '%s' "$a" >"$S/policy.json" ;;
    policy.rules) printf '%s' "$a" >"$S/added-rule.json" ;;
  esac
  prev="$a"
done

case "$*" in
  "account show -o none") exit 0 ;;
  "account show --query name -o tsv") echo "Fake Subscription" ;;
  "account show --query id -o tsv") echo "00000000-0000-0000-0000-000000000000" ;;
  "group exists "*) if [ "$(state rg)" = 1 ]; then echo true; else echo false; fi ;;
  "group show "*) echo eastus ;;
  "storage account list --resource-group "*) state account ;;
  "storage account list --query "*) echo None ;;
  "storage account check-name "*"nameAvailable"*) echo true ;;
  "storage account keys list "*) echo "ZmFrZWtleQ==" ;;
  "storage account show "*"allowSharedKeyAccess"*) state sharedkey ;;
  "storage account management-policy show "*"policy.rules[].name"*)
    case "$(state lifecycle)" in
      none)
        echo "ERROR: (ManagementPolicyNotFound) No ManagementPolicy found for account" >&2
        exit 3
        ;;
      error)
        echo "ERROR: (AuthorizationFailed) The client does not have authorization" >&2
        exit 1
        ;;
      other) echo "archiveoldlogs" ;;
      ours) printf '%s\n' archiveoldlogs "$(state rule)" ;;
    esac
    ;;
  "storage account management-policy show "*"daysAfterCreationGreaterThan"*) echo 45 ;;
  "storage container exists "*) if [ "$(state container)" = 1 ]; then echo true; else echo false; fi ;;
  "storage container policy list "*"contains(keys(@)"*) if [ "$(state policy)" = 1 ]; then echo true; else echo false; fi ;;
  "storage container policy list "*"stradl.expiry"*) echo "2099-01-01T00:00:00+00:00" ;;
  "storage container policy list "*"stradl.permission"*) echo racwdl ;;
  "storage container generate-sas "*) echo "sv=2024-11-04&si=stradl&sr=c&spr=https&sig=FAKE" ;;
  "account set "* | "group create "* | "storage account create "* | "storage account update "* | \
    "storage account blob-service-properties update "* | "storage account management-policy create "* | \
    "storage account management-policy update "* | "storage container create "* | \
    "storage container policy create "* | "storage container policy update "*) ;;
  *)
    echo "fake az: unexpected call: $*" >&2
    exit 99
    ;;
esac
EOF
chmod 755 "$WORK/bin/az"

# --- harness ------------------------------------------------------------------

STATE=""
OUT=""
RC=0

# setup_state NAME rg account container policy sharedkey lifecycle [rule]
setup_state() {
  STATE="$WORK/$1"
  mkdir -p "$STATE"
  : >"$STATE/calls.log"
  echo "$2" >"$STATE/rg"
  echo "$3" >"$STATE/account"
  echo "$4" >"$STATE/container"
  echo "$5" >"$STATE/policy"
  echo "$6" >"$STATE/sharedkey"
  echo "$7" >"$STATE/lifecycle"
  echo "${8:-stradloldversions}" >"$STATE/rule"
}

run_setup() {
  set +e
  OUT="$(env -i HOME="$WORK" PATH="$WORK/bin:/usr/bin:/bin" FAKE_AZ="$STATE" NO_COLOR=1 \
    bash "$ROOT/scripts/azure-setup.sh" --resource-group rg-stradl --account bpstradl --yes "$@" 2>&1 </dev/null)"
  RC=$?
  set -e
  if [ -n "${VERBOSE:-}" ]; then printf '%s\n(exit %s)\n\n' "$OUT" "$RC"; fi
}

called() { grep -qF -- "$1" "$STATE/calls.log"; }
calls() { grep -cF -- "$1" "$STATE/calls.log" || true; }

# check_rule FILE CONTAINER RULE: the lifecycle rule in FILE (a whole policy
# or one rule) deletes previous versions of CONTAINER's blobs after 30 days.
check_rule() {
  python3 -I - "$1" "$2" "$3" <<'EOF'
import json, sys
path, container, name = sys.argv[1:]
doc = json.load(open(path))
rules = doc["rules"] if "rules" in doc else [doc]
assert len(rules) == 1, rules
r = rules[0]
assert r["name"] == name and r["name"].isalnum(), r["name"]
assert r["enabled"] is True and r["type"] == "Lifecycle", r
assert r["definition"]["actions"] == {"version": {"delete": {"daysAfterCreationGreaterThan": 30}}}, r
assert r["definition"]["filters"] == {"blobTypes": ["blockBlob"], "prefixMatch": [container + "/"]}, r
EOF
}

# --- cases --------------------------------------------------------------------

# 1. Nothing exists yet: everything is created, including a lifecycle policy
#    that holds only the version cleanup rule.
setup_state fresh 0 0 0 0 None none
run_setup --location eastus
if [ "$RC" -eq 0 ] && called "storage account create" && called "--allow-shared-key-access true" &&
  [ "$(calls "management-policy create")" = 1 ] && [ "$(calls "management-policy update")" = 0 ] &&
  check_rule "$STATE/policy.json" stradl stradloldversions; then
  pass "new account: lifecycle policy created with the version cleanup rule"
else
  fail "new account (rc=$RC): $OUT"
fi
case "$OUT" in
  *"sig=FAKE"*) pass "new account: SAS token printed" ;;
  *) fail "new account: no SAS token in output: $OUT" ;;
esac
if called "management-policy show"; then
  fail "new account: looked for a lifecycle policy on an account that doesn't exist yet"
else
  pass "new account: no lifecycle lookup before the account exists"
fi

# 2. Reused account without a lifecycle policy, custom container: the policy
#    is created, and the output says why shared key access stays on.
setup_state reuse-nopolicy 1 1 1 1 None none
run_setup --container my-team
if [ "$RC" -eq 0 ] && [ "$(calls "management-policy create")" = 1 ] &&
  check_rule "$STATE/policy.json" my-team myteamoldversions; then
  pass "reused account without a policy: lifecycle policy created"
else
  fail "reused account without a policy (rc=$RC): $OUT"
fi
case "$OUT" in
  *"Shared key access stays on"*"SAS"*) pass "reused account: says shared key access stays on for SAS" ;;
  *) fail "reused account: no shared key explanation: $OUT" ;;
esac

# 3. Reused account with someone else's lifecycle rules and shared key access
#    off: the rule is added next to the existing ones, never replacing them,
#    and turning shared key back on is called out before and during the run.
setup_state reuse-other 1 1 1 1 False other
run_setup
if [ "$RC" -eq 0 ] && [ "$(calls "management-policy create")" = 0 ] &&
  [ "$(calls "management-policy update")" = 1 ] && called "--add policy.rules" &&
  check_rule "$STATE/added-rule.json" stradl stradloldversions; then
  pass "existing lifecycle policy: rule added, other rules kept"
else
  fail "existing lifecycle policy (rc=$RC): $OUT"
fi
case "$OUT" in
  *"shared key access is off"*"Shared key access is back on"*) pass "shared key access off: warned, then turned back on" ;;
  *) fail "shared key access off: no warning or confirmation: $OUT" ;;
esac
if called "storage account update" && called "--allow-shared-key-access true"; then
  pass "shared key access off: account updated to allow it"
else
  fail "shared key access off: account not updated"
fi

# 4. Re-run after the rule exists (here edited to 45 days in the portal): the
#    lifecycle policy is left alone.
setup_state reuse-ours 1 1 1 1 True ours
run_setup
if [ "$RC" -eq 0 ] && [ "$(calls "management-policy create")" = 0 ] &&
  [ "$(calls "management-policy update")" = 0 ]; then
  pass "re-run: existing version cleanup rule left as it is"
else
  fail "re-run with the rule in place (rc=$RC): $OUT"
fi
case "$OUT" in
  *"after 45 days"*) pass "re-run: plan shows the rule's current setting" ;;
  *) fail "re-run: plan doesn't show the existing rule: $OUT" ;;
esac

# 5. A lifecycle policy that can't be read stops the run rather than being
#    replaced.
setup_state reuse-error 1 1 1 1 True error
run_setup
if [ "$RC" -ne 0 ] && [ "$(calls "management-policy create")" = 0 ] &&
  [ "$(calls "management-policy update")" = 0 ] && ! called "generate-sas"; then
  pass "unreadable lifecycle policy: stops without touching it"
else
  fail "unreadable lifecycle policy (rc=$RC): $OUT"
fi

# 6. --rotate only moves the token expiry.
setup_state rotate 1 1 1 1 False none
run_setup --rotate
if [ "$RC" -eq 0 ] && called "storage container policy update" && ! called "management-policy" &&
  ! called "storage account update" && ! called "allowSharedKeyAccess"; then
  pass "--rotate: changes only the access policy expiry"
else
  fail "--rotate (rc=$RC): $OUT"
fi

echo
if [ "$FAILURES" -gt 0 ]; then
  echo "$FAILURES Azure setup test(s) failed"
  exit 1
fi
echo "all Azure setup tests passed"
