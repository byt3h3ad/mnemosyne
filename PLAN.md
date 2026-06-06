# Raindrop Archiver — Implementation Plan

A Go service that archives Raindrop.io bookmarks to the Wayback Machine and writes the archive URL back as a note on each bookmark.

---

## Overview

- First run: archives all existing bookmarks
- Subsequent runs (weekly cron): archives only bookmarks created since the last run
- After archiving, writes the Wayback Machine URL back to the Raindrop bookmark's note field
- State is tracked in a local SQLite database

---

## Authentication

### Raindrop.io
- Generate a **test token** at `https://app.raindrop.io/settings/integrations`
- Test tokens do not expire — no OAuth flow needed for a personal tool
- All requests use: `Authorization: Bearer <token>`
- Rate limit: 120 requests/minute

### Wayback Machine (SPN2 API)
- Create a free account at `https://archive.org`
- Generate S3 API keys at `https://archive.org/account/s3.php`
- All requests use: `Authorization: LOW <access_key>:<secret_key>`
- Unauthenticated requests do not reliably work

---

## Config (`config.yaml`)

```yaml
raindrop_token: ""
wayback_access_key: ""
wayback_secret_key: ""
db_path: "./archive.db"
rate_limit_ms: 2000       # delay between Wayback submissions
```

`first_run` state is stored in the DB, not config.

---

## Project Structure

```
/cmd/archiver/main.go       # entry point, wires everything, runs the pipeline
/internal/config/           # load and validate config.yaml
/internal/raindrop/         # Raindrop API client
/internal/wayback/          # Wayback Machine SPN2 API client
/internal/db/               # SQLite schema, migrations, queries
/internal/archiver/         # orchestration logic
```

---

## Data Model (SQLite)

```sql
CREATE TABLE archived_bookmarks (
    raindrop_id     INTEGER PRIMARY KEY,
    original_url    TEXT NOT NULL,
    archive_url     TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    -- 'pending' | 'archived' | 'failed_permanent' | 'failed_transient'
    status_ext      TEXT,         -- raw status_ext from Wayback on failure
    synced_back     INTEGER NOT NULL DEFAULT 0,  -- 1 when note written to Raindrop
    attempted_at    TIMESTAMP,
    archived_at     TIMESTAMP,
    error           TEXT
);

CREATE TABLE run_state (
    key   TEXT PRIMARY KEY,
    value TEXT
    -- keys used:
    --   'last_run_at'   ISO8601 timestamp of last successful run
    --   'first_run'     '1' until first run completes, then '0'
);
```

---

## API Reference (Verified)

### Raindrop — Fetch bookmarks

```
GET https://api.raindrop.io/rest/v1/raindrops/0
    ?sort=-created
    &page=0
    &perpage=50
```

- `collectionId=0` means all collections
- `sort=-created` returns newest first
- Pages are 0-indexed, max 50 per page
- **Date filtering is not supported server-side.** Paginate sorted by `-created` and stop when `item.created` is older than `last_run_at`
- Response includes `_id`, `link`, `note`, `created` per item

### Raindrop — Update bookmark note

```
PUT https://api.raindrop.io/rest/v1/raindrop/{id}
Content-Type: application/json
Authorization: Bearer <token>

{ "note": "<existing note>\n[Archived: https://web.archive.org/web/.../{url}]" }
```

- Always append to existing note, never overwrite
- Note max length: 10,000 chars
- Fetch current note content before updating

### Wayback Machine — Submit for archiving

```
POST https://web.archive.org/save
Authorization: LOW <access_key>:<secret_key>
Content-Type: application/x-www-form-urlencoded
Accept: application/json

url=<target_url>&skip_first_archive=1
```

- Always async — always returns a `job_id`, never an immediate result
- Response: `{"url": "...", "job_id": "ac58789b-f3ca-48d0-9ea6-1d1225e98695"}`

### Wayback Machine — Poll job status

```
GET https://web.archive.org/save/status/{job_id}
Authorization: LOW <access_key>:<secret_key>
Accept: application/json
```

**Success response:**
```json
{
  "status": "success",
  "job_id": "...",
  "original_url": "https://example.com/",
  "timestamp": "20240315120000"
}
```
Construct archive URL as: `https://web.archive.org/web/{timestamp}/{original_url}`

