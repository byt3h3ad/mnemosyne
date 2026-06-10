# mnemosyne

Archives your [Raindrop.io](https://raindrop.io) bookmarks to the [Wayback Machine](https://web.archive.org) and writes the archive URL back as a note on each bookmark.

## How it works

```
Raindrop.io ──fetch──► archiver ──submit──► Wayback Machine
                           │                      │
                           │      ◄──archive URL──┘
                           │
                           └──append note──► Raindrop.io
```

On the **first run** every existing bookmark is archived. On **subsequent runs** only bookmarks created since the last run are processed — previously failed bookmarks are skipped unless `--retry-failed` is passed. State is persisted in a local SQLite database so runs are resumable and idempotent.

## High-level design

```
cmd/
  mnemosyne/main.go       entry point — parses flags, wires dependencies, runs the pipeline

internal/
  config/                 loads and validates config.yaml; applies defaults
  db/                     SQLite schema, migrations, and all query helpers
                          tables: archived_bookmarks, run_state
  raindrop/               Raindrop REST API client
                          FetchAll / FetchSince — paginated bookmark retrieval (rate-limited)
                          AppendNote            — GET existing note, append archive URL, PUT back (idempotent)
  wayback/                Wayback Machine SPN2 API client
                          Archive — submit URL, poll until success/error/timeout
                          typed errors: PermanentError (skip forever) / TransientError (retry with --retry-failed)
  archiver/               orchestration — runs the four pipeline stages in order
                          Run(retryFailed) — full pipeline: fetch → archive → sync back → save state
                          SyncBack         — sync-only mode: write archive URLs to notes, skip archiving
```

### Pipeline stages

| Stage | What happens |
|---|---|
| **Fetch** | Pull bookmarks from Raindrop (all on first run, incremental after) and upsert into DB as `pending` |
| **Archive** | For each `pending` row, submit to Wayback Machine and poll for result. Mark `archived`, `failed_permanent`, or `failed_transient` |
| **Sync back** | For each `archived` + `synced_back=0` row, append the archive URL to the Raindrop note |
| **Finalise** | Write `last_run_at` and `first_run=0` to DB |

### Retry behaviour

| Status | Default run | `--retry-failed` run |
|---|---|---|
| `failed_transient` | Skipped | Reset to `pending` and retried |
| `failed_permanent` | Skipped forever | Skipped forever |
| `archived`, `synced_back=0` | Sync-back retried | Sync-back retried |
| `archived`, `synced_back=-1` | Sync-back skipped forever¹ | Sync-back skipped forever¹ |

¹ Set when the sync-back can never succeed: the bookmark was deleted in Raindrop, or its note is already at the 10,000-character limit.

## Prerequisites

- Go 1.21+
- A [Raindrop.io](https://app.raindrop.io/settings/integrations) test token
- An [Internet Archive](https://archive.org/account/s3.php) S3 API key pair

## Setup

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` and fill in your credentials:

```yaml
raindrop_token: "your-raindrop-test-token"
wayback_access_key: "your-ia-access-key"
wayback_secret_key: "your-ia-secret-key"
db_path: "./archive.db"
rate_limit_ms: 2000             # delay between API submissions (ms)
skip_archived_within_days: 90   # reuse existing captures this recent (0 = off)
```

When `skip_archived_within_days` is set, each URL is first checked against the
[Wayback Availability API](https://archive.org/help/wayback_api.php). If a
capture newer than the limit already exists, its URL is reused instead of
making a new capture — on a first run over an old collection this saves most
of your capture quota.

## Build

**Windows**
```powershell
go build -o mnemo.exe ./cmd/mnemosyne/
```

**Linux**
```bash
go build -o mnemo ./cmd/mnemosyne/
```

**macOS**
```bash
go build -o mnemo ./cmd/mnemosyne/
```

## Usage

```bash
# Full run — archives new bookmarks only
./mnemo.exe

# Preview what a run would do without writing anything
./mnemo.exe --dry-run

# Also retry previously failed (transient) bookmarks
./mnemo.exe --retry-failed

# Sync archive URLs to Raindrop notes only (skips archiving)
./mnemo.exe --sync-only

# Show DB state (no API calls)
./mnemo.exe status

# Custom config path
./mnemo.exe --config /path/to/config.yaml
```

### Status output

```
Pending:             0
Archived:          139
  synced back:     135
  unsynced:          3
  unsyncable:        1
Failed permanent:    2
Failed transient:    1
Last run:          2026-06-08T09:00:00Z
```

### Example output

```
Fetched:      142 bookmarks
Archived:     139
Failed:          3  (2 permanent, 1 transient)
Synced back:  139
```

## Releases

Pre-built binaries for Windows, Linux, and macOS are published automatically to GitHub Releases whenever a version tag is pushed.

### Creating a release

1. Make sure all changes are committed and pushed to `main`.

2. Tag the commit with a version number following [semver](https://semver.org):
   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```

3. GitHub Actions will build all five binaries and publish the release automatically. You can follow the progress under the **Actions** tab on GitHub. Once complete, the release appears under **Releases** with the following assets:

   | File | Platform |
   |---|---|
   | `mnemo-windows-amd64.exe` | Windows (64-bit) |
   | `mnemo-linux-amd64` | Linux (64-bit) |
   | `mnemo-linux-arm64` | Linux (ARM64) |
   | `mnemo-darwin-amd64` | macOS (Intel) |
   | `mnemo-darwin-arm64` | macOS (Apple Silicon) |

### Downloading a release

Go to the **Releases** page on GitHub, pick the latest version, and download the binary for your platform. On Linux and macOS, mark it executable before running:

```bash
chmod +x mnemo-linux-amd64
./mnemo-linux-amd64 --config ./config.yaml
```

### If the workflow fails

- **Permission denied on release creation:** Go to `Settings → Actions → General` on your GitHub repo and make sure **Workflow permissions** is set to *Read and write permissions*.
- **Build errors:** Check the **Actions** tab for the full log. Each platform builds independently, so a failure on one does not block the others.

## Scheduling

Run weekly as a cron job (Linux/macOS):

```cron
0 9 * * 1 /path/to/archiver >> /var/log/archiver.log 2>&1
```

On Windows, use Task Scheduler to run `mnemo.exe` on a weekly trigger.
