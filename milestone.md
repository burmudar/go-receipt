# Receipt service milestones

## Goal

Run a service that reads receipts from WebDAV or a local directory, extracts structured data, matches receipts to Xero transactions, and uploads approved receipt attachments. Archive completed receipts in Paperless-ngx with tags and final metadata. Remove intake files only after both attachment and archiving are confirmed. Provide a UI that shows completed work and items that need manual review.

Start with manual approval for every attachment. Add automatic approval only after the manual workflow is reliable.

## Baseline before milestone 1

- The CLI contains WebDAV download code and OpenAI extraction code. The WebDAV service is currently unavailable to us, so live receipt import cannot be validated.
- Extraction includes merchant, total, currency, date, and confidence.
- The scan command processes at most three receipts and prints results. It does not persist receipts or processing state.
- Files have a SHA-256 hash, but duplicate detection is not implemented.
- SQLite stores OAuth client credentials. Xero login, token refresh, transaction access, and attachment upload are not implemented.
- There is no web service, UI, matching workflow, or approval history.
- There are no test files. `go test ./...` currently fails on the non-constant format string in `fmt.Errorf(err.Message)` in `cmd/scancli/openai.go`.

## Scope decisions

- Treat **receipt attachment** and **Xero accounting reconciliation** as separate operations. The user reconciles in Xero. The service attaches receipts only to transactions that are already reconciled. It never reconciles transactions or changes their reconciliation status.
- Attachment targets are reconciled, authorised Xero bank transactions of type `SPEND` or `RECEIVE` only. Manually entered transactions that are not reconciled are not eligible. Transfers, prepayments, overpayments, bills, and bill payments are out of scope. Receipts for bill payments need manual handling.
- The Accounting API does not expose bank statement lines. A bank feed line becomes a bank transaction only when the user creates it in Xero, usually during reconciliation. Receipts therefore stay pending until the matching transaction is reconciled. The service does not create accounting records. See [docs/xero-api.md](docs/xero-api.md).
- Keep extraction confidence separate from match confidence. Accurate extraction does not establish a correct transaction match.
- Use one long-running Go service with SQLite and a simple web UI. Introduce the server in milestone 2a and extend it in later milestones. CLI commands support setup and diagnostics; they are not the main user workflow. Separate deployments are not required for the first version.
- WebDAV or the local directory holds original receipts during processing. Files remain at their intake locations until processing is complete. SQLite stores hashes, source references, extracted data, and workflow state, not file bytes. No separate staging directory is required.
- Paperless-ngx holds the completed archive. Upload to Paperless after the Xero attachment succeeds, not during initial import. Include final receipt tags and metadata.
- Track Xero attachment, Paperless consumption, and intake cleanup separately. Remove the intake file only after both remote operations are confirmed and the file still matches the processed hash. An archive retry must not repeat a successful Xero attachment.
- Keep unmatched and pending receipts in the intake. Ignored receipts need an explicit retention decision before cleanup.
- Use a local receipt folder while WebDAV is unavailable. Local and WebDAV input must use the same extraction, persistence, review, and attachment workflow.
- The service runs on a personal media PC for one user. There are no user accounts or roles. The service must be accessible from other devices on the home network. Caddy on the media PC terminates HTTPS for the home network and forwards requests to the service on localhost. Make the listen address and the public base URL configurable. Listen on localhost by default. Require single-user authentication when the public base URL is not a localhost URL or the listen address is not a loopback address, because requests through Caddy arrive from localhost. Do not expose the service to the internet.
- Connect one Xero organisation. Scope stored transactions and receipt links by organisation identifier. Do not allow a change of organisation while approvals or uploads are pending.
- Protect the SQLite database, intake files, and backups with file permissions for the service user. Encryption at rest is not required for this deployment.
- Do not block milestones 1–5 on WebDAV access. Live WebDAV validation remains required before claiming the WebDAV deployment is ready.

## Milestone 1: Reliable extraction and persistent receipt records

**Outcome:** Each discovered receipt has a durable record that survives a restart.

**Status:** Implemented for local input. The CLI regression test uses real SQLite and a controlled OpenAI endpoint. Live OpenAI and WebDAV validation remain unverified. The database stores source references and hashes, not receipt bytes. Retries read retained intake files and verify their content hashes before extraction.

### Work

