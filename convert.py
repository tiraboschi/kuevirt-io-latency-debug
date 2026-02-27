#!/usr/bin/env python3
"""Convert QMP block latency histogram JSON to OpenMetrics/Prometheus format.

Input: NDJSON stream (concatenated JSON objects) as produced by
       kubevirt-qmp-latency-histogram.sh query|collect

Output: OpenMetrics text format suitable for import into VictoriaMetrics via
        /api/v1/import/prometheus

Metrics emitted:
  kubevirt_vmi_storage_latency_ns_bucket  - cumulative op count with latency <= le (ns)
  kubevirt_vmi_storage_latency_ns_count   - total I/O operation count
  kubevirt_vmi_storage_latency_ns_sum     - approximate total latency (ns), estimated
                                            from bucket midpoints (QMP does not expose
                                            per-operation latency totals)

Labels: vm, drive, operation (read|write|flush), le (bucket boundary or +Inf)

The 'drive' label value is taken from the 'disk_alias' field embedded in each
JSON record (emitted by kubevirt-qmp-latency-histogram.sh). The --drive flag
acts as a fallback for records that pre-date that field.
"""

import argparse
import json
import sys

from dateutil import parser as dateutil_parser

METRIC_BUCKET = "kubevirt_vmi_storage_latency_ns_bucket"
METRIC_COUNT = "kubevirt_vmi_storage_latency_ns_count"
METRIC_SUM = "kubevirt_vmi_storage_latency_ns_sum"

# Map QMP histogram keys to Prometheus operation label values
HIST_MAP = {
    "rd_latency_histogram": "read",
    "wr_latency_histogram": "write",
    "flush_latency_histogram": "flush",
}


def emit_headers():
    return [
        f"# HELP {METRIC_BUCKET} Cumulative operation count with latency <= le (ns)",
        f"# TYPE {METRIC_BUCKET} counter",
        f"# HELP {METRIC_COUNT} Total I/O operation count",
        f"# TYPE {METRIC_COUNT} counter",
        f"# HELP {METRIC_SUM} Total cumulative latency in nanoseconds (estimated from bucket midpoints)",
        f"# TYPE {METRIC_SUM} counter",
    ]


def convert_record(data, default_drive):
    """Convert a single QMP record to OpenMetrics lines."""
    lines = []
    vm = data["vm_name"]
    drive = data.get("disk_alias", default_drive)
    ts = f"{dateutil_parser.isoparse(data['timestamp']).timestamp():.3f}"
    stats = data.get("stats", {})

    for hist_key, operation in HIST_MAP.items():
        hist = stats.get(hist_key)
        if hist is None:
            continue

        bins = hist["bins"]
        boundaries = hist["boundaries"]
        # bins has len(boundaries)+1 elements; bins[i] counts ops in (boundaries[i-1], boundaries[i]]
        # bins[-1] counts ops above the last boundary (the +Inf overflow bucket)

        base_labels = f'vm="{vm}",drive="{drive}",operation="{operation}"'

        cumulative = 0
        approx_sum = 0.0
        prev = 0

        for i, boundary in enumerate(boundaries):
            count = bins[i]
            cumulative += count
            # Approximate sum: use midpoint of (prev, boundary] for each op in this bucket
            approx_sum += count * (prev + boundary) / 2
            prev = boundary

            le_labels = f'{base_labels},le="{boundary}"'
            lines.append(f"{METRIC_BUCKET}{{{le_labels}}} {cumulative} {ts}")

        # Overflow bin: latency > last boundary; use last boundary as lower-bound estimate
        overflow = bins[len(boundaries)]
        approx_sum += overflow * boundaries[-1]
        cumulative += overflow

        lines.append(f"{METRIC_BUCKET}{{{base_labels},le=\"+Inf\"}} {cumulative} {ts}")
        lines.append(f"{METRIC_COUNT}{{{base_labels}}} {cumulative} {ts}")
        lines.append(f"{METRIC_SUM}{{{base_labels}}} {approx_sum:.0f} {ts}")

    return lines


def parse_ndjson(text):
    """Yield JSON objects from a stream of concatenated JSON objects."""
    decoder = json.JSONDecoder()
    pos = 0
    text = text.strip()
    while pos < len(text):
        while pos < len(text) and text[pos] in " \t\n\r":
            pos += 1
        if pos >= len(text):
            break
        obj, end = decoder.raw_decode(text, pos)
        yield obj
        pos = end


def main():
    ap = argparse.ArgumentParser(
        description="Convert QMP block latency histogram JSON to OpenMetrics format",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    ap.add_argument(
        "input",
        nargs="?",
        default="-",
        help="Input file (NDJSON). Use '-' or omit for stdin. (default: stdin)",
    )
    ap.add_argument(
        "-o", "--output",
        default="-",
        help="Output file. Use '-' for stdout. (default: stdout)",
    )
    ap.add_argument(
        "--drive",
        default="rootdisk",
        help="Fallback drive alias for the 'drive' label when a record does not "
             "contain a 'disk_alias' field (default: rootdisk)",
    )
    args = ap.parse_args()

    if args.input == "-":
        text = sys.stdin.read()
    else:
        with open(args.input) as f:
            text = f.read()

    lines = emit_headers()
    for record in parse_ndjson(text):
        lines.extend(convert_record(record, args.drive))  # args.drive is the fallback
    lines.append("# EOF")

    output = "\n".join(lines) + "\n"

    if args.output == "-":
        sys.stdout.write(output)
    else:
        with open(args.output, "w") as f:
            f.write(output)


if __name__ == "__main__":
    main()