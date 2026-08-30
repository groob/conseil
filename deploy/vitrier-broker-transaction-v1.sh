#!/usr/bin/env bash
set -euo pipefail
umask 077
readonly transaction_version=1
readonly transaction_parent=/var/lib/vitrier-broker
readonly transaction_dir=$transaction_parent/deploy-v1
readonly staging_dir=$transaction_parent/deploy-v1-staging
readonly lock_dir=/run/vitrier-broker
readonly lock_file=$lock_dir/transaction-v1.lock
readonly artifacts_dir=$transaction_dir/artifacts
readonly backup_dir=$transaction_dir/backup
readonly service=vitrier-broker
readonly transaction_program=/usr/local/libexec/vitrier-broker-transaction-v1
transaction_id=
fail() {
  echo "$*" >&2
  return 1
}
require_root() {
  if (( EUID != 0 )); then
    fail "the Vitrier deployment transaction requires root"
  fi
}
acquire_lock() {
  if [[ -e $lock_dir || -L $lock_dir ]]; then
    validate_private_dir "$lock_dir" || fail "invalid Vitrier runtime directory"
  else
    install -d -o root -g root -m 0700 "$lock_dir"
  fi
  if [[ ! -e $lock_file && ! -L $lock_file ]]; then
    (set -o noclobber; : > "$lock_file") 2>/dev/null || true
  fi
  validate_regular_file "$lock_file" 600 || fail "invalid Vitrier transaction lock"
  exec 9<>"$lock_file"
  flock -x 9
}
validate_program() {
  [[ $0 == "$transaction_program" ]] || fail "the transaction program has an unexpected path"
  validate_regular_file "$transaction_program" 755 || fail "the transaction program is unsafe"
}
validate_private_dir() {
  local path=$1
  [[ -d $path && ! -L $path ]] || return 1
  [[ $(stat -c '%u:%g:%a' -- "$path") == 0:0:700 ]]
}
validate_regular_file() {
  local path=$1
  local mode=$2
  [[ -f $path && ! -L $path ]] || return 1
  [[ $(stat -c '%u:%g:%a' -- "$path") == "0:0:$mode" ]]
}
validate_id() {
  [[ $1 =~ ^[0-9a-f]{32}$ ]] || fail "transaction ID must be 128-bit lowercase hexadecimal"
}
write_state() {
  local state=$1
  printf '%s\n' "$state" > "$transaction_dir/.phase.tmp"
  chmod 0600 "$transaction_dir/.phase.tmp"
  mv -f -- "$transaction_dir/.phase.tmp" "$transaction_dir/phase"
  sync -f "$transaction_dir"
}
validate_transaction() {
  validate_private_dir "$transaction_dir" || fail "invalid Vitrier transaction directory"
  validate_private_dir "$artifacts_dir" || fail "invalid Vitrier artifact directory"
  validate_private_dir "$backup_dir" || fail "invalid Vitrier backup directory"
  validate_regular_file "$transaction_dir/version" 600 || fail "invalid Vitrier transaction version file"
  validate_regular_file "$transaction_dir/phase" 600 || fail "invalid Vitrier transaction phase file"
  validate_regular_file "$transaction_dir/id" 600 || fail "invalid Vitrier transaction ID file"
  [[ $(<"$transaction_dir/version") == "$transaction_version" ]] || fail "unsupported Vitrier transaction version"
  [[ $(<"$transaction_dir/id") == "$transaction_id" ]] || fail "Vitrier transaction ID mismatch"
}
phase_is() {
  local want
  local phase
  phase=$(<"$transaction_dir/phase")
  for want in "$@"; do
    [[ $phase == "$want" ]] && return 0
  done
  fail "Vitrier transaction phase is $phase, want $*"
}
validate_staging() {
  validate_private_dir "$staging_dir" || return 1
  validate_private_dir "$staging_dir/artifacts" || return 1
  validate_private_dir "$staging_dir/backup" || return 1
  validate_regular_file "$staging_dir/version" 600 || return 1
  validate_regular_file "$staging_dir/id" 600 || return 1
  validate_regular_file "$staging_dir/phase" 600 || return 1
  [[ $(<"$staging_dir/version") == "$transaction_version" ]] || return 1
  [[ $(<"$staging_dir/phase") == preparing ]] || return 1
  validate_id "$(<"$staging_dir/id")"
}
prepare() {
  if [[ -e $transaction_parent || -L $transaction_parent ]]; then
    validate_private_dir "$transaction_parent" || fail "invalid Vitrier transaction parent"
  else
    install -d -o root -g root -m 0700 "$transaction_parent"
    sync -f "$(dirname -- "$transaction_parent")"
  fi
  if [[ -e $transaction_dir || -L $transaction_dir ]]; then
    fail "retained Vitrier transaction state already exists at $transaction_dir"
  fi
  if [[ -e $staging_dir || -L $staging_dir ]]; then
    validate_private_dir "$staging_dir" || fail "invalid retained Vitrier staging directory"
    rm -rf -- "$staging_dir"
  fi
  mkdir -- "$staging_dir"
  chmod 0700 "$staging_dir"
  mkdir -- "$staging_dir/artifacts" "$staging_dir/backup"
  chmod 0700 "$staging_dir/artifacts" "$staging_dir/backup"
  printf '%s\n' "$transaction_version" > "$staging_dir/version"
  printf '%s\n' "$transaction_id" > "$staging_dir/id"
  printf 'preparing\n' > "$staging_dir/phase"
  chmod 0600 "$staging_dir/version" "$staging_dir/id" "$staging_dir/phase"
  validate_staging || fail "invalid Vitrier staging skeleton"
  mv -- "$staging_dir" "$transaction_dir"
  sync -f "$transaction_parent"
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$artifacts_dir/decryption-key.pem" 2>/dev/null
  chmod 0400 "$artifacts_dir/decryption-key.pem"
  write_state prepared
  openssl pkey -in "$artifacts_dir/decryption-key.pem" -pubout 2>/dev/null
}
put_artifact() {
  local name=${1:-}
  local mode
  validate_transaction
  phase_is prepared
  case $name in
  source.tgz | private-key.enc | key.enc) mode=0600 ;;
  vitrier-broker.service) mode=0644 ;;
  vitrier-broker.env) mode=0600 ;;
  *) fail "invalid Vitrier deployment artifact $name"; return ;;
  esac
  if [[ -e $artifacts_dir/$name || -L $artifacts_dir/$name ]]; then
    fail "Vitrier deployment artifact already exists: $name"
    return
  fi
  cat > "$artifacts_dir/.$name.tmp"
  chmod "$mode" "$artifacts_dir/.$name.tmp"
  mv -- "$artifacts_dir/.$name.tmp" "$artifacts_dir/$name"
  sync -f "$artifacts_dir"
}
build_candidate() {
  validate_transaction
  phase_is prepared
  validate_regular_file "$artifacts_dir/source.tgz" 600 || fail "invalid broker source archive"
  [[ ! -e $artifacts_dir/vitrier-broker && ! -L $artifacts_dir/vitrier-broker ]] || fail "broker candidate already exists"
  local build_dir=$transaction_dir/build
  mkdir -- "$build_dir"
  chmod 0700 "$build_dir"
  tar -tzf "$artifacts_dir/source.tgz" > "$transaction_dir/source-manifest"
  while IFS= read -r name; do
    case $name in
    /* | .. | ../* | */../* | */..) fail "unsafe path in broker source archive: $name"; return ;;
    esac
  done < "$transaction_dir/source-manifest"
  tar --no-same-owner --no-same-permissions -xzf "$artifacts_dir/source.tgz" -C "$build_dir"
  if find "$build_dir" -type l -print -quit | grep -q .; then
    fail "broker source archive contains a symbolic link"
    return
  fi
  (
    cd -- "$build_dir"
    HOME=$build_dir/home GOCACHE=$build_dir/cache CGO_ENABLED=0 go build -trimpath -o "$artifacts_dir/vitrier-broker" .
  )
  chown root:root "$artifacts_dir/vitrier-broker"
  chmod 0755 "$artifacts_dir/vitrier-broker"
  rm -rf -- "$build_dir" "$artifacts_dir/source.tgz"
  sync -f "$artifacts_dir"
}
backup_file() {
  local name=$1
  local path=$2
  if [[ -e $path || -L $path ]]; then
    touch "$backup_dir/present-$name"
    chmod 0600 "$backup_dir/present-$name"
    cp -a -- "$path" "$backup_dir/$name"
  fi
}
capture_directory_state() {
  local name=$1
  local path=$2
  if [[ -e $path || -L $path ]]; then
    [[ -d $path && ! -L $path ]] || fail "deployment directory is unsafe: $path"
    stat -c 'present %u %g %a' -- "$path" > "$backup_dir/directory-$name"
  else
    printf 'absent\n' > "$backup_dir/directory-$name"
  fi
  chmod 0600 "$backup_dir/directory-$name"
}
capture_service_state() {
  local load active enabled
  load=$(systemctl show "$service" --property=LoadState --value 2>/dev/null || true)
  if [[ $load == not-found ]]; then
    active=not-found
    enabled=not-found
  else
    active=$(systemctl is-active "$service" 2>/dev/null || true)
    enabled=$(systemctl is-enabled "$service" 2>/dev/null || true)
    case $active in active | inactive) ;; *) fail "unsupported prior service active state: $active"; return ;; esac
    case $enabled in enabled | disabled | static | indirect | masked | alias) ;; *) fail "unsupported prior service enabled state: $enabled"; return ;; esac
    if [[ $active == active && $enabled == masked ]]; then
      fail "unsupported prior service state: active and masked"
      return
    fi
  fi
  printf '%s\n' "$load" > "$backup_dir/service-load"
  printf '%s\n' "$active" > "$backup_dir/service-active"
  printf '%s\n' "$enabled" > "$backup_dir/service-enabled"
  chmod 0600 "$backup_dir"/service-*
}
decrypt_private_key() {
  validate_regular_file "$artifacts_dir/decryption-key.pem" 400 || fail "invalid deployment decryption key"
  validate_regular_file "$artifacts_dir/key.enc" 600 || fail "invalid encrypted envelope key"
  validate_regular_file "$artifacts_dir/private-key.enc" 600 || fail "invalid encrypted App key"
  openssl pkeyutl -decrypt \
    -inkey "$artifacts_dir/decryption-key.pem" \
    -pkeyopt rsa_padding_mode:oaep \
    -pkeyopt rsa_oaep_md:sha256 \
    -pkeyopt rsa_mgf1_md:sha256 \
    -in "$artifacts_dir/key.enc" \
    -out "$artifacts_dir/aes.key"
  chmod 0400 "$artifacts_dir/aes.key"
  openssl enc -d -aes-256-cbc -pbkdf2 \
    -in "$artifacts_dir/private-key.enc" \
    -out "$artifacts_dir/private-key.pem" \
    -pass "file:$artifacts_dir/aes.key"
  chmod 0400 "$artifacts_dir/private-key.pem"
  openssl pkey -in "$artifacts_dir/private-key.pem" -check -noout >/dev/null
}
install_transaction() {
  local verify_vm=$1
  local repository=$2
  validate_transaction
  phase_is prepared
  validate_regular_file "$artifacts_dir/vitrier-broker" 755 || fail "invalid broker candidate"
  validate_regular_file "$artifacts_dir/vitrier-broker.service" 644 || fail "invalid systemd unit candidate"
  validate_regular_file "$artifacts_dir/vitrier-broker.env" 600 || fail "invalid environment candidate"
  "$artifacts_dir/vitrier-broker" check "$verify_vm" "$repository"
  write_state installing
  decrypt_private_key
  capture_directory_state config /etc/vitrier-broker
  capture_service_state
  backup_file private-key.pem /etc/vitrier-broker/private-key.pem
  backup_file vitrier-broker /usr/local/bin/vitrier-broker
  backup_file vitrier-broker.service /etc/systemd/system/vitrier-broker.service
  backup_file vitrier-broker.env /etc/vitrier-broker.env
  touch "$backup_dir/complete"
  chmod 0600 "$backup_dir/complete"
  sync -f "$backup_dir"
  install -d -o root -g root -m 0755 /etc/vitrier-broker
  install -o root -g root -m 0400 "$artifacts_dir/private-key.pem" /etc/vitrier-broker/private-key.pem
  install -o root -g root -m 0755 "$artifacts_dir/vitrier-broker" /usr/local/bin/vitrier-broker
  install -o root -g root -m 0644 "$artifacts_dir/vitrier-broker.service" /etc/systemd/system/vitrier-broker.service
  install -o root -g root -m 0600 "$artifacts_dir/vitrier-broker.env" /etc/vitrier-broker.env
  systemctl daemon-reload
  touch "$backup_dir/candidate-service-started"
  chmod 0600 "$backup_dir/candidate-service-started"
  sync -f "$backup_dir"
  systemctl enable "$service"
  systemctl restart "$service"
  curl --fail --silent --show-error --connect-timeout 2 --max-time 5 --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:8000/healthz >/dev/null
  write_state installed
}
rollback_step() {
  local description=$1
  shift
  if ! "$@"; then
    echo "rollback failed: $description" >&2
    rollback_failed=true
  fi
}
restore_file() {
  local name=$1
  local path=$2
  local failed=false
  if ! rm -f -- "$path"; then
    failed=true
  fi
  if [[ -f $backup_dir/present-$name ]]; then
    if ! cp -a -- "$backup_dir/$name" "$path"; then
      failed=true
    fi
  fi
  [[ $failed == false ]]
}
restore_directory_state() {
  local name=$1
  local path=$2
  local state uid gid mode
  [[ -f $backup_dir/directory-$name ]] || return 0
  read -r state uid gid mode < "$backup_dir/directory-$name"
  if [[ $state == absent ]]; then
    [[ ! -e $path && ! -L $path ]] || rmdir -- "$path"
    return
  fi
  [[ $state == present && -n $uid && -n $gid && -n $mode ]] || return 1
  chown "$uid:$gid" "$path" || return 1
  chmod "$mode" "$path"
}
restore_service_state() {
  local load active enabled
  load=$(<"$backup_dir/service-load")
  active=$(<"$backup_dir/service-active")
  enabled=$(<"$backup_dir/service-enabled")
  rollback_step "reloading systemd" systemctl daemon-reload
  if [[ $load != not-found ]]; then
    case $enabled in
    enabled) rollback_step "restoring enabled service state" systemctl enable "$service" ;;
    disabled) rollback_step "restoring disabled service state" systemctl disable "$service" ;;
    esac
    case $active in
    active) rollback_step "restoring active service state" systemctl restart "$service" ;;
    inactive) rollback_step "restoring inactive service state" systemctl stop "$service" ;;
    esac
  fi
  local got_load got_active got_enabled
  got_load=$(systemctl show "$service" --property=LoadState --value 2>/dev/null || true)
  if [[ $load == not-found ]]; then
    [[ $got_load == not-found ]] || { echo "rollback failed: service remains present" >&2; rollback_failed=true; }
    return
  fi
  got_active=$(systemctl is-active "$service" 2>/dev/null || true)
  got_enabled=$(systemctl is-enabled "$service" 2>/dev/null || true)
  [[ $got_load == "$load" ]] || { echo "rollback failed: load state is $got_load, want $load" >&2; rollback_failed=true; }
  [[ $got_active == "$active" ]] || { echo "rollback failed: active state is $got_active, want $active" >&2; rollback_failed=true; }
  [[ $got_enabled == "$enabled" ]] || { echo "rollback failed: enabled state is $got_enabled, want $enabled" >&2; rollback_failed=true; }
}
shred_files() {
  local failed=false
  local path
  for path in "$@"; do
    if [[ -e $path || -L $path ]] && ! shred -u -- "$path"; then
      echo "could not securely remove $path" >&2
      failed=true
    fi
  done
  [[ $failed == false ]]
}
remove_new_secrets() {
  shred_files "$artifacts_dir/private-key.pem" "$artifacts_dir/aes.key" "$artifacts_dir/decryption-key.pem"
}
remove_all_transaction_state() {
  shred_files "$backup_dir/private-key.pem" "$artifacts_dir/private-key.pem" "$artifacts_dir/aes.key" "$artifacts_dir/decryption-key.pem" || return 1
  rm -rf -- "$artifacts_dir" "$backup_dir" "$transaction_dir/build" "$transaction_dir/source-manifest" || return 1
  local unexpected
  unexpected=$(find "$transaction_dir" -mindepth 1 -maxdepth 1 ! -name id ! -name phase ! -name version -print -quit)
  [[ -z $unexpected ]] || { echo "unexpected transaction state remains: $unexpected" >&2; return 1; }
  rm -- "$transaction_dir/phase" "$transaction_dir/version" "$transaction_dir/id" || return 1
  rmdir -- "$transaction_dir" || return 1
  rmdir -- "$(dirname -- "$transaction_dir")" 2>/dev/null || true
  rm -f -- "$transaction_program" || return 1
  rmdir -- "$(dirname -- "$transaction_program")" 2>/dev/null || true
}
rollback_transaction() {
  validate_transaction || return 1
  phase_is preparing prepared installing installed rollback-failed rolled-back || return 1
  if phase_is rolled-back 2>/dev/null; then
    remove_all_transaction_state
    return
  fi
  rollback_failed=false
  if [[ -f $backup_dir/complete ]]; then
    if [[ -f $backup_dir/candidate-service-started ]]; then
      rollback_step "stopping candidate service" systemctl disable --now "$service"
    fi
    rollback_step "restoring private key" restore_file private-key.pem /etc/vitrier-broker/private-key.pem
    rollback_step "restoring broker binary" restore_file vitrier-broker /usr/local/bin/vitrier-broker
    rollback_step "restoring systemd unit" restore_file vitrier-broker.service /etc/systemd/system/vitrier-broker.service
    rollback_step "restoring environment file" restore_file vitrier-broker.env /etc/vitrier-broker.env
    restore_service_state
  fi
  rollback_step "restoring broker configuration directory" restore_directory_state config /etc/vitrier-broker
  if [[ $rollback_failed == true ]]; then
    write_state rollback-failed || true
    remove_new_secrets || true
    echo "rollback failed; retaining $transaction_dir" >&2
    return 1
  fi
  write_state rolled-back
  if ! remove_all_transaction_state; then
    echo "rollback succeeded but transaction cleanup failed; retaining $transaction_dir" >&2
    return 1
  fi
}
status_transaction() {
  [[ -e $transaction_dir && ! -L $transaction_dir ]] || fail "no retained Vitrier transaction"
  validate_regular_file "$transaction_dir/id" 600 || fail "invalid Vitrier transaction ID file"
  transaction_id=$(<"$transaction_dir/id")
  validate_id "$transaction_id"
  validate_transaction
  printf '%s %s\n' "$transaction_id" "$(<"$transaction_dir/phase")"
}
commit_transaction() {
  validate_transaction
  phase_is installed committed
  if phase_is installed 2>/dev/null; then
    write_state committed
  fi
}
cleanup_transaction() {
  validate_transaction
  phase_is committed
  if ! remove_all_transaction_state; then
    fail "deployment is committed; retaining cleanup debris at $transaction_dir"
  fi
}
validate_invocation() {
  local operation=$1
  local count=$2
  case $operation in
  prepare | build | rollback | commit | cleanup) (( count == 0 )) ;;
  put) (( count == 1 )) ;;
  install) (( count == 2 )) ;;
  *) fail "usage: $transaction_program OPERATION TRANSACTION_ID [ARGUMENTS]"; return ;;
  esac || fail "usage: $transaction_program OPERATION TRANSACTION_ID [ARGUMENTS]"
}
main() {
  local operation=${1:-}
  if [[ $operation == status ]]; then
    (( $# == 1 )) || fail "usage: $transaction_program status"
    require_root
    acquire_lock
    validate_program
    status_transaction
    return
  fi
  transaction_id=${2:-}
  shift 2 2>/dev/null || true
  validate_id "$transaction_id"
  validate_invocation "$operation" "$#"
  require_root
  acquire_lock
  validate_program
  case $operation in
  prepare) prepare ;;
  put) put_artifact "$1" ;;
  build) build_candidate ;;
  install) install_transaction "$1" "$2" ;;
  rollback) rollback_transaction ;;
  commit) commit_transaction ;;
  cleanup) cleanup_transaction ;;
  esac
}
main "$@"
