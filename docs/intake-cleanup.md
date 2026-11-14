# Intake cleanup and completion

Milestone 4c uses **operator-approved best-effort deletion**, not atomic
replacement-safe deletion. The operator explicitly accepted the residual race
and revision of the original milestone guarantee. No custom upload service,
custody system, remote locks or infrastructure change is introduced.

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
A missing file is treated as already cleaned up only when its parent directory is
accessible (for WebDAV, a 404 is followed by a successful HEAD of the inbox).
Mismatches and access failures never authorize deletion and remain retryable.

**There is still a gap between verification and removal.** An uncooperative
writer can replace or modify a local file or WebDAV resource during that gap;
the replacement can be deleted. Local identity checks do not make unlink atomic
with verification. Copyparty does not enforce the investigated WebDAV locks or
conditional DELETE checks. Filename counters, ETags and advisory locks are not
safety proofs. Controlled tests deliberately demonstrate this remaining race on
both sources. Avoid replacing files while processing; this is an operational
risk reduction, not an enforced guarantee.

## In-memory intake snapshots

One shared `intake.Store` coordinates the service's import and cleanup workers.
Before listing, discovery waits for any already-started cleanup pass to finish.
Local and WebDAV listings are captured before downloads; the snapshot retains
references/metadata, not inbox bytes. Its protection lasts through import and
automatic processing, and is released on success, errors, cancellation or panic.
No mutex or SQLite transaction is held throughout those operations.

Protection is deliberately global across configured sources: **all app cleanup
waits while a snapshot is active**. A persisted local mount path may alias a
WebDAV URL, so comparing raw references would be unsafe. This conservative policy
needs no alias inference or basename matching. It does not translate old local
paths into WebDAV URLs; unavailable local mounts remain unavailable and retryable.
The trade-off is that unrelated cleanup also waits for the processing cycle.
Queued cleanup gets a turn between snapshots.

The receipt worker refreshes on its configured interval without overlapping
cycles. Long scans, processing, cleanup and pauses can delay refresh; the interval
is not an exact deadline. Remote enumeration need not be atomic. Appends omitted
from a listing are discovered by a subsequent successful scan. External removals
remain possible; WebDAV 404s are suppressed only when a fresh listing confirms
absence. Other access failures remain errors.

Snapshots are transient and rebuilt after restart. SQLite retains all import,
review, attachment, archive and cleanup progress, including first-source-per-hash
deduplication. This coordination requires one service instance per database; it
does not protect against external deletion, replacement, or server inconsistency.

## Durable states and recovery

- `pending`: both remote confirmations and approval authorize a cleanup attempt.
- `failed`: an access, verification, authorization, or deletion error occurred.
  The worker retries when eligible. A hash mismatch is reported and is never deleted.
- `deleting`: durable marker written before deletion or confirming absence.
  If interrupted, it becomes retryable on restart.
- `uncertain`: legacy interrupted or unconfirmed deletions are retried under this
  policy. Every retry rechecks eligibility and verifies any existing file's hash.
- `confirmed`: removal returned success, or the file was absent with an accessible
  parent, and completion was committed in SQLite. Absence does not prove how or
  when the file was removed. Only this state together with both confirmed remote
  records renders complete, labelled **attached and archived**, never reconciled.

A retry can delete a newly created file at the same path if its contents match the
original receipt. A replacement can also occur between verification and deletion.
The operator considers replacement unlikely and accepts these risks in exchange
for automatic recovery. An inaccessible mount must not be mistaken for an empty
directory. HTTP servers that conceal inaccessible resources as 404 can also make
absence ambiguous. Cleanup does not repeat Xero attachment or Paperless archival.

Local unlink success is the filesystem API acknowledgement, not a new fsync-based
custody protocol; filesystem crash/power-loss durability is not guaranteed beyond
the host filesystem. SQLite failure after removal is reconciled on the next cycle.
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

A synthetic nested file on the configured live WebDAV inbox was successfully
deleted after permissions were changed. This does not establish the rules for
all other paths. The operator has accepted the best-effort cleanup risks described
above. The user-owned Copyparty research document is historical evidence, not an
approved custom-service implementation plan.
