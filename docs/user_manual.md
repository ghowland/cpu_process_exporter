# process_exporter — User Manual

---

# Table of contents

1. Installation
2. First run and verification
3. Understanding the output
4. Configuration reference
5. Cardinality: the central concern
6. Filtering in depth
7. Naming rules in depth
8. Scan tuning: what each knob changes
9. Privilege and coverage
10. Scaling scenarios
11. Prometheus integration
12. Operating the exporter
13. Troubleshooting

---

# 1. Installation

## 1.1 Build

```bash
git clone <repo> process_exporter
cd process_exporter
./build.sh
```

Produces `./process_exporter` in the repository root.

## 1.2 Install

```bash
sudo install -m 0755 process_exporter /usr/local/bin/process_exporter
sudo mkdir -p /etc/process_exporter
sudo install -m 0644 config.example.yaml /etc/process_exporter/config.yaml
```

## 1.3 systemd unit

**File: `/etc/systemd/system/process_exporter.service`**

```ini
[Unit]
Description=Process group Prometheus exporter
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/process_exporter -config /etc/process_exporter/config.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5

# The exporter is a monitoring agent and must not compete with the
# workload it measures. Nice and the idle IO class make the kernel
# prefer everything else when there is contention.
Nice=10
IOSchedulingClass=idle

ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now process_exporter
```

### Root or not?

| Running as | CPU, memory, threads | I/O bytes | Open file descriptors |
|---|---|---|---|
| `root` | All processes | All processes | All processes |
| Unprivileged | All processes | Own processes only | Own processes only |

The kernel exposes `/proc/<pid>/stat` and `/proc/<pid>/statm` for every process regardless of ownership, so **CPU and memory data are always complete**. Only `/proc/<pid>/io` and `/proc/<pid>/fd/` require a matching UID or `CAP_SYS_PTRACE`.

Running as root gives complete data. Running unprivileged is fully supported and produces no errors — the exporter reports coverage ratios instead, so partial data is visible rather than silent.

To get I/O and descriptor data without full root:

```ini
User=process_exporter
AmbientCapabilities=CAP_SYS_PTRACE
CapabilityBoundingSet=CAP_SYS_PTRACE
NoNewPrivileges=false
```

---

# 2. First run and verification

## 2.1 Validate the configuration first

```bash
process_exporter -check -config /etc/process_exporter/config.yaml
```

Prints `configuration is valid`, or lists **every** problem at once:

```
scan.batch_size must be at least 1
naming.rules[2].regex: error parsing regexp: missing closing )
scan.batch_sleep 50ms at batch_size 10 would take about 10s for 2000
processes, which exceeds scan.interval 5s
```

That last line is a duration estimate against a nominal 2,000 processes. It catches the most common tuning mistake before it causes overruns in production.

## 2.2 Run in the foreground

```bash
sudo process_exporter -config /etc/process_exporter/config.yaml
```

Expected startup:

```
level=INFO msg="process_exporter starting" version=dev listen=0.0.0.0:9256
  interval=15s clock_ticks=100 page_size=4096 root=true
```

If unprivileged, one additional line appears:

```
level=INFO msg="running unprivileged; CPU and memory will be complete,
  while io and fd data will cover only processes owned by this user"
```

## 2.3 The two-scan rule

**Counter values do not exist until the second scan.**

A counter in `/proc` is a lifetime total. Computing a rate requires two readings separated by a known interval. The first scan establishes the baseline; the second produces the first delta.

Immediately after start you will see this:

```
process_group_cpu_seconds_total{mode="user",name="bash",user="geoff"} 0
process_group_cpu_seconds_total{mode="system",name="bash",user="geoff"} 0
```

This is correct, not broken. Confirm which state you are in:

```bash
curl -s localhost:9256/metrics | grep process_exporter_scans_total
```

A value of 1 means the priming scan has run and counters are still empty. After `scan.interval` elapses, the value becomes 2 and counters populate.

`/readyz` encodes this and returns 503 until two scans have completed, so a health check will not mark the exporter ready while its counters are meaningless.

## 2.4 Inspect what it sees

```bash
curl -s localhost:9256/stats | jq
```

```json
{
  "scan_at": "2026-08-14T09:24:02Z",
  "scan_number": 7,
  "duration": "48.2ms",
  "self_cpu_secs": 0.012,
  "procs_total": 187,
  "procs_scanned": 43,
  "procs_ignored": 141,
  "procs_vanished": 3,
  "procs_denied": 0,
  "read_errors": {},
  "groups": 12,
  "names_seen": 12,
  "state_entries": 184
}
```

Read this carefully on first run:

| Field | What it tells you |
|---|---|
| `procs_ignored` far exceeding `procs_scanned` | Normal. Most processes on a Linux box are kernel threads |
| `procs_vanished` non-zero | Normal. Processes exit mid-scan at any real turnover rate |
| `procs_denied` non-zero | You are unprivileged and cannot read some processes' `stat` |
| `groups` equal to `names_seen` | No name churn. Cardinality is stable |
| `self_cpu_secs` | The exporter's own cost for that scan |

## 2.5 The top-equivalent view

```bash
curl -s localhost:9256/groups | jq '.groups[:10] | .[] | {
  name: .key.name,
  user: .key.user,
  procs: .num_procs,
  rss_mb: (.rss_bytes / 1048576 | floor),
  cpu: (.accum.utime_seconds + .accum.stime_seconds)
}'
```

Sortable with `?sort=`:

| Value | Orders by |
|---|---|
| `cpu` (default) | Accumulated CPU seconds |
| `rss` or `memory` | Resident memory |
| `procs` | Process count |
| `fds` | Open descriptors |
| `name` | Alphabetical |

This is the fastest way to answer "what is this exporter actually seeing" without going through Prometheus.

---

# 3. Understanding the output

## 3.1 Every metric has exactly two labels

```
process_group_cpu_seconds_total{mode="user",name="python/app",user="geoff"} 0.42
                                            └──────┬──────┘ └─────┬─────┘
                                               group name       owner
```

`name` and `user`. No PID, no command line, no container ID, no PPID.

Some metrics add one more dimension: `mode` on CPU, `kind` on context switches, `state` on the state gauge. These have fixed, small value sets and do not affect cardinality meaningfully.

## 3.2 Reading a real scrape

```
process_group_num_procs{name="nginx",user="www-data"} 40
process_group_num_procs{name="nginx",user="root"} 1
```

Forty workers and one master. The user label separated them, which is the structure you want to see.

```
process_group_memory_rss_bytes{name="nginx",user="www-data"} 2147483648
```

Two gigabytes across forty workers. **This over-counts.** Workers forked from a master share most of their text and much of their data, and RSS is summed per member. See §3.4.

```
process_group_cpu_seconds_total{mode="user",name="nginx",user="www-data"} 8134.2
```

An accumulator, not a sum of lifetime totals. If a worker exits and a new one starts, this value continues rising smoothly. That is the entire point of the design.

```
process_group_open_fds{name="nginx",user="www-data"} 4820
process_group_max_fds{name="nginx",user="www-data"} 40960
process_group_fd_coverage_ratio{name="nginx",user="www-data"} 1
```

Coverage of 1.0 means all forty workers' descriptor counts were read, so `4820/40960` is a true ratio. A coverage below 1.0 means the numerator is a lower bound.

## 3.3 Truncated names

```
process_group_cpu_seconds_total{name="process_exporte",user="root"} 0.01
```

Missing the final `r`. The kernel truncates `comm` to fifteen characters. This is expected.

If a truncated name matters, add a naming rule that reads the executable instead:

```yaml
naming:
  rules:
    - match_comm: "process_exporte"
      name: "process_exporter"
```

Or change the fallback globally:

```yaml
naming:
  fallback: exe_basename
```

That reads `/proc/<pid>/exe` for uncached processes, which costs one readlink per new process. With `cache_cmdline: true` this is paid once per process lifetime, not once per scan.

## 3.4 The RSS caveat, stated plainly

**A group's RSS total can exceed the machine's physical memory.** This is an artefact of summation, not a bug.

Forty nginx workers each showing 50 MB resident sum to 2 GB, but most of those pages are the same physical pages shared between them. The true footprint might be 200 MB.

Three responses, in order of usefulness:

1. **Use `process_group_memory_data_bytes` instead.** Private writable pages are not shared between forked processes, so the sum is closer to real unshared memory.
2. **Compare groups against each other**, not against `node_memory_MemTotal_bytes`. The relative sizes are meaningful even when the absolutes are inflated.
3. **Watch the trend, not the value.** A group whose RSS doubles has doubled its footprint, whatever the absolute over-count.

Computing proportional set size would require reading `/proc/<pid>/smaps`, which is expensive enough to defeat the low-CPU requirement this exporter exists to meet. That trade was made deliberately.

## 3.5 Absent metrics versus zero

When the exporter cannot read something, it **omits the metric** rather than reporting zero.

```
# Root: present
process_group_read_bytes_total{name="postgres",user="postgres"} 8.4e+09

# Unprivileged: absent entirely, and:
process_group_io_coverage_ratio{name="postgres",user="postgres"} 0
```

Zero and unknown are different facts. A group that does no I/O and a group whose I/O cannot be seen must not look identical in a graph.

Always check the coverage ratio before concluding a group is idle.

---

# 4. Configuration reference

```yaml
scan:
  interval: 15s
  batch_size: 50
  batch_sleep: 5ms
  fd_scan_every: 4
  read_io: true
  read_status: true
  cache_cmdline: true
  group_retention: 1h
  proc_path: /proc

filter:
  ignore_kernel_threads: true
  ignore_comm: [...]
  ignore_comm_prefix: [...]
  ignore_comm_regex: [...]
  ignore_cmdline_regex: []
  ignore_users: []
  include_only: []

naming:
  fallback: comm
  resolve_users: true
  rules: [...]

server:
  listen: "0.0.0.0:9256"
  metrics_path: /metrics

log:
  level: info
  format: json
```

## 4.1 `scan` section

### `interval` — default 15s

Time between scan starts. This is also the **sampling resolution for every counter**, because rates are computed from the difference between consecutive scans.

| Value | Effect |
|---|---|
| 5s | Fine resolution, roughly 3× the CPU of the default |
| 15s | Default. Adequate for almost everything |
| 60s | Coarse; a CPU spike lasting 30 seconds may be averaged away |

A shorter interval does not make any individual reading more accurate. It makes short-lived events visible that a longer interval would smooth over.

### `batch_size` — default 50

Processes read between yields. Combined with `batch_sleep`, this is what keeps the exporter off the top of `top`.

### `batch_sleep` — default 5ms

Sleep duration after each batch.

**This sleep is not wasted time.** It is what stops the Linux scheduler from classifying the exporter as a CPU-bound task. A process that yields voluntarily and frequently is scheduled differently from one that runs to the end of its quantum, and that difference is the whole reason the exporter can scan two thousand processes without appearing in a `top` listing.

Scan duration is approximately:

```
duration ≈ (procs ÷ batch_size) × batch_sleep + procs × read_cost
```

With 2,000 processes, batch 50, sleep 5ms:

- Sleeping: `(2000 ÷ 50) × 5ms = 200ms`
- Reading: roughly 40 to 100ms
- **Total: 250 to 300ms out of a 15-second interval**

CPU share: about `100ms ÷ 15s ≈ 0.7%` of one core.

### `fd_scan_every` — default 4

Read `/proc/<pid>/fd/` every Nth scan.

**Descriptor counting is the dominant cost in the entire scan.** The kernel must walk the file descriptor table and produce a directory entry for each open descriptor. A process holding ten thousand descriptors costs more than a hundred `stat` reads.

Descriptor counts change slowly. At the default `interval: 15s` and `fd_scan_every: 4`, they refresh once per minute, which is adequate for detecting a leak long before it exhausts a limit.

**Between fd scans the cached value is reported.** Without this the gauge would drop to zero on three scans out of four, which would look like every process closing all its files.

| Value | Refresh at 15s interval | Cost |
|---|---|---|
| 1 | Every 15s | Highest |
| 4 | Every 60s | Default |
| 20 | Every 5 minutes | Lowest |

Set to a high value on a machine running processes with very large descriptor tables — a database, a proxy, a file server.

### `read_io` — default true

Read `/proc/<pid>/io` for byte counters.

Set to false when running unprivileged and you do not want the wasted attempt on every process you cannot read. The permission errors are not logged and do not count as read errors, so leaving it true is harmless — it simply attempts a read that will fail.

### `read_status` — default true

Read `/proc/<pid>/status` for the UID and context switch counters.

Setting it false loses context switch metrics and forces UID resolution to rely entirely on the per-PID cache. The UID is needed for the `user` label, so `status` is read anyway when a process has no cached user. In practice this option only saves a read for long-lived processes, and only if you do not want context switch data.

### `cache_cmdline` — default true

Cache the derived group name and user per PID.

A process's command line, executable, and UID are fixed for its lifetime, so the derived name is too. Caching removes **one read and one regex evaluation per process per scan** for every long-lived process — the large majority.

The trade: a process that rewrites its own argv (some servers do this to display status) keeps its original name until it restarts.

Set false only if you have such a process and the changing name matters. The cost is one `cmdline` read plus full namer evaluation for every process on every scan.

### `group_retention` — default 1h

How long a group's counter accumulator survives after its last process exits.

**This is why a service restart does not reset its counters.** If `nginx` is stopped and started three minutes later, the accumulator is still there and the counter continues from where it left off. Without it, Prometheus would see a counter reset and discard the interval.

| Value | Behaviour |
|---|---|
| 0 | No retention. Every restart resets the counter |
| 1h | Default. Covers deploys, restarts, and brief outages |
| 24h | A group stopped overnight resumes its counter |

The memory cost is one small struct per retained group. Even 24h retention on a busy machine is a few kilobytes.

### `proc_path` — default `/proc`

Override for testing against a captured `/proc` tree, or for reading a container's `/proc` mounted elsewhere on the host.

## 4.2 `server` section

```yaml
server:
  listen: "0.0.0.0:9256"
  metrics_path: /metrics
```

Port 9256 is the Prometheus registry allocation for process exporters. **It collides with the community `process-exporter`** if both run on the same host. Change it if you are running both during a migration.

Bind to `127.0.0.1:9256` if Prometheus scrapes through a local agent.

## 4.3 `log` section

```yaml
log:
  level: info      # debug | info | warn | error
  format: json     # json | text
```

`debug` logs every scan with its duration and counts, and every ignored process with the rule that excluded it. Useful for tuning the ignore list; far too noisy for steady state on a machine with two thousand processes.

---

# 5. Cardinality: the central concern

Everything in this exporter's design serves one goal: producing useful process data without unbounded series growth. This section explains the mechanism, the failure mode, and how to detect it.

## 5.1 Why PID is not a label

A Prometheus series is identified by its label set. Adding PID would create a new series on every process start.

| Machine behaviour | Series per day with PID |
|---|---|
| Forks once per second | 86,400 |
| Forks once per 100ms | 864,000 |
| A build server | Unbounded |

Each of those series stays in memory until it ages out. The series count becomes a function of workload churn — something no operator chose and nobody can predict.

Grouping by name makes the count a function of the machine's **software inventory**, which changes when software is installed rather than when a program runs.

## 5.2 The actual counts

```
series ≈ groups × (12 gauges + 6 counters + state variants)
```

| Machine | Groups | Approximate series |
|---|---|---|
| Small VM | 20 | 400 |
| Typical server | 60 | 1,200 |
| Busy application server | 200 | 4,000 |
| Kubernetes node | 80 | 1,600 |

At 500 nodes, a typical server figure gives 600,000 series across the fleet. That is a real number that needs planning, but it is bounded and predictable.

## 5.3 Why user is a label despite the cost

Adding `user` multiplies group count by the number of distinct users running each program. In practice this is close to 1 for most programs and exactly 2 for master-and-worker services.

It earns its place:

```
process_group_num_procs{name="nginx",user="root"} 1
process_group_num_procs{name="nginx",user="www-data"} 40
```

Without the user label these merge to 41 and you lose the structure. With it, you can see the master separately from the workers, which is exactly what you want when the master is misbehaving.

The user set on a machine is bounded by `/etc/passwd`, so this does not introduce unbounded growth.

## 5.4 The one unguarded failure mode

There is **no hard cardinality cap**. This was a deliberate decision: a cap that silently folds groups into an `other` bucket hides the problem rather than surfacing it.

The failure mode is a naming rule that extracts an unbounded value.

**Anti-patterns — each creates one series per process instance:**

```yaml
# Instance IDs
- match_comm: java
  regex: '-Dinstance=(\S+)'
  name: 'java/$1'

# Container IDs
- match_comm_prefix: containerd-shim
  regex: '-id\s+(\S+)'
  name: 'shim/$1'

# Port numbers on a dynamically-ported service
- match_comm: myapp
  regex: '--port[= ](\d+)'
  name: 'myapp/$1'

# Anything containing a timestamp, UUID, or PID
```

**Correct patterns — bounded value sets:**

```yaml
- match_comm: java
  regex: '-Dservice=(\S+)'      # service names come from a finite list
  name: 'java/$1'

- match_comm_prefix: containerd-shim
  name: 'containerd-shim'       # collapse to a constant
```

## 5.5 Detecting it

Two metrics:

| Metric | Meaning |
|---|---|
| `process_exporter_groups_total` | Groups currently exported |
| `process_exporter_group_names_seen_total` | Distinct names **ever** observed |

A healthy machine has these roughly equal and both flat.

```promql
# The diagnostic
process_exporter_group_names_seen_total - process_exporter_groups_total
```

A **widening gap** means names appear briefly and disappear — the signature of an unbounded extraction.

```yaml
- alert: ProcessExporterNameChurn
  expr: |
    process_exporter_group_names_seen_total
      - process_exporter_groups_total > 200
  for: 30m
  annotations:
    summary: "a naming rule is extracting an unbounded value"

- alert: ProcessExporterTooManyGroups
  expr: process_exporter_groups_total > 500
  for: 30m
```

## 5.6 Fixing it after the fact

1. Identify the offending pattern:
   ```bash
   curl -s localhost:9256/groups?sort=name | jq -r '.groups[].key.name' | sort
   ```
   Look for names that share a prefix but differ in a numeric or hex suffix.

2. Fix the rule — either narrow the extraction or drop it and emit a constant.

3. `systemctl reload process_exporter`.

