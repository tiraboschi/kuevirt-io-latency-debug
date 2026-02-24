#!/bin/bash

CMD=$1
INTERVAL_MIN=${2:-5}
DISK_ALIAS=${3:-rootdisk}

VMNAMESPACE="virtual-machines"

VM_NAMES=(
    vmname1
    vmname2
    vmname3
    vmname4
    vmname5
)

if [ -z "$CMD" ]; then
    echo "Usage: $0 <setup|query|collect [interval_minutes]>" >&2
    exit 1
fi

find_pod() {
    local vm_name=$1
    kubectl get pods -n "$VMNAMESPACE" -l "vm.kubevirt.io/name=$vm_name" \
        --field-selector=status.phase=Running \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | head -n1
}

setup_vm() {
    local VM_NAME=$1
    local POD_NAME TARGET_ID

    POD_NAME=$(find_pod "$VM_NAME")
    if [ -z "$POD_NAME" ]; then
        echo "Error: Could not find running pod for VM $VM_NAME" >&2
        return 1
    fi

    TARGET_ID="ua-$DISK_ALIAS/virtio-backend"
    echo "[$VM_NAME] Targeting Pod: $POD_NAME" >&2

    # Boundaries are in nanoseconds
    kubectl exec -n "$VMNAMESPACE" "$POD_NAME" -c compute -- virsh qemu-monitor-command 1 \
        '{"execute": "block-latency-histogram-set", "arguments": {"id": "'$TARGET_ID'", "boundaries": [     1000000,
                                                                                                           10000000,
                                                                                                          100000000,
                                                                                                         1000000000,
                                                                                                        10000000000,
                                                                                                        30000000000,
                                                                                                        60000000000
                                                                                                       ]}}'
}

query_vm() {
    local VM_NAME=$1
    local POD_NAME DEVICE

    POD_NAME=$(find_pod "$VM_NAME")
    if [ -z "$POD_NAME" ]; then
        echo "Error: Could not find running pod for VM $VM_NAME" >&2
        return 1
    fi

    DEVICE="/machine/peripheral/ua-$DISK_ALIAS/virtio-backend"
    echo "[$VM_NAME] Targeting Pod: $POD_NAME" >&2

    kubectl exec -n "$VMNAMESPACE" "$POD_NAME" -c compute -- virsh qemu-monitor-command 1 \
        '{"execute": "query-blockstats"}' | \
        jq --arg qdev "$DEVICE" '.return[] | select(.qdev == $qdev or .device == $qdev)' | \
        jq --arg vm_name "$VM_NAME" --arg timestamp "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" \
        '{vm_name: $vm_name, timestamp: $timestamp} + (reduce (paths(scalars) as $p | select(any($p[]; type == "string" and contains("histo"))) | {path: $p, value: getpath($p)}) as $item ({}; setpath($item.path; $item.value)))'
}

case "$CMD" in
  setup)
    for VM_NAME in "${VM_NAMES[@]}"; do
        setup_vm "$VM_NAME"
    done
    ;;

  query)
    for VM_NAME in "${VM_NAMES[@]}"; do
        sleep $(( RANDOM % 6 ))
        query_vm "$VM_NAME"
    done
    ;;

  collect)
    echo "Collecting every $INTERVAL_MIN minute(s). Press Ctrl+C to stop." >&2
    while true; do
        for VM_NAME in "${VM_NAMES[@]}"; do
            sleep $(( RANDOM % 6 ))
            query_vm "$VM_NAME"
        done
        sleep $(( INTERVAL_MIN * 60 ))
    done
    ;;

  *)
    cat <<EOF
UNKNOWN COMMAND '$CMD'

Usage:
$0 setup|query|collect [interval_minutes] [disk_alias]

  setup                         Enable latency histograms on all VMs
  query                         Query latency histograms once for all VMs
  collect [N] [disk_alias]      Query repeatedly every N minutes (default: 5, disk: rootdisk)
EOF
    exit 1
    ;;
esac