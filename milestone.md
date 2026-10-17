# Receipt service milestones

## Goal

Run a service that reads receipts from WebDAV, extracts structured data, matches receipts to Xero transactions, and uploads approved receipt attachments. Provide a UI that shows completed work and items that need manual review.

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
- Keep Paperless integration outside the first end-to-end workflow.
- Use a local receipt folder while WebDAV is unavailable. Local and WebDAV input must use the same extraction, persistence, review, and attachment workflow.
- Do not block milestones 1–5 on WebDAV access. Live WebDAV validation remains required before claiming the WebDAV deployment is ready.

## Milestone 1: Reliable extraction and persistent receipt records

**Outcome:** Each discovered receipt has a durable record that survives a restart.

**Status:** Implemented for local input. The CLI regression test uses real SQLite and a controlled OpenAI endpoint. Live OpenAI and WebDAV validation remain unverified. The database stores original receipt bytes, so retries do not require source access.

### Work

- Add a configurable local receipt folder as an input source. Keep receipt processing independent of WebDAV access.
- Remove password logging and correct the image MIME-type check, confidence JSON field name, and format-string error.
- Replace hard-coded connection settings with configuration. Return actionable errors instead of panicking or silently ignoring failures.
- Persist receipt identity, source location, extraction results, processing status, timestamps, and errors.
- Use the content hash to detect receipts already imported.
- Preserve access to the original receipt for preview and attachment upload.
- Remove the temporary three-receipt limit. Record failures per receipt so one failure does not stop the whole batch.
- Validate extracted data before it becomes eligible for matching. Use an exact representation for monetary amounts.

### Done when

- [x] Receipts from a local folder can be imported and extracted without a WebDAV connection (controlled extraction endpoint).
- [x] Importing the same local folder twice does not create duplicate receipt records or repeat successful extraction.
- [x] A restart preserves extracted data and processing status.
- [x] An extraction failure is visible and can be retried without losing the receipt.
- [x] `go test ./...` passes.

## Milestone 2: Xero connection and transaction access

**Outcome:** The application can maintain a Xero connection and retrieve supported attachment targets.

### Work

- Confirm the target Xero record types, API permissions, and attachment constraints.
- Implement OAuth login, callback validation, token persistence, and token refresh.
- Select and persist the Xero organisation connection.
- Fetch and store the transaction data needed for review and matching, including stable Xero identifiers.
- Handle pagination, rate limits, connection expiry, and reconnect requirements.
- Keep credentials and tokens out of logs and UI responses.

### Done when

- [ ] A user can connect the intended Xero organisation.
- [ ] The connection survives an application restart and access-token expiry.
- [ ] The application can retrieve real transactions from that organisation.
- [ ] A revoked connection produces a clear reconnect requirement.
- [ ] Supported attachment targets and the boundary with accounting reconciliation are documented.

## Milestone 3: Service and manual review UI

**Outcome:** A running service imports receipts, and the UI lets the user inspect and approve transaction links.

### Work

- Add a server command and a background worker for receipt import, extraction, and transaction refresh.
- Add receipt list and detail pages with the original document beside extracted data.
- Allow corrections to extracted fields while retaining the original extraction result.
- Allow transaction search and manual selection within the connected organisation.
- Persist approval, rejection, and ignore decisions with timestamps and an audit history.
- Separate needs-review, unmatched, approved, completed, ignored, and failed items in the UI. Approved must not mean uploaded.
- Protect UI access according to the deployment environment before exposing financial data.

### Done when

- [ ] New receipts in the configured local folder appear without running the scan CLI manually.
- [ ] The user can inspect a receipt, correct its data, and approve a transaction link.
- [ ] An unmatched receipt can remain pending or be ignored with a recorded decision.
- [ ] A restart preserves decisions and does not repeat completed work.
- [ ] Approved receipts remain visibly pending until attachment upload succeeds.

## Milestone 4: Confirmed Xero attachment upload

**Outcome:** An approved receipt is attached to the selected Xero transaction, with a durable completion record.

### Work

- Upload the original receipt to the approved Xero target.
- Record the target identifier, attachment identifier where available, upload result, and completion time.
- Make upload retries safe. Check for an existing attachment when a timeout leaves the result uncertain.
- Show pending uploads, failures, and confirmed completion separately.
- Provide a link to the Xero record where supported.
- Prevent changes to an approved target from silently causing an additional upload. Require an explicit follow-up decision.

### Done when

- [ ] One receipt completes the full path from local folder import to manual approval and confirmed Xero attachment.
- [ ] The attachment is visible on the intended Xero record.
- [ ] A retry or restart does not create a duplicate attachment.
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
- Document deployment, configuration, backups, and restore procedures for the database and receipt files.
- Define and validate a conservative automatic-approval policy using reviewed receipts before enabling it.
- Keep automatic approval disabled by default. Record whether each approval was manual or automatic and which policy applied.
- Provide a way to pause automatic processing without losing pending work.

### Done when

- [ ] The service recovers from restarts and temporary receipt-source, OpenAI, and Xero failures. Live WebDAV checks remain separate until access is restored.
- [ ] A backup can restore receipt records, decisions, and access to original files.
- [ ] The user can see all items that need intervention without reading logs.
- [ ] If automatic approval is enabled, only eligible, unambiguous matches proceed automatically.
- [ ] Completed items show their approval method and confirmed upload result.

## Deferred validation: Live WebDAV access

**Blocked by:** Access to the WebDAV service. These checks can run as soon as access is restored; they do not need to wait for milestone 6.

- [ ] Validate authentication, inbox listing, and receipt downloads against the live service.
- [ ] Confirm WebDAV receipts use the same processing workflow as local receipts.
- [ ] Confirm a receipt already imported locally is not duplicated or extracted again when discovered through WebDAV.
- [ ] Complete the WebDAV-to-Xero workflow with a controlled receipt.
- [ ] Confirm connection failures are visible and processing resumes safely after access returns.

Do not mark these checks complete based on local-folder tests alone.

## Delivery order

Implement milestones 1–4 in order. Complete one manually approved receipt attachment before adding matching automation. Add milestone 5 to reduce review effort. Use milestone 6 to prepare for unattended operation; basic access protection, secret handling, and safe uploads are required earlier, not deferred to this milestone.

## Validation approach

Run existing checks before each change. Add focused regression coverage for changed behavior where coverage is missing. Use controlled local receipt files while WebDAV is unavailable. Validate OpenAI and Xero separately with appropriate credentials and an appropriate Xero organisation. Record live WebDAV validation as blocked until access is restored. Record actual commands and observed results for each milestone. Do not treat a successful compile as proof of a working external integration.
