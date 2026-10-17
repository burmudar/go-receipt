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

- Treat **receipt attachment** and **Xero accounting reconciliation** as separate operations. An uploaded attachment does not establish that Xero considers a transaction reconciled.
- Before implementing Xero transaction access, confirm which Xero record types are attachment targets and which operations the API supports.
- Keep extraction confidence separate from match confidence. Accurate extraction does not establish a correct transaction match.
- Use one Go service with SQLite and a simple web UI initially. Separate deployments are not required for the first version.
- WebDAV or the local directory holds original receipts during processing. Files remain at their intake locations until processing is complete. SQLite stores hashes, source references, extracted data, and workflow state, not file bytes. No separate staging directory is required.
- Paperless-ngx holds the completed archive. Upload to Paperless after the Xero attachment succeeds, not during initial import. Include final receipt tags and metadata.
- Track Xero attachment, Paperless consumption, and intake cleanup separately. Remove the intake file only after both remote operations are confirmed and the file still matches the processed hash. An archive retry must not repeat a successful Xero attachment.
- Keep unmatched and pending receipts in the intake. Ignored receipts need an explicit retention decision before cleanup.
- Use a local receipt folder while WebDAV is unavailable. Local and WebDAV input must use the same extraction, persistence, review, and attachment workflow.
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

## Milestone 2: Xero connection and transaction access

**Outcome:** The application can maintain a Xero connection and retrieve supported attachment targets.

**Status:** Implemented with controlled Xero endpoints and real SQLite. The CLI supports OAuth login, organisation selection, durable token refresh, and paginated target snapshots. Supported targets are spend-money bank transactions and purchase bills. Live Xero consent, token refresh, and transaction retrieval remain unverified. See [docs/xero.md](docs/xero.md) for API constraints, test results, and the live validation checklist.

### Work

- Confirm the target Xero record types, API permissions, and attachment constraints.
- Implement OAuth login, callback validation, token persistence, and token refresh.
- Select and persist the Xero organisation connection.
- Fetch and store the transaction data needed for review and matching, including stable Xero identifiers.
- Handle pagination, rate limits, connection expiry, and reconnect requirements.
- Keep credentials and tokens out of logs and UI responses.

### Done when

- [ ] A user can connect the intended Xero organisation (controlled login and selection pass; live consent remains unverified).
- [x] The connection survives an application restart and access-token expiry (controlled endpoint test).
- [ ] The application can retrieve real transactions from that organisation.
- [x] A revoked connection produces a clear reconnect requirement (controlled endpoint test).
- [x] Supported attachment targets and the boundary with accounting reconciliation are documented.

## Milestone 3: Service and manual review UI

**Outcome:** A running service imports receipts, and the UI lets the user inspect and approve transaction links.

### Work

- Add a server command and a background worker for receipt import, extraction, and transaction refresh.
- Add receipt list and detail pages with the original document beside extracted data.
- Allow corrections to extracted fields while retaining the original extraction result.
- Allow transaction search and manual selection within the connected organisation.
- Persist approval, rejection, and ignore decisions with timestamps and an audit history.
- Separate needs-review, unmatched, approved, completed, ignored, and failed items in the UI. Approved must not mean uploaded. Show Xero attachment, Paperless archive, and intake cleanup progress separately.
- Protect UI access according to the deployment environment before exposing financial data.

### Done when

- [ ] New receipts in the configured local folder appear without running the scan CLI manually.
- [ ] The user can inspect a receipt, correct its data, and approve a transaction link.
- [ ] An unmatched receipt can remain pending or be ignored with a recorded decision.
- [ ] A restart preserves decisions and does not repeat completed work.
- [ ] Approved receipts remain visibly pending until attachment upload succeeds. Attached receipts remain incomplete until Paperless archiving and intake cleanup finish.

## Milestone 4: Xero attachment, Paperless archive, and intake cleanup

**Outcome:** An approved receipt is attached to the selected Xero transaction, archived in Paperless, and safely removed from the intake. Each step has a durable completion record.

### Work

- Read the original from the intake and verify its hash before uploading to the approved Xero target.
- Record the target identifier, attachment identifier where available, upload result, and completion time.
- Make upload retries safe. Check for an existing attachment when a timeout leaves the result uncertain.
- After confirmed Xero attachment, upload the original to Paperless with receipt tags, corrected metadata, and useful transaction references.
- Persist the Paperless task reference. Wait for successful consumption and save the document reference; upload acceptance alone is not archive confirmation.
- Handle uncertain Paperless uploads without blindly submitting another copy. Resume only incomplete steps after an error or restart.
- Remove the intake file only after confirmed Xero attachment and Paperless archiving. Verify that the file still matches the processed hash and prevent deletion of a replacement file. Record cleanup progress separately.
- Show pending uploads, pending archives, pending cleanup, failures, and confirmed completion separately.
- Provide a link to the Xero record where supported.
- Prevent changes to an approved target from silently causing an additional upload. Require an explicit follow-up decision.

### Done when

- [ ] One receipt completes the full path from local folder import through manual approval, confirmed Xero attachment, confirmed Paperless archive, and intake cleanup.
- [ ] The attachment is visible on the intended Xero record.
- [ ] A retry or restart does not create a duplicate attachment or archive document.
- [ ] A failed Paperless archive leaves the intake file intact and does not repeat the confirmed Xero upload.
- [ ] Paperless contains the original receipt with final tags and metadata before intake cleanup.
- [ ] Cleanup cannot remove an unarchived receipt or a replacement file. Unmatched and pending receipts remain in the intake.
- [ ] The UI shows what was approved, what was uploaded, and what still needs attention.
- [ ] Attachment completion is not labelled as accounting reconciliation unless that status is separately verified.

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

- Add bounded retries, backoff, request timeouts, and graceful shutdown.
- Recover interrupted jobs without repeating confirmed side effects.
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

Implement milestones 1–4 in order. Complete one manually approved receipt attachment, Paperless archive, and intake cleanup before adding matching automation. Add milestone 5 to reduce review effort. Use milestone 6 to prepare for unattended operation; basic access protection, secret handling, and safe uploads are required earlier, not deferred to this milestone.

## Validation approach

Run existing checks before each change. Add focused regression coverage for changed behavior where coverage is missing. Use controlled local receipt files while WebDAV is unavailable. Validate OpenAI, Xero, and Paperless separately with appropriate credentials and an appropriate Xero organisation. Record live WebDAV validation as blocked until access is restored. Record actual commands and observed results for each milestone. Do not treat a successful compile as proof of a working external integration.
