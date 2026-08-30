#!/usr/bin/env bash
set -euo pipefail
umask 077
readonly broker_vm=groob-tools
readonly integration_name=vitrier
readonly organization=maintenancewindows
readonly remote_transaction=/usr/local/libexec/vitrier-broker-transaction-v1
readonly remote_transaction_lock=/run/vitrier-broker/transaction-v1.lock
if (( $# != 2 )); then
  echo "usage: $0 VERIFY_VM REPOSITORY" >&2
  exit 2
fi
verify_vm=$1
repository=$2
private_key_file=${VITRIER_PRIVATE_KEY_FILE:-"$HOME/maintenance-windows/.secrets/vitrier-priv.pem"}
app_id=${VITRIER_APP_ID:-4691351}
if [[ ! $verify_vm =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
  echo "invalid name: $verify_vm" >&2
  exit 2
fi
if (( ${#repository} == 0 || ${#repository} > 100 )) || [[ $repository == . || $repository == .. || ! $repository =~ ^[A-Za-z0-9_.-]+$ ]]; then
  echo "invalid GitHub repository name: $repository" >&2
  exit 2
fi
if [[ ! $app_id =~ ^[1-9][0-9]*$ ]]; then
  echo "VITRIER_APP_ID must be a positive integer" >&2
  exit 2
fi
if [[ ! -r $private_key_file ]]; then
  echo "cannot read Vitrier private key: $private_key_file" >&2
  exit 1
fi
for command in openssl python3 shred ssh tar; do
  if ! command -v "$command" >/dev/null; then
    echo "required command is not installed: $command" >&2
    exit 1
  fi
done
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/.." && pwd)
work_dir=$(mktemp -d)
transaction_id=$(openssl rand -hex 16)
transaction_owned=false
peer_verified=false
program_maybe_owned=false
lobby() {
  # The lobby command arguments are intentionally assembled on this host.
  # shellcheck disable=SC2029
  ssh exe.dev "$@"
}
broker() {
  lobby ssh "$broker_vm" "$@"
}
transaction_for() {
  local id=$1
  local operation=$2
  shift 2
  broker sudo "$remote_transaction" "$operation" "$id" "$@"
}
transaction() {
  local operation=$1
  shift
  transaction_for "$transaction_id" "$operation" "$@"
}
transaction_status() {
  broker sudo "$remote_transaction" status
}
transaction_cleanup_for() {
  local id=$1
  if transaction_for "$id" cleanup; then
    return
  fi
  broker "sudo test ! -e '$remote_transaction' && sudo test ! -e /var/lib/vitrier-broker/deploy-v1"
}
transaction_cleanup() {
  transaction_cleanup_for "$transaction_id"
}
install_transaction_program() {
  local nonce remote_temp
  nonce=$(openssl rand -hex 16)
  remote_temp=/usr/local/libexec/.vitrier-broker-transaction-v1-$nonce
  program_maybe_owned=true
  broker "set -e; trap 'sudo rm -f -- $remote_temp' EXIT; sudo install -d -o root -g root -m 0755 /usr/local/libexec; sudo tee '$remote_temp' >/dev/null; sudo chown root:root '$remote_temp'; sudo chmod 0755 '$remote_temp'; if sudo ln '$remote_temp' '$remote_transaction' 2>/dev/null || sudo cmp -s '$remote_temp' '$remote_transaction'; then exit 0; fi; echo 'installed transaction v1 differs from this deployment' >&2; exit 1" < "$script_dir/vitrier-broker-transaction-v1.sh"
}
remove_unused_program() {
  local output
  if output=$(transaction_status 2>&1); then
    return 1
  fi
  [[ $output == *"no retained Vitrier transaction"* ]] || return 1
  broker "sudo flock -x '$remote_transaction_lock' sh -c 'if test -e /var/lib/vitrier-broker/deploy-v1 || test -e /var/lib/vitrier-broker/deploy-v1-staging; then exit 1; fi; rm -f -- $remote_transaction || exit 1; rmdir /usr/local/libexec 2>/dev/null || true'"
}
cleanup() {
  local status=$?
  local rollback_failed=false
  if [[ -f $work_dir/encryption-key ]]; then
    shred -u -- "$work_dir/encryption-key" 2>/dev/null || rollback_failed=true
  fi
  if [[ $transaction_owned == true ]]; then
    if [[ $peer_verified == true ]]; then
      if transaction commit; then
        if transaction_cleanup; then
          transaction_owned=false
        else
          rollback_failed=true
        fi
      else
        rollback_failed=true
      fi
    else
      echo "deployment did not pass peer verification; restoring broker state and preserving integration state" >&2
      if transaction rollback; then
        transaction_owned=false
      else
        rollback_failed=true
      fi
    fi
  fi
  if [[ $transaction_owned == false && $program_maybe_owned == true ]]; then
    remove_unused_program || true
  fi
  rm -rf -- "$work_dir"
  if [[ $rollback_failed == true ]]; then
    status=1
  fi
  trap - EXIT
  exit "$status"
}
trap cleanup EXIT
if ! broker 'command -v cmp >/dev/null && command -v curl >/dev/null && command -v flock >/dev/null && command -v go >/dev/null && command -v openssl >/dev/null && command -v shred >/dev/null && command -v systemctl >/dev/null'; then
  echo "$broker_vm lacks a deployment dependency" >&2
  exit 1
fi
lobby share set-private "$broker_vm" >/dev/null
install_transaction_program
set +e
retained=$(transaction_status 2>&1)
retained_status=$?
set -e
if (( retained_status == 0 )); then
  read -r retained_id retained_phase extra <<< "$retained"
  if [[ ! $retained_id =~ ^[0-9a-f]{32}$ || -z $retained_phase || -n ${extra:-} ]]; then
    echo "invalid retained transaction status: $retained" >&2
    exit 1
  fi
  if [[ $retained_phase == committed ]]; then
    echo "cleaning committed Vitrier transaction $retained_id" >&2
    if ! transaction_cleanup_for "$retained_id"; then
      echo "recover with: ssh exe.dev ssh $broker_vm sudo $remote_transaction cleanup $retained_id" >&2
      exit 1
    fi
    program_maybe_owned=false
    install_transaction_program
  else
    echo "refusing new deployment: retained transaction $retained_id is $retained_phase" >&2
    echo "recover with: ssh exe.dev ssh $broker_vm sudo $remote_transaction rollback $retained_id" >&2
    exit 1
  fi
elif [[ $retained != *"no retained Vitrier transaction"* ]]; then
  printf '%s\n' "$retained" >&2
  exit 1
fi
printf 'Vitrier transaction ID: %s\n' "$transaction_id"
transaction_owned=true
if ! transaction prepare > "$work_dir/deployment-key.pub"; then
  exit 1
fi
if ! openssl pkey -pubin -in "$work_dir/deployment-key.pub" -noout >/dev/null 2>&1; then
  echo "broker returned an invalid deployment key" >&2
  exit 1
fi
mkdir "$work_dir/source"
find "$repo_root/cmd/vitrier-broker" -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' -exec cp {} "$work_dir/source/" \;
printf 'module vitrier-broker\n\ngo 1.25.0\n' > "$work_dir/source/go.mod"
COPYFILE_DISABLE=1 tar -czf "$work_dir/source.tgz" -C "$work_dir/source" .
transaction put source.tgz < "$work_dir/source.tgz"
echo "building the broker on $broker_vm"
transaction build
openssl rand 32 > "$work_dir/encryption-key"
openssl enc -aes-256-cbc -pbkdf2 -salt \
  -in "$private_key_file" \
  -out "$work_dir/private-key.enc" \
  -pass "file:$work_dir/encryption-key"
openssl pkeyutl -encrypt \
  -pubin \
  -inkey "$work_dir/deployment-key.pub" \
  -pkeyopt rsa_padding_mode:oaep \
  -pkeyopt rsa_oaep_md:sha256 \
  -pkeyopt rsa_mgf1_md:sha256 \
  -in "$work_dir/encryption-key" \
  -out "$work_dir/key.enc"
cat > "$work_dir/vitrier-broker.env" <<EOF
VITRIER_APP_ID=$app_id
EOF
transaction put private-key.enc < "$work_dir/private-key.enc"
transaction put key.enc < "$work_dir/key.enc"
transaction put vitrier-broker.service < "$script_dir/vitrier-broker.service"
transaction put vitrier-broker.env < "$work_dir/vitrier-broker.env"
shred -u -- "$work_dir/encryption-key"
echo "installing the broker"
if ! transaction install "$verify_vm" "$repository"; then
  exit 1
fi
integration_list=$(lobby integrations list --json)
integration_state=$(printf '%s' "$integration_list" | python3 "$script_dir/vitrier-integration-state.py" "vm:$verify_vm")
if [[ $integration_state == missing ]]; then
  if lobby integrations add http-proxy \
    --name="$integration_name" \
    --target="https://${broker_vm}.exe.xyz/" \
    --peer >/dev/null; then
    integration_state=detached
  else
    integration_list=$(lobby integrations list --json)
    integration_state=$(printf '%s' "$integration_list" | python3 "$script_dir/vitrier-integration-state.py" "vm:$verify_vm")
    if [[ $integration_state == missing ]]; then
      echo "integration creation failed and exact integration is absent" >&2
      exit 1
    fi
  fi
fi
if [[ $integration_state == detached ]]; then
  if ! lobby integrations attach "$integration_name" "vm:$verify_vm" >/dev/null; then
    integration_list=$(lobby integrations list --json)
    integration_state=$(printf '%s' "$integration_list" | python3 "$script_dir/vitrier-integration-state.py" "vm:$verify_vm")
    if [[ $integration_state != attached ]]; then
      echo "integration attachment failed and exact attachment is absent" >&2
      exit 1
    fi
  fi
elif [[ $integration_state != attached ]]; then
  echo "invalid integration attachment state: $integration_state" >&2
  exit 1
fi
cat > "$work_dir/verify.py" <<PY
import json
import urllib.request
request = urllib.request.Request("https://${integration_name}.int.exe.xyz/token", method="POST")
with urllib.request.urlopen(request, timeout=30) as response:
    minted = json.load(response)
token = minted["token"]
request = urllib.request.Request(
    "https://api.github.com/repos/${organization}/${repository}",
    headers={
        "Accept": "application/vnd.github+json",
        "Authorization": "Bearer " + token,
        "User-Agent": "vitrier-broker-verification",
        "X-GitHub-Api-Version": "2022-11-28",
    },
)
with urllib.request.urlopen(request, timeout=30) as response:
    result = json.load(response)
if result["full_name"] != "${organization}/${repository}":
    raise SystemExit("GitHub returned the wrong repository")
print("verified ${organization}/${repository} access")
PY
echo "verifying the broker from $verify_vm"
lobby ssh "$verify_vm" python3 - < "$work_dir/verify.py"
# Peer verification is the commit point. No later control-plane or cleanup
# failure may roll back the verified broker or its integration.
peer_verified=true
if ! transaction commit; then
  echo "deployment is active and verified, but its commit record was not confirmed on $broker_vm" >&2
  exit 1
fi
if ! transaction_cleanup; then
  echo "deployment is active and verified, but transaction cleanup failed on $broker_vm" >&2
  exit 1
fi
transaction_owned=false
echo "deployed the broker on $broker_vm and attached $integration_name to $verify_vm"