4. The exporter resets its accumulators and its per-PID name cache, and logs:
   ```
   naming changed; accumulators and cached names discarded, every time
   series restarts from zero
   ```

5. Delete the stale series in Prometheus, or wait for them to age out.

**Verify a naming rule before deploying it fleet-wide.** Run the exporter on one machine, let it settle, and check that `groups_total` and `names_seen_total` are equal and flat.

---

# 6. Filtering in depth

## 6.1 Why the ignore list exists

A stock Linux server runs a large number of kernel threads:

```
kworker/0:1  kworker/u16:3  ksoftirqd/0  migration/0  rcu_sched
kthreadd     watchdog/0     kdevtmpfs    kcompactd0   khugepaged
```

Three problems with them:

1. **Meaningless memory.** Kernel threads have no address space of their own. Their RSS and VSize readings do not describe anything real.
2. **Wrong tool.** Their CPU is real, but system-wide kernel CPU is better observed through `node_exporter`.
3. **Unbounded names.** `kworker/u16:3` and `kworker/u16:4` are distinct names, and the kernel creates and destroys them dynamically. On a 64-core machine this is a genuine cardinality problem.

On a typical server, `procs_ignored` will exceed `procs_scanned` by a factor of three or more. That is correct.

## 6.2 Rule types and evaluation order

Rules are evaluated in this order:

1. **`include_only`** — when non-empty, a process must match one of these to survive
2. **`ignore_kernel_threads`**
3. **`ignore_comm`** — exact
4. **`ignore_comm_prefix`** — prefix
5. **`ignore_comm_regex`** — regex
6. **`ignore_cmdline_regex`** — regex against joined argv
7. **`ignore_users`** — exact user match

## 6.3 The defaults, and how to override them

The shipped defaults cover roughly fifty exact names, eighteen prefixes, and one regex.

**Omitting a list entirely gets the defaults:**

```yaml
filter:
  ignore_kernel_threads: true
  # ignore_comm not mentioned → shipped defaults apply
```

**Setting it to an empty list disables the defaults:**

```yaml
filter:
  ignore_comm: []      # nothing ignored by exact name
```

**Setting it to your own list replaces the defaults entirely:**

```yaml
filter:
  ignore_comm:
    - my_noisy_daemon
    # the fifty shipped names are now NOT ignored
```

To extend rather than replace, copy the defaults from `config.example.yaml` and add to them. There is no merge behaviour, by design — implicit merging makes it impossible to remove a default.

## 6.4 The read-cost consequence

This matters for tuning.

The filter runs as early as possible. When no rule needs the command line, an ignored process costs **exactly one small read** — just `stat`.

But three settings force `cmdline` to be read before the filter can decide:

- `ignore_kernel_threads: true` (needs empty-argv detection)
- `ignore_cmdline_regex` non-empty
- `include_only` non-empty

`ignore_kernel_threads` is true by default, so the default configuration does read `cmdline`. However, `cache_cmdline: true` means this is paid **once per process lifetime**, not once per scan. A machine whose processes are long-lived pays almost nothing.

If you have very high process churn and want the cheapest possible ignore path:

```yaml
filter:
  ignore_kernel_threads: false   # rely on comm patterns instead
  ignore_comm_prefix:
    - "kworker/"
    - "ksoftirqd/"
    - "migration/"
    - "irq/"
    - "cpuhp/"
    # ... the full default prefix list
```

This catches kernel threads by name rather than by empty argv, and the filter then runs after `stat` alone.

## 6.5 Focusing on a named set

`include_only` inverts the model: only listed processes are exported.

```yaml
filter:
  include_only:
    - "^nginx"
    - "^postgres"
    - "^myapp"
```

Patterns are regexes matched against `comm` and against the joined command line.

Useful when a machine runs one workload you care about and a large amount of infrastructure you do not. It reduces series count to almost nothing at the cost of blindness to anything unexpected — which is a real cost during an incident.

## 6.6 Ignoring by user

```yaml
filter:
  ignore_users:
    - nobody
    - systemd-network
    - systemd-resolve
    - messagebus
```

Removes whole categories of system service in one rule.

## 6.7 Tuning the ignore list

```bash
# See what is being ignored and why
sudo process_exporter -config /etc/process_exporter/config.yaml 2>&1 \
  | grep "process ignored"
```

Requires `log.level: debug`. Each line names the process and the rule that excluded it.

To find what is *not* being ignored but should be:

```bash
curl -s localhost:9256/groups?sort=name | jq -r \
  '.groups[] | select(.num_procs > 0) | "\(.key.name) \(.num_procs)"'
```

Look for names you do not recognise and did not intend to measure.

---

# 7. Naming rules in depth

## 7.1 The problem restated

For most programs, `comm` is the right name. For interpreters, it is not:

| Process | `comm` | What you want |
|---|---|---|
| A Spring service | `java` | `java/OrderService` |
| A Django app | `python3` | `python/manage` |
| An Express server | `node` | `node/server` |
| A container shim | `containerd-shim` | `containerd-shim` |

Without rules, every JVM on the machine merges into one `java` group, which destroys exactly the distinction you need.

## 7.2 Rule anatomy

```yaml
- match_comm: java              # 1. does this rule apply?
  from: cmdline                 # 2. what text to search
  regex: '-jar\s+\S*?([^/\s]+)\.jar'   # 3. what to extract
  name: 'java/$1'               # 4. how to build the name
```

**Match conditions** — all specified must be satisfied:

| Field | Match against |
|---|---|
| `match_comm` | `comm`, exact |
| `match_comm_prefix` | `comm`, prefix |
| `match_comm_regex` | `comm`, regex |
| `match_exe_regex` | resolved executable path, regex |

**`from`** selects the extraction target: `comm`, `exe`, or `cmdline` (default). For `cmdline`, argv is joined with spaces.

**`regex`** with capture groups. Omit it to emit `name` literally.

**`name`** is the template. `$1`, `$2` reference capture groups.

## 7.3 The fall-through cascade

**A rule that matches but whose extraction fails falls through to the next rule.**

This is the most important behaviour in the naming system. It lets you write a cascade:

```yaml
# 1. Try the jar name
- match_comm: java
  from: cmdline
  regex: '-jar\s+\S*?([^/\s]+)\.jar'
  name: 'java/$1'

# 2. Fall through to the main class
- match_comm: java
  from: cmdline
  regex: '\s([a-zA-Z][a-zA-Z0-9_]*(?:\.[a-zA-Z0-9_]+)*\.[A-Z][a-zA-Z0-9_]*)(?:\s|$)'
  name: 'java/$1'

# 3. Fall through to a system property
- match_comm: java
  from: cmdline
  regex: '-Dservice\.name=(\S+)'
  name: 'java/$1'

# 4. Nothing matched → fallback gives "java"
```

A JVM invoked any of those three ways gets a useful name. One invoked some fourth way falls back to `java`, which is unhelpful but not wrong.

## 7.4 Collapsing to a constant

A rule with no `regex` emits its `name` literally:

```yaml
- match_comm_prefix: containerd-shim
  name: containerd-shim
```

**This is a cardinality control, not a convenience.** The full command line of a shim contains the container ID. Any extraction from it would create one series per container. Collapsing to a constant means one series regardless of how many containers run.

Apply the same treatment to anything with a per-instance identifier in its name or argv.

## 7.5 The fallback

```yaml
naming:
  fallback: comm      # comm | exe_basename | cmdline_basename
```

| Value | Behaviour | Cost |
|---|---|---|
| `comm` | Kernel short name, 15 chars max | Free — already read from `stat` |
| `exe_basename` | Basename of `/proc/<pid>/exe` | One readlink per new process |
| `cmdline_basename` | Basename of `cmdline[0]` | One read per new process |

`comm` is the default because it is free. Choose `exe_basename` if truncation is causing problems across many programs; with `cache_cmdline: true`, the readlink is paid once per process lifetime.

## 7.6 Worked examples

### Naming a Java service by system property

Add to your JVM launch arguments:

```
-Dservice.name=order-api
```

Then:

```yaml
- match_comm: java
  from: cmdline
  regex: '-Dservice\.name=(\S+)'
  name: 'java/$1'
```

This is the most reliable Java naming approach, because it does not depend on how the JVM was invoked.

### Naming Python by module

```bash
python3 -m myapp.worker
```

```yaml
- match_comm_prefix: python
  from: cmdline
  regex: '-m\s+(\S+)'
  name: 'python/$1'
```

Place this **before** the default `.py` script rule so it wins for module invocations.

### Separating a supervisor's children

```yaml
- match_comm: supervisord
  name: supervisord

- match_comm_regex: '^worker-'
  from: comm
  regex: '^(worker-[a-z]+)'    # worker-http, not worker-http-12
  name: '$1'
```

The extraction deliberately stops before the instance number.

### Handling a wrapper script

```yaml
- match_comm: bash
  from: cmdline
  regex: '/opt/myapp/bin/(\w+)\.sh'
  name: 'myapp/$1'
```

Without this, every wrapper script merges into `bash` alongside interactive shells.

## 7.7 Testing a rule safely

1. Copy your configuration to a scratch file and change the port:
   ```yaml
   server:
     listen: "127.0.0.1:19256"
   ```

2. Run in the foreground with debug logging.

3. Check the derived names:
   ```bash
   curl -s localhost:19256/groups?sort=name | jq -r '.groups[].key.name'
   ```

4. Let it run for several minutes and watch for churn:
   ```bash
   watch -n5 'curl -s localhost:19256/stats | jq "{groups, names_seen}"'
   ```

   `groups` and `names_seen` should be equal and flat. A rising `names_seen` means the rule is extracting something unbounded.

5. Only then deploy fleet-wide.

---

# 8. Scan tuning: what each knob changes

## 8.1 The complete tuning table

| Machine | interval | batch_size | batch_sleep | fd_scan_every | Approx CPU |
|---|---|---|---|---|---|
| Small, under 200 procs | 15s | 100 | 2ms | 2 | 0.1% |
| Typical, under 1,000 | 15s | 50 | 5ms | 4 | 0.5% |
| Busy, 1,000–5,000 | 30s | 50 | 5ms | 8 | 0.5% |
| Very busy, over 5,000 | 60s | 100 | 10ms | 10 | 0.4% |
| Latency-sensitive | 60s | 25 | 20ms | 20 | 0.1% |
| High-resolution debug | 5s | 200 | 1ms | 2 | 3% |

## 8.2 What each change does

### Increasing `interval`

| Improves | Costs |
|---|---|
| Proportionally lower CPU | Coarser counter resolution |
| | Short spikes averaged away |
| | Slower detection of changes |

Doubling the interval halves the CPU. It is the single most effective knob and should be tried first.

**It does not** affect accuracy of any individual reading. A rate over a 60-second interval is exactly as accurate as one over 15 seconds; it simply cannot resolve events shorter than 60 seconds.

### Increasing `batch_size`

| Improves | Costs |
|---|---|
| Shorter scan duration | Higher CPU during the scan |
| Better snapshot coherence | More scheduler pressure per batch |

The scan finishes faster because it sleeps less often. The CPU it uses is more concentrated, which is exactly what the batching exists to avoid.

Raise it when scans are overrunning the interval and you cannot lengthen the interval.

### Increasing `batch_sleep`

| Improves | Costs |
|---|---|
| Lower CPU, more yielding | Longer scan duration |
| Less scheduler impact | Worse snapshot coherence |
| | Risk of overruns |

The most direct lever on CPU. Doubling it roughly doubles the sleeping portion of the scan.

**Watch for overruns.** At 5,000 processes, batch 50, sleep 20ms:

```
(5000 ÷ 50) × 20ms = 2 seconds of sleeping
```

Fine at a 60s interval; an overrun at a 1s interval.

### Increasing `fd_scan_every`

| Improves | Costs |
|---|---|
| Substantially lower CPU on some machines | Staler descriptor counts |

This has the largest effect on machines running processes with big descriptor tables. On a database server holding twenty thousand descriptors, going from 1 to 8 can halve total scan cost.

It has almost no effect on a machine where processes hold a handful of descriptors each.

Descriptor counts change slowly. Refreshing every five minutes is adequate for leak detection.

### Disabling `read_io`

| Improves | Costs |
|---|---|
| One fewer read per process per scan | No I/O byte metrics |

Worth doing when running unprivileged and you have accepted that I/O data is unavailable anyway.

### Disabling `read_status`

| Improves | Costs |
|---|---|
| One read per long-lived process per scan | No context switch metrics |

Modest saving. The file is still read for processes with no cached UID.

### Disabling `cache_cmdline`

**This makes things worse, not better.** It is listed only for completeness: set it false if a process rewrites its argv and you need to track the change. The cost is one read plus a full namer evaluation for every process on every scan.

## 8.3 Diagnosing cost

The exporter measures itself:

```promql
rate(process_exporter_scan_cpu_seconds_total[5m])
```

| Value | Meaning |
|---|---|
| Below 0.005 | Excellent |
| 0.005–0.02 | Normal for a typical server |
| 0.02–0.05 | High; consider tuning |
| Above 0.05 | Over 5% of a core; tune now |

Also watch scan duration against the interval:

```promql
histogram_quantile(0.99,
  rate(process_exporter_scan_duration_seconds_bucket[5m]))
```

Should sit well below `scan.interval`. Approaching it means overruns are imminent.

```promql
rate(process_exporter_scan_overruns_total[1h]) > 0
```

Any non-zero value means scans are exceeding the interval and ticks are being skipped.

## 8.4 Tuning procedure

1. Deploy with defaults.
2. Wait an hour.
3. Check `rate(process_exporter_scan_cpu_seconds_total[5m])`.
4. If above target:
   - First, double `interval`. Halves the cost, costs only resolution.
   - Second, raise `fd_scan_every`. Large effect if descriptor tables are big.
   - Third, raise `batch_sleep`. Direct but risks overruns.
5. Re-check after each change.
6. Verify no overruns.

Do not tune blind. `process_exporter_scan_cpu_seconds_total` gives you the answer directly.

## 8.5 Snapshot coherence

At a 300ms scan, the first and last process read are separated by 300ms. The gauge snapshot is smeared across that window.

**This does not affect counter accuracy.** Each PID's delta uses its own two readings and its own elapsed time. The smear affects only the simultaneity of gauges, and at sub-second durations it is irrelevant.

It becomes worth thinking about above roughly five seconds of scan duration, which means you have tuned `batch_sleep` too high for your process count.

---

# 9. Privilege and coverage

## 9.1 What each file needs

| File | Privilege | Provides |
|---|---|---|
| `/proc/<pid>/stat` | None | CPU, state, threads, faults, start time |
| `/proc/<pid>/statm` | None | Memory |
| `/proc/<pid>/status` | None | UID, context switches |
| `/proc/<pid>/cmdline` | None | argv |
| `/proc/<pid>/io` | **Same UID or `CAP_SYS_PTRACE`** | I/O bytes |
| `/proc/<pid>/fd/` | **Same UID or `CAP_SYS_PTRACE`** | Open descriptors |

**CPU and memory are always complete.** The kernel exposes those files to everyone.

## 9.2 Reading the coverage ratios

```
process_group_fd_coverage_ratio{name="postgres",user="postgres"} 0
process_group_io_coverage_ratio{name="postgres",user="postgres"} 0
```

Zero means no member's data was readable. The corresponding metrics are **absent**, not zero, so you cannot mistake "cannot see" for "does none".

```
process_group_fd_coverage_ratio{name="myapp",user="deploy"} 0.6
process_group_open_fds{name="myapp",user="deploy"} 240
```

Sixty percent coverage: 240 is a lower bound. The real total is roughly 400.

## 9.3 Alerting on partial coverage

```yaml
- alert: ProcessExporterPartialCoverage
  expr: |
    process_group_io_coverage_ratio > 0
      and process_group_io_coverage_ratio < 1
  for: 15m
  annotations:
    summary: "{{ $labels.name }} I/O totals are a lower bound"
```

Note `> 0` in the condition. A ratio of exactly 0 means the exporter cannot see the group at all, which is a different and usually expected situation.

## 9.4 Choosing a privilege level

| Requirement | Configuration |
|---|---|
| Complete data | Run as root |
| Complete data, minimum privilege | `AmbientCapabilities=CAP_SYS_PTRACE` |
| CPU and memory only, no privilege | Unprivileged, `read_io: false` |

`CAP_SYS_PTRACE` is a genuinely powerful capability — it permits attaching to and inspecting any process. If your security posture does not allow it, running unprivileged with `read_io: false` gives complete CPU, memory, thread, and state data, which covers the majority of use cases.

---

# 10. Scaling scenarios

## 10.1 Small fleet, under 50 machines

Defaults everywhere. Roughly 1,200 series per machine, 60,000 total. No special handling needed.

## 10.2 Medium fleet, 50 to 500 machines

At 500 machines the fleet produces 600,000 series. Worth managing.

```yaml
scan:
  interval: 30s        # halve the CPU, halve the sample volume
  fd_scan_every: 8
```

And a tighter ignore list. Audit what each machine actually exports:

```bash
for h in $(cat hosts.txt); do
  echo -n "$h: "
  curl -s "http://$h:9256/stats" | jq -r '.groups'
done | sort -t: -k2 -rn | head -20
```

Machines at the top of that list are worth investigating — they may have a naming rule producing churn, or they may genuinely run more software.

## 10.3 Large fleet, over 500 machines

Two approaches.

**Restrict what is measured.** On machines with a known, fixed role:

```yaml
filter:
  include_only:
    - "^nginx"
    - "^myapp"
    - "^postgres"
```

Cuts series count by an order of magnitude. The cost is blindness during an incident, which is a real cost — an unexpected process consuming CPU will not appear.

**Drop metrics at the Prometheus end.** Keeps full local visibility through `/groups` while reducing stored series:

```yaml
metric_relabel_configs:
  - source_labels: [__name__]
    regex: 'process_group_memory_vsize_bytes|process_group_memory_shared_bytes'
    action: drop
```

`vsize` is nearly meaningless summed, and `shared` is rarely queried. Dropping both removes about 11% of series.

The second approach is generally better: the data is still available locally for debugging, and the decision is centralised rather than replicated across every machine's configuration.

## 10.4 Kubernetes nodes

Run as a DaemonSet with `hostPID: true`, so the container sees the host's process tree.

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: process-exporter
  namespace: monitoring
spec:
  selector:
    matchLabels:
      app: process-exporter
  template:
    metadata:
      labels:
        app: process-exporter
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9256"
    spec:
      hostPID: true
      tolerations:
        - operator: Exists
      containers:
        - name: process-exporter
          image: your-registry/process_exporter:1.0
          args: ["-config", "/etc/process_exporter/config.yaml"]
          securityContext:
            runAsUser: 0
            capabilities:
              add: ["SYS_PTRACE"]
          ports:
            - containerPort: 9256
              name: metrics
              hostPort: 9256
          volumeMounts:
            - name: proc
              mountPath: /host/proc
              readOnly: true
            - name: config
              mountPath: /etc/process_exporter
          resources:
            requests:
              cpu: 20m
              memory: 32Mi
            limits:
              memory: 128Mi
      volumes:
        - name: proc
          hostPath:
            path: /proc
        - name: config
          configMap:
            name: process-exporter-config
```

With `proc_path: /host/proc` in the configuration.

**Container processes need naming rules**, or every container's main process appears under its binary name with no indication of which workload it belongs to. The container ID is in the shim's command line and must **not** be extracted — it is unbounded.

```yaml
naming:
  rules:
    - match_comm_prefix: containerd-shim
      name: containerd-shim
    - match_comm: runc
      name: runc
    - match_comm: pause
      name: pause
```

The `pause` rule matters: a busy node runs one pause container per pod, and without the rule they all group correctly anyway — but making it explicit documents the intent.

Kubelet, kube-proxy, and the container runtime will appear as their own groups, which is useful.

## 10.5 Container-heavy hosts

Docker or containerd hosts with high container churn are the highest-risk environment for cardinality problems, because container IDs are everywhere in command lines.

Audit before deploying naming rules:

```bash
curl -s localhost:9256/groups?sort=name | jq -r '.groups[].key.name' \
  | grep -E '[0-9a-f]{12,}'
```

Any output is a name containing a hex ID and is a cardinality bomb.

Rule of thumb: for any process whose command line contains a container ID, a UUID, or a hash, either collapse it to a constant or match on something else entirely.

---

# 11. Prometheus integration

## 11.1 Scrape configuration

```yaml
scrape_configs:
  - job_name: process
    scrape_interval: 30s
    static_configs:
      - targets: ['host1:9256', 'host2:9256']
```

**Scrape interval should be at or above `scan.interval`.** Scraping faster returns the same snapshot repeatedly, which is harmless but pointless.

## 11.2 Essential queries

```promql
# Top CPU consumers
topk(10, sum by (name, user) (rate(process_group_cpu_seconds_total[5m])))

# Top memory consumers
topk(10, process_group_memory_rss_bytes)

# Unshared memory — more honest than RSS
topk(10, process_group_memory_data_bytes)

# Descriptor pressure
process_group_open_fds / process_group_max_fds > 0.8

# Process count changing — leak or crash loop
delta(process_group_num_procs[1h]) != 0

# A group restarted
changes(process_group_oldest_start_time_seconds[1h]) > 0

# Blocked on I/O
process_group_states{state="disk_sleep"} > 0

# Zombies
process_group_states{state="zombie"} > 5

# CPU as a fraction of a core
sum by (name, user) (rate(process_group_cpu_seconds_total[5m]))

# Disk I/O by group
sum by (name, user) (rate(process_group_read_bytes_total[5m]))
```

## 11.3 Cross-referencing with node_exporter

```promql
# What fraction of system CPU does this group use?
sum by (name) (rate(process_group_cpu_seconds_total[5m]))
  / on() group_left
count(count by (cpu) (node_cpu_seconds_total))
```

Do **not** do the same with memory:

```promql
# WRONG — RSS sums over-count shared pages and can exceed MemTotal
sum(process_group_memory_rss_bytes) / node_memory_MemTotal_bytes
```

Use `process_group_memory_data_bytes` for any comparison against total memory, and treat even that as approximate.

## 11.4 Alerting rules

```yaml
groups:
  - name: process_groups
    rules:
      - alert: ProcessGroupFDExhaustion
        expr: process_group_open_fds / process_group_max_fds > 0.9
        for: 5m
        labels: { severity: critical }
        annotations:
          summary: "{{ $labels.name }} ({{ $labels.user }}) near its fd limit"

      - alert: ProcessGroupGone
        expr: process_group_num_procs == 0
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "{{ $labels.name }} has no running processes"

      - alert: ProcessGroupCrashLooping
        expr: changes(process_group_oldest_start_time_seconds[15m]) > 3
        for: 5m
        annotations:
          summary: "{{ $labels.name }} restarted {{ $value }} times in 15m"

      - alert: ProcessGroupZombies
        expr: process_group_states{state="zombie"} > 10
        for: 10m

      - alert: ProcessGroupMemoryGrowth
        expr: |
          delta(process_group_memory_data_bytes[6h])
            / process_group_memory_data_bytes > 0.5
        for: 30m
        annotations:
          summary: "{{ $labels.name }} private memory grew 50% in 6h"

  - name: process_exporter_health
    rules:
      - alert: ProcessExporterExpensive
        expr: rate(process_exporter_scan_cpu_seconds_total[5m]) > 0.05
        for: 15m
        annotations:
          summary: "using over 5% of a core; check scan tuning"

      - alert: ProcessExporterStale
        expr: time() - process_exporter_last_scan_timestamp_seconds > 120
        for: 5m

      - alert: ProcessExporterOverruns
        expr: rate(process_exporter_scan_overruns_total[1h]) > 0
        for: 30m
        annotations:
          summary: "scans exceeding the interval; raise interval or batch_size"

      - alert: ProcessExporterNameChurn
        expr: |
          process_exporter_group_names_seen_total
            - process_exporter_groups_total > 200
        for: 30m
        annotations:
          summary: "a naming rule is extracting an unbounded value"

      - alert: ProcessExporterReadErrors
        expr: rate(process_exporter_read_errors_total[10m]) > 0.1
        for: 10m
```

`ProcessExporterReadErrors` fires only on genuine failures. Vanished processes and permission denials are excluded from that counter, because both are normal.

## 11.5 Recording rules

For frequently-used expressions on large fleets:

```yaml
groups:
  - name: process_recording
    interval: 60s
    rules:
      - record: instance:process_group_cpu:rate5m
        expr: sum by (instance, name, user) (
                rate(process_group_cpu_seconds_total[5m]))

      - record: instance:process_group_memory:sum
        expr: sum by (instance, name, user) (
                process_group_memory_data_bytes)
```

---

# 12. Operating the exporter

## 12.1 Daily inspection

```bash
# Scan health and cost
curl -s localhost:9256/stats | jq '{
  scan_number, duration, self_cpu_secs,
  procs_scanned, procs_ignored, groups, names_seen
}'

# Top consumers
curl -s localhost:9256/groups | jq -r '.groups[:10][] |
  "\(.key.name)\t\(.key.user)\t\(.num_procs)\t\(.rss_bytes/1048576|floor)MB"'

# Ready?
curl -s localhost:9256/readyz | jq
```

## 12.2 Configuration changes

Always validate first:

```bash
process_exporter -check -config /etc/process_exporter/config.yaml
```

Then reload:

```bash
sudo systemctl reload process_exporter
```

A configuration that fails validation is **rejected** and the previous one stays active. You cannot break a running exporter with a bad file.

### What each change does

| Change | Effect |
|---|---|
| `scan.*` | Applies from the next scan. No series disruption |
| `filter.*` | Applies from the next scan. Newly ignored groups age out via `group_retention` |
| `naming.*` | **Resets accumulators and the name cache. Every series restarts from zero** |
| `server.*` | Requires a restart |
| `log.*` | Requires a restart |

A naming change logs a warning saying exactly this:

```
naming changed; accumulators and cached names discarded, every time
series restarts from zero
```

Both the accumulators and the per-PID name cache must be discarded: the accumulators are keyed on names that no longer exist, and the cache holds names derived under the old rules. Keeping either would produce wrong output.

## 12.3 Restarts

A restart resets **every counter**. Prometheus sees a counter reset and `rate()` handles it correctly by discarding the interval containing the restart.

No state is persisted. This is correct and expected behaviour for a process exporter — `node_exporter` and every other agent of this class behave the same way.

Gauges are unaffected: after the priming scan, memory and process counts are immediately correct.

## 12.4 Rolling deployment

1. Deploy to one machine.
2. Wait ten minutes.
3. Check:
   ```bash
   curl -s host:9256/stats | jq '{groups, names_seen, self_cpu_secs}'
   ```
   `groups` and `names_seen` should be equal. `self_cpu_secs` should be a small fraction of your scan interval.
4. Check for overruns:
   ```promql
   process_exporter_scan_overruns_total
   ```
5. Only then proceed fleet-wide.

The step that matters is 3. A naming rule that produces churn will show it within ten minutes, and finding it on one machine is much cheaper than finding it on five hundred.

---

# 13. Troubleshooting

## 13.1 All counters are zero

```bash
curl -s localhost:9256/metrics | grep process_exporter_scans_total
```

A value of 1 means only the priming scan has run. **Counter values require two scans** — the first establishes the baseline, the second produces the first delta. Wait one `scan.interval`.

If it stays at 1, scans are failing. Check the log.

## 13.2 A process is missing

```bash
# Is it being ignored?
curl -s localhost:9256/stats | jq '.procs_ignored'
```

Enable debug logging and look for it:

```bash
sudo process_exporter -config ... 2>&1 | grep "process ignored" | grep myapp
```

The log line names the rule that excluded it.

Common causes:

- It matched an ignore prefix. Check `ignore_comm_prefix` against its `comm`.
- `include_only` is set and it does not match.
- Its owner is in `ignore_users`.
- It is short-lived and exits between scans.

## 13.3 I/O or descriptor metrics are absent

```bash
curl -s localhost:9256/metrics | grep coverage_ratio
```

A ratio of 0 means nothing was readable — you are unprivileged and do not own those processes.

Fix by running as root, or by granting `CAP_SYS_PTRACE`, or by accepting the limitation and setting `read_io: false` to avoid the futile attempt.

If descriptor metrics are absent but the coverage ratio is 1, `fd_scan_every` has not yet come round. Wait `interval × fd_scan_every`.

## 13.4 Group count growing steadily

```promql
process_exporter_group_names_seen_total - process_exporter_groups_total
```

A widening gap confirms name churn.

```bash
curl -s localhost:9256/groups?sort=name | jq -r '.groups[].key.name' | sort
```

Look for names sharing a prefix but differing in a numeric or hex suffix. That is your rule.

Fix it, reload, and delete the stale series in Prometheus or wait for them to age out.

## 13.5 High exporter CPU

```promql
rate(process_exporter_scan_cpu_seconds_total[5m])
```

In order of effectiveness:

1. Double `scan.interval`. Halves the cost.
2. Raise `fd_scan_every`. Large effect if descriptor tables are big.
3. Raise `batch_sleep`. Direct but risks overruns.
4. Broaden the ignore list. Each ignored process costs one read instead of five.
5. Set `read_io: false` if you are not using the data.

## 13.6 Scans are overrunning

```promql
rate(process_exporter_scan_overruns_total[1h]) > 0
```

The scan takes longer than the interval and ticks are being skipped.

```bash
curl -s localhost:9256/stats | jq '{duration, procs_total}'
```

Either lengthen `interval`, or shorten the scan by raising `batch_size` or lowering `batch_sleep`. Note that the last two increase CPU — the trade is unavoidable.

## 13.7 Metrics are stale

```promql
time() - process_exporter_last_scan_timestamp_seconds
```

Should stay below `scan.interval × 2`. Higher means scans are failing or hanging. Check the log for scan errors and check `procs_denied`.

## 13.8 RSS exceeds physical memory

Expected. RSS is summed per member and shared pages are counted once per member.

Use `process_group_memory_data_bytes` for anything compared against total memory. See §3.4.

## 13.9 Names are truncated

`comm` is fifteen characters, kernel-imposed. Either add a specific naming rule, or set `naming.fallback: exe_basename`.

## 13.10 Debug logging

```yaml
log:
  level: debug
  format: text
```

Requires a restart. Produces one line per scan with timings and counts, plus one line per ignored process.

**Far too noisy for steady state** on a machine with two thousand processes. Use it to tune the ignore list, then return to `info`.