- Add a configurable local receipt folder as an input source. Keep receipt processing independent of WebDAV access.
- Remove password logging and correct the image MIME-type check, confidence JSON field name, and format-string error.
- Replace hard-coded connection settings with configuration. Return actionable errors instead of panicking or silently ignoring failures.
- Persist receipt identity, source location, extraction results, processing status, timestamps, and errors.
- Use the content hash to detect receipts already imported.
- Retain original receipts in the intake for retries, preview, and attachment upload. Record missing or changed source files as failures. Do not delete intake files in this milestone.
- Remove the temporary three-receipt limit. Record failures per receipt so one failure does not stop the whole batch.
- Validate extracted data before it becomes eligible for matching. Use an exact representation for monetary amounts.

### Done when

- [x] Receipts from a local folder can be imported and extracted without a WebDAV connection (controlled extraction endpoint).
- [x] Importing the same local folder twice does not create duplicate receipt records or repeat successful extraction.
- [x] A restart preserves extracted data and processing status.
- [x] An extraction failure is visible and can be retried from the retained source file.
- [x] Missing or changed source files fail before extraction. Restoring the original permits a retry.
- [x] `go test ./...` passes.

## Milestone 2a: Server and Xero connection

**Outcome:** A running Go service on the media PC lets the user connect one Xero organisation through a browser and keeps that connection valid.

**Status:** Implemented. The server test uses real SQLite and a controlled Xero server. A live Xero connection through localhost works and connects the intended organisation. Restart, token expiry, and revocation also pass with live Xero. Home-network access through Caddy and the Xero connection from another device also pass. The service requires login when the public URL or the listen address is not local. The first connection must authorise exactly one organisation. A reconnect must authorise the connected organisation. To change the organisation, disconnect first.

### Work

- Confirm the Xero API behaviour, OAuth scopes, and attachment constraints. Done: see [docs/xero-api.md](docs/xero-api.md). The Xero app was created after 2 March 2026, so use granular scopes: `offline_access accounting.banktransactions.read accounting.attachments`. Confirm these names in the Xero app settings.
- Add a server command with configurable listen address, public base URL, database path, request timeouts, and graceful shutdown. Listen on localhost by default.
- Add single-user authentication for home-network access. Refuse to start when authentication is required and not configured. Use secure session cookies when the public base URL uses HTTPS.
- Document a Caddy configuration for the service: an HTTPS site with a certificate that the home-network browsers trust, and a reverse proxy to the localhost listen address. Register the Caddy callback URL as the Xero redirect URI.
- Use the same database configuration for the server and all CLI commands. Remove the fixed `app.db` path from the `oauth` commands.
- Keep `oauth set` as the setup step for Xero client credentials. Derive the OAuth redirect URI from the public base URL. Xero requires HTTPS for every redirect URI except `http://localhost`. For home-network access, set the public base URL to the Caddy HTTPS address.
- Add a browser-based Xero connection flow and an OAuth callback endpoint. Request the `offline_access` scope, validate OAuth state, persist tokens, and refresh expired access tokens. Access tokens expire after 30 minutes.
- Refresh the connection at least once in each 60-day period, also when there is no other activity. An unused refresh token expires after 60 days.
- Use one refresh path for all callers. Save each rotated refresh token in the same database transaction as the new access token so that concurrent requests cannot invalidate the stored connection. If a refresh response is lost, retry with the previous refresh token within the 30-minute grace period.
- Persist the identifier of the one authorised Xero organisation. Reject a connection that authorises more than one organisation.
- Show connection status and a reconnect action when a refresh fails or the connection is revoked.
- Keep Xero connection logic independent of HTTP handlers and CLI commands.
- Keep credentials and tokens out of logs, UI responses, and CLI output. Remove client secrets from `oauth get` output.

### Done when

- [x] The server command runs a persistent HTTP service and shuts down cleanly.
- [x] With a configured home-network address, another device on the home network can open the service after authentication. The service refuses that address when authentication is not configured (live, through Caddy).
- [x] The Xero connection flow works from another device on the home network (live Xero, through Caddy).
- [x] The user can connect the intended Xero organisation through the browser without running a CLI login command (live Xero, localhost).
- [x] The connection survives a service restart and access-token expiry (live Xero).
- [x] A revoked connection produces a clear reconnect requirement in the browser (live Xero).
- [x] Logs, pages, and CLI output do not contain client secrets or tokens (controlled Xero server and manual CLI check).
- [x] The attachment target scope, required OAuth scopes, the reconciliation dependency, and the boundary with accounting reconciliation are documented in [docs/xero-api.md](docs/xero-api.md).

