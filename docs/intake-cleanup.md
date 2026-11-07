# Intake cleanup and completion

Milestone 4c uses **operator-approved best-effort deletion**, not atomic
replacement-safe deletion. The operator explicitly accepted the residual race
and revision of the original milestone guarantee. No custom upload service,
custody system, locks or infrastructure change is introduced.

## Operation

Apply migrations before starting the service. A single cleanup worker per database
runs on the receipt interval, independently of the Xero and Paperless clients.
This is a single-worker assumption across all processes using the database, not
cross-process coordination; do not run two service instances against it.
Only the first imported source for a content hash is eligible; duplicate copies
at other locations are not removed. The worker requires extracted status, current
manual approval matching the attachment target, a confirmed Xero attachment with
ID and completion time, and confirmed Paperless consumption with document ID and
completion time. Pending, unmatched and ignored receipts remain; ignoring retains
the intake as recorded by the review decision. Upload acceptance is insufficient.
Normal review changes are rejected once attachment dispatch locks the target.
Cleanup also atomically rechecks current eligibility and source when recording
dispatch, rather than trusting the candidate selected before file verification.

The source is reread and SHA-256 verified immediately before dispatch. Local
cleanup rejects symlink paths/ancestors, nonregular files and observed inode
replacement. WebDAV cleanup confines deletion to a direct file in the configured
inbox and endpoint, disallows redirects, and requires a successful verified GET.
Missing sources, mismatches and access failures never authorize deletion.

**There is still a gap between verification and removal.** An uncooperative
writer can replace or modify a local file or WebDAV resource during that gap;
the replacement can be deleted. Local identity checks do not make unlink atomic
with verification. Copyparty does not enforce the investigated WebDAV locks or
conditional DELETE checks. Filename counters, ETags and advisory locks are not
safety proofs. Controlled tests deliberately demonstrate this remaining race on
both sources. Avoid replacing files while processing; this is an operational
risk reduction, not an enforced guarantee.

## Durable states and recovery

- `pending`: both remote confirmations and approval authorize a cleanup attempt.
- `failed`: verification, authorization or pre-dispatch persistence failed;
  retained and retried on a later processing cycle when eligible. Restore access
  or original bytes to recover verification failures.
- `deleting`: durable marker written before the destructive call.
- `uncertain`: any error after dispatch, or restart with a deleting marker.
  **Never automatically retried**, even if the path is now absent or holds matching
  bytes. A lost response, permission error or database failure after removal must
  not authorize a second removal of a reused pathname.
- `confirmed`: removal returned success and completion was committed in SQLite.
  Only this state together with both confirmed remote records renders complete,
  labelled **attached and archived**, never reconciled.

Uncertain work needs manual investigation; there is intentionally no blind retry
button. Stop the single service before operator database recovery, preserve a
backup, check both remote references and the source history, and establish the
actual deletion outcome. Do not reset an uncertain row just because the pathname
is missing or has the same hash. No automatic reconciliation from absence exists.
If the outcome cannot be established, leave it uncertain. Recovery actions are
not automated in this release. Cleanup does not repeat attachment or archival.

Local unlink success is the filesystem API acknowledgement, not a new fsync-based
custody protocol; filesystem crash/power-loss durability is not guaranteed beyond
the host filesystem. SQLite failure after removal leaves an uncertain marker.
The original preview refers to intake and is unavailable after cleanup; use the
persisted Xero/Paperless references for the archived original.

## Validation and deployment limits

Tests use temporary files, SQLite and HTTP test servers only. No real receipt was
removed and no live service was contacted. `TestCleanupFullWorkflow` in
`paperless/cleanup_workflow_test.go` passes the combined local folder discovery →
import → extraction → manual approval → confirmed Xero attachment → confirmed
Paperless consumption → cleanup path. It uses the real managers and review store;
only the connected account and transaction cache are seeded, not attachment or
archive confirmations. Intake bytes remain through upload acceptance and an active
task even when its document is visible. Reopening SQLite and rebuilding managers
resumes consumption and later preserves completion without repeating either upload
or deleting a replacement at the completed path. It also verifies that review
changes during cleanup verification are blocked by the attachment lock.

`TestCleanupRechecksAuthorizationAtDispatch` separately changes candidate
eligibility during verification and proves no removal is dispatched. Full Go
tests, vet and race tests for receipt, intake, Paperless and server pass.

The external Nix `william /inbox` permission edit from `rw` to `rwd` was already
syntax checked but is **not deployed** and was not edited here. Live WebDAV delete
permissions/behavior, phone filename reuse and deployment readiness remain
unvalidated. Applying migrations and running this version enables best-effort
cleanup for eligible configured sources; do not deploy without accepting these
limits and preserving backups. The user-owned Copyparty research document is
historical evidence, not an approved custom-service implementation plan.
