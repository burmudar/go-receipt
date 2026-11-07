# Operating and restoring the receipt service

## Deployment

Run one service process per SQLite database. Do not run CLI scans concurrently.
Build with `go build -o go-receipt .`, apply migrations with
`go tool goose -dir . sqlite3 ./app.db up`, then run
`./go-receipt serve --config config.toml`. Use the same checkout for migrations
and the binary. Stop the old process before upgrades and back up first.

Use `config.example.toml` as the configuration reference. Run as a dedicated,
non-root account with access only to the intake, database and secret files.
Keep secrets and backups outside Git; restrict their permissions (files 0600,
directories 0700). Use HTTPS through the configured reverse proxy and require the
UI password for network access. Keep source URLs stable: persisted source paths
are how retries find original files. Do not enable destructive cleanup on shared
writable intake directories without accepting the documented replacement race in
[intake-cleanup.md](intake-cleanup.md).

Configure the process supervisor to send SIGTERM and allow at least 30 seconds
before killing it. HTTP requests and background work receive cancellation;
HTTP shutdown allows 10 seconds. Each background cycle has a five-minute timeout,
in addition to provider-specific HTTP timeouts. A cycle interrupted midway resumes
from durable receipt state. This is cooperative cancellation: custom sources and
scanners must honor context cancellation.

## Pause and health

The status page has authenticated, cross-origin-protected pause/resume controls.
Pause is persisted in SQLite and survives restart. It prevents new background
cycles for import/extraction, Xero checks/refresh/upload, Paperless and cleanup.
An admitted cycle may finish; pause is **not** an emergency cancellation mechanism.
Explicit browser operations and CLI commands are not paused. Stop the service
and wait for exit before a coordinated backup or running CLI recovery commands.

Worker health records last successful cycle, failures and next eligible cycle.
Receipt-import success means a complete successful discovery/processing cycle,
not necessarily a new receipt. Health timestamps survive restart; an interrupted
worker is marked for recovery on startup. Disabled/unconfigured workers do not
appear until run. A success does not mean every receipt is complete: exhausted
extraction failures remain in the receipt list.

Failed background cycles retry with exponential backoff capped at one hour,
checked at their configured interval. This is a bounded retry *rate*, not a limit
on lifetime recovery attempts. Durable upload/archive/cleanup state remains the
authority for whether an external side effect is safe to repeat. Uncertain uploads
are reconciled rather than blindly resubmitted. Pausing does not reset backoff.

Automatic extraction makes at most five attempts total per receipt (including
previous CLI attempts), with delays of 1, 2, 4, and 8 minutes between failures.
Temporarily unavailable sources remain pending until the fifth failed attempt;
then they require intervention. A canceled extraction is not recorded as a failed
result; pending interrupted work remains eligible even after repeated restarts.
Failed or missing source content is never sent to extraction before its hash is
verified. Restore the original before retrying. Once the budget is exhausted,
stop the service and run `go run . --db ./app.db scan retry` with the required
source and OpenAI options (the service TOML does not configure CLI scans).
Explicit retry bypasses the automatic budget; it does not erase audit history.

Inspect **View receipts and unresolved failures** for failed extraction, receipts
needing review, uncertain/failed attachments, archives and cleanup. Receipt pages
show durable failure details and remote confirmation. Worker health avoids echoing
raw provider errors, which may contain secrets. Source discovery failures with no
receipt record show a worker failure: check source configuration and connectivity.
Xero reconnect requirements are shown separately on the status page.

## Approval policy

Automatic approval is unavailable, not merely hidden behind a setting. All
approvals record `manual` / `manual-v1` in append-only review history, together
with the selected tenant, transaction and reviewed values. Historical approvals
are backfilled as manual by the migration. Suggestions never dispatch an upload
without a manual approval.

A prospective policy must require exact decimal amount and currency, a unique
reconciled AUTHORISED SPEND transaction in the connected tenant, a close date,
strong merchant agreement, no existing receipt link or remote attachment, and no
missing/corrected/low-confidence extraction fields. Recheck eligibility at dispatch.
Before implementing or enabling it, evaluate a versioned policy against reviewed
receipts, including ambiguous and rejected matches, and a held-out set. Record
candidate count, eligibility, selected ID, false approvals and abstentions. Require
zero observed false approvals and operator approval of representative coverage;
zero errors on a tiny sample is not evidence of safety. No such evaluation or
production enablement is claimed in this milestone.

## Coordinated backup

1. Pause processing in the UI, stop the service, and wait for it to exit. Stop other
   intake writers and Paperless ingestion while taking the coordinated snapshot.
2. Back up SQLite with its online backup mechanism, for example:
   `sqlite3 app.db ".backup '/secure/backup/app.db'"`.
   Do not copy only `app.db` while a writer is active (WAL/journal may be required).
3. Copy retained local intake files and snapshot the WebDAV intake, preserving
   relative names, permissions and endpoint mapping. Receipts already cleaned up
   depend on the Paperless originals, not on intake backups.
4. Back up Paperless database, media/original files, configuration and encryption
   secrets as a coordinated native backup. Preserve document IDs: receipt records
   reference those IDs. Verify the restored originals' SHA-256 against receipt
   hashes. A document export/import that changes IDs is not a drop-in restore.
5. Back up the application config and referenced secret files separately with
   encryption and restricted access. SQLite also contains OAuth credentials and
   tokens, extracted financial data and approval decisions; encrypt it too.
6. Record application revision, migration version, source locations, Paperless
   endpoint/version, snapshot times and checksums. Keep off-host copies. Resume
   writers and processing only after the backup completes.

## Restore drill

Restore into an isolated environment first, with outbound uploads and cleanup
blocked. Never run the production and restored service against the same intake.

1. Restore SQLite and run `sqlite3 app.db 'PRAGMA integrity_check;'` (expect `ok`).
   Force `UPDATE service_control SET paused=1 WHERE id=1;` before starting.
2. Restore retained intake files at their original absolute paths, or restore the
   same WebDAV endpoint mapping. Restore config and secrets with restrictive
   ownership. Do not casually rewrite persisted source/remote identity fields.
3. Restore the matching Paperless backup with the same document IDs and endpoint.
   Verify confirmed documents exist and their original byte hashes match SQLite.
4. Start the matching service version while paused. Check receipts, reviewed
   values, selected transaction IDs, approval method, upload/archive confirmation
   and audit history. Open representative retained files and Paperless originals,
   including one completed receipt whose intake file was removed.
5. Check Xero connectivity before resuming. Restored refresh tokens may be stale
   due to rotation; reconnect to the **same organisation** if required. Remote
   Xero uploads are not rolled back by a local restore. Recovery must reconcile
   existing attachments, never force a second upload by clearing confirmed state.
6. Resume only after identity, original-file integrity and remote reconciliation
   checks pass. Monitor worker health and all incomplete receipts.

Controlled tests cover SQLite snapshot restoration, durable pause/backoff,
extraction exhaustion, cancellation, and existing side-effect recovery workflows.
A full deployment restore drill involving real WebDAV and Paperless backups remains
an operator validation requirement. Live WebDAV validation is still blocked.