## Milestone 2b: Bank transaction access

**Outcome:** The service retrieves and stores reconciled bank transactions from the connected organisation, and keeps them current.

**Status:** Implemented. The test uses real SQLite and a controlled Accounting API: pagination, incremental refresh, changed and voided records, a short rate limit, and a stopped refresh. With live Xero, a full refresh stored 1354 transactions across 14 pages, including 25 deleted ones. An incremental refresh fetched only changes and created no duplicates. `If-Modified-Since` returns records that changed to `DELETED`, so a periodic full refresh is not necessary. The live rate-limit behaviour and the `VOIDED` status are not tested. The user starts a refresh on the status page. Milestone 3a adds the background refresh.

### Work

- Fetch `SPEND` and `RECEIVE` bank transactions with the fields needed for review and matching, including `BankTransactionID`, `Status`, `IsReconciled`, and `UpdatedDateUTC`. Only reconciled, `AUTHORISED` transactions are eligible for new links.
- Store transactions scoped by organisation identifier. Update changed transactions instead of creating duplicates.
- Refresh incrementally with the `If-Modified-Since` header. Record the last successful refresh time. Test whether this returns transactions that changed to `DELETED` or `VOIDED`. If it does not, add a periodic full refresh.
- Mark deleted or voided transactions as unavailable for new links. Keep their records for existing receipt links and audit history.
- Handle pagination (up to 100 records for each page) and rate limits (5 concurrent calls, 60 calls each minute, and 5000 calls each day for each organisation). Record refresh failures where the user can see them.
- Keep transaction access logic independent of HTTP handlers and CLI commands.

### Done when

- [x] The service can retrieve real reconciled bank transactions from the connected organisation (live Xero: 1329 of 1354).
- [ ] A transaction reconciled after the previous refresh appears at the next refresh.
- [x] A changed, deleted, or voided transaction is updated at the next refresh without duplicate records (controlled Accounting API; live Xero returns deleted records to `If-Modified-Since`).
- [x] A refresh across more than one page retrieves all matching transactions (live Xero: 14 pages).
- [x] A rate-limit response delays the refresh and does not lose stored data (controlled Accounting API only).

## Milestone 3a: Background processing and receipt pages

**Outcome:** A running service imports receipts, extracts their data, and refreshes bank transactions without CLI commands. The UI shows each receipt with its original document.

**Status:** Implemented for local input. Tests use real SQLite, a controlled scanner, and a controlled Xero server. Live OpenAI and background Xero validation remain unverified. Milestone 3c adds WebDAV background import and preview.

### Work

- Extend the existing server with a background worker for receipt import, extraction, and transaction refresh. Reuse the receipt processing workflow from milestone 1 and the transaction refresh from milestone 2b. An incremental refresh is sufficient, because Xero returns deleted records to `If-Modified-Since`.
- Add receipt list and detail pages with the original document beside extracted data.
- Show the processing state of each receipt: pending, extracted, or failed with its error.
- Keep the receipt pages and original documents behind the same access rules as the Xero connection pages.

### Done when

- [x] New receipts in the configured local folder appear without running the scan CLI manually (controlled scanner).
- [x] Bank transactions refresh in the background. The user does not need to select **Refresh transactions** (controlled Xero server).
- [x] The user can inspect a receipt: the original document beside its extracted data.
- [x] A failed extraction is visible in the UI with its error.
- [x] A restart does not repeat completed import or extraction.

## Milestone 3b: Manual review decisions

**Outcome:** The user can correct receipt data and approve, reject, or ignore transaction links. Each decision has an audit history.

**Status:** Implemented on top of milestone 3a with real SQLite and a controlled Xero transaction set. Ignored receipts are retained in the intake; each ignore decision records that retention choice. Attachment, archive, and cleanup are not implemented, so completion after attachment remains to be validated in milestones 4a–4c.

### Work

- Allow corrections to extracted fields while retaining the original extraction result.
- Allow transaction search and manual selection within the connected organisation.
- Persist approval, rejection, and ignore decisions with timestamps and an audit history.
- Separate needs-review, unmatched, approved, rejected, completed, ignored, and failed items in the UI. Approved must not mean uploaded. Show Xero attachment, Paperless archive, and intake cleanup progress separately.
- Decide whether ignored receipts stay in the intake, are archived in Paperless, or are removed. Record that decision before milestone 4c cleanup.
- Keep the review actions behind the same access rules as the Xero connection pages.

### Done when

- [x] The user can correct the data of a receipt and approve a transaction link (controlled SQLite transaction set).
- [x] An unmatched receipt can remain pending or be ignored with a recorded decision. The UI shows that a pending receipt can wait for reconciliation in Xero.
- [x] A restart preserves decisions.
- [ ] Approved receipts remain visibly pending until attachment upload succeeds. Attached receipts remain incomplete until Paperless archiving and intake cleanup finish (approved pending is implemented; subsequent operations are milestones 4a–4c).

## Milestone 3c: WebDAV service input and preview

**Outcome:** The running service can import WebDAV receipts and display their retained originals using the same processing and review workflow as local receipts.

**Status:** Implemented and tested with real SQLite and a controlled WebDAV server. `go test ./...`, `go vet ./...`, and `go test -race ./intake ./cmd/scancli ./server` passed. Live WebDAV access remains unverified; deployment readiness still requires the deferred validation below. Pending receipts with an unconfigured or unavailable WebDAV source are retried automatically; changed source content fails verification and requires an explicit retry after restoration.

### Work

- Add configurable WebDAV endpoint, inbox, username, and secret settings to the service. Reuse the existing WebDAV input and receipt processing workflow, and keep credentials out of URLs, logs, and browser responses.
- Discover and import WebDAV receipts in the background, deduplicating by content hash across local and WebDAV inputs. Preserve source references and verify retained bytes before extraction and preview.
- Read and preview WebDAV originals on the authenticated receipt pages, restricting requests and credentials to the configured WebDAV endpoint. Keep the same document safety and access protections as local previews.
- Handle records from both local and WebDAV sources safely. Do not mark a pending receipt as an extraction failure merely because its source is not configured for this service; report the configuration problem and leave it available for processing when the source is configured.
- Surface discovery and source failures, resume safely after recovery or restart, and retain intake files. Do not add WebDAV cleanup before milestone 4c, after the attachment and archive safeguards in milestones 4a and 4b.

### Done when

- [x] New receipts on a controlled WebDAV server are imported and extracted by the running service without the scan CLI.
- [x] WebDAV originals can be inspected beside their extracted data, subject to authentication and hash verification; missing or changed originals are not presented as the processed receipt.
- [x] Identical local and WebDAV content does not create a duplicate record or repeat a successful extraction, including across a restart.
- [x] Unconfigured, unavailable, or changed sources do not silently corrupt receipt state; processing can resume after access is restored.
- [x] WebDAV credentials are not sent to another endpoint or exposed in logs or pages. Controlled tests cover the service workflow and access rules.
- [ ] Live WebDAV deployment is not declared ready until the deferred validation checks pass.

## Milestone 4a: Xero attachment

**Outcome:** A manually approved receipt is safely attached to the selected Xero transaction, with a durable attachment confirmation. Intake files remain untouched; Paperless archiving and cleanup are not part of this milestone.

**Status:** Implemented with real SQLite and controlled Xero responses. Live Xero attachment validation remains outstanding. The application enforces a conservative 10,000,000-byte cap supported by integration-provider documentation; direct Xero bank-transaction limit confirmation is still outstanding. No verified organisation-safe browser deep link is available; the UI displays the target ID. The migration returns old approvals to review because they lack an approval-time target snapshot. Once dispatched, a target stays locked even on an uncertain result; additional uploads require manual follow-up, not silent retargeting. `go test ./...`, `go vet ./...`, and `go test -race ./xero ./server` pass. Tests cover local import/extraction/approval, remote confirmation, retry keys, uncertain responses and Manager restart, changed targets, source failures, size rejection, collisions, and UI progress.

### Work

- Read the original from the intake and verify its hash before uploading to the approved Xero target.
- Immediately before upload, fetch the approved bank transaction from Xero again. If it is no longer an eligible reconciled, authorised `SPEND` or `RECEIVE` transaction, or no longer matches the approved amount, currency, or date, stop the upload and return the receipt to review.
- Record the target identifier, attachment identifier where available, upload result, and completion time.
- Make upload retries safe. Send a stable `Idempotency-Key` for each receipt and target. Use a unique attachment file name for each receipt so that an upload cannot replace another attachment. Check the attachment list when a timeout leaves the result uncertain. Resume only incomplete work after an error or restart.
- Confirm the attachment size limit for bank transactions. Reject a receipt that exceeds it before upload.
- Show pending uploads, upload failures, and confirmed attachments separately from approval. Keep archive and cleanup progress separate and do not mark an attached receipt complete.
- Provide a link to the Xero record where supported.
- Prevent changes to an approved target from silently causing an additional upload. Require an explicit follow-up decision.

### Done when

- [ ] One receipt goes from local folder import through manual approval to a confirmed attachment visible on the intended Xero record.
- [x] Missing or changed originals and oversized receipts are rejected before upload (controlled tests, conservative application cap).
- [x] A retry or restart, including after an uncertain upload response, does not create a duplicate attachment or overwrite another receipt's attachment (controlled tests).
- [x] A target that is deleted, voided, otherwise ineligible, or changed after approval is not uploaded to. The receipt returns to review with the reason (controlled tests; previously dispatched uncertain uploads remain locked for manual investigation).
- [x] Changing an approved target cannot silently trigger an additional upload.
- [x] The UI distinguishes approval, pending upload, upload failure, and confirmed attachment. Attached receipts remain incomplete and are not labelled reconciled.
- [x] Intake files remain intact. No Paperless upload or intake cleanup is performed.

## Milestone 4b: Paperless archive

**Outcome:** A receipt with a confirmed Xero attachment is archived in Paperless with its final metadata and a durable document reference. Intake files remain untouched.

**Depends on:** Milestone 4a's confirmed attachment records. Archive failures and retries must not repeat a confirmed Xero upload.

### Work

- After confirmed Xero attachment, read and verify the retained original and upload it to Paperless with receipt tags, corrected metadata, and useful transaction references.
- Persist the Paperless task reference. Wait for successful consumption and save the document reference; upload acceptance alone is not archive confirmation.
- Handle uncertain Paperless uploads without blindly submitting another copy. Before a retry, search Paperless for the receipt checksum. Confirm whether the Paperless duplicate check rejects a second copy, and do not depend on it until confirmed.
- Resume only incomplete archive work after an error or restart. Preserve the confirmed Xero attachment independently.
- Show pending archives, archive failures, and confirmed archives separately from Xero attachment and intake cleanup. Do not mark an archived receipt complete while cleanup is pending.
- Retain intake files regardless of archive success or failure; deletion belongs to milestone 4c.

### Done when

- [ ] Paperless contains the original receipt with final tags, corrected metadata, and transaction references after confirmed Xero attachment.
- [ ] The saved task and document references distinguish upload acceptance from successful consumption.
- [ ] A retry or restart, including after an uncertain upload response, does not create a duplicate archive document or repeat the confirmed Xero upload.
- [ ] A failed Paperless archive is visible in the UI and leaves the intake file intact. Processing can resume after recovery.
- [ ] A successful archive is visible in the UI, with cleanup still pending and the intake file still intact.

## Milestone 4c: Safe intake cleanup and completion

**Outcome:** An attached and archived receipt is safely removed from the intake and marked complete. Attachment, archive, and cleanup each have a durable completion record.

**Depends on:** Durable confirmation of both the Xero attachment from milestone 4a and Paperless consumption from milestone 4b.

### Work

- Remove the intake file only after confirmed Xero attachment and Paperless archiving. Verify that the file still matches the processed hash and prevent deletion of a replacement file, including one replaced between verification and deletion.
- Apply the cleanup safeguards to local and WebDAV input. Use controlled WebDAV tests while live access remains unavailable; retain files if safe deletion cannot be established.
- Record cleanup progress separately. Recover safely from failures or a restart around deletion without repeating confirmed attachment or archive operations.
- Keep unmatched, pending, and ignored receipts in the intake, respecting the recorded retain-in-intake decision for ignored receipts.
- Show pending cleanup, cleanup failures, and confirmed completion alongside the separate attachment and archive results. Mark a receipt complete only after all three steps finish.
- Label completed items as attached and archived, not reconciled.

### Done when

- [ ] One receipt completes the full path from local folder import through manual approval, confirmed Xero attachment, confirmed Paperless archive, and intake cleanup.
- [ ] Cleanup cannot remove an unarchived receipt or a replacement file, including a file replaced between verification and deletion.
- [ ] Unmatched, pending, and ignored receipts remain in the intake.
- [ ] A cleanup failure is visible and can be retried. A restart around deletion recovers without deleting a replacement file or repeating confirmed remote operations.
- [ ] Controlled WebDAV tests cover the same cleanup safeguards; live deployment readiness remains subject to deferred validation.
- [ ] The UI shows what was approved, attached, archived, and cleaned up, with unresolved failures kept visible.
- [ ] Only receipts with all three steps finished are marked complete. The UI labels them as attached and archived, not reconciled.

