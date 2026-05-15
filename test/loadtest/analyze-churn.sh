#!/usr/bin/env bash
# Analyze pod churn test results and calculate reconciliation latency
set -euo pipefail

LOG_DIR="${1:?Usage: $0 <log_dir>}"
export KUBECONFIG="${KUBECONFIG:-/home/asn/DevOps/asnStripe-eastus2euap-kubeconfig.yaml}"

if [ ! -f "$LOG_DIR/events.csv" ]; then
    echo "ERROR: $LOG_DIR/events.csv not found"
    exit 1
fi

export CHURN_LOG_DIR="$LOG_DIR"
python3 << 'PYEOF'
import csv
import json
import sys
import os
from datetime import datetime, timezone, timedelta
from collections import defaultdict

log_dir = os.environ['CHURN_LOG_DIR']

def parse_ts(s):
    """Parse ISO timestamp, tolerant of trailing Z and fractional seconds."""
    s = s.strip()
    if s.endswith('Z'):
        s = s[:-1] + '+00:00'
    return datetime.fromisoformat(s)

# ── Parse churn events ─────────────────────────────────────────────
creates = []   # (timestamp, name, role)
deletes = []   # (timestamp, name)
create_fails = 0

with open(os.path.join(log_dir, 'events.csv'), 'r') as f:
    reader = csv.reader(f)
    next(reader)  # header
    for row in reader:
        if len(row) < 3:
            continue
        ts_str, op, name = row[0], row[1], row[2]
        role = row[3] if len(row) > 3 else ''
        try:
            ts = parse_ts(ts_str)
        except Exception:
            continue
        if op == 'CREATE':
            creates.append((ts, name, role))
        elif op == 'DELETE':
            deletes.append((ts, name))
        elif op == 'CREATE_FAIL':
            create_fails += 1

all_event_times = [c[0] for c in creates] + [d[0] for d in deletes]
if not all_event_times:
    print("No events found.")
    sys.exit(1)

duration = (max(all_event_times) - min(all_event_times)).total_seconds()
total_ops = len(creates) + len(deletes)
rate = total_ops * 60 / duration if duration > 0 else 0

print("================================================================")
print("  Pod Churn Test — Reconciliation Analysis")
print("================================================================")
print()
print(f"  Churn Duration:     {duration:.0f}s ({duration/60:.1f} min)")
print(f"  Total Creates:      {len(creates)}")
print(f"  Total Deletes:      {len(deletes)}")
print(f"  Create Failures:    {create_fails}")
print(f"  Total Operations:   {total_ops}")
print(f"  Effective Rate:     {rate:.1f} ops/min")

# Per-role breakdown
by_role = defaultdict(int)
for _, _, r in creates:
    by_role[r] += 1
for r, cnt in sorted(by_role.items()):
    if r:
        print(f"    {r}: {cnt} creates")
print()

# ── Parse PodASGMapping status timeline (2s polling) ───────────────
status_file = os.path.join(log_dir, 'mapping_status.csv')
mapping_timeline = defaultdict(list)  # mapping -> [(ts, reconciled, matchedPods, syncState)]

if os.path.exists(status_file):
    with open(status_file, 'r') as f:
        reader = csv.reader(f)
        next(reader)
        for row in reader:
            if len(row) < 5:
                continue
            ts_str, mapping, reconciled, matched, sync = row[0], row[1], row[2], row[3], row[4]
            try:
                ts = parse_ts(ts_str)
                matched_int = int(matched) if matched.isdigit() else 0
                mapping_timeline[mapping].append((ts, reconciled, matched_int, sync))
            except Exception:
                continue

print("  ── PodASGMapping Status Timeline ──")
print()
for mapping, entries in sorted(mapping_timeline.items()):
    matched = [e[2] for e in entries]
    synced_count = sum(1 for e in entries if e[3] == 'Synced')
    not_synced = len(entries) - synced_count

    print(f"  {mapping}:")
    print(f"    Pod count: start={matched[0]}  peak={max(matched)}  end={matched[-1]}")
    print(f"    Synced:    {synced_count}/{len(entries)} ({synced_count/len(entries)*100:.1f}%)")
    print(f"    Not synced: {not_synced}/{len(entries)} ({not_synced/len(entries)*100:.1f}%)")

    # State change frequency → reconcile cycle detection
    changes = []
    for i in range(1, len(entries)):
        if entries[i][2] != entries[i-1][2]:
            changes.append((entries[i][0], entries[i][2] - entries[i-1][2]))
    print(f"    State changes: {len(changes)} detected")

    if len(changes) > 1:
        intervals = [(changes[i][0] - changes[i-1][0]).total_seconds()
                      for i in range(1, len(changes))]
        intervals = [x for x in intervals if 0 < x < 300]
        if intervals:
            intervals.sort()
            n = len(intervals)
            print(f"    Reconcile interval: avg={sum(intervals)/n:.1f}s  "
                  f"min={min(intervals):.1f}s  max={max(intervals):.1f}s  "
                  f"P50={intervals[n//2]:.1f}s  P95={intervals[int(n*0.95)]:.1f}s")
    print()

# ── Pod IP assignment data ─────────────────────────────────────────
pod_ip_times = {}  # pod_name -> first IP assignment timestamp
ip_file = os.path.join(log_dir, 'pod_ips.csv')
if os.path.exists(ip_file):
    with open(ip_file, 'r') as f:
        reader = csv.reader(f)
        next(reader)
        for row in reader:
            if len(row) < 3:
                continue
            try:
                ts = parse_ts(row[0])
                name = row[1]
                if name not in pod_ip_times:
                    pod_ip_times[name] = ts
            except Exception:
                continue
    print(f"  Pod IP assignments tracked: {len(pod_ip_times)}")
    print()

# ── Reconciliation latency: pod event → ASG status update ─────────
# For each create event, find the next status snapshot where matchedPods changed
# This measures end-to-end: pod created → controller detects → ARM call → status update

all_latencies = []
for create_ts, name, role in creates:
    mapping = f"{role}-asg-mapping"
    entries = mapping_timeline.get(mapping, [])
    if not entries:
        continue

    # Find snapshot just before this create
    before = [e for e in entries if e[0] <= create_ts]
    after  = [e for e in entries if e[0] > create_ts]
    if not before or not after:
        continue

    baseline_count = before[-1][2]
    for snap in after:
        if snap[2] != baseline_count:
            latency = (snap[0] - create_ts).total_seconds()
            if 0 < latency < 300:
                all_latencies.append(latency)
            break

# Also measure from IP assignment time if available
ip_latencies = []
for create_ts, name, role in creates:
    if name not in pod_ip_times:
        continue
    mapping = f"{role}-asg-mapping"
    entries = mapping_timeline.get(mapping, [])
    ip_ts = pod_ip_times[name]

    before = [e for e in entries if e[0] <= ip_ts]
    after  = [e for e in entries if e[0] > ip_ts]
    if not before or not after:
        continue

    baseline_count = before[-1][2]
    for snap in after:
        if snap[2] != baseline_count:
            latency = (snap[0] - ip_ts).total_seconds()
            if 0 < latency < 300:
                ip_latencies.append(latency)
            break

def print_latency_table(title, data):
    if not data:
        print(f"  ({title}: no data)")
        return
    data.sort()
    n = len(data)
    avg = sum(data) / n
    p50 = data[n // 2]
    p95 = data[int(n * 0.95)]
    p99 = data[min(int(n * 0.99), n - 1)]
    print(f"  ┌─────────────────────────────────────────────────────┐")
    print(f"  │  {title:<49}│")
    print(f"  │  (n={n})                                             │")
    print(f"  ├─────────────────────────────────────────────────────┤")
    print(f"  │  Average:    {avg:6.1f}s                               │")
    print(f"  │  Min:        {min(data):6.1f}s                               │")
    print(f"  │  Max:        {max(data):6.1f}s                               │")
    print(f"  │  P50:        {p50:6.1f}s                               │")
    print(f"  │  P95:        {p95:6.1f}s                               │")
    print(f"  │  P99:        {p99:6.1f}s                               │")
    print(f"  └─────────────────────────────────────────────────────┘")

print()
print("  ── Reconciliation Latency ──")
print()
print_latency_table("Pod Create → ASG Status Update", all_latencies)
print()
if ip_latencies:
    print_latency_table("Pod IP Assigned → ASG Status Update", ip_latencies)
    print()

# ── Controller log analysis ────────────────────────────────────────
log_file = os.path.join(log_dir, 'controller_logs_during.txt')
if os.path.exists(log_file):
    etag_conflicts = 0
    throttles = 0
    other_errors = 0
    with open(log_file, 'r') as f:
        for line in f:
            idx = line.find('{')
            if idx < 0:
                continue
            try:
                entry = json.loads(line[idx:])
            except json.JSONDecodeError:
                continue
            msg = entry.get('msg', '')
            err = str(entry.get('error', ''))
            if 'PreconditionFailed' in err or '412' in err:
                etag_conflicts += 1
            elif 'Throttled' in err or '429' in err:
                throttles += 1
            elif entry.get('level') == 'error':
                other_errors += 1

    print("  ── ARM API Errors ──")
    print(f"    ETag conflicts (412):  {etag_conflicts} (auto-retried)")
    print(f"    Throttling (429):      {throttles}")
    print(f"    Other errors:          {other_errors}")
    print()

# ── Throughput over time (30s buckets) ─────────────────────────────
print("  ── Throughput Timeline (30s buckets) ──")
bucket_size = timedelta(seconds=30)
start = min(all_event_times)
end = max(all_event_times)
bucket_start = start
while bucket_start < end:
    bucket_end = bucket_start + bucket_size
    bucket_creates = sum(1 for ts, _, _ in creates if bucket_start <= ts < bucket_end)
    bucket_deletes = sum(1 for ts, _ in deletes if bucket_start <= ts < bucket_end)
    bucket_total = bucket_creates + bucket_deletes
    offset = int((bucket_start - start).total_seconds())
    bar = '█' * (bucket_total // 2)
    print(f"    [{offset:>4}s] {bucket_total:>4} ops ({bucket_creates:>3}C {bucket_deletes:>3}D) {bar}")
    bucket_start = bucket_end

print()
print("================================================================")
PYEOF
