#!/usr/bin/env bash
#
# Provision Azure Blob Storage for Stradl and print the container SAS token
# that every Mac needs.
#
# Built for Azure Cloud Shell (Bash), where az is installed and signed in:
#
#   curl -fsSL <release url>/azure-setup.sh | bash -s -- \
#     --resource-group rg-stradl --location eastus --account bpstradl
#
# Any shell with a signed-in Azure CLI works the same way.
#
# What it ensures, reusing anything that already exists:
#   - the resource group
#   - a StorageV2 / Standard_LRS account: HTTPS only, TLS 1.2 minimum, no
#     public blob access, shared key access on (SAS tokens are signed with
#     the account key, so generating one needs it; a reused account that had
#     it off gets it turned back on, and the plan says so)
#   - blob versioning, 30-day blob soft delete, 30-day container soft delete
#   - a lifecycle rule ("<container>oldversions") that deletes previous blob
#     versions 30 days after they were written, so versioning can't grow the
#     bill forever; added next to any rules the account already has
#   - a private container
#   - a stored access policy ("stradl", racwdl) that holds the token's expiry
#
# The account key and the SAS token live only in memory: neither is written
# to disk, and the token is printed exactly once.
#
# Run with --help for every option. docs/azure.md covers the security model.

set -Eeuo pipefail

readonly POLICY_NAME="stradl"
readonly POLICY_PERMISSIONS="racwdl"
readonly RETENTION_DAYS=30
readonly VERSION_DAYS=30
readonly APP_TAG="app=stradl"

# Options
RESOURCE_GROUP=""
LOCATION=""
ACCOUNT=""
CONTAINER="stradl"
YEARS=1
SUBSCRIPTION=""
ROTATE=0
ASSUME_YES=0

# What already exists, filled in by discover
RG_EXISTS=0
RG_LOCATION=""
ACCOUNT_EXISTS=0
CONTAINER_EXISTS=0
POLICY_EXISTS=0
POLICY_EXPIRY=""
POLICY_PERMS=""
# allowSharedKeyAccess of a reused account: True, False, or None (never set,
# which Azure treats as on)
SHARED_KEY=""
# The lifecycle rule that expires old versions, named after the container
# (rule names are letters and digits only). LIFECYCLE_STATE is "create" (the
# account has no lifecycle policy), "add" (it has one without this rule) or
# "keep" (the rule is there; its settings are left as they are).
LIFECYCLE_RULE=""
LIFECYCLE_STATE="create"
LIFECYCLE_DAYS_FOUND=""

# Results. ACCOUNT_KEY and SAS_TOKEN are secrets: never echo or log them.
ACCOUNT_KEY=""
SAS_TOKEN=""
NEW_EXPIRY=""
TOKEN_EXPIRY=""
CURRENT_STEP=""

usage() {
	cat <<'EOF'
Usage: azure-setup.sh --resource-group NAME --account NAME [--location REGION] [options]

Create (or reuse) the Azure storage Stradl syncs into, then print the
container SAS token for the Stradl installer. Safe to re-run.

It turns on blob versioning and soft delete, and adds a lifecycle rule that
deletes previous blob versions 30 days after they were written. Shared key
access stays on (or is turned back on) because SAS tokens need it.

Required:
  --resource-group NAME  Resource group to create or reuse
  --account NAME         Storage account: 3-24 lowercase letters and digits,
                         unique across all of Azure
  --location REGION      Azure region, e.g. eastus. Needed only when the
                         resource group does not exist yet

Options:
  --container NAME       Blob container (default: stradl)
  --years N              Token lifetime in years, kept on the stored access
                         policy (default: 1)
  --subscription ID      Subscription name or ID (runs az account set)
  --rotate               Only move the policy expiry to now + --years and
                         print the token again; existing tokens keep working
  --yes                  Skip the confirmation prompt
  -h, --help             Show this help

Example (Azure Cloud Shell):
  curl -fsSL <release url>/azure-setup.sh | bash -s -- \
    --resource-group rg-stradl --location eastus --account bpstradl
EOF
}

# --- output ---------------------------------------------------------------------

setup_colors() {
	if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
		BOLD=$'\033[1m'
		DIM=$'\033[2m'
		RED=$'\033[31m'
		GREEN=$'\033[32m'
		YELLOW=$'\033[33m'
		RESET=$'\033[0m'
	else
		BOLD="" DIM="" RED="" GREEN="" YELLOW="" RESET=""
	fi
}

step() {
	CURRENT_STEP="$*"
	printf '%s==>%s %s\n' "$BOLD" "$RESET" "$*"
}

warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }

die() {
	printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2
	exit 1
}

# on_error reports which step failed. az has already printed the reason. The
# guard keeps command substitutions (subshells, which inherit the trap under
# set -E) from reporting the same failure twice.
on_error() {
	local status=$?
	if [ "${BASH_SUBSHELL:-0}" -eq 0 ]; then
		printf '\n%sSetup stopped%s while: %s\n' "$RED" "$RESET" "${CURRENT_STEP:-starting up}" >&2
		printf 'Fix the error above and run the same command again; finished steps are reused.\n' >&2
	fi
	exit "$status"
}

# display_time turns an ISO 8601 UTC timestamp into "2027-10-25 18:04 UTC".
display_time() {
	if [ -z "$1" ]; then
		printf 'not set'
	else
		printf '%s %s UTC' "${1:0:10}" "${1:11:5}"
	fi
}

# is_before reports whether ISO 8601 UTC timestamp $1 is earlier than $2.
# Azure returns UTC, so comparing the fixed-width prefixes as text (bytewise,
# hence LC_ALL=C) is enough.
is_before() {
	local LC_ALL=C
	[[ "${1:0:19}" < "${2:0:19}" ]]
}

is_past() {
	is_before "$1" "$(date -u +%Y-%m-%dT%H:%M:%S)"
}

# expiry_after_years prints the UTC time N years from now in the format az
# expects (Y-m-dTH:M:SZ). Plain string arithmetic behaves the same under GNU
# date (Cloud Shell) and BSD date (macOS).
expiry_after_years() {
	local now year rest
	now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	year="${now%%-*}"
	rest="${now#*-}"
	case "$rest" in
	02-29T*) rest="02-28${rest#02-29}" ;;
	esac
	printf '%d-%s\n' "$((10#$year + $1))" "$rest"
}

# sorted_letters normalizes a permission string so "rwdlac" equals "racwdl".
sorted_letters() {
	printf '%s' "$1" | fold -w1 | sort | tr -d '\n'
}

# --- arguments ------------------------------------------------------------------

set_option() {
	case "$2" in
	-*) die "$1 needs a value (run with --help for usage)" ;;
	esac
	case "$1" in
	--resource-group) RESOURCE_GROUP="$2" ;;
	--location) LOCATION="$2" ;;
	--account) ACCOUNT="$2" ;;
	--container) CONTAINER="$2" ;;
	--years) YEARS="$2" ;;
	--subscription) SUBSCRIPTION="$2" ;;
	esac
}

parse_args() {
	local opt
	while [ $# -gt 0 ]; do
		opt="$1"
		shift
		case "$opt" in
		-h | --help)
			usage
			exit 0
			;;
		--rotate) ROTATE=1 ;;
		-y | --yes) ASSUME_YES=1 ;;
		--resource-group | --location | --account | --container | --years | --subscription)
			[ $# -gt 0 ] || die "$opt needs a value (run with --help for usage)"
			set_option "$opt" "$1"
			shift
			;;
		--resource-group=* | --location=* | --account=* | --container=* | --years=* | --subscription=*)
			set_option "${opt%%=*}" "${opt#*=}"
			;;
		*) die "unknown argument: $opt (run with --help for usage)" ;;
		esac
	done
}

validate_args() {
	local account_re='^[a-z0-9]{3,24}$'
	local container_re='^[a-z0-9]([a-z0-9]|-[a-z0-9]){2,62}$'
	local years_re='^[1-9][0-9]?$'

	[ -n "$RESOURCE_GROUP" ] || die "--resource-group is required (run with --help for usage)"
	[ -n "$ACCOUNT" ] || die "--account is required (run with --help for usage)"
	if ! [[ "$ACCOUNT" =~ $account_re ]]; then
		die "invalid --account '$ACCOUNT': storage account names are 3-24 lowercase letters and digits, with no hyphens or capitals"
	fi
	if ! [[ "$CONTAINER" =~ $container_re ]] || [ "${#CONTAINER}" -gt 63 ]; then
		die "invalid --container '$CONTAINER': use 3-63 lowercase letters, digits and single hyphens, starting and ending with a letter or digit"
	fi
	if ! [[ "$YEARS" =~ $years_re ]]; then
		die "invalid --years '$YEARS': use a whole number from 1 to 99"
	fi
	LIFECYCLE_RULE="${CONTAINER//-/}oldversions"
}

# --- Azure ----------------------------------------------------------------------

# az_cli runs the Azure CLI with stdin closed, so nothing it does can read
# from the pipe this script may have arrived on (curl ... | bash).
az_cli() {
	az "$@" </dev/null
}

# az_data runs a data-plane command signed with the account key. The key
# travels in AZURE_STORAGE_KEY, which az reads in place of --account-key, so
# it never shows up in the process list.
az_data() {
	AZURE_STORAGE_KEY="$ACCOUNT_KEY" az "$@" --account-name "$ACCOUNT" --auth-mode key </dev/null
}

check_prerequisites() {
	command -v az >/dev/null 2>&1 ||
		die "the Azure CLI (az) is not installed. Run this in Azure Cloud Shell (shell.azure.com), where it is ready to go."

	# Credentials left in the environment would override the account key.
	unset AZURE_STORAGE_CONNECTION_STRING AZURE_STORAGE_SAS_TOKEN AZURE_STORAGE_ACCOUNT \
		AZURE_STORAGE_KEY AZURE_STORAGE_AUTH_MODE AZURE_STORAGE_SERVICE_ENDPOINT

	if ! az_cli account show -o none 2>/dev/null; then
		die "the Azure CLI is not signed in. Run 'az login' first (Cloud Shell signs you in automatically)."
	fi
	if [ -n "$SUBSCRIPTION" ]; then
		step "Switching to subscription $SUBSCRIPTION"
		az_cli account set --subscription "$SUBSCRIPTION"
	fi
}

fetch_account_key() {
	CURRENT_STEP="reading the storage account key"
	ACCOUNT_KEY="$(az_cli storage account keys list --account-name "$ACCOUNT" --resource-group "$RESOURCE_GROUP" --query '[0].value' -o tsv)"
	[ -n "$ACCOUNT_KEY" ] || die "az returned no access key for storage account '$ACCOUNT'"
}

# check_account_name fails unless a new account named $ACCOUNT can be created.
check_account_name() {
	local available other_rg reason
	available="$(az_cli storage account check-name --name "$ACCOUNT" --query nameAvailable -o tsv)"
	if [ "$available" = "true" ]; then
		return 0
	fi

	other_rg="$(az_cli storage account list --query "[?name=='$ACCOUNT'].resourceGroup | [0]" -o tsv)"
	if [ "$other_rg" = "None" ]; then
		other_rg=""
	fi
	if [ -n "$other_rg" ]; then
		die "storage account '$ACCOUNT' already exists in resource group '$other_rg'. Run again with --resource-group $other_rg to reuse it."
	fi

	reason="$(az_cli storage account check-name --name "$ACCOUNT" --query reason -o tsv)"
	if [ "$reason" = "AccountNameInvalid" ]; then
		die "Azure rejected '$ACCOUNT' as a storage account name. Use 3-24 lowercase letters and digits."
	fi
	die "the storage account name '$ACCOUNT' is taken. Names are unique across all of Azure, so choose another --account (for example ${ACCOUNT:0:20}$((RANDOM % 9000 + 1000)))."
}

discover_container() {
	local out
	out="$(az_data storage container exists --name "$CONTAINER" --query exists -o tsv)"
	if [ "$out" != "true" ]; then
		if [ "$ROTATE" = 1 ]; then
			die "nothing to rotate: container '$CONTAINER' does not exist in '$ACCOUNT'. Run once without --rotate to create it."
		fi
		return 0
	fi
	CONTAINER_EXISTS=1

	out="$(az_data storage container policy list --container-name "$CONTAINER" --query "contains(keys(@), '$POLICY_NAME')" -o tsv)"
	if [ "$out" != "true" ]; then
		if [ "$ROTATE" = 1 ]; then
			die "nothing to rotate: container '$CONTAINER' has no '$POLICY_NAME' access policy. Run once without --rotate to create it."
		fi
		return 0
	fi
	POLICY_EXISTS=1
	POLICY_EXPIRY="$(az_data storage container policy list --container-name "$CONTAINER" --query "$POLICY_NAME.expiry" -o tsv)"
	POLICY_PERMS="$(az_data storage container policy list --container-name "$CONTAINER" --query "$POLICY_NAME.permission" -o tsv)"
	if [ "$POLICY_EXPIRY" = "None" ]; then
		POLICY_EXPIRY=""
	fi
}

# discover_lifecycle finds out whether the account's lifecycle management
# policy already has the rule that expires old versions. An account without
# any policy makes show fail with a NotFound error; anything else is a real
# failure, and guessing "no policy" then could replace someone's rules.
discover_lifecycle() {
	local out status=0
	out="$(az_cli storage account management-policy show --account-name "$ACCOUNT" \
		--resource-group "$RESOURCE_GROUP" --query "policy.rules[].name" -o tsv 2>&1)" || status=$?
	if [ "$status" -ne 0 ]; then
		case "$out" in
		*NotFound*)
			LIFECYCLE_STATE="create"
			return 0
			;;
		esac
		printf '%s\n' "$out" >&2
		die "couldn't read the lifecycle management policy of storage account '$ACCOUNT'. Fix the error above and run the same command again."
	fi
	if ! printf '%s\n' "$out" | grep -qxF "$LIFECYCLE_RULE"; then
		LIFECYCLE_STATE="add"
		return 0
	fi
	LIFECYCLE_STATE="keep"
	LIFECYCLE_DAYS_FOUND="$(az_cli storage account management-policy show --account-name "$ACCOUNT" \
		--resource-group "$RESOURCE_GROUP" \
		--query "policy.rules[?name=='$LIFECYCLE_RULE'] | [0].definition.actions.version.delete.daysAfterCreationGreaterThan" -o tsv)"
	if [ "$LIFECYCLE_DAYS_FOUND" = "None" ]; then
		LIFECYCLE_DAYS_FOUND=""
	fi
}

# discover records what already exists so the plan, and the run, only touch
# what is missing. Everything here is read-only.
discover() {
	local out
	step "Checking what already exists"

	out="$(az_cli group exists --name "$RESOURCE_GROUP")"
	if [ "$out" = "true" ]; then
		RG_EXISTS=1
		RG_LOCATION="$(az_cli group show --name "$RESOURCE_GROUP" --query location -o tsv)"
	elif [ "$ROTATE" = 1 ]; then
		die "nothing to rotate: resource group '$RESOURCE_GROUP' does not exist. Run once without --rotate to create everything."
	elif [ -z "$LOCATION" ]; then
		die "--location is required because resource group '$RESOURCE_GROUP' does not exist yet (for example --location eastus)"
	fi
	if [ -z "$LOCATION" ]; then
		LOCATION="$RG_LOCATION"
	fi

	if [ "$RG_EXISTS" = 1 ]; then
		out="$(az_cli storage account list --resource-group "$RESOURCE_GROUP" --query "[?name=='$ACCOUNT'] | length(@)" -o tsv)"
		if [ "$out" = "1" ]; then
			ACCOUNT_EXISTS=1
		fi
	fi

	if [ "$ACCOUNT_EXISTS" = 1 ]; then
		fetch_account_key
		discover_container
		if [ "$ROTATE" = 0 ]; then
			SHARED_KEY="$(az_cli storage account show --name "$ACCOUNT" --resource-group "$RESOURCE_GROUP" \
				--query allowSharedKeyAccess -o tsv)"
			discover_lifecycle
		fi
	elif [ "$ROTATE" = 1 ]; then
		die "nothing to rotate: no storage account '$ACCOUNT' in resource group '$RESOURCE_GROUP'."
	else
		check_account_name
	fi
}

# --- plan -----------------------------------------------------------------------

plan_line() {
	printf '  %-17s %-20s %s\n' "$1" "$2" "$3"
}

show_plan() {
	local sub_name sub_id action
	sub_name="$(az_cli account show --query name -o tsv)"
	sub_id="$(az_cli account show --query id -o tsv)"

	printf '\n%sStradl Azure setup%s\n' "$BOLD" "$RESET"
	plan_line "Subscription" "$sub_name" "$sub_id"

	if [ "$ROTATE" = 1 ]; then
		plan_line "Storage account" "$ACCOUNT" "reuse"
		plan_line "Container" "$CONTAINER" "reuse"
		plan_line "Access policy" "$POLICY_NAME" "move expiry from $(display_time "$POLICY_EXPIRY") to $(display_time "$NEW_EXPIRY")"
		if [ -n "$POLICY_EXPIRY" ] && is_before "$NEW_EXPIRY" "$POLICY_EXPIRY"; then
			warn "the new expiry is earlier than the current one; pass a larger --years to extend it"
		fi
		printf '\n'
		return 0
	fi

	if [ "$RG_EXISTS" = 1 ]; then
		plan_line "Resource group" "$RESOURCE_GROUP" "reuse ($RG_LOCATION)"
	else
		plan_line "Resource group" "$RESOURCE_GROUP" "create in $LOCATION"
	fi

	if [ "$ACCOUNT_EXISTS" = 1 ]; then
		plan_line "Storage account" "$ACCOUNT" "reuse; re-apply HTTPS-only, TLS 1.2, no public access"
		if shared_key_off; then
			plan_line "Shared key access" "" "turn back on: SAS tokens are signed with the account key"
		else
			plan_line "Shared key access" "" "keep on: SAS tokens are signed with the account key"
		fi
	else
		plan_line "Storage account" "$ACCOUNT" "create in $LOCATION: StorageV2, Standard_LRS, HTTPS only"
		plan_line "Shared key access" "" "on: SAS tokens are signed with the account key"
	fi
	plan_line "Data protection" "" "blob versioning, ${RETENTION_DAYS}-day soft delete for blobs and containers"
	case "$LIFECYCLE_STATE" in
	keep)
		if [ -n "$LIFECYCLE_DAYS_FOUND" ]; then
			action="reuse; deletes previous versions after $LIFECYCLE_DAYS_FOUND days"
		else
			action="reuse as it is"
		fi
		;;
	add) action="add to the existing lifecycle policy: delete previous versions after ${VERSION_DAYS} days" ;;
	*) action="create lifecycle policy: delete previous versions after ${VERSION_DAYS} days" ;;
	esac
	plan_line "Version cleanup" "$LIFECYCLE_RULE" "$action"

	if [ "$CONTAINER_EXISTS" = 1 ]; then
		plan_line "Container" "$CONTAINER" "reuse"
	else
		plan_line "Container" "$CONTAINER" "create (private)"
	fi

	if [ "$POLICY_EXISTS" = 0 ]; then
		action="create, expires $(display_time "$NEW_EXPIRY")"
	elif [ -z "$POLICY_EXPIRY" ]; then
		action="reuse; set expiry to $(display_time "$NEW_EXPIRY")"
	elif [ "$(sorted_letters "$POLICY_PERMS")" != "$(sorted_letters "$POLICY_PERMISSIONS")" ]; then
		action="reuse; reset permissions to $POLICY_PERMISSIONS, expires $(display_time "$POLICY_EXPIRY")"
	else
		action="reuse, expires $(display_time "$POLICY_EXPIRY")"
	fi
	plan_line "Access policy" "$POLICY_NAME ($POLICY_PERMISSIONS)" "$action"
	if [ -n "$POLICY_EXPIRY" ] && is_past "$POLICY_EXPIRY"; then
		warn "the $POLICY_NAME policy expired on $(display_time "$POLICY_EXPIRY"); add --rotate to extend it"
	fi
	if shared_key_off; then
		warn "shared key access is off on '$ACCOUNT'. This turns it back on, because the SAS token Stradl uses is signed with the account key and stops working without it. Stop here if your organization turned it off on purpose."
	fi
	printf '\n'
}

# shared_key_off reports whether a reused account has shared key access
# turned off.
shared_key_off() {
	[ "$SHARED_KEY" = "False" ] || [ "$SHARED_KEY" = "false" ]
}

confirm() {
	local reply=""
	if [ "$ASSUME_YES" = 1 ]; then
		return 0
	fi
	# Piped through bash, stdin is this script, so ask on the terminal.
	if ! (: </dev/tty) 2>/dev/null; then
		die "no terminal for the confirmation prompt. Run again with --yes to proceed without it."
	fi
	printf 'Proceed? [y/N] ' >/dev/tty
	IFS= read -r reply </dev/tty || reply=""
	case "$reply" in
	y | Y | yes | YES | Yes) ;;
	*)
		printf 'Stopped. Nothing was changed.\n'
		exit 1
		;;
	esac
}

# --- apply ----------------------------------------------------------------------

# lifecycle_rule_json prints the rule that deletes previous versions of the
# container's blobs. Azure ages a previous version from when it was written,
# not from when it was replaced, and soft delete keeps what the rule deletes
# for another RETENTION_DAYS.
lifecycle_rule_json() {
	printf '{"enabled":true,"name":"%s","type":"Lifecycle","definition":{"actions":{"version":{"delete":{"daysAfterCreationGreaterThan":%d}}},"filters":{"blobTypes":["blockBlob"],"prefixMatch":["%s/"]}}}' \
		"$LIFECYCLE_RULE" "$VERSION_DAYS" "$CONTAINER"
}

# ensure_lifecycle adds the version cleanup rule, leaving any other rules on
# the account alone. A rule that already has this name is kept as it is, so
# a value changed in the portal survives a re-run.
ensure_lifecycle() {
	case "$LIFECYCLE_STATE" in
	keep) ;;
	add)
		step "Adding lifecycle rule $LIFECYCLE_RULE: delete previous versions after ${VERSION_DAYS} days"
		az_cli storage account management-policy update --account-name "$ACCOUNT" \
			--resource-group "$RESOURCE_GROUP" --add policy.rules "$(lifecycle_rule_json)" -o none
		;;
	*)
		step "Creating lifecycle rule $LIFECYCLE_RULE: delete previous versions after ${VERSION_DAYS} days"
		az_cli storage account management-policy create --account-name "$ACCOUNT" \
			--resource-group "$RESOURCE_GROUP" --policy "{\"rules\":[$(lifecycle_rule_json)]}" -o none
		;;
	esac
}

ensure_policy() {
	if [ "$POLICY_EXISTS" = 0 ]; then
		step "Creating stored access policy $POLICY_NAME ($POLICY_PERMISSIONS)"
		az_data storage container policy create --container-name "$CONTAINER" --name "$POLICY_NAME" \
			--permissions "$POLICY_PERMISSIONS" --expiry "$NEW_EXPIRY" -o none
		TOKEN_EXPIRY="$NEW_EXPIRY"
	elif [ "$ROTATE" = 1 ]; then
		step "Moving the $POLICY_NAME policy expiry to $(display_time "$NEW_EXPIRY")"
		az_data storage container policy update --container-name "$CONTAINER" --name "$POLICY_NAME" \
			--expiry "$NEW_EXPIRY" -o none
		TOKEN_EXPIRY="$NEW_EXPIRY"
	elif [ -z "$POLICY_EXPIRY" ]; then
		step "Setting the $POLICY_NAME policy expiry"
		az_data storage container policy update --container-name "$CONTAINER" --name "$POLICY_NAME" \
			--permissions "$POLICY_PERMISSIONS" --expiry "$NEW_EXPIRY" -o none
		TOKEN_EXPIRY="$NEW_EXPIRY"
	elif [ "$(sorted_letters "$POLICY_PERMS")" != "$(sorted_letters "$POLICY_PERMISSIONS")" ]; then
		step "Resetting the $POLICY_NAME policy permissions to $POLICY_PERMISSIONS"
		az_data storage container policy update --container-name "$CONTAINER" --name "$POLICY_NAME" \
			--permissions "$POLICY_PERMISSIONS" -o none
		TOKEN_EXPIRY="$POLICY_EXPIRY"
	else
		TOKEN_EXPIRY="$POLICY_EXPIRY"
	fi
}

apply_changes() {
	if [ "$RG_EXISTS" = 0 ]; then
		step "Creating resource group $RESOURCE_GROUP in $LOCATION"
		az_cli group create --name "$RESOURCE_GROUP" --location "$LOCATION" --tags "$APP_TAG" -o none
	fi

	if [ "$ACCOUNT_EXISTS" = 0 ]; then
		step "Creating storage account $ACCOUNT (this can take a minute)"
		az_cli storage account create --name "$ACCOUNT" --resource-group "$RESOURCE_GROUP" \
			--location "$LOCATION" --kind StorageV2 --sku Standard_LRS --access-tier Hot \
			--https-only true --min-tls-version TLS1_2 \
			--allow-blob-public-access false --allow-shared-key-access true \
			--tags "$APP_TAG" -o none
	elif [ "$ROTATE" = 0 ]; then
		step "Re-applying security settings to storage account $ACCOUNT"
		az_cli storage account update --name "$ACCOUNT" --resource-group "$RESOURCE_GROUP" \
			--https-only true --min-tls-version TLS1_2 \
			--allow-blob-public-access false --allow-shared-key-access true -o none
		if shared_key_off; then
			printf '    Shared key access is back on: Azure signs SAS tokens with the account key, so generating the token needs it.\n'
		else
			printf '    Shared key access stays on: Azure signs SAS tokens with the account key, so generating the token needs it.\n'
		fi
	fi

	if [ "$ROTATE" = 0 ]; then
		step "Turning on blob versioning and ${RETENTION_DAYS}-day soft delete"
		az_cli storage account blob-service-properties update --account-name "$ACCOUNT" \
			--resource-group "$RESOURCE_GROUP" --enable-versioning true \
			--enable-delete-retention true --delete-retention-days "$RETENTION_DAYS" \
			--enable-container-delete-retention true --container-delete-retention-days "$RETENTION_DAYS" \
			-o none
		ensure_lifecycle
	fi

	if [ -z "$ACCOUNT_KEY" ]; then
		fetch_account_key
	fi

	if [ "$CONTAINER_EXISTS" = 0 ]; then
		step "Creating private container $CONTAINER"
		az_data storage container create --name "$CONTAINER" --public-access off -o none
	fi

	ensure_policy

	# Bound to the policy, the token carries no start, expiry or permissions of
	# its own: the policy supplies them, so changing the policy changes every
	# token issued from it.
	step "Generating the SAS token"
	SAS_TOKEN="$(az_data storage container generate-sas --name "$CONTAINER" --policy-name "$POLICY_NAME" --https-only -o tsv)"
	[ -n "$SAS_TOKEN" ] || die "az returned an empty SAS token"
}

# --- summary --------------------------------------------------------------------

# rerun_command prints how to run this script again with the same target.
rerun_command() {
	local cmd flags
	flags="--resource-group $(printf '%q' "$RESOURCE_GROUP") --account $ACCOUNT"
	if [ "$CONTAINER" != "stradl" ]; then
		flags="$flags --container $CONTAINER"
	fi
	if [ -n "$SUBSCRIPTION" ]; then
		flags="$flags --subscription $(printf '%q' "$SUBSCRIPTION")"
	fi
	case "$0" in
	*.sh) cmd="bash $(printf '%q' "$0")" ;;
	*) cmd="curl -fsSL <script url> | bash -s --" ;;
	esac
	printf '%s %s' "$cmd" "$flags"
}

print_summary() {
	local rule="━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
	local thin="────────────────────────────────────────────────────────────────────────"
	local title="Stradl storage is ready"
	if [ "$ROTATE" = 1 ]; then
		title="Token expiry extended"
	fi

	printf '\n%s\n' "$rule"
	printf '  %s%s%s\n' "$BOLD$GREEN" "$title" "$RESET"
	printf '%s\n\n' "$rule"
	printf '  Storage account   %s\n' "$ACCOUNT"
	printf '  Container         %s\n' "$CONTAINER"
	printf '  Token expires     %s  %s(stored access policy "%s")%s\n' \
		"$(display_time "$TOKEN_EXPIRY")" "$DIM" "$POLICY_NAME" "$RESET"
	if [ -n "$TOKEN_EXPIRY" ] && is_past "$TOKEN_EXPIRY"; then
		printf '\n  %sThis policy has expired, so the token below is refused until you\n' "$RED"
		printf '  extend it:%s %s --rotate\n' "$RESET" "$(rerun_command)"
	fi

	printf '\n  %sSAS token%s (shown once; copy it now):\n\n' "$BOLD" "$RESET"
	printf '%s\n\n' "$SAS_TOKEN"
	printf '  %s! Paste this token into your password manager and the Stradl installer,\n' "$YELLOW"
	printf '  ! nowhere else. Anyone holding it can read, overwrite and delete this\n'
	printf '  ! container until it expires or you revoke it. Stradl encrypts every\n'
	printf '  ! session on the Mac before upload, so a leak exposes only ciphertext.%s\n' "$RESET"
	printf '  This script saved neither the token nor the account key. Run clear once\n'
	printf '  the token is safely stored.\n'

	printf '\n%s\n' "$thin"
	if [ "$ROTATE" = 1 ]; then
		printf '  %sOn your Macs%s\n' "$BOLD" "$RESET"
		printf '%s\n' "$thin"
		printf '  Nothing to change. Tokens bound to the policy read their expiry from it,\n'
		printf '  so the token each Mac already has now lasts until the new date. The\n'
		printf '  token above normally matches the one in your password manager.\n'
	else
		printf '  %sNext, on each Mac%s\n' "$BOLD" "$RESET"
		printf '%s\n' "$thin"
		printf '  Run the Stradl installer. When it prompts, enter:\n\n'
		printf '    Storage           Azure Blob Storage\n'
		printf '    Storage account   %s\n' "$ACCOUNT"
		printf '    Container         %s\n' "$CONTAINER"
		printf '    SAS token         paste it from your password manager\n\n'
		printf '  Use the same encryption passphrase on every Mac. Azure can take up to\n'
		printf '  30 seconds to activate a new access policy, so retry a first sync that\n'
		printf '  fails with 403.\n'
	fi

	printf '\n%s\n' "$thin"
	printf '  %sLater%s\n' "$BOLD" "$RESET"
	printf '%s\n' "$thin"
	printf '  Extend the expiry without changing the token:\n'
	printf '    %s --rotate\n' "$(rerun_command)"
	printf '  This moves the policy expiry to now + --years (default 1). Every token\n'
	printf '  bound to the policy picks up the new date, so the Macs need no changes.\n\n'
	printf '  Revoke every token at once by deleting the policy (takes effect within\n'
	printf '  30 seconds):\n'
	printf '    az storage container policy delete --account-name %s --container-name %s --name %s --auth-mode key\n' \
		"$ACCOUNT" "$CONTAINER" "$POLICY_NAME"
	printf '  Before issuing a replacement, renew the account key; a re-created policy\n'
	printf '  with the same name would otherwise accept the old token again:\n'
	printf '    az storage account keys renew --resource-group %s --account-name %s --key primary\n' \
		"$(printf '%q' "$RESOURCE_GROUP")" "$ACCOUNT"
	printf '  Then run this script again for a new token.\n'
	printf '%s\n' "$rule"
}

# --- main -----------------------------------------------------------------------

main() {
	setup_colors
	trap on_error ERR
	parse_args "$@"
	validate_args
	check_prerequisites

	NEW_EXPIRY="$(expiry_after_years "$YEARS")"
	discover
	show_plan
	confirm
	apply_changes
	print_summary

	SAS_TOKEN=""
	ACCOUNT_KEY=""
}

# Called last so bash has read the whole script before anything runs, which
# matters when the script arrives through a pipe.
main "$@"