**This is the first usable end-to-end release.**

## Milestone 5: Suggested matches and faster review

**Outcome:** The service proposes transaction matches and explains them, while the user retains approval control.

### Work

- Generate candidates using amount, currency, transaction date, and merchant information.
- Show the reasons for each suggestion and distinguish match confidence from extraction confidence.
- Keep ambiguous matches and receipts with no suitable match in the review queue.
- Make existing receipt links visible to reduce accidental duplicate assignments.
- Let the user accept a suggestion, choose a different transaction, or leave the receipt pending.

### Done when

- [ ] Receipts with suitable candidates show ranked suggestions and their reasons.
- [ ] Ambiguous candidates do not produce an automatic attachment.
- [ ] Manual selection remains available when suggestions are wrong or absent.
- [ ] The audit history records the selected match and approval decision.

## Milestone 6: Unattended operation and optional automatic approval

**Outcome:** The service operates reliably without supervision, with clear exceptions for manual review.

### Work

- Add bounded retries and backoff. Extend request timeouts and graceful shutdown across all background jobs.
- Extend the restart recovery from milestones 4a–4c to all background jobs. Do not repeat confirmed side effects.
- Show worker health, last successful import, Xero connection status, and unresolved failures.
- Document deployment, configuration, backups, and restore procedures for SQLite, the intake files, and the Paperless archive.
- Define and validate a conservative automatic-approval policy using reviewed receipts before enabling it.
- Keep automatic approval disabled by default. Record whether each approval was manual or automatic and which policy applied.
- Provide a way to pause automatic processing without losing pending work.

### Done when

- [ ] The service recovers from restarts and temporary receipt-source, OpenAI, Xero, and Paperless failures. Live WebDAV checks remain separate until access is restored.
- [ ] A backup can restore receipt records, decisions, and access to original files.
- [ ] The user can see all items that need intervention without reading logs.
- [ ] If automatic approval is enabled, only eligible, unambiguous matches proceed automatically.
- [ ] Completed items show their approval method and confirmed upload result.

## Deferred validation: Live WebDAV access

**Blocked by:** Access to the WebDAV service. These checks can run as soon as access is restored; they do not need to wait for milestone 6.

- [ ] Validate authentication, inbox listing, and receipt downloads against the live service.
- [ ] Confirm WebDAV receipts use the same processing workflow as local receipts.
- [ ] Confirm a receipt already imported locally is not duplicated or extracted again when discovered through WebDAV.
- [ ] Complete the WebDAV-to-Xero-to-Paperless workflow with a controlled receipt, including safe intake cleanup.
- [ ] Confirm connection failures are visible and processing resumes safely after access returns.

Do not mark these checks complete based on local-folder tests alone.

## Delivery order

Implement milestones 1, 2a, 2b, 3a, 3b, 4a, 4b, and 4c in order. Introduce the long-running server and browser-based Xero connection in milestone 2a. Add bank transaction access in milestone 2b. Extend that server with background processing and receipt pages in milestone 3a, and with manual review decisions in milestone 3b. Implement milestone 3c against a controlled WebDAV server after 3a and 3b; it can proceed alongside 4a and 4b without live WebDAV access and must precede WebDAV cleanup in 4c. Deliver Xero attachment in 4a, Paperless archiving in 4b, and safe intake cleanup and full completion in 4c. Each milestone includes its own durable progress, UI states, failure handling, and restart recovery; do not defer these to 4c or 6. Keep intake files intact through 4a and 4b. Complete one manually approved receipt attachment, Paperless archive, and intake cleanup before adding matching automation. Add milestone 5 to reduce review effort. Use milestone 6 to prepare for unattended operation; basic access protection, secret handling, and safe uploads are required earlier, not deferred to this milestone.

## Validation approach

Run existing checks before each change. Add focused regression coverage for changed behavior where coverage is missing. Use controlled local receipt files while WebDAV is unavailable. Validate OpenAI, Xero, and Paperless separately with appropriate credentials and an appropriate Xero organisation. Record live WebDAV validation as blocked until access is restored. Record actual commands and observed results for each milestone. Do not treat a successful compile as proof of a working external integration.
