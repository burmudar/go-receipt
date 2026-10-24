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

**Status:** Implemented. The server test uses real SQLite and a controlled Xero server. A live Xero connection through localhost works and connects the intended organisation. Live validation of restart, token expiry, revocation, and Caddy remains open. The service requires login when the public URL or the listen address is not local. The first connection must authorise exactly one organisation. A reconnect must authorise the connected organisation. To change the organisation, disconnect first.

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
- [ ] With a configured home-network address, another device on the home network can open the service after authentication. The service refuses that address when authentication is not configured.
- [ ] The Xero connection flow works from another device on the home network.
- [x] The user can connect the intended Xero organisation through the browser without running a CLI login command (live Xero, localhost).
- [ ] The connection survives a service restart and access-token expiry.
- [ ] A revoked connection produces a clear reconnect requirement in the browser.
- [x] Logs, pages, and CLI output do not contain client secrets or tokens (controlled Xero server and manual CLI check).
- [x] The attachment target scope, required OAuth scopes, the reconciliation dependency, and the boundary with accounting reconciliation are documented in [docs/xero-api.md](docs/xero-api.md).

## Milestone 2b: Bank transaction access

**Outcome:** The service retrieves and stores reconciled bank transactions from the connected organisation, and keeps them current.

### Work

- Fetch `SPEND` and `RECEIVE` bank transactions with the fields needed for review and matching, including `BankTransactionID`, `Status`, `IsReconciled`, and `UpdatedDateUTC`. Only reconciled, `AUTHORISED` transactions are eligible for new links.
- Store transactions scoped by organisation identifier. Update changed transactions instead of creating duplicates.
- Refresh incrementally with the `If-Modified-Since` header. Record the last successful refresh time. Test whether this returns transactions that changed to `DELETED` or `VOIDED`. If it does not, add a periodic full refresh.
- Mark deleted or voided transactions as unavailable for new links. Keep their records for existing receipt links and audit history.
- Handle pagination (up to 100 records for each page) and rate limits (5 concurrent calls, 60 calls each minute, and 5000 calls each day for each organisation). Record refresh failures where the user can see them.
- Keep transaction access logic independent of HTTP handlers and CLI commands.

### Done when

- [ ] The service can retrieve real reconciled bank transactions from the connected organisation.
- [ ] A transaction reconciled after the previous refresh appears at the next refresh.
- [ ] A changed, deleted, or voided transaction is updated at the next refresh without duplicate records.
- [ ] A refresh across more than one page retrieves all matching transactions.
- [ ] A rate-limit response delays the refresh and does not lose stored data.

## Milestone 3: Background processing and manual review UI

**Outcome:** A running service imports receipts, and the UI lets the user inspect and approve transaction links.

### Work

- Extend the existing server with a background worker for receipt import, extraction, and transaction refresh. Reuse the receipt processing workflow from milestone 1 and the transaction refresh from milestone 2b.
- Add receipt list and detail pages with the original document beside extracted data.
- Allow corrections to extracted fields while retaining the original extraction result.
- Allow transaction search and manual selection within the connected organisation.
- Persist approval, rejection, and ignore decisions with timestamps and an audit history.
- Separate needs-review, unmatched, approved, rejected, completed, ignored, and failed items in the UI. Approved must not mean uploaded. Show Xero attachment, Paperless archive, and intake cleanup progress separately.
- Decide whether ignored receipts stay in the intake, are archived in Paperless, or are removed. Record that decision before milestone 4 cleanup.
- Keep the review pages, original documents, and review actions behind the same access rules as the Xero connection pages.

### Done when

- [ ] New receipts in the configured local folder appear without running the scan CLI manually.
- [ ] The user can inspect a receipt, correct its data, and approve a transaction link.
- [ ] An unmatched receipt can remain pending or be ignored with a recorded decision. The UI shows that a pending receipt can wait for reconciliation in Xero.
- [ ] A restart preserves decisions and does not repeat completed work.
- [ ] Approved receipts remain visibly pending until attachment upload succeeds. Attached receipts remain incomplete until Paperless archiving and intake cleanup finish.

## Milestone 4: Xero attachment, Paperless archive, and intake cleanup

**Outcome:** An approved receipt is attached to the selected Xero transaction, archived in Paperless, and safely removed from the intake. Each step has a durable completion record.

### Work

- Read the original from the intake and verify its hash before uploading to the approved Xero target.
- Immediately before upload, fetch the approved bank transaction from Xero again. If it is deleted, voided, or no longer matches the approved amount, currency, or date, stop the upload and return the receipt to review.
- Record the target identifier, attachment identifier where available, upload result, and completion time.
- Make upload retries safe. Send a stable `Idempotency-Key` for each receipt and target. Use a unique attachment file name for each receipt so that an upload cannot replace another attachment. Check the attachment list when a timeout leaves the result uncertain.
- Confirm the attachment size limit for bank transactions. Reject a receipt that exceeds it before upload.
- After confirmed Xero attachment, upload the original to Paperless with receipt tags, corrected metadata, and useful transaction references.
- Persist the Paperless task reference. Wait for successful consumption and save the document reference; upload acceptance alone is not archive confirmation.
- Handle uncertain Paperless uploads without blindly submitting another copy. Before a retry, search Paperless for the receipt checksum. Confirm whether the Paperless duplicate check rejects a second copy, and do not depend on it until confirmed. Resume only incomplete steps after an error or restart.
- Remove the intake file only after confirmed Xero attachment and Paperless archiving. Verify that the file still matches the processed hash and prevent deletion of a replacement file. Record cleanup progress separately.
- Show pending uploads, pending archives, pending cleanup, failures, and confirmed completion separately.
- Provide a link to the Xero record where supported.
- Prevent changes to an approved target from silently causing an additional upload. Require an explicit follow-up decision.

### Done when

- [ ] One receipt completes the full path from local folder import through manual approval, confirmed Xero attachment, confirmed Paperless archive, and intake cleanup.
- [ ] The attachment is visible on the intended Xero record.
- [ ] A retry or restart does not create a duplicate attachment or archive document.
- [ ] A target that is deleted, voided, or changed after approval is not uploaded to. The receipt returns to review with the reason.
- [ ] A failed Paperless archive leaves the intake file intact and does not repeat the confirmed Xero upload.
- [ ] Paperless contains the original receipt with final tags and metadata before intake cleanup.
- [ ] Cleanup cannot remove an unarchived receipt or a replacement file. Unmatched and pending receipts remain in the intake.
- [ ] The UI shows what was approved, what was uploaded, and what still needs attention.
- [ ] The UI labels completed items as attached and archived. It does not label them as reconciled.

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
- Extend the restart recovery from milestone 4 to all background jobs. Do not repeat confirmed side effects.
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

Implement milestones 1, 2a, 2b, 3, and 4 in order. Introduce the long-running server and browser-based Xero connection in milestone 2a. Add bank transaction access in milestone 2b. Extend that server with background processing and manual review in milestone 3. Complete one manually approved receipt attachment, Paperless archive, and intake cleanup before adding matching automation. Add milestone 5 to reduce review effort. Use milestone 6 to prepare for unattended operation; basic access protection, secret handling, and safe uploads are required earlier, not deferred to this milestone.

## Validation approach

Run existing checks before each change. Add focused regression coverage for changed behavior where coverage is missing. Use controlled local receipt files while WebDAV is unavailable. Validate OpenAI, Xero, and Paperless separately with appropriate credentials and an appropriate Xero organisation. Record live WebDAV validation as blocked until access is restored. Record actual commands and observed results for each milestone. Do not treat a successful compile as proof of a working external integration.
