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
their snapshots, verify, prune, garbage collection and sync jobs, the task log, datastores, ZFS pools, node health and more.
One exporter can monitor any number of Proxmox Backup Servers, each with its own API token.

- [Quick start](#quick-start)
- [Configuration](#configuration)
- [API token and privileges](#api-token-and-privileges)
- [Collectors](#collectors)
- [Exported metrics](#exported-metrics)
- [Prometheus](#prometheus)
- [Running the exporter](#running-the-exporter)
- [Upgrading from earlier versions](#upgrading-from-earlier-versions)

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
| `listen_address`        | `:10019`   | Address the HTTP server listens on, `[host]:port`.                                                           |
| `metrics_path`          | `/metrics` | Path of the metrics, e.g. `/metrics` or `/pbs/metrics` (letters, digits and `. _ ~ -` between slashes).     |
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
| `user`             | (required)     | User the API token belongs to, e.g. `monitoring@pbs`.                                                       |
| `token_name`       | (required)     | Name of the API token.                                                                                       |
| `token_value`      | (required¹)    | Secret of the API token.                                                                                     |
| `token_value_file` | (required¹)    | File that holds the secret of the API token, e.g. a Docker or Kubernetes secret; read on start.            |
| `insecure`         | `false`        | Skip TLS certificate verification.                                                                           |
| `timeout`          | `5s`           | Timeout of each request to the Proxmox Backup Server.                                                       |
| `max_concurrency`  | `4`            | Concurrent requests to the Proxmox Backup Server, over all scrapes of the module.                          |
| `task_lookback`    | `192h`         | How far back the task log is read (long enough to see the last run of a weekly job).                       |
| `task_limit`       | `10000`        | Maximum number of tasks read from the task log; the exporter warns when it is reached.                     |
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
  parameters (like `target` of earlier versions) are ignored. Set the `instance` label in Prometheus explicitly (see
  [Prometheus](#prometheus)).
- The config file holds the API tokens unless every module uses `token_value_file`. Make it readable for the exporter's user
  only (`chmod 600`); the exporter warns on start when it is readable by others.

## API token and privileges

The exporter only reads. Create a user and an API token for it, and give the **token** (tokens have their own ACL entries):

| Path      | Role          | Propagate | Needed for                                                                                   |
| --------- | ------------- | --------- | -------------------------------------------------------------------------------------------- |
| `/`       | `Audit`       | yes       | Everything else: datastores, namespaces, snapshots, jobs, the task log, node and users.      |
| `/remote` | `RemoteAudit` | yes       | Sync jobs of remotes and the list of remotes (`Audit` does not include `Remote.Audit`).      |

```bash
proxmox-backup-manager user create monitoring@pbs
proxmox-backup-manager user generate-token monitoring@pbs exporter
proxmox-backup-manager acl update / Audit --auth-id 'monitoring@pbs!exporter'
proxmox-backup-manager acl update /remote RemoteAudit --auth-id 'monitoring@pbs!exporter'
```

Without some privileges, PBS does not refuse the requests but silently returns less. The exporter checks the token's
privileges (`/access/permissions`) and reports what is missing as failed collector with a log message:

- Without `Remote.Audit` on `/remote` (propagated), sync jobs of remotes are not listed. Their last run is then taken from the
  task log (`source="tasks"`), and the `sync` collector reports the missing privilege.
- Without `Sys.Audit` on `/system/tasks`, the task log only shows the token's own tasks; the collectors that use it report it.
- Without `Datastore.Audit` on a whole datastore, PBS leaves out the namespaces the token can not see; backup tasks are then not
  matched to groups of that datastore, and the `groups` collector reports the missing privilege.
- Without `Sys.Audit` on `/access/users`, the user list only shows the token's own user (`access` collector).

## Collectors

The metrics are gathered by independent collectors that run concurrently. A collector that fails (e.g. a missing privilege
or a timeout) reports `pbs_collector_success{collector="..."} 0` and logs the API path, the HTTP status and the message of the
Proxmox Backup Server; the other collectors still report. `pbs_up` is 0 only when the server does not answer the version call.

| Collector    | Default | Metrics                                                                    | Privilege                                   |
| ------------ | ------- | -------------------------------------------------------------------------- | ------------------------------------------- |
| `version`    | on      | `pbs_version` (can not be switched off, sets `pbs_up`)                     | none                                        |
| `node`       | on      | `pbs_host_*`                                                               | `Sys.Audit` on `/system`                    |
| `datastores` | on      | `pbs_datastore_*`, `pbs_gc_*`                                              | `Datastore.Audit` on `/datastore`           |
| `snapshots`  | on      | `pbs_snapshot_count`                                                       | `Datastore.Audit` on `/datastore`           |
| `groups`     | on      | `pbs_group_*`                                                              | `Datastore.Audit`; `Sys.Audit` on `/system/tasks` for the backup tasks |
| `jobs`       | on      | `pbs_job_*` of verify, prune and GC jobs, `pbs_verify_job_info`, `pbs_prune_job_keep` | `Datastore.Audit` on `/datastore`  |
| `sync`       | on      | `pbs_job_*` of sync jobs, `pbs_sync_job_info`, `pbs_remote_info`           | `Datastore.Audit`, `Remote.Audit` on `/remote` |
| `tasks`      | on      | `pbs_tasks_running`, `pbs_task_failures`                                   | `Sys.Audit` on `/system/tasks`              |
| `zfs`        | on      | `pbs_zfs_pool_*`                                                           | `Sys.Audit` on `/system/disks`              |
| `time`       | on      | `pbs_node_time_offset_seconds`                                             | none                                        |
| `apt`        | on      | `pbs_apt_updates_available`, `pbs_reboot_required`                         | `Sys.Audit` on `/`                          |
| `access`     | off     | `pbs_user_*`, `pbs_token_expiry_timestamp_seconds`, `pbs_reader_tasks`     | `Sys.Audit` on `/access/users` and `/system/tasks` |
| `disks`      | off     | `pbs_disk_*` (SMART)                                                       | `Sys.Audit` on `/system/disks`              |

Collectors are switched per module (or for all modules in `defaults`):

```yaml
defaults:
  collectors:
    access: true     # off by default
    apt: false       # on by default
```

Each response of the Proxmox Backup Server is fetched once per scrape, also when several collectors use it (e.g. the
snapshot list of a namespace by `snapshots` and `groups`, the task log by `groups`, `sync`, `jobs`, `tasks` and `access`).
Datastores that are not mounted or in a maintenance mode other than `read-only` are not scanned.

## Exported metrics

All metrics are gauges. Values the Proxmox Backup Server does not report are omitted rather than exported as 0, e.g. a
timestamp that does not exist (0 would read as 1970) or the usage of a datastore that can not be read.

Labels used by many metrics:

| Label            | Meaning                                                                          |
| ---------------- | -------------------------------------------------------------------------------- |
| `datastore`      | Name of the datastore.                                                           |
| `pbs_namespace`  | Namespace path, `""` for the root namespace.                                     |
| `namespace_root` | First component of the namespace, e.g. `site-a` for `site-a/vms`.                |
| `backup_type`    | `vm`, `ct` or `host`.                                                            |
| `backup_id`      | ID of the backup group, e.g. the VM ID.                                          |

### Exporter

| Metric                           | Labels                     | Meaning                                                     |
| -------------------------------- | -------------------------- | ----------------------------------------------------------- |
| `pbs_up`                         |                            | 1 if the Proxmox Backup Server answered the version call.   |
| `pbs_info`                       | `module`, `endpoint_host`  | Module and host of the scraped server, always 1.            |
| `pbs_collector_success`          | `collector`                | 1 if the collector succeeded in this scrape.                |
| `pbs_collector_duration_seconds` | `collector`                | Duration of the collector in this scrape.                   |
| `pbs_version`                    | `version`, `repoid`, `release` | Version of the Proxmox Backup Server, always 1.         |

### Node (`node`, `time`, `zfs`, `apt`, `disks`)

| Metric                                         | Labels                                | Meaning                                                                       |
| ---------------------------------------------- | ------------------------------------- | ----------------------------------------------------------------------------- |
| `pbs_host_cpu_usage_ratio`                     |                                       | CPU usage (0–1).                                                              |
| `pbs_host_io_wait_ratio`                       |                                       | I/O wait (0–1).                                                               |
| `pbs_host_load1`, `pbs_host_load5`, `pbs_host_load15` |                                | Load average.                                                                 |
| `pbs_host_memory_{free,used,total}_bytes`      |                                       | Memory.                                                                       |
| `pbs_host_swap_{free,used,total}_bytes`        |                                       | Swap.                                                                         |
| `pbs_host_disk_{available,used,total}_bytes`   |                                       | Root file system.                                                             |
| `pbs_host_uptime_seconds`                      |                                       | Uptime.                                                                       |
| `pbs_host_subscription_info`                   | `productname`, `status`               | Subscription, always 1.                                                       |
| `pbs_host_subscription_status`                 | `status`                              | 1 for the current status, 0 for `new`, `notfound`, `active`, `invalid`, `expired`, `suspended`. |
| `pbs_host_subscription_due_timestamp_seconds`  | `productname`                         | Next due date; omitted without one.                                           |
| `pbs_node_time_offset_seconds`                 |                                       | Clock of the node minus the clock of the exporter (about ±0.5 s plus half the round trip). |
| `pbs_zfs_pool_healthy`                         | `pool`, `health`                      | 1 if the pool is `ONLINE`. PBS does not list pools it can not read the usage of (e.g. `FAULTED`, `UNAVAIL`): watch for pools that disappear. |
| `pbs_zfs_pool_{size,allocated}_bytes`          | `pool`                                | Size and allocated space.                                                     |
| `pbs_zfs_pool_fragmentation_ratio`             | `pool`                                | Fragmentation of the free space (0–1).                                        |
| `pbs_apt_updates_available`                    |                                       | Packages with a pending update (as of the node's last package list refresh).  |
| `pbs_reboot_required`                          |                                       | 1 if a newer kernel than the running one is installed.                        |
| `pbs_disk_smart_status`                        | `disk`, `model`, `serial`, `status`   | SMART health (`passed`, `failed`, `unknown`), always 1.                      |
| `pbs_disk_wearout_ratio`                       | `disk`                                | Used up share of the SSD's rated life (0 = new), as the PBS web interface shows it. |

### Datastores (`datastores`, `snapshots`)

| Metric                                           | Labels                                     | Meaning                                                                 |
| ------------------------------------------------ | ------------------------------------------ | ----------------------------------------------------------------------- |
| `pbs_datastore_{available,used,size}_bytes`      | `datastore`                                | Usage of the underlying storage; omitted when PBS can not read it.     |
| `pbs_datastore_estimated_full_timestamp_seconds` | `datastore`                                | When PBS expects the datastore to be full (linear trend of the last 30 days); omitted without an estimate or when it is in the past. |
| `pbs_datastore_mounted`                          | `datastore`                                | 1 if a removable datastore is mounted.                                  |
| `pbs_datastore_maintenance`                      | `datastore`, `mode`                        | Present while the datastore is in maintenance, always 1.                |
| `pbs_datastore_info`                             | `datastore`, `verify_new`, `notification_mode`, `gc_schedule`, `backend_type` | Settings, always 1. |
| `pbs_datastore_keep`                             | `datastore`, `keep`                        | Datastore-level retention (`keep` = `last`, `hourly`, `daily`, `weekly`, `monthly`, `yearly`). |
| `pbs_gc_{disk,index_data,pending,removed}_bytes` | `datastore`                                | Statistics of the last garbage collection; index data ÷ disk bytes is the deduplication factor. |
| `pbs_gc_bad_chunks`                              | `datastore`, `state`                       | Corrupt chunks found by the last garbage collection: `still_bad` (still referenced), `removed_bad`. |
| `pbs_snapshot_count`                             | `datastore`, `pbs_namespace`, `namespace_root` | Snapshots per namespace.                                            |

### Backup groups (`groups`)

All with `datastore`, `pbs_namespace`, `namespace_root`, `backup_type` and `backup_id`. "Finished" snapshots are the ones with a
manifest; PBS lists a running backup without one.

| Metric                                              | Extra labels         | Meaning                                                                    |
| --------------------------------------------------- | -------------------- | -------------------------------------------------------------------------- |
| `pbs_group_snapshots`                               |                      | Number of snapshots (groups without snapshots are included).              |
| `pbs_group_info`                                    | `owner`, `comment`   | Owner, and the group notes or else the notes of the newest snapshot, always 1. |
| `pbs_group_last_backup_timestamp_seconds`           |                      | Time of the newest finished snapshot.                                      |
| `pbs_group_oldest_backup_timestamp_seconds`         |                      | Time of the oldest finished snapshot.                                      |
| `pbs_group_last_backup_size_bytes`                  |                      | Size of the newest finished snapshot.                                      |
| `pbs_group_last_backup_verify_state`                | `state`              | Verification of the newest finished snapshot (`ok`, `failed`, `none`), always 1. |
| `pbs_group_snapshots_by_verify_state`               | `state`              | Snapshots per verification state.                                          |
| `pbs_group_last_verify_timestamp_seconds`           |                      | Start of the newest verification of any snapshot.                          |
| `pbs_group_protected_snapshots`                     |                      | Protected snapshots.                                                       |
| `pbs_group_last_backup_encrypted`                   |                      | 1 if all archives of the newest snapshot are encrypted (`sign-only` is not). |
| `pbs_group_backup_interval_seconds`                 |                      | Typical time between snapshots: median of the gaps between the newest 4; omitted below 3 snapshots. |
| `pbs_group_last_backup_task_success`                |                      | 1 if the newest finished backup task ended without error.                  |
| `pbs_group_last_backup_task_end_timestamp_seconds`  |                      | End of that task.                                                          |
| `pbs_group_last_backup_duration_seconds`            |                      | Duration of that task.                                                     |

Backup task worker IDs carry no namespace: a task is attributed to the group its user can write to (the owner, or the user of
an owning token), and left out when that is ambiguous. Tasks are from the task log, so they are only seen within
`task_lookback` and the server's task log retention.

### Jobs (`jobs`, `sync`)

The run metrics have the labels `job_type` (`verify`, `prune`, `gc`, `sync`), `job_id` (for GC: the datastore), `datastore`,
`pbs_namespace` and `source` (`config`, or `tasks` for sync jobs the token can not read).

| Metric                                     | Labels                                                                 | Meaning                                                         |
| ------------------------------------------ | ---------------------------------------------------------------------- | --------------------------------------------------------------- |
| `pbs_job_last_run_success`                 | job labels                                                             | 1 if the last run ended without error (OK or with warnings).    |
| `pbs_job_last_run_end_timestamp_seconds`   | job labels                                                             | End of the last run.                                            |
| `pbs_job_last_run_duration_seconds`        | job labels                                                             | Duration of the last run.                                       |
| `pbs_job_next_run_timestamp_seconds`       | job labels                                                             | Next scheduled run.                                             |
| `pbs_job_interval_seconds`                 | job labels                                                             | Next run minus the start of the last run.                       |
| `pbs_verify_job_info`                      | `job_id`, `datastore`, `pbs_namespace`, `outdated_after`, `ignore_verified`, `max_depth`, `schedule` | Settings, always 1. |
| `pbs_prune_job_keep`                       | `job_id`, `datastore`, `pbs_namespace`, `keep`                         | Retention of a prune job.                                       |
| `pbs_sync_job_info`                        | `job_id`, `datastore`, `pbs_namespace`, `source`, `remote`, `remote_store`, `remote_ns`, `sync_direction`, `remove_vanished`, `transfer_last`, `group_filter`, `schedule` | Settings, always 1. |
| `pbs_remote_info`                          | `remote`, `host`                                                       | Configured remotes, always 1.                                   |

While a job runs, PBS reports no state of its last run; it is then taken from the task log.

### Tasks and access (`tasks`, `access`)

| Metric                               | Labels                      | Meaning                                                                                   |
| ------------------------------------ | --------------------------- | ----------------------------------------------------------------------------------------- |
| `pbs_tasks_running`                  | `worker_type`               | Running tasks.                                                                            |
| `pbs_task_failures`                  | `worker_type`, `datastore`  | Tasks that ended with an error in the last 24 hours (warnings do not count).             |
| `pbs_user_info`                      | `userid`, `enabled`         | Users, always 1.                                                                          |
| `pbs_user_expiry_timestamp_seconds`  | `userid`                    | Expiry of enabled users; omitted when they never expire.                                 |
| `pbs_token_expiry_timestamp_seconds` | `tokenid`                   | Expiry of enabled API tokens; omitted when they never expire.                            |
| `pbs_reader_tasks`                   | `userid`                    | Reader tasks (restores, and syncs from other servers) started in the last 24 hours.      |

Comments of users and tokens are never exported. The exporter also exposes its own `go_*`, `process_*` and
`promhttp_*` metrics.

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

## Upgrading from earlier versions

This version has breaking changes in the configuration and the metrics.

### Configuration

Flags and environment variables were removed; everything is configured in the [config file](#configuration). A set
`PBS_*` environment variable is reported on start.

| Earlier flag / environment variable                     | Now                                                      |
| ------------------------------------------------------- | -------------------------------------------------------- |
| `pbs.config-file` / `PBS_CONFIG_FILE`                   | `-config.file`                                           |
| `pbs.endpoint` / `PBS_ENDPOINT`                         | `modules.<name>.endpoint`                                |
| `pbs.username` / `PBS_USERNAME(_FILE)`                  | `user`                                                   |
| `pbs.api.token.name` / `PBS_API_TOKEN_NAME(_FILE)`      | `token_name`                                             |
| `pbs.api.token` / `PBS_API_TOKEN`, `PBS_API_TOKEN_FILE` | `token_value`, `token_value_file`                        |
| `pbs.timeout`, `pbs.insecure`                           | `timeout`, `insecure`                                    |
| `pbs.listen-address`, `pbs.metrics-path`, `pbs.loglevel` | `server.listen_address`, `server.metrics_path`, `server.log_level` |
| `target` URL parameter                                  | one module per server, `module` URL parameter            |
| config file with modules at the top level               | modules under `modules:`                                 |

The default `user` (`root@pam`) and `token_name` (`pbs-exporter`) of earlier versions are gone: set them explicitly.

### Metrics

| Earlier                                                          | Now                                                                 |
| ---------------------------------------------------------------- | ------------------------------------------------------------------- |
| `pbs_snapshot_vm_count`                                          | `pbs_group_snapshots`                                               |
| `pbs_snapshot_vm_last_timestamp`                                 | `pbs_group_last_backup_timestamp_seconds`                           |
| `pbs_snapshot_vm_last_verify` (1/0)                              | `pbs_group_last_backup_verify_state{state="ok"\|"failed"\|"none"}` |
| label `vm_id`                                                    | `backup_id` (together with `backup_type`)                           |
| label `vm_name`                                                  | `comment` of `pbs_group_info`                                       |
| label `namespace`                                                | `pbs_namespace` (and `namespace_root`)                              |
| `pbs_available`, `pbs_size`, `pbs_used`                          | `pbs_datastore_available_bytes`, `pbs_datastore_size_bytes`, `pbs_datastore_used_bytes` |
| `pbs_host_memory_*`, `pbs_host_swap_*`, `pbs_host_disk_*`        | the same names with `_bytes`                                        |
| `pbs_host_uptime`                                                | `pbs_host_uptime_seconds`                                           |
| `pbs_host_cpu_usage`, `pbs_host_io_wait`                         | `pbs_host_cpu_usage_ratio`, `pbs_host_io_wait_ratio`                |
| `pbs_up` 0 on any error                                          | `pbs_up` 0 only when the version call fails; see `pbs_collector_success` |

Join the group metrics with their notes, e.g. for alerts:

```promql
(time() - pbs_group_last_backup_timestamp_seconds)
  * on (instance, datastore, pbs_namespace, backup_type, backup_id) group_left (comment) pbs_group_info
```

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
