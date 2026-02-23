# kubevirt-io-latency-debug

A Prometheus exporter that surfaces QEMU block-device **latency histograms** for
KubeVirt VMIs running on OpenShift. It is designed to be deployed as a
DaemonSet and work at scale — hundreds to thousands of VMs across many nodes —
without placing any extra load on the Kubernetes API server.

---

## Why

KubeVirt's built-in metrics (`kubevirt_vmi_storage_iops_read_total`,
`kubevirt_vmi_storage_read_times_ms_total`, …) expose totals and averages.
When a VM reports slow I/O, averages hide the distribution: a P99 of 500 ms
and a mean of 2 ms are indistinguishable from the outside.

QEMU already supports per-device latency histograms via the QMP commands
`block-latency-histogram-set` and `query-blockstats`. This tool:

1. Arms the histograms once per disk (idempotent, non-destructive).
2. Reads them on every Prometheus scrape.
3. Exposes the data as proper histogram counter families, making
   `histogram_quantile` and rate-based alerting work out of the box.


---

## How it works

```
┌───────────────────────────── OCP Node ──────────────────────────────────┐
│                                                                         │
│  virt-launcher-vm1 (compute)       virt-launcher-vm2 (compute)          │
│    virtqemud + qemu-kvm              virtqemud + qemu-kvm               │
│    /run/libvirt/virtqemud-sock ◄──┐  /run/libvirt/virtqemud-sock  ◄──┐  │
│                                   │                                  │  │
│  ┌────────────────────────────────┼──────────────────────────────────┼─┐│
│  │  kubevirt-io-latency-exporter (DaemonSet pod)                     │ ││
│  │                                                                   │ ││
│  │  1. CRI-O gRPC (/run/crio/crio.sock)                              │ ││
│  │     → list running compute containers + host PIDs                 │ ││
│  │                                                                   │ ││
│  │  2. /proc/<pid>/root/run/libvirt/virtqemud-sock                   │ ││
│  │     → libvirt remote protocol (go-libvirt, pure Go, no CGO)       │ ││
│  │                                                                   │ ││
│  │  3. QMP via virtqemud (QEMUDomainMonitorCommand RPC):             │ ││
│  │     block-latency-histogram-set  (once per disk)                  │ ││
│  │     query-blockstats             (every scrape)                   │ ││
│  │                                                                   │ ││
│  │  4. :9100/metrics  ◄── OCP cluster-monitoring Prometheus          │ ││
│  └───────────────────────────────────────────────────────────────────┘ ││
└─────────────────────────────────────────────────────────────────────────┘
```

In modern KubeVirt, `virtqemud` (the split QEMU daemon) holds the sole QMP
connection to each QEMU process. External clients must proxy QMP commands
through it via the libvirt remote XDR protocol rather than connecting to
QEMU's monitor socket directly.

### Key design decisions

| Problem | Solution |
|---|---|
| `kubectl exec` overloads the API server | CRI-O gRPC socket — no API server involved |
| `virtqemud` holds the sole QMP connection | libvirt remote protocol via `go-libvirt` (pure Go, no CGO) |
| `nsenter` requires a subprocess per VM | `/proc/<pid>/root/` path reaches the container's `virtqemud-sock` from the host |
| Point-in-time snapshots miss transient spikes | Histogram counters accumulate; Prometheus rates show distributions over time |
| Scaling across nodes | One DaemonSet pod per node; each handles only its local VMs |

---

## Metrics

All metrics carry the constant label `node` (from the Downward API) and the
variable labels `namespace`, `vmi`, `drive`, and `operation`.

| Metric | Type | Description |
|---|---|---|
| `kubevirt_vmi_storage_latency_ns_bucket` | counter | Cumulative operation count with latency ≤ `le` (ns) |
| `kubevirt_vmi_storage_latency_ns_count` | counter | Total I/O operation count |
| `kubevirt_vmi_storage_latency_ns_sum` | counter | Total cumulative latency in nanoseconds |

**Label values**

- `operation` — `read`, `write`, or `flush`
- `drive` — disk alias as defined in the VMI spec (e.g. `rootdisk`, `datadisk`)
- `le` — bucket boundary in **nanoseconds**, or `+Inf`

The default bucket boundaries (overridable via `--boundaries`) are:

| `le` value | Human-readable |
|---|---|
| `1000000` | 1 ms |
| `10000000` | 10 ms |
| `100000000` | 100 ms |
| `1000000000` | 1 s |
| `10000000000` | 10 s |
| `30000000000` | 30 s |
| `60000000000` | 60 s |
| `+Inf` | all observations |

> **Units:** all `_bucket`, `_count`, and `_sum` values use nanoseconds.
> `histogram_quantile` therefore returns nanoseconds.  Divide by `1e6` to
> convert to milliseconds in PromQL.

---

## PromQL queries

### Latency percentiles

Results are in **nanoseconds**; divide by `1e6` to get **milliseconds**.

**P99 read latency per VMI — 5-minute window (ms)**
```promql
histogram_quantile(0.99,
  sum(rate(kubevirt_vmi_storage_latency_ns_bucket{operation="read"}[5m]))
  by (le, vmi, namespace)
) / 1e6
```

**P99 across all operations and disks per VMI (ms)**
```promql
histogram_quantile(0.99,
  sum(rate(kubevirt_vmi_storage_latency_ns_bucket[5m]))
  by (le, vmi)
) / 1e6
```

**Compare P50 / P95 / P99 write latency for a single VMI (ms)**
```promql
histogram_quantile(0.50, sum(rate(kubevirt_vmi_storage_latency_ns_bucket{vmi="my-vm", operation="write"}[5m])) by (le)) / 1e6
histogram_quantile(0.95, sum(rate(kubevirt_vmi_storage_latency_ns_bucket{vmi="my-vm", operation="write"}[5m])) by (le)) / 1e6
histogram_quantile(0.99, sum(rate(kubevirt_vmi_storage_latency_ns_bucket{vmi="my-vm", operation="write"}[5m])) by (le)) / 1e6
```

> **Note:** `histogram_quantile` returns NaN when no I/O has occurred in the
> rate window (all bucket counters are zero).  This is expected behaviour —
> it means the VM is idle, not that something is broken.

### Outlier detection

**VMIs whose P99 read latency exceeds 100 ms**
```promql
histogram_quantile(0.99,
  sum(rate(kubevirt_vmi_storage_latency_ns_bucket{operation="read"}[5m]))
  by (le, vmi, namespace)
) / 1e6 > 100
```

**Top 10 VMIs by P95 write latency (ms)**
```promql
topk(10,
  histogram_quantile(0.95,
    sum(rate(kubevirt_vmi_storage_latency_ns_bucket{operation="write"}[5m]))
    by (le, vmi, namespace)
  ) / 1e6
)
```

### Throughput and averages

**Average read latency per VMI (ms)**
```promql
rate(kubevirt_vmi_storage_latency_ns_sum{operation="read"}[5m])
/
rate(kubevirt_vmi_storage_latency_ns_count{operation="read"}[5m])
/ 1e6
```

### SLO-style error budget

**Fraction of reads slower than 10 ms** (1.0 = all reads slow, 0.0 = none)
```promql
1 - (
  rate(kubevirt_vmi_storage_latency_ns_bucket{operation="read", le="10000000"}[5m])
  /
  rate(kubevirt_vmi_storage_latency_ns_bucket{operation="read", le="+Inf"}[5m])
)
```

### Alerting example

```yaml
- alert: VMIHighReadLatencyP99
  expr: |
    histogram_quantile(0.99,
      sum(rate(kubevirt_vmi_storage_latency_ns_bucket{operation="read"}[10m]))
      by (le, vmi, namespace)
    ) / 1e6 > 100
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "P99 read latency > 100ms for VMI {{ $labels.vmi }}"
```

---

## Deploy on OpenShift

### Prerequisites

- OpenShift 4.17+ with KubeVirt / OpenShift Virtualization (CRI-O runtime, cluster-monitoring stack enabled)
- `oc` CLI authenticated with `cluster-admin` or equivalent
- A container registry you can push to

> **Node placement:** The DaemonSet uses `nodeSelector: kubevirt.io/schedulable: "true"`,
> so it runs only on nodes where KubeVirt has enabled VM scheduling. Control-plane and
> infrastructure nodes are automatically excluded.

### 1. Build and push the image

```bash
make image push IMAGE=quay.io/<your-org>/kubevirt-io-latency-exporter TAG=latest
```

Or pull a pre-built image and update `deploy/daemonset.yaml` accordingly.

### 2. Apply the manifests

```bash
# SecurityContextConstraints (requires cluster-admin).
# The SCC already lists the service account in its users field, so no
# separate oc adm policy add-scc-to-user step is needed.
oc apply -f deploy/scc.yaml

# ServiceAccount, ClusterRole, ClusterRoleBinding
oc apply -f deploy/rbac.yaml

# DaemonSet, headless Service, ServiceMonitor
oc apply -f deploy/daemonset.yaml
oc apply -f deploy/service.yaml
oc apply -f deploy/servicemonitor.yaml
```

Or use the Makefile shortcut:
```bash
make deploy IMAGE=quay.io/<your-org>/kubevirt-io-latency-exporter
```

### 3. Verify

```bash
# Pods should be Running only on kubevirt.io/schedulable=true nodes
oc get pods -n openshift-cnv -l app=kubevirt-io-latency-exporter -o wide

# Check a single pod's metrics endpoint via port-forward
# (the ubi-micro base image has no wget/curl)
POD=$(oc get pods -n openshift-cnv -l app=kubevirt-io-latency-exporter -o name | head -1)
oc port-forward -n openshift-cnv $POD 9100:9100 &
curl -s http://localhost:9100/metrics | grep kubevirt_vmi_storage
```

Metrics appear in the OpenShift web console under
**Observe → Metrics** within one scrape interval (default 30 s).
The ServiceMonitor is picked up by the cluster-monitoring Prometheus automatically
because the `openshift-cnv` namespace carries `openshift.io/cluster-monitoring: "true"`.

### Tear down

```bash
make undeploy
```

---

## Configuration

All flags are set via the `args` array in `deploy/daemonset.yaml`.

| Flag | Default | Description |
|---|---|---|
| `--namespaces` | _(all)_ | Comma-separated namespace list to monitor |
| `--label-filter` | _(none)_ | Pod label selector, e.g. `environment=prod` |
| `--boundaries` | `1ms,10ms,…,60s` | Histogram bucket boundaries (ns integers or Go durations) |
| `--poll-interval` | `30s` | How often to reconcile the running VMI set |
| `--port` | `9100` | Port for `/metrics` and `/healthz` |
| `--crio-socket` | `/run/crio/crio.sock` | Path to the CRI-O UNIX socket |

**Scope to specific namespaces:**
```yaml
args:
  - --namespaces=production,staging
  - --poll-interval=30s
  - --port=9100
```

**Scope to a VM label:**
```yaml
args:
  - --label-filter=environment=production
  - --poll-interval=30s
  - --port=9100
```

---

## Development

```bash
# Build the binary locally
make build

# Run tests
go test ./...

# Lint
make lint

# Build and push a development image
make image push IMAGE=quay.io/<your-org>/kubevirt-io-latency-exporter TAG=dev
```
