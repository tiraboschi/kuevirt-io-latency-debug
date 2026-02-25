# KubeVirt I/O Latency Debug

Tools for collecting and analysing block I/O latency histograms from KubeVirt VMIs via the QEMU Monitor Protocol (QMP).

## Overview

`kubevirt-qmp-latency-histogram.sh` enables latency histograms on running VMIs and periodically queries them.
`convert.py` converts the collected JSON into OpenMetrics/Prometheus format for import into VictoriaMetrics.

Metrics produced:

| Metric | Type | Description |
|--------|------|-------------|
| `kubevirt_vmi_storage_latency_ns_bucket` | counter | Cumulative op count with latency ≤ le (ns) |
| `kubevirt_vmi_storage_latency_ns_count`  | counter | Total I/O operation count |
| `kubevirt_vmi_storage_latency_ns_sum`    | counter | Total latency in ns (estimated from bucket midpoints) |

Labels: `vm`, `drive`, `operation` (`read`\|`write`\|`flush`), `le` (bucket boundary in ns, or `+Inf`).

---

## 1. Collect data from the cluster

Edit `VM_NAMES` and `VMNAMESPACE` in `kubevirt-qmp-latency-histogram.sh`, then:

```bash
# One-time setup: enable histograms on all VMs (run once per VM lifecycle)
./kubevirt-qmp-latency-histogram.sh setup

# Query once
./kubevirt-qmp-latency-histogram.sh query >> output.json

# Or collect continuously every 5 minutes
./kubevirt-qmp-latency-histogram.sh collect 5 >> output.json

# For a non-default disk alias (e.g. datadisk)
./kubevirt-qmp-latency-histogram.sh collect 5 datadisk >> output.json
```

---

## 2. Convert to OpenMetrics format

Requires Python 3 and `python-dateutil`:

```bash
pip install python-dateutil   # or: dnf install python3-dateutil
```

```bash
# Default drive label is "rootdisk"
python3 convert.py output.json -o import_data.txt

# Override the drive label
python3 convert.py output.json --drive datadisk -o import_data.txt

# Pipe directly into curl
python3 convert.py output.json | \
    curl -X POST http://localhost:8428/api/v1/import/prometheus --data-binary @-
```

---

## 3. Start VictoriaMetrics + Grafana

Requires `podman` and `podman-compose`:

```bash
dnf install podman podman-compose   # if not already installed

podman-compose -f podman-compose.yaml up -d
```

Check both containers are running:

```bash
podman-compose -f podman-compose.yaml ps
```

Stop the stack:

```bash
podman-compose -f podman-compose.yaml down
```

---

## 4. Import data into VictoriaMetrics

```bash
curl -X POST http://localhost:8428/api/v1/import/prometheus \
     --data-binary @import_data.txt
```

Verify the import succeeded:

```bash
curl -s 'http://localhost:8428/api/v1/export?match=kubevirt_vmi_storage_latency_ns_count' | head -5
```

> **Note:** do not use `/api/v1/query` for verification. That endpoint performs an instant query
> at "now" with a 5-minute lookback window, so it will return empty results for historical data
> even when the import was successful. `/api/v1/export` returns all stored samples regardless
> of timestamp.

You can re-import at any time; VictoriaMetrics deduplicates on `(metric, timestamp)`.

---

## 5. Connect Grafana to VictoriaMetrics

The VictoriaMetrics datasource is provisioned automatically via
`grafana-provisioning/datasources/victoriametrics.yaml` — no manual setup required.

Open Grafana at <http://localhost:3000> (anonymous access is pre-enabled) and both the
**VictoriaMetrics** datasource and the **KubeVirt VMI Storage Latency** dashboard will
already be configured.

The dashboard includes `VM` and `Operation` drop-down filters at the top and the following
panels:

| Panel | Description |
|-------|-------------|
| IOPS by VM and Operation | ops/s rate per VM and operation type |
| P99 Write Latency by VM | 99th percentile write latency (ms) |
| Worst-case Write Latency by VM | Upper bound of the highest non-empty bucket (ms) |
| Latency Percentiles | P50 / P95 / P99 for the selected VMs and operations (ms) |
| Average Latency | Estimated mean latency from bucket midpoints (ms) |
| Fraction of Writes < 1 ms | Proportion of writes completing within 1 ms |
| Write Latency Heatmap | Bucket distribution over time |

> **Important:** if you imported historical data, set the Grafana time picker (top-right) to
> the time range covered by the collected data. The default "Last 1 hour" / "Last 6 hours"
> views will appear empty because the data timestamps fall outside that window.

---

## 6. Useful queries

> For ad-hoc exploration in Grafana's Explore view. All queries use `[5m]` rate windows —
> adjust to match your collection interval.

### Operation count rate (IOPS by operation type)

```promql
sum by (vm, operation) (
  rate(kubevirt_vmi_storage_latency_ns_count[5m])
)
```

### Average latency per VM and operation (ms)

> `_sum` is estimated from bucket midpoints, so this is approximate.

```promql
sum by (vm, operation) (
  rate(kubevirt_vmi_storage_latency_ns_sum[5m])
)
/
sum by (vm, operation) (
  rate(kubevirt_vmi_storage_latency_ns_count[5m])
)
/ 1e6
```

### P50 / P95 / P99 latency per VM and operation (ms)

```promql
histogram_quantile(0.99,
  sum by (vm, operation, le) (
    rate(kubevirt_vmi_storage_latency_ns_bucket[5m])
  )
) / 1e6
```

Change `0.99` to `0.95` or `0.50` for other percentiles.

### P99 write latency — one curve per VM

```promql
histogram_quantile(0.99,
  sum by (vm, le) (
    rate(kubevirt_vmi_storage_latency_ns_bucket{operation="write"}[5m])
  )
) / 1e6
```

### Highest (worst-case) write latency per VM

`histogram_quantile(1.0, ...)` returns the upper bound of the highest non-empty bucket, giving
the worst observed latency class per VM. It returns `+Inf` if any operation landed in the
overflow bucket (i.e. latency > 60 s with the default boundaries).

```promql
histogram_quantile(1.0,
  sum by (vm, le) (
    rate(kubevirt_vmi_storage_latency_ns_bucket{operation="write"}[5m])
  )
) / 1e6
```

### Fraction of writes completing under 1 ms

```promql
sum by (vm) (
  rate(kubevirt_vmi_storage_latency_ns_bucket{operation="write", le="1000000"}[5m])
)
/
sum by (vm) (
  rate(kubevirt_vmi_storage_latency_ns_count{operation="write"}[5m])
)
```

### Latency heatmap (for Grafana Heatmap panel)

Use this as the query and set the panel type to **Heatmap**:

```promql
sum by (le) (
  rate(kubevirt_vmi_storage_latency_ns_bucket{vm="$vm", operation="$operation"}[5m])
)
```

Create Grafana variables `$vm` and `$operation` from label values to make the panel interactive.

---

## Notes

- **Histogram quantiles require at least two data points** at different timestamps. A single snapshot produces a flat line; continuous collection (`collect` mode) gives meaningful quantile graphs.
- **`_sum` is an approximation.** QMP does not expose the exact sum of per-operation latencies; the converter uses bucket midpoints as an estimate. `histogram_quantile` is unaffected — it only uses `_bucket`.
- **Timestamps.** The converter preserves the original collection timestamp, so historical data imported in bulk will appear at the correct points on the Grafana timeline. Instant queries (`/api/v1/query` without an explicit `time=` parameter, or Grafana's default "now") use a 5-minute lookback and will return no results for historical data — always use a time range that covers the collection period.