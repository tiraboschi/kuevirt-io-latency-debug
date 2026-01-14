#!/bin/bash

CMD=$1
VM_NAME=$2
DISK_ALIAS=${3:-rootdisk}

if [ -z "$CMD" -o -z "$VM_NAME" ]; then
    echo "Usage: $0 <setup|query> <vm-name>"
    exit 1
fi

# 1. Find the corresponding virt-launcher pod
POD_NAME=$(kubectl get pods -l "vm.kubevirt.io/name=$VM_NAME" -o jsonpath='{.items[0].metadata.name}')

if [ -z "$POD_NAME" ]; then
    echo "Error: Could not find pod for VM $VM_NAME"
    exit 1
fi

DEVICE="/machine/peripheral/ua-$DISK_ALIAS/virtio-backend"
# Note: For the histogram command, we often use the node-name or id. 
# In KubeVirt, 'ua-rootdisk' is typically the id assigned to the backend.
TARGET_ID="ua-$DISK_ALIAS/virtio-backend"

echo "Targeting Pod: $POD_NAME"

case "$CMD" in
  setup)

# 2. Setup block histogram for write operations
# Boundaries are in nanoseconds: 1ms (1000000), 10ms (10000000), 100ms (100000000)
echo "Setting up latency histogram..."
kubectl exec "$POD_NAME" -c compute -- virsh qemu-monitor-command 1 \
    '{"execute": "block-latency-histogram-set", "arguments": {"id": "'$TARGET_ID'", "boundaries": [     1000000,
                                                                                                       10000000,
                                                                                                      100000000,
                                                                                                     1000000000,
                                                                                                    10000000000,
                                                                                                    30000000000,
                                                                                                    60000000000
                                                                                                   ]}}'

  ;;

  query)
# 4. Query blockstats and filter for the specific device using jq
echo "Fetching stats for $DEVICE..."
kubectl exec "$POD_NAME" -c compute -- virsh qemu-monitor-command 1 \
    '{"execute": "query-blockstats"}' | \
    jq --arg qdev "$DEVICE" '.return[] | select(.qdev == $qdev or .device == $qdev)' | jq 'reduce (paths(scalars) as $p | select(any($p[]; type == "string" and contains("histo"))) | {path: $p, value: getpath($p)}) as $item ({}; setpath($item.path; $item.value))'
  ;;

  *)
  cat <<EOF
UNKNOWN COMMAND '$CMD'

Usage:
$0 setup|query <VM NAME>
EOF
esac
