#!/bin/bash

CMD=$1
INTERVAL_MIN=${2:-5}
DISK_ALIAS=${3:-rootdisk}

VMNAMESPACE="virtual-machines"

# Prefer oc (OpenShift CLI) when available, fall back to kubectl.
if command -v oc &>/dev/null; then
    KUBECTL=oc
else
    KUBECTL=kubectl
fi

# Each entry is either "vmname" (uses DISK_ALIAS as default) or
# "vmname,disk1,disk2,..." to specify one or more disk aliases explicitly.
VM_NAMES=(
    vmname1
    "vmname2,vol-0,vol-1"
    "vmname3,vol-0,vol-1,vol-2"
    vmname4
    vmname5
)

# get_disks <entry>
# Prints the disk aliases for a VM_NAMES entry, one per line.
# Falls back to $DISK_ALIAS when no disks are listed in the entry.
get_disks() {
    local entry=$1
    IFS=',' read -ra parts <<< "$entry"
    if [ "${#parts[@]}" -gt 1 ]; then
        printf '%s\n' "${parts[@]:1}"
    else
        echo "$DISK_ALIAS"
    fi
}

if [ -z "$CMD" ]; then
    echo "Usage: $0 <setup|query|collect [interval_minutes]>" >&2
    exit 1
fi

find_pod() {
    local vm_name=$1
    $KUBECTL get pods -n "$VMNAMESPACE" -l "vm.kubevirt.io/name=$vm_name" \
        --field-selector=status.phase=Running \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | head -n1
}

setup_vm() {
    local VM_NAME=$1
    local DISK=$2
    local POD_NAME TARGET_ID

    POD_NAME=$(find_pod "$VM_NAME")
    if [ -z "$POD_NAME" ]; then
        echo "Error: Could not find running pod for VM $VM_NAME" >&2
        return 1
    fi

    TARGET_ID="ua-$DISK/virtio-backend"
    echo "[$VM_NAME/$DISK] Targeting Pod: $POD_NAME" >&2

    # Boundaries are in nanoseconds
    $KUBECTL exec -n "$VMNAMESPACE" "$POD_NAME" -c compute -- virsh qemu-monitor-command 1 \
        '{"execute": "block-latency-histogram-set", "arguments": {"id": "'"$TARGET_ID"'", "boundaries": [     1000000,
                                                                                                           10000000,
                                                                                                          100000000,
                                                                                                         1000000000,
                                                                                                        10000000000,
                                                                                                        30000000000,
                                                                                                        60000000000
                                                                                                       ]}}'
}

scan_vm() {
    local VM_NAME=$1
    local POD_NAME

    POD_NAME=$(find_pod "$VM_NAME")
    if [ -z "$POD_NAME" ]; then
        echo "Error: Could not find running pod for VM $VM_NAME" >&2
        return 1
    fi

    local disks
    disks=$($KUBECTL exec -n "$VMNAMESPACE" "$POD_NAME" -c compute -- virsh qemu-monitor-command 1 \
        '{"execute": "query-blockstats"}' | \
        jq -r '[.return[] | .qdev // "" | select(startswith("/machine/peripheral/ua-")) | split("/")[3] | ltrimstr("ua-")] | unique | join(" ")')
    echo "[$VM_NAME] disks: $disks"
}

query_vm() {
    local VM_NAME=$1
    local DISK=$2
    local POD_NAME DEVICE

    POD_NAME=$(find_pod "$VM_NAME")
    if [ -z "$POD_NAME" ]; then
        echo "Error: Could not find running pod for VM $VM_NAME" >&2
        return 1
    fi

    DEVICE="/machine/peripheral/ua-$DISK/virtio-backend"
    echo "[$VM_NAME/$DISK] Targeting Pod: $POD_NAME" >&2

    $KUBECTL exec -n "$VMNAMESPACE" "$POD_NAME" -c compute -- virsh qemu-monitor-command 1 \
        '{"execute": "query-blockstats"}' | \
        jq --arg qdev "$DEVICE" '.return[] | select(.qdev == $qdev or .device == $qdev)' | \
        jq --arg vm_name "$VM_NAME" --arg disk_alias "$DISK" --arg timestamp "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" \
        '{vm_name: $vm_name, disk_alias: $disk_alias, timestamp: $timestamp} + (reduce (paths(scalars) as $p | select(any($p[]; type == "string" and contains("histo"))) | {path: $p, value: getpath($p)}) as $item ({}; setpath($item.path; $item.value)))'
}

case "$CMD" in
  scan)
    for VM_ENTRY in "${VM_NAMES[@]}"; do
        IFS=',' read -r VM_NAME _ <<< "$VM_ENTRY"
        scan_vm "$VM_NAME"
    done
    ;;

  setup)
    for VM_ENTRY in "${VM_NAMES[@]}"; do
        IFS=',' read -r VM_NAME _ <<< "$VM_ENTRY"
        while IFS= read -r DISK; do
            setup_vm "$VM_NAME" "$DISK"
        done < <(get_disks "$VM_ENTRY")
    done
    ;;

  query)
    for VM_ENTRY in "${VM_NAMES[@]}"; do
        IFS=',' read -r VM_NAME _ <<< "$VM_ENTRY"
        while IFS= read -r DISK; do
            sleep $(( RANDOM % 6 ))
            query_vm "$VM_NAME" "$DISK"
        done < <(get_disks "$VM_ENTRY")
    done
    ;;

  collect)
    echo "Collecting every $INTERVAL_MIN minute(s). Press Ctrl+C to stop." >&2
    while true; do
        for VM_ENTRY in "${VM_NAMES[@]}"; do
            IFS=',' read -r VM_NAME _ <<< "$VM_ENTRY"
            while IFS= read -r DISK; do
                sleep $(( RANDOM % 6 ))
                query_vm "$VM_NAME" "$DISK"
            done < <(get_disks "$VM_ENTRY")
        done
        sleep $(( INTERVAL_MIN * 60 ))
    done
    ;;

  *)
    cat <<EOF
UNKNOWN COMMAND '$CMD'

Usage:
$0 scan|setup|query|collect [interval_minutes] [disk_alias]

  scan                          List all block devices found on each VM
  setup                         Enable latency histograms on all VMs
  query                         Query latency histograms once for all VMs
  collect [N] [disk_alias]      Query repeatedly every N minutes (default: 5, disk: rootdisk)
EOF
    exit 1
    ;;
esac