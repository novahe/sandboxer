#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","tun","userfaultfd"]
# Snapshot/restore captures root plus ordered writable data-disk state.
# Small shared COW budgets must bound real I/O before and after restore.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker ip mkfs.ext4 python3 timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/runtime" "$WORK/base" "$WORK/snapshot"
TAP_NAME="sd$((BASHPID % 1000000))"
TAP_CREATED=0
PIDS=()
cleanup() {
    local status=$?
    set +e
    for pid in "${PIDS[@]}"; do
        kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
mkdir -p "$WORK/dataset-source"
printf 'DATASET-OK\n' >"$WORK/dataset-source/DATASET-OK"
"$BIN/flatten-ctl" export --no-progress \
    --tmpdir "$WORK/tmp" --output "$WORK/dataset.img" "$WORK/dataset-source"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
DATASET_REF="$(plaintext_tarstream_ref "$WORK/dataset.img")"

truncate -s 512M "$WORK/root.ext4.template"
mkfs.ext4 -q -F -O ^has_journal "$WORK/root.ext4.template"
truncate -s 256M "$WORK/scratch.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/scratch.ext4"
truncate -s 256M "$WORK/dataset.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/dataset.ext4"

cat >"$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB }, diff_cow: { cache_size: 4MiB, max_dirty_size: 2MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: snapshot-disks }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$WORK/root.ext4, diff_template: file://$WORK/root.ext4.template }
  disks:
    - { name: scratch, diff_template: file://$WORK/scratch.ext4 }
    - { name: dataset, base: $DATASET_REF, overlay: { diff_template: file://$WORK/dataset.ext4 } }
mounts:
  - { target: /scratch, type: disk, source: scratch }
  - { target: /data, type: disk, source: dataset }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF

wait_exec() {
    local sid=$1 pid=$2
    for _ in $(seq 1 90); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$WORK/runtime" -- /bin/true >/dev/null 2>&1; then
            return 0
        fi
        kill -0 "$pid" 2>/dev/null || return 1
        sleep 1
    done
    return 1
}

# The cache stats are shared totals repeated by each writable backend, not
# separate per-disk allowances. Do not sum them. Peaks survive final draining.
assert_cache_budget() { # stats JSON, total bytes, dirty bytes
    python3 - "$1" "$2" "$3" <<'PY_STATS'
import json, sys
with open(sys.argv[1], encoding='utf-8') as source:
    report = json.load(source)
capacity, dirty = map(int, sys.argv[2:])
caches = [backend['extra']['diff_cow_cache'] for backend in report['backends']
          if 'diff_cow_cache' in backend.get('extra', {})]
assert len(caches) == 3, caches  # root, scratch and dataset
for cache in caches:
    assert cache['Capacity'] == capacity, cache
    assert cache['MaxDirty'] == dirty, cache
    assert 0 <= cache['Used'] <= capacity, cache
    assert 0 <= cache['DirtyUsed'] <= dirty, cache
    assert cache['PeakUsed'] == capacity, cache  # workload exercised eviction
    assert 0 < cache['PeakDirty'] <= dirty, cache
    assert cache['WrittenBytes'] >= 48 * (1 << 20), cache
    assert cache['WriteBatches'] > 0, cache
# Each backend must report the same shared lifetime counters/budget. Occupancy
# can change between snapshots, so only compare stable final/monotonic fields.
for field in ('Capacity', 'MaxDirty', 'PeakUsed', 'PeakDirty', 'WrittenBytes', 'WriteBatches'):
    assert len({cache[field] for cache in caches}) == 1, (field, caches)
PY_STATS
}

write_cache_payload() {
    "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c '
        set -e
        for dir in / /scratch/ /data/; do
            dd if=/dev/urandom of="${dir}cache-payload" bs=1048576 count=16 conv=fsync 2>/dev/null
        done
        sync
    '
}
cache_payload_hash() {
    "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c '
        set -e
        echo 3 > /proc/sys/vm/drop_caches
        sha256sum /cache-payload /scratch/cache-payload /data/cache-payload
    '
}

SID="snapshot-disks-$BASHPID"
LOG1="$OUT/cold.log"
timeout -k 10s 120 "$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --sandbox-id "$SID" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" --stats-json "$OUT/cold.stats.json" >"$LOG1" 2>&1 &
PID1=$!
PIDS+=("$PID1")
wait_exec "$SID" "$PID1" || {
    tail -80 "$LOG1" >&2 || true
    e2e_fail "multi-disk source sandbox did not become executable"
}
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c \
    'set -e; grep -qx DATASET-OK /data/DATASET-OK; echo ROOT-S-OK > /root-s; echo SCRATCH-S-OK > /scratch/persist; echo DATA-S-OK > /data/persist; sync'

write_cache_payload
cache_payload_hash >"$OUT/cold.sha256"

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --output "$WORK/snapshot" --run-root "$WORK/runtime" >"$OUT/snapshot.log" 2>&1
wait "$PID1" || true
PIDS=()
assert_cache_budget "$OUT/cold.stats.json" $((4 << 20)) $((2 << 20))
SNAPSHOT="$WORK/snapshot/$SID.snapshot"
[ -f "$SNAPSHOT" ] || e2e_fail "multi-disk snapshot is missing"
"$BIN/sandbox-ctl" info --json "$SNAPSHOT" >"$WORK/snapshot.json"
E_NAME="$(python3 - "$WORK/snapshot.json" <<'PY'
import json, os, sys
with open(sys.argv[1], encoding='utf-8') as source:
    print(os.path.basename(json.load(source)['SandboxRef'].split('@', 1)[0]))
PY
)"
E_FILE="$WORK/snapshot/$E_NAME"
[ -f "$E_FILE" ] || e2e_fail "Snapshot S references missing Sandbox E"
"$BIN/sandbox-ctl" info --json "$E_FILE" >"$WORK/sandbox-e.json"
python3 - "$WORK/sandbox-e.json" <<'PY'
import json, sys
with open(sys.argv[1], encoding='utf-8') as source:
    cfg = json.load(source)
assert 'DiffCOW' not in cfg['Resources'], cfg['Resources']
disks = cfg['Boot'].get('Disks') or []
assert [disk.get('Name') for disk in disks] == ['scratch', 'dataset'], disks
root = cfg['Boot']['Root']
scratch, dataset = disks
root_overlay = root.get('Overlay')
dataset_overlay = dataset.get('Overlay')
assert root_overlay and root_overlay.get('Base'), root
assert scratch.get('Base') and not scratch.get('Overlay'), scratch
assert dataset_overlay and dataset_overlay.get('Base'), dataset
PY

# Restore creates active DIFFs from the captured filesystem and capacity.
test ! -e "$WORK/root-restore.ext4"
test ! -e "$WORK/dataset-restore.ext4"
cat >"$WORK/restore.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB }, diff_cow: { cache_size: 8MiB, max_dirty_size: 4MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: snapshot-disks-r }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: { diff: file://$WORK/root-restore.ext4 }
  disks:
    - { name: scratch }
    - { name: dataset, overlay: { diff: file://$WORK/dataset-restore.ext4 } }
EOF

LOG2="$OUT/restore.log"
timeout -k 10s 120 "$BIN/sandbox-ctl" run --restore "$SNAPSHOT" --config "$WORK/restore.yaml" --sandbox-id "$SID" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" --stats-json "$OUT/restore.stats.json" >"$LOG2" 2>&1 &
PID2=$!
PIDS+=("$PID2")
wait_exec "$SID" "$PID2" || {
    tail -80 "$LOG2" >&2 || true
    e2e_fail "multi-disk restore did not become executable"
}
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c \
    'set -e; grep -qx ROOT-S-OK /root-s; grep -qx SCRATCH-S-OK /scratch/persist; grep -qx DATA-S-OK /data/persist; grep -qx DATASET-OK /data/DATASET-OK'

cache_payload_hash >"$OUT/restore.sha256"
cmp "$OUT/cold.sha256" "$OUT/restore.sha256"
# Force writeback and eviction in the new cache too, not just a read-only restore.
write_cache_payload
cache_payload_hash >"$OUT/restore-rewrite.sha256"
if cmp -s "$OUT/cold.sha256" "$OUT/restore-rewrite.sha256"; then
    e2e_fail "restored random overwrite did not change payloads"
fi

kill -TERM "$PID2"
wait "$PID2" || true
PIDS=()
assert_cache_budget "$OUT/restore.stats.json" $((8 << 20)) $((4 << 20))

echo "PASS snapshot.disks.sh"
