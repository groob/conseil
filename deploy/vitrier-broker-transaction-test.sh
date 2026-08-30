#!/usr/bin/env bash
# Run all root-only broker tests from the repository root:
# docker run --rm -e VITRIER_DISPOSABLE_LINUX_CONTAINER=1 -v "$PWD:/src:ro" -w /src golang:1.25-bookworm bash -c 'apt-get update && apt-get install -y openssl util-linux coreutils findutils python3 && go test -race ./cmd/vitrier-broker && ./deploy/vitrier-broker-transaction-test.sh'
set -euo pipefail
umask 077
if (( EUID != 0 )) || [[ ${VITRIER_DISPOSABLE_LINUX_CONTAINER:-} != 1 ]] || [[ $(uname -s) != Linux ]] || [[ ! -f /.dockerenv && ! -f /run/.containerenv ]]; then
  echo "refusing to run outside an explicitly marked disposable root Linux container" >&2
  exit 2
fi
for command in bash find flock openssl shred stat sync; do
  command -v "$command" >/dev/null || { echo "missing test dependency: $command" >&2; exit 2; }
done
source_program=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/vitrier-broker-transaction-v1.sh
readonly source_program
readonly transaction_program=/usr/local/libexec/vitrier-broker-transaction-v1
readonly transaction_parent=/var/lib/vitrier-broker
readonly transaction_dir=$transaction_parent/deploy-v1
readonly staging_dir=$transaction_parent/deploy-v1-staging
readonly lock_file=/run/vitrier-broker/transaction-v1.lock
readonly service_unit=/etc/systemd/system/vitrier-broker.service
readonly broker_binary=/usr/local/bin/vitrier-broker
readonly broker_key=/etc/vitrier-broker/private-key.pem
readonly broker_env=/etc/vitrier-broker.env
for path in /etc/vitrier-broker "$broker_env" "$service_unit" "$broker_binary" "$transaction_parent" /run/vitrier-broker /usr/local/libexec; do
  [[ ! -e $path && ! -L $path ]] || { echo "refusing container with preexisting test path: $path" >&2; exit 2; }
done
work=$(mktemp -d)
pids=()
cleanup() {
  local pid
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf -- /etc/vitrier-broker "$broker_env" "$service_unit" "$broker_binary" "$transaction_parent" /run/vitrier-broker /usr/local/libexec "$work"
}
trap cleanup EXIT
mkdir -p "$work/bin" "$work/systemctl" /etc/systemd/system /usr/local/bin
printf 'inactive\n' > "$work/systemctl/active"
printf 'disabled\n' > "$work/systemctl/enabled"
cat > "$work/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state=${VITRIER_TEST_SYSTEMCTL_STATE:?}
unit=/etc/systemd/system/vitrier-broker.service
case ${1:-} in
show)
  [[ -e $unit || -L $unit ]] && printf 'loaded\n' || printf 'not-found\n'
  ;;
is-active) cat "$state/active" ;;
is-enabled)
  [[ -e $unit || -L $unit ]] && cat "$state/enabled" || printf 'not-found\n'
  ;;
daemon-reload) ;;
enable) printf 'enabled\n' > "$state/enabled" ;;
disable)
  printf 'disabled\n' > "$state/enabled"
  [[ " $* " == *" --now "* ]] && printf 'inactive\n' > "$state/active"
  ;;
restart) printf 'active\n' > "$state/active" ;;
stop) printf 'inactive\n' > "$state/active" ;;
*) echo "unexpected systemctl invocation: $*" >&2; exit 1 ;;
esac
EOF
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${VITRIER_TEST_BLOCK_CURL:-} == 1 ]]; then
  printf 'ready\n' > "${VITRIER_TEST_CURL_READY:?}"
  read -r _ < "${VITRIER_TEST_CURL_RELEASE:?}"
fi
EOF
chmod 0755 "$work/bin/systemctl" "$work/bin/curl"
export PATH="$work/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export VITRIER_TEST_SYSTEMCTL_STATE=$work/systemctl
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/app-key.pem" >/dev/null 2>&1
fail() { echo "transaction test failed: $*" >&2; exit 1; }
assert_absent() { [[ ! -e $1 && ! -L $1 ]] || fail "$1 should be absent"; }
assert_file_text() { [[ -f $1 ]] && [[ $(<"$1") == "$2" ]] || fail "$1 does not contain $2"; }
new_id() { openssl rand -hex 16; }
install_program() {
  install -d -o root -g root -m 0755 /usr/local/libexec
  install -o root -g root -m 0755 "$source_program" "$transaction_program"
}
tx() { "$transaction_program" "$@"; }
cleanup_transaction() {
  local id=$1
  if [[ -x $transaction_program ]]; then
    tx cleanup "$id"
  else
    assert_absent "$transaction_program"
    assert_absent "$transaction_dir"
  fi
}
setup_grant() {
  install -d -o root -g root -m 0755 /etc/vitrier-broker /etc/vitrier-broker/grants
  printf 'conseil\n' > /etc/vitrier-broker/grants/worker
  chown root:root /etc/vitrier-broker/grants/worker
  chmod 0444 /etc/vitrier-broker/grants/worker
}
prepare_candidate() {
  local id=$1
  tx prepare "$id" > "$work/deployment-key.pub"
  cat > "$transaction_dir/artifacts/vitrier-broker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == check && ${2:-} == worker && ${3:-} == conseil && $(</etc/vitrier-broker/grants/worker) == conseil ]]
EOF
  chmod 0755 "$transaction_dir/artifacts/vitrier-broker"
  printf 'candidate unit\n' > "$transaction_dir/artifacts/vitrier-broker.service"
  chmod 0644 "$transaction_dir/artifacts/vitrier-broker.service"
  printf 'VITRIER_APP_ID=1\n' > "$transaction_dir/artifacts/vitrier-broker.env"
  chmod 0600 "$transaction_dir/artifacts/vitrier-broker.env"
  openssl rand 32 > "$work/envelope-key"
  openssl enc -aes-256-cbc -pbkdf2 -salt -in "$work/app-key.pem" -out "$transaction_dir/artifacts/private-key.enc" -pass "file:$work/envelope-key"
  openssl pkeyutl -encrypt -pubin -inkey "$work/deployment-key.pub" -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 -in "$work/envelope-key" -out "$transaction_dir/artifacts/key.enc"
  chmod 0600 "$transaction_dir/artifacts/private-key.enc" "$transaction_dir/artifacts/key.enc"
  shred -u -- "$work/envelope-key"
}
assert_transaction_removed() {
  assert_absent "$transaction_dir"
  assert_absent "$transaction_program"
  assert_absent /usr/local/libexec
}
# Reject a transaction-parent symlink without changing its target.
install_program
mkdir "$work/transaction-parent-target"
chmod 0755 "$work/transaction-parent-target"
ln -s "$work/transaction-parent-target" "$transaction_parent"
if tx prepare "$(new_id)" >/dev/null 2>&1; then fail "prepare accepted a transaction-parent symlink"; fi
[[ $(stat -c %a "$work/transaction-parent-target") == 755 ]] || fail "prepare changed the transaction-parent symlink target"
rm -- "$transaction_parent"

# Recover an unpublished partial staging directory, then verify status and ID ownership.
install -d -o root -g root -m 0700 "$transaction_parent" "$staging_dir"
printf 'partial\n' > "$staging_dir/incomplete"
id=$(new_id)
prepare_candidate "$id"
[[ $(tx status) == "$id prepared" ]] || fail "status did not report the prepared transaction"
wrong_id=$(new_id)
if tx rollback "$wrong_id" >/dev/null 2>&1; then fail "rollback accepted the wrong transaction ID"; fi
[[ $(tx status) == "$id prepared" ]] || fail "wrong-ID rollback mutated transaction state"
tx rollback "$id"
assert_transaction_removed
# First install rollback restores service and file absence without touching the factory grant.
setup_grant
install_program
id=$(new_id)
prepare_candidate "$id"
tx install "$id" worker conseil
[[ -x $broker_binary && -f $broker_key && -f $service_unit && -f $broker_env ]] || fail "first install did not install candidate files"
tx rollback "$id"
for path in "$broker_binary" "$broker_key" "$service_unit" "$broker_env"; do assert_absent "$path"; done
assert_file_text /etc/vitrier-broker/grants/worker conseil
[[ $(cat "$work/systemctl/active") == inactive ]] || fail "first rollback left service active"
assert_transaction_removed
# Upgrade rollback restores old files, directory metadata, and active/enabled state.
printf 'old binary\n' > "$broker_binary"; chmod 0751 "$broker_binary"
printf 'old key\n' > "$broker_key"; chmod 0400 "$broker_key"
printf 'old unit\n' > "$service_unit"; chmod 0640 "$service_unit"
printf 'old env\n' > "$broker_env"; chmod 0600 "$broker_env"
chown 123:456 "$broker_binary" "$broker_key" "$service_unit" "$broker_env" /etc/vitrier-broker
chmod 0710 /etc/vitrier-broker
printf 'active\n' > "$work/systemctl/active"
printf 'enabled\n' > "$work/systemctl/enabled"
install_program
id=$(new_id)
prepare_candidate "$id"
tx install "$id" worker conseil
tx rollback "$id"
assert_file_text "$broker_binary" "old binary"
assert_file_text "$broker_key" "old key"
assert_file_text "$service_unit" "old unit"
assert_file_text "$broker_env" "old env"
[[ $(stat -c '%u:%g:%a' "$broker_binary") == 123:456:751 ]] || fail "binary metadata was not restored"
[[ $(stat -c '%u:%g:%a' "$broker_key") == 123:456:400 ]] || fail "key metadata was not restored"
[[ $(stat -c '%u:%g:%a' "$service_unit") == 123:456:640 ]] || fail "unit metadata was not restored"
[[ $(stat -c '%u:%g:%a' "$broker_env") == 123:456:600 ]] || fail "environment metadata was not restored"
[[ $(stat -c '%u:%g:%a' /etc/vitrier-broker) == 123:456:710 ]] || fail "configuration directory metadata was not restored"
[[ $(cat "$work/systemctl/active") == active && $(cat "$work/systemctl/enabled") == enabled ]] || fail "service state was not restored"
assert_transaction_removed
# A blocked install owns the flock; status starts but completes only after FIFO release.
install_program
id=$(new_id)
prepare_candidate "$id"
mkfifo "$work/curl-ready" "$work/curl-release" "$work/status-started"
export VITRIER_TEST_BLOCK_CURL=1 VITRIER_TEST_CURL_READY=$work/curl-ready VITRIER_TEST_CURL_RELEASE=$work/curl-release
(tx install "$id" worker conseil > "$work/install-output" 2>&1) & install_pid=$!; pids+=("$install_pid")
read -r _ < "$work/curl-ready"
if flock -n "$lock_file" true; then fail "blocked install did not hold the transaction flock"; fi
(printf 'started\n' > "$work/status-started"; tx status > "$work/status-output") & status_pid=$!; pids+=("$status_pid")
read -r _ < "$work/status-started"
if flock -n "$lock_file" true; then fail "transaction flock was released before curl"; fi
printf 'release\n' > "$work/curl-release"
wait "$install_pid"; wait "$status_pid"
unset VITRIER_TEST_BLOCK_CURL VITRIER_TEST_CURL_READY VITRIER_TEST_CURL_RELEASE
[[ $(<"$work/status-output") == "$id installed" ]] || fail "blocked status did not resume after install"
tx rollback "$id"
assert_transaction_removed
# Repeated commit is harmless; cleanup keeps the candidate and removes every transaction artifact.
install_program
id=$(new_id)
prepare_candidate "$id"
tx install "$id" worker conseil
tx commit "$id"
tx commit "$id"
cleanup_transaction "$id"
cleanup_transaction "$id"
[[ -x $broker_binary ]] || fail "committed cleanup removed the candidate binary"
assert_file_text "$service_unit" "candidate unit"
assert_file_text "$broker_env" "VITRIER_APP_ID=1"
assert_transaction_removed
printf 'vitrier broker transaction behavior: PASS\n'