**Pending response:**
```json
{ "status": "pending", "job_id": "..." }
```

**Error response:**
```json
{
  "status": "error",
  "status_ext": "error:not-found",
  "message": "...",
  "job_id": "..."
}
```

---

## Wayback Error Classification

Use `status_ext` to distinguish permanent from transient failures:

| `status_ext` | Classification | Action |
|---|---|---|
| `error:not-found` | permanent | mark `failed_permanent` |
| `error:no-access` | permanent | mark `failed_permanent` |
| `error:blocked` | permanent | mark `failed_permanent` |
| `error:blocked-url` | permanent | mark `failed_permanent` |
| `error:invalid-url-syntax` | permanent | mark `failed_permanent` |
| `error:too-many-daily-captures` | transient | mark `failed_transient`, retry next run |
| `error:user-session-limit` | transient | mark `failed_transient`, retry next run |
| `error:cannot-fetch` | transient | mark `failed_transient`, retry next run |
| `error:soft-time-limit-exceeded` | transient | mark `failed_transient`, retry next run |
| all other `error:*` | transient | mark `failed_transient`, retry next run |

Only `failed_permanent` rows are skipped on future runs. `failed_transient` rows are retried as `pending`.

---

## Execution Flow

### 1. Startup
- Load and validate `config.yaml`
- Open SQLite, run migrations
- Read `first_run` and `last_run_at` from `run_state`

### 2. Fetch bookmarks from Raindrop
- If `first_run = '1'`: paginate through all pages until response is empty
- If `first_run = '0'`: paginate sorted by `-created`, stop when `item.created < last_run_at`
- For each item:
  - If `raindrop_id` not in DB → insert with `status='pending'`
  - If already present with `status='archived'` → skip
  - If `status='failed_transient'` → reset to `pending` for retry

### 3. Archive loop
For each row where `status = 'pending'`:

1. POST to `https://web.archive.org/save` with the URL
2. Parse `job_id` from response
3. Poll `https://web.archive.org/save/status/{job_id}` every 5 seconds
4. On `status=success`:
   - Build archive URL: `https://web.archive.org/web/{timestamp}/{original_url}`
   - Update row: `status='archived'`, `archive_url=...`, `archived_at=now()`
5. On `status=error`:
   - Classify via `status_ext`
   - Update row with appropriate status and `error=status_ext`
6. On poll timeout (2 minutes): mark `failed_transient`
7. Sleep `rate_limit_ms` between submissions (not between polls)

Wayback limits:
- Max 12 concurrent pending captures per user (submit sequentially to stay safe)
- Max 10 captures of the same URL per day
- Max capture duration 2 minutes

### 4. Write archive URLs back to Raindrop
For each row where `status='archived'` and `synced_back=0`:

1. GET current raindrop to fetch existing note: `GET /rest/v1/raindrop/{id}`
2. Append to note: `\n[Archived: {archive_url}]`
3. PUT updated note back
4. On success: set `synced_back=1`
5. On failure: leave `synced_back=0`, will retry next run

### 5. Finalise run state
- Write current UTC timestamp to `run_state.last_run_at`
- Set `run_state.first_run = '0'`

### 6. Print summary
```
Fetched:    142 bookmarks
Archived:   139
Failed:       3  (2 permanent, 1 transient)
Synced back: 139
```

---

## Retry Behaviour

| Status | Next run behaviour |
|---|---|
| `pending` | retried (shouldn't normally persist between runs) |
| `archived`, `synced_back=0` | sync back attempted again |
| `failed_transient` | reset to `pending`, archived again |
| `failed_permanent` | skipped entirely |

---

## Error Handling Notes

- Make the first run **resumable**: if it crashes mid-way, already-inserted rows are skipped on restart (they're not `pending` again unless transient)
- If Wayback returns HTTP 5xx on submit: treat as transient, skip that URL for this run
- If Raindrop PUT fails on sync-back: log and continue — the archive still exists, just not written back yet

---

## Scheduling

No internal scheduler. Run as a cron job:

```cron
0 9 * * 1 /path/to/archiver >> /var/log/archiver.log 2>&1
```

Runs every Monday at 9am.

---

## Intentionally Out of Scope

- No web UI
- No retry backoff / jitter (simple enough at weekly cadence)
- No concurrent Wayback submissions (sequential is safe and avoids session limit errors)