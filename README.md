# Proxmox Backup Server Exporter

[![license](https://img.shields.io/github/license/natrontech/pbs-exporter)](https://github.com/natrontech/pbs-exporter/blob/main/LICENSE)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/natrontech/pbs-exporter/badge)](https://securityscorecards.dev/viewer/?uri=github.com/natrontech/pbs-exporter)
[![release](https://img.shields.io/github/v/release/natrontech/pbs-exporter)](https://github.com/natrontech/pbs-exporter/releases)
[![go-version](https://img.shields.io/github/go-mod/go-version/natrontech/pbs-exporter)](https://github.com/natrontech/pbs-exporter/blob/main/go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/natrontech/pbs-exporter)](https://goreportcard.com/report/github.com/natrontech/pbs-exporter)
[![SLSA 3](https://slsa.dev/images/gh-badge-level3.svg)](https://slsa.dev)

---

Export [Proxmox Backup Server](https://www.proxmox.com/en/proxmox-backup-server/overview) statistics to [Prometheus](https://prometheus.io/).

Metrics are retrieved using the [Proxmox Backup Server API](https://pbs.proxmox.com/docs/api-viewer/index.html): backup groups and
their snapshots, verify, prune, garbage collection and sync jobs, running tasks, datastores, ZFS pools, node health and more.
One exporter can monitor any number of Proxmox Backup Servers, each with its own API token.

- [Quick start](#quick-start)
- [Configuration](#configuration)
- [API token and privileges](#api-token-and-privileges)
- [Collectors](#collectors)
- [Exported metrics](#exported-metrics)
- [Prometheus](#prometheus)
- [Running the exporter](#running-the-exporter)

## Quick start

1. Create an API token on the Proxmox Backup Server (see [API token and privileges](#api-token-and-privileges)).
2. Write a config file, e.g. `/etc/pbs-exporter/config.yml`:

   ```yaml
   modules:
     default:
       endpoint: https://pbs.example.org:8007
       user: monitoring@pbs
       token_name: exporter
       token_value: 00000000-0000-0000-0000-000000000000
   ```

3. Start the exporter and scrape it:

   ```bash
   pbs-exporter -config.file /etc/pbs-exporter/config.yml
   curl http://localhost:10019/metrics
   ```

## Configuration

The exporter is configured with one YAML file only. Its path is given with `-config.file` (default
`/etc/pbs-exporter/config.yml`); the only other flag is `-version`. The file is read on start: restart the exporter to apply
changes (it ignores `SIGHUP`). A complete, commented example is [pbs-exporter.example.yml](pbs-exporter.example.yml).

The file has three sections:

```yaml
server:          # the exporter's HTTP server (optional)
  ...
defaults:        # settings every module inherits (optional)
  ...
modules:         # one module per Proxmox Backup Server (required)
  <name>:
    ...
```

### `server`

| Setting                 | Default    | Description                                                                                                  |
| ----------------------- | ---------- | ------------------------------------------------------------------------------------------------------------ |
| `listen_address`        | `:10019`   | Address the HTTP server listens on, `[host]:port` (port 1–65535).                                            |
| `metrics_path`          | `/metrics` | Path of the metrics, e.g. `/metrics` or `/pbs/metrics` (letters, digits and `. _ ~ -` between slashes, no `.` or `..` segment; not `/exporter-metrics`). |
| `log_level`             | `info`     | `debug`, `info`, `warn` or `error`.                                                                          |
| `scrape_timeout`        | `30s`      | Time limit of a scrape when the client sends no `X-Prometheus-Scrape-Timeout-Seconds` header.              |
| `scrape_timeout_offset` | `500ms`    | Subtracted from the scrape timeout Prometheus sends, to leave time for the response.                       |

### `modules` and `defaults`

Each module is one Proxmox Backup Server. A scrape selects it with the `module` URL parameter
(`/metrics?module=site-a`); a scrape without `module` uses the module named `default`. Every setting of a module can also be
written under `defaults`, except `endpoint`; a module inherits all settings it does not set itself. Collectors are merged
(the module's switches win), every other setting is replaced.

| Setting            | Default        | Description                                                                                                  |
| ------------------ | -------------- | ------------------------------------------------------------------------------------------------------------ |
| `endpoint`         | (required)     | Base URL of the Proxmox Backup Server, `http(s)://host[:port]` (only per module).                          |
| `user`             | (required)     | User the API token belongs to, `<name>@<realm>`, e.g. `monitoring@pbs`.                                     |
| `token_name`       | (required)     | Name of the API token only, e.g. `exporter` (not `monitoring@pbs!exporter`).                                |
| `token_value`      | (required¹)    | Secret of the API token.                                                                                     |
| `token_value_file` | (required¹)    | File that holds the secret of the API token, e.g. a Docker or Kubernetes secret; read on start.            |
| `insecure`         | `false`        | Skip TLS certificate verification.                                                                           |
| `timeout`          | `5s`           | Timeout of each request to the Proxmox Backup Server.                                                       |
| `max_concurrency`  | `4`            | Concurrent requests to the Proxmox Backup Server, over all scrapes of the module.                          |
| `collectors`       | see [Collectors](#collectors) | Switch collectors on (`true`) or off (`false`).                                               |

¹ Exactly one of `token_value` and `token_value_file`. A module that sets one of them uses neither of the defaults.

Example with shared settings:

```yaml
server:
  log_level: info

defaults:
  user: monitoring@pbs
  token_name: exporter
  timeout: 15s
  collectors:
    access: true

modules:
  site-a:
    endpoint: https://10.10.10.10:8007
    token_value_file: /run/secrets/pbs-site-a-token
  site-b:
    endpoint: https://10.10.10.11:8007
    token_value: 11111111-1111-1111-1111-111111111111
    insecure: true            # self-signed certificate
    collectors:
      disks: true             # in addition to access from the defaults
```

The config file is validated strictly on start, and all problems are reported at once:

- Unknown keys, duplicate keys, more than one YAML document, and empty or null module or collector names are rejected.
  A collector listed without a value (`groups:`) is an error, not `false`.
- Durations are Go durations (`15s`, `192h`; there is no `d`). Durations and numbers must be greater than 0.
- `endpoint` must be a base URL: a path, query, fragment or user info is rejected.
- `user`, `token_name`, `token_value` and module names must not contain control characters, e.g. use `token_value: abc`,
  not a `|` block, which adds a line break.
- A token file must be a regular file of at most 4 KiB; one final line break and a byte order mark are removed.

Security:

- A module always scrapes its `endpoint` and never follows redirects, so its token is only sent to that server. Other URL
  parameters (like `target`) are ignored. Set the `instance` label in Prometheus explicitly (see
  [Prometheus](#prometheus)).
- The config file holds the API tokens unless every module uses `token_value_file`. Make it readable for the exporter's user
  only (`chmod 600`); the exporter warns on start when it is readable by others.

## API token and privileges

The exporter only reads. Create a user and an API token for it, and give the **token** (tokens have their own ACL entries):

| Path      | Role          | Propagate | Needed for                                                                                   |
| --------- | ------------- | --------- | -------------------------------------------------------------------------------------------- |
| `/`       | `Audit`       | yes       | Everything else: datastores, namespaces, snapshots, jobs, tasks, node and users.             |
| `/remote` | `RemoteAudit` | yes       | Sync jobs of remotes and the list of remotes (`Audit` does not include `Remote.Audit`).      |

```bash
proxmox-backup-manager user create monitoring@pbs
proxmox-backup-manager user generate-token monitoring@pbs exporter
proxmox-backup-manager acl update / Audit --auth-id 'monitoring@pbs!exporter'
proxmox-backup-manager acl update /remote RemoteAudit --auth-id 'monitoring@pbs!exporter'
```

Without some privileges, PBS does not refuse the requests but silently returns less. The exporter checks the token's
privileges (`/access/permissions`) and reports what is missing as failed collector with a log message:

- Without `Remote.Audit` on `/remote` (propagated), the sync jobs and remotes of the remotes the token can not audit are not
  listed. The `sync` collector exports what it sees and reports the missing privilege.
- Without `Sys.Audit` on `/system/tasks`, the task list only shows the token's own tasks; the `tasks` collector reports it.
- Without `Datastore.Audit` on `/datastore` (propagated), PBS leaves out the datastores, namespaces and jobs the token can
  not audit; the `datastores`, `groups`, `jobs` and `sync` collectors report the missing privilege.
- Without `Sys.Audit` on `/` itself, the subscription is listed without product name and due date; the `node` collector
  reports it.
- Without `Sys.Audit` on `/access/users`, the user list only shows the token's own user (`access` collector).

If `/access/permissions` can not be read, a complete list can not be told apart from a filtered one, so these collectors fail
as well.

## Collectors

The metrics are gathered by independent collectors that run concurrently. A collector that fails (e.g. a missing privilege
or a timeout) reports `pbs_collector_success{collector="..."} 0` and logs the API path, the HTTP status and the message of the
Proxmox Backup Server; the other collectors still report. `pbs_up` is 0 when the version call fails (no answer, an error status such as 401, or an answer without data).

| Collector    | Default | Metrics                                                                    | Privilege                                   |
| ------------ | ------- | -------------------------------------------------------------------------- | ------------------------------------------- |
| `version`    | on      | `pbs_version_info` (can not be switched off, sets `pbs_up`)                | none                                        |
| `node`       | on      | `pbs_node_*` (status, kernel, subscription, identity)                      | `Sys.Audit` on `/system/status`, and on `/` for the subscription |
| `datastores` | on      | `pbs_datastore_*`, `pbs_gc_*`                                              | `Datastore.Audit` on `/datastore`           |
| `groups`     | on      | `pbs_group_*`                                                              | `Datastore.Audit` on `/datastore`           |
| `jobs`       | on      | `pbs_job_*` of verify, prune and GC jobs, `pbs_verify_job_info`, `pbs_prune_job_info`, `pbs_prune_job_keep` | `Datastore.Audit` on `/datastore` |
| `sync`       | on      | `pbs_job_*` of sync jobs, `pbs_sync_job_info`, `pbs_remote_info`           | `Datastore.Audit` on `/datastore`, `Remote.Audit` on `/remote` |
| `tasks`      | on      | `pbs_tasks_running`                                                        | `Sys.Audit` on `/system/tasks`              |
| `zfs`        | on      | `pbs_zfs_pool_*`                                                           | `Sys.Audit` on `/system/disks`              |
| `time`       | on      | `pbs_node_time_seconds`                                                    | none                                        |
| `apt`        | on      | `pbs_apt_updates_available`                                                | `Sys.Audit` on `/`                          |
| `access`     | off     | `pbs_user_*`, `pbs_token_expiry_timestamp_seconds`                         | `Sys.Audit` on `/access/users`              |
| `disks`      | off     | `pbs_disk_*` (SMART)                                                       | `Sys.Audit` on `/system/disks`              |

Collectors are switched per module (or for all modules in `defaults`):

```yaml
defaults:
  collectors:
    access: true     # off by default
    apt: false       # on by default
```

Each response of the Proxmox Backup Server is fetched once per scrape, also when several collectors use it (e.g. the
datastore list by `datastores` and `groups`, the token's permissions by `groups`, `sync`, `tasks` and `access`).
Datastores that are not mounted, in a maintenance mode other than `read-only`, or that PBS reports an error for (see
`pbs_datastore_error`) are not scanned.

## Exported metrics

All metrics are gauges, with the values as the Proxmox Backup Server reports them: the exporter fills in no defaults and
filters no values. A value PBS does not report has no series (e.g. a timestamp that does not exist, or the usage of a
datastore that can not be read), and a setting PBS does not report is an empty label (e.g. `sync_direction`, `enabled`).

Labels used by many metrics:

| Label            | Meaning                                                                          |
| ---------------- | -------------------------------------------------------------------------------- |
| `datastore`      | Name of the datastore.                                                           |
| `datastore_namespace` | Namespace path inside the datastore, `""` for the root namespace.           |
| `backup_type`    | `vm`, `ct` or `host`.                                                            |
| `backup_id`      | ID of the backup group, e.g. the VM ID.                                          |

### Exporter

| Metric                           | Labels                     | Meaning                                                     |
| -------------------------------- | -------------------------- | ----------------------------------------------------------- |
| `pbs_up`                         |                            | 1 if the Proxmox Backup Server answered the version call.   |
| `pbs_info`                       | `endpoint_host`            | Host of the scraped server, always 1; the module is the target label set by Prometheus. |
| `pbs_node_info`                  | `fingerprint`              | SHA-256 fingerprint of the server's TLS certificate (lower case), always 1. It identifies the server however it is addressed, e.g. to match the `fingerprint` of `pbs_remote_info` on other servers. |
| `pbs_collector_success`          | `collector`                | 1 if the collector succeeded in this scrape.                |
| `pbs_collector_duration_seconds` | `collector`                | Duration of the collector in this scrape.                   |
| `pbs_version_info`               | `version`, `repoid`, `release` | Version of the Proxmox Backup Server, always 1.         |

### Node (`node`, `time`, `zfs`, `apt`, `disks`)

| Metric                                         | Labels                                | Meaning                                                                       |
| ---------------------------------------------- | ------------------------------------- | ----------------------------------------------------------------------------- |
| `pbs_node_cpu_usage_ratio`                     |                                       | CPU usage (0–1).                                                              |
| `pbs_node_io_wait_ratio`                       |                                       | I/O wait (0–1).                                                               |
| `pbs_node_load1`, `pbs_node_load5`, `pbs_node_load15` |                                | Load average.                                                                 |
| `pbs_node_memory_{free,used,size}_bytes`       |                                       | Memory.                                                                       |
| `pbs_node_swap_{free,used,size}_bytes`         |                                       | Swap.                                                                         |
| `pbs_node_rootfs_{available,used,size}_bytes`  |                                       | Root file system.                                                             |
| `pbs_node_uptime_seconds`                      |                                       | Uptime, as PBS reports it.                                                    |
| `pbs_node_subscription_info`                   | `productname`, `status`               | Subscription, always 1.                                                       |
| `pbs_node_subscription_due_timestamp_seconds`  |                                       | Next due date; omitted without one.                                           |
| `pbs_node_time_seconds`                        |                                       | The node's clock (`/nodes/localhost/time`); `pbs_node_time_seconds - timestamp(pbs_node_time_seconds)` is its offset from the scrape time. |
| `pbs_zfs_pool_health_info`                     | `pool`, `health`                      | Health PBS reports (`ONLINE`, `DEGRADED`, ...), always 1. PBS does not list pools it can not read the usage of (e.g. `FAULTED`, `UNAVAIL`): watch for pools that disappear. |
| `pbs_zfs_pool_{size,allocated}_bytes`          | `pool`                                | Size and allocated space.                                                     |
| `pbs_zfs_pool_fragmentation_ratio`             | `pool`                                | Fragmentation of the free space (0–1).                                        |
| `pbs_apt_updates_available`                    |                                       | Packages with a pending update (as of the node's last package list refresh).  |
| `pbs_node_kernel_info`                         | `release`                             | The running kernel (`current-kernel` of the node status), always 1.           |
| `pbs_disk_smart_status`                        | `disk`, `model`, `serial`, `status`   | SMART health as PBS reports it (`passed`, `failed`, `unknown`), always 1.    |
| `pbs_disk_wearout_percent`                     | `disk`                                | The `wearout` field PBS reports: remaining rated life of an SSD in percent (100 = new). |

### Datastores (`datastores`)

| Metric                                           | Labels                                     | Meaning                                                                 |
| ------------------------------------------------ | ------------------------------------------ | ----------------------------------------------------------------------- |
| `pbs_datastore_{available,used,size}_bytes`      | `datastore`                                | Usage of the underlying storage as PBS reports it (`available` may be negative); omitted when PBS can not read it. |
| `pbs_datastore_estimated_full_timestamp_seconds` | `datastore`                                | When PBS expects the datastore to be full (linear trend of the last 30 days); a date in the past means PBS has no estimate, compare with `time()`. |
| `pbs_datastore_error`                            | `datastore`                                | 1 if PBS reports an error for the datastore in `/status/datastore-usage` (e.g. it can not open it: failed storage, offline maintenance), 0 otherwise. |
| `pbs_datastore_mounted`                          | `datastore`                                | 1 if a removable datastore is mounted.                                  |
| `pbs_datastore_maintenance`                      | `datastore`, `mode`                        | Present while the datastore is in maintenance, always 1.                |
| `pbs_datastore_info`                             | `datastore`, `verify_new`, `notification_mode`, `gc_schedule`, `backend_type` | Settings, always 1. |
| `pbs_datastore_keep`                             | `datastore`, `keep`                        | Datastore-level retention (`keep` = `last`, `hourly`, `daily`, `weekly`, `monthly`, `yearly`). |
| `pbs_gc_{disk,index_data,pending,removed}_bytes` | `datastore`                                | Statistics of the last garbage collection (0 before the first one); index data ÷ disk bytes is the deduplication factor. |
| `pbs_gc_bad_chunks`                              | `datastore`, `state`                       | Corrupt chunks found by the last garbage collection: `still_bad` (still referenced), `removed_bad`. |

### Backup groups (`groups`)

All with `datastore`, `datastore_namespace`, `backup_type` and `backup_id`. "Finished" snapshots are the ones with a
manifest; PBS lists a running backup without one. When the snapshot list of a namespace can not be read, its groups have
only `pbs_group_snapshots` and `pbs_group_info`, and the collector fails.

| Metric                                              | Extra labels         | Meaning                                                                    |
| --------------------------------------------------- | -------------------- | -------------------------------------------------------------------------- |
| `pbs_group_snapshots`                               |                      | Number of snapshots (groups without snapshots are included).              |
| `pbs_group_info`                                    | `owner`, `comment`   | Owner and the first line of the group notes, always 1.                     |
| `pbs_group_last_backup_timestamp_seconds`           |                      | Time of the newest finished snapshot.                                      |
| `pbs_group_oldest_backup_timestamp_seconds`         |                      | Time of the oldest finished snapshot.                                      |
| `pbs_group_last_backup_size_bytes`                  |                      | Size of the newest finished snapshot.                                      |
| `pbs_group_last_backup_verify_state`                | `state`              | Verification of the newest finished snapshot (`ok`, `failed`; empty when never verified), always 1. |
| `pbs_group_snapshots_by_verify_state`               | `state`              | Snapshots per verification state (`ok`, `failed`; empty when never verified); omitted for states without snapshots. |
| `pbs_group_last_verify_timestamp_seconds`           |                      | Start of the newest verification of any snapshot.                          |
| `pbs_group_protected_snapshots`                     |                      | Protected snapshots; omitted when there are none.                         |

Join the group metrics with their notes, e.g. for alerts:

```promql
(time() - pbs_group_last_backup_timestamp_seconds)
  * on (instance, datastore, datastore_namespace, backup_type, backup_id) group_left (comment) pbs_group_info
```

### Jobs (`jobs`, `sync`)

The run metrics have the labels `job_type` (`verify`, `prune`, `gc`, `sync`), `job_id` (for GC: the datastore), `datastore`
and `datastore_namespace`. They are the job state PBS reports: while a job runs, its last run is the running one, which has a
start but no state and no end yet.

| Metric                                         | Labels     | Meaning                                                         |
| ---------------------------------------------- | ---------- | --------------------------------------------------------------- |
| `pbs_job_last_run_success`                     | job labels | 1 if the last run ended without error (`last-run-state` OK or with warnings); omitted while it runs. |
| `pbs_job_last_run_start_timestamp_seconds`     | job labels | Start of the last run, running or finished (from `last-run-upid`; for GC only after a successful run, as PBS names only the last successful GC). |
| `pbs_job_last_run_end_timestamp_seconds`       | job labels | End of the last run (`last-run-endtime`); omitted while it runs. |
| `pbs_job_next_run_timestamp_seconds`           | job labels | Next scheduled run as computed by PBS; it stays in the past when a scheduled run did not start. |
| `pbs_verify_job_info`                          | `job_id`, `datastore`, `datastore_namespace`, `outdated_after`, `ignore_verified`, `max_depth`, `schedule` | Settings, always 1. |
| `pbs_prune_job_info`                           | `job_id`, `datastore`, `datastore_namespace`, `max_depth`, `schedule`, `disable` | Settings, always 1. |
| `pbs_prune_job_keep`                           | `job_id`, `datastore`, `datastore_namespace`, `keep` | Retention of a prune job.                   |
| `pbs_sync_job_info`                            | `job_id`, `datastore`, `datastore_namespace`, `remote`, `remote_store`, `remote_ns`, `sync_direction`, `max_depth`, `remove_vanished`, `transfer_last`, `group_filter` (sorted), `verified_only`, `encrypted_only`, `schedule` | Settings, always 1. `datastore`/`datastore_namespace` are local; `sync_direction` is empty when PBS reports none (PBS treats that as `pull`). |
| `pbs_remote_info`                              | `remote`, `host`, `port`, `fingerprint`, `auth_id` | Configured remotes, always 1. `fingerprint` (lower case) is empty when the remote's certificate is trusted otherwise. |

### Tasks and access (`tasks`, `access`)

| Metric                               | Labels                      | Meaning                                                                                   |
| ------------------------------------ | --------------------------- | ----------------------------------------------------------------------------------------- |
| `pbs_tasks_running`                  | `worker_type`               | Running tasks.                                                                            |
| `pbs_user_info`                      | `userid`, `enabled`         | Users, always 1. `enabled` is empty when PBS reports no `enable` setting (PBS treats that as enabled). |
| `pbs_user_expiry_timestamp_seconds`  | `userid`                    | `expire` setting of the user as PBS reports it (`0` = never); omitted when not set.     |
| `pbs_token_expiry_timestamp_seconds` | `tokenid`                   | `expire` setting of the API token as PBS reports it (`0` = never); omitted when not set. |

Comments of users and tokens are never exported. The exporter's own `go_*`, `process_*` and `promhttp_*` metrics are on
`/exporter-metrics`, not in the module scrapes, so they are scraped once per exporter.

Derived values are not exported: the snapshots of a namespace are `sum by (datastore, datastore_namespace)
(pbs_group_snapshots)`, the duration of a job's last run is `pbs_job_last_run_end_timestamp_seconds -
pbs_job_last_run_start_timestamp_seconds`, and the subscription status is the `status` label of
`pbs_node_subscription_info`. Nothing is inferred either: values are what the API returns, re-encoded (e.g. a state as 0/1)
or counted over one API list (e.g. snapshots per verification state, only states that occur); the exporter reads no task logs, keeps no state
between scrapes and never falls back to a guessed value. The `groups` collector skips only the datastores PBS reports as
unmounted, failed or in a maintenance mode that blocks reading; any other error while reading a datastore is a failure of the
collector, and without the maintenance modes (`/admin/datastore`) it reads no datastore.

## Prometheus

Each scrape reads the current state from the Proxmox Backup Server; nothing is cached. A scrape is bounded by the scrape
timeout Prometheus sends (`X-Prometheus-Scrape-Timeout-Seconds`) minus `scrape_timeout_offset`: requests still running then
are cancelled, their collectors report `pbs_collector_success 0`, and the exporter answers with what it has. Scrapes of large
datastores take a few seconds (mostly the snapshot lists, which PBS reads from disk), so scrape rarely, e.g. every 1–5 minutes
with a `scrape_timeout` of 30 s or more. `timeout` of a module applies to each single request; raise it if listing the
snapshots of a namespace with many thousands of snapshots takes longer.

One target per module, with the module as URL parameter and the server as `instance` label:

```yaml
scrape_configs:
- job_name: 'pbs-exporter'
  scrape_interval: 2m
  scrape_timeout: 60s
  static_configs:
  - targets: ['https://10.10.10.10:8007']   # endpoint of module site-a
    labels:
      module: site-a
  - targets: ['https://10.10.10.11:8007']   # endpoint of module site-b
    labels:
      module: site-b
  relabel_configs:
  - source_labels: [module]
    target_label: __param_module
  - source_labels: [__address__]
    target_label: instance
  - target_label: __address__
    replacement: pbs-exporter:10019          # address of the exporter
```

More examples are in [prometheus/static-config](prometheus/static-config), example alerts in
[prometheus/rules/example-rules.yaml](prometheus/rules/example-rules.yaml), and a Grafana dashboard in
[grafana-dashboard](grafana-dashboard).

## Running the exporter

### Docker Compose

[docker-compose.yaml](docker-compose.yaml) mounts `pbs-exporter.yml` next to it as config file;
[docker-compose-secrets.yaml](docker-compose-secrets.yaml) additionally passes the API tokens as Docker secrets
(`token_value_file: /run/secrets/...`). The container runs as user 65534, which must be able to read the config file and the
secrets.

### systemd

The exporter can also run directly on a Proxmox Backup Server:

```bash
# Download the exporter
mkdir /opt/pbs-exporter
cd /opt/pbs-exporter
wget https://github.com/natrontech/pbs-exporter/releases/download/<version>/pbs-exporter_<version>_linux_amd64.tar.gz
tar xfvz pbs-exporter_<version>_linux_amd64.tar.gz

# Create a dedicated user
useradd -m pbs-exporter -s /sbin/nologin

# Write the config file (see pbs-exporter.example.yml), readable for the exporter only
mkdir -p /etc/pbs-exporter
vi /etc/pbs-exporter/config.yml
chown pbs-exporter /etc/pbs-exporter/config.yml
chmod 600 /etc/pbs-exporter/config.yml

# Install the systemd unit and start the service
wget -O /etc/systemd/system/prometheus-pbs-exporter.service \
  https://raw.githubusercontent.com/natrontech/pbs-exporter/refs/heads/main/systemd/prometheus-pbs-exporter.service
systemctl daemon-reload
systemctl enable --now prometheus-pbs-exporter.service
```

On the server itself, use `endpoint: https://localhost:8007` with `insecure: true` (the certificate is issued for the host
name), or the host name the certificate is issued for.

## Node metrics

According to the [API documentation](https://pbs.proxmox.com/docs/api-viewer/index.html#/nodes/{node}), a node name has to be
given (the node IP does not work), but any name seems to work, so the exporter uses `localhost`. This setup is tested with one
Proxmox Backup Server host.

## Supported versions

The exporter is tested with Proxmox Backup Server **4.2**. The sync job list needs PBS 3.3 or newer (`sync-direction`), the
garbage collection job state PBS 3.2 or newer; on older versions the `sync` and `jobs` collectors report failures. If you have
tested the exporter with another version, or have encountered problems, please let us know.

## Release

Each release of the application includes Go-binary archives, checksums file, SBOMs and container images.

The release workflow creates provenance for its builds using the [SLSA standard](https://slsa.dev), which conforms to the [Level 3 specification](https://slsa.dev/spec/v1.2/build-track-basics#build-l3). Each artifact can be verified using the `slsa-verifier` or `cosign` tool (see [Release verification](SECURITY.md#release-verification)).
