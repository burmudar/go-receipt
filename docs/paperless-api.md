# Paperless archive (milestone 4b)

## Setup

Apply migrations, including `20261102120000_paperless_archive.sql`, before starting the service.
Archiving is disabled unless a Paperless URL is configured. Only receipts with a
confirmed Xero attachment are eligible. Run one service per database.

Create a Paperless API token and at least one receipt tag in your instance. Give the
token user permission to upload, view/download, change documents, and add notes.
The token must be able to see every document that could match a receipt checksum.
Store the token in a restricted secret file; never commit it.

Add to your service TOML (paths are relative to that file):

```toml
[paperless]
url = "https://paperless.home.arpa"
token_file = "secrets/paperless-token"
tags = "7,12" # existing tag IDs, comma separated
timeout = "1m"
```

Equivalent flags: `--paperless-url`, `--paperless-token-file`,
`--paperless-tags`, `--paperless-timeout`. No environment token override.
Use HTTPS outside a trusted local connection. Redirects are refused, including
same-host redirects; configure the final base URL. No credentials in URLs.

The worker runs at the receipt interval, independently of the Xero connection.
It reads local or WebDAV originals through the same source abstraction, verifies
SHA-256, and limits uploads/downloads to 10 MB (the attachment limit).

## Metadata and confirmation

- Upload the original with configured receipt tags and the corrected receipt date.
- Title: corrected merchant and exact amount/currency, truncated to Paperless's
  128-character title limit.
- A note preserves the full merchant, amount/currency, receipt SHA-256, Xero
  organisation ID and bank transaction ID. Existing tags and notes are retained.
- Persist the upload's task UUID before polling `/api/tasks/?task_id=...`.
  Acceptance is not confirmation. Known active tasks remain consuming.
- A successful task must identify a document. Download its original and verify
  SHA-256, then apply final metadata before recording the document ID and completion.
- Existing or recovered checksum matches are verified the same way.
- Receipt pages show archive state, errors, task and document references; the list
  can filter pending, failed and confirmed archives. Cleanup remains pending even
  after archive confirmation. Intake files are never removed.

## Recovery

Before a first upload, search `/api/documents/?checksum__iexact=...` using
Paperless's MD5 checksum. MD5 is a lookup hint only, never identity proof.
Multiple matches or a different SHA-256 require manual investigation.

Dispatch is persisted **before** sending the upload. If the response is lost, or
the service crashes before saving the task UUID, subsequent runs search by the
persisted checksum. An absent match does not prove the upload failed: it may still
be queued. The service does not submit a second copy automatically.

Temporary source/read/search/metadata failures retry at the next interval.
Known tasks continue to be polled after restart. Failed or missing tasks also
allow checksum recovery, but never automatic resubmission. For a terminal failure
with no document, fix the problem in Paperless and manually consume the retained
original there; once visible by checksum, the worker verifies it and adds final
metadata. Do not reset dispatch state merely because a search returned no result.
Confirmed Xero attachments are not changed or repeated by archive processing.

The configured endpoint is recorded per receipt. Changing it blocks unfinished
work until the original endpoint is restored; this prevents mistaking another
Paperless installation for the destination of an uncertain upload.

## API evidence and validation limits

Checked against Paperless-ngx v2.20.0 source:
- [Upload/task API documentation](https://github.com/paperless-ngx/paperless-ngx/blob/v2.20.0/docs/api.md)
- [Checksum filter](https://github.com/paperless-ngx/paperless-ngx/blob/v2.20.0/src/documents/filters.py)
- [Original download, task filter and notes endpoints](https://github.com/paperless-ngx/paperless-ngx/blob/v2.20.0/src/documents/views.py)
- [Unique original checksum and title limit](https://github.com/paperless-ngx/paperless-ngx/blob/v2.20.0/src/documents/models.py)

Source defines a unique original checksum. Live validation on the supplied
Paperless instance confirmed that a second upload is accepted as a task but
consumption fails with a duplicate error, leaving one checksum match. Recovery
still deliberately does not depend on duplicate rejection. This is not proof of
compatibility with every deployed Paperless version.

Tests cover real SQLite migrations, acceptance versus consumption, restart,
uncertain response recovery, source loss/change, checksum collisions, metadata
failure, terminal task failure, endpoint changes and credential-safe redirects.
Live validation also passed acceptance versus consumption, original SHA-256,
corrected date/title/tags and transaction notes, restart-style manager recovery,
and checksum recovery after removing the local task reference. The synthetic PDF
used an isolated SQLite database with a seeded Xero confirmation, not a real
receipt or live Xero request. The test document and tag were deleted afterward;
Paperless trash retention may retain the deleted synthetic document. Live WebDAV
validation remains deferred separately.

Run the opt-in live test (creates and removes a synthetic document and tag):

```sh
RCPT_TEST_PAPERLESS_URL=https://your-paperless-host \
RCPT_TEST_PAPERLESS_PASSWORD_FILE=/absolute/path/to/admin-password \
  go test ./paperless -run '^TestLivePaperless$' -v -count=1
```

The harness authenticates as `admin`; credentials and tokens are never printed.
Normal tests skip live access unless the URL environment variable is explicitly set.
