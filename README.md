# go-receipt

Read receipts from WebDAV or a local directory, extract structured data with OpenAI, and preserve the results in SQLite. Original files remain in the intake during processing. Xero attachment upload, Paperless archiving, intake cleanup, and the review UI are planned in [milestone.md](milestone.md).

## Setup

Use the Nix development shell or the Go version specified in `go.mod`. Run commands from the repository root.

Create the database and apply all migrations before using the CLI:

```sh
umask 077
go tool goose -dir . sqlite3 ./app.db up
mkdir -p inbox
```

The database contains receipt hashes, source locations, extracted data, and OAuth client secrets. It does not contain original file bytes. Restrict access to the database, intake files, and their backups. The default inbox, database files, and default secret files are excluded from Git.

## Local receipt workflow

Place JPEG, PNG, WebP, GIF, or PDF receipts in `inbox`. The importer reads top-level regular files. It skips directories and symlinks.

Set `OPENAI_API_KEY` or save the key in `.openai-password`. Then import and extract:

```sh
go run . scan --dir ./inbox
```

Use `--api-key-file` to select a different secret file. The environment variable takes precedence. Use `--model` to change the extraction model and `--timeout` to change the per-receipt timeout.

To record source locations and hashes without contacting OpenAI:

```sh
go run . scan --dir ./inbox --import-only
```

Inspect persisted status, extracted data, and errors:

```sh
go run . scan list
```

Process pending receipts and retry failed extractions:

```sh
go run . scan retry
```

Retries read the saved source locations and verify each file against its recorded hash before extraction. Local retries do not need `--dir`, but the original folder must remain accessible. WebDAV retries need the connection settings shown below. All scan commands accept `--db` to select a different migrated database.

### Processing rules

- A SHA-256 content hash identifies each receipt across input sources. Importing identical bytes does not create another record or reset an existing result.
- SQLite stores the first source location, not the original bytes. Keep intake files at their original paths and do not replace their contents during processing.
- There is no separate staging directory. This version does not delete intake files or upload to Paperless.
- A missing or changed source is recorded as a failure. Restore the original file before retrying. No extraction request is sent for that receipt until the hash matches.
- `pending` means the receipt record is saved but has no completed extraction.
- `extracted` means the extraction passed field validation. It does not mean approved, matched, attached, or reconciled.
- `failed` means source access, source verification, extraction, or validation failed. `scan list` includes the error and attempt count.
- Normal scans process pending receipts. Successful and failed receipts are skipped. Use `scan retry` to include failed receipts.
- A failure does not stop extraction of other receipts. The command prints a JSON summary and exits with an error if that run had failures.
- Files that cannot be read or imported are reported as import errors. They do not have saved receipt records and must be imported again after the source problem is fixed.
- Totals remain decimal strings in extraction JSON and SQLite. They are not converted to floating-point values.
- Run only one scan or retry command per database at a time. Concurrent worker coordination is not implemented.

## WebDAV input

WebDAV is optional. It feeds the same processing workflow as local files.

Set `WEBDAV_PASSWORD` or save the password in `.storage-password`. Then run:

```sh
go run . scan \
  --webdav-url https://storage.example.com \
  --webdav-user your-user \
  --webdav-inbox inbox
```

Use `--webdav-password-file` to select a different secret file. Do not put credentials in the URL. Choose either `--dir` or `--webdav-url`, not both.

Retry previously imported WebDAV receipts without listing the inbox again:

```sh
go run . scan retry \
  --webdav-url https://storage.example.com \
  --webdav-user your-user
```

Use the same endpoint as the original import. Credentials are not stored in receipt records. If the source is unavailable, restore access and retry.

Live WebDAV validation is blocked until service access is restored.

## Planned completion workflow

WebDAV or the local directory holds work in progress. Paperless holds the completed archive:

1. Extract and review the receipt.
2. Confirm attachment to the approved Xero transaction.
3. Archive the original in Paperless with the final tags and metadata.
4. Confirm successful Paperless consumption and save its document reference.
5. Verify that the intake file still matches the receipt hash, then remove it.

Xero attachment, Paperless archiving, and cleanup need separate persisted progress. A failed archive must not repeat a successful Xero upload. Unmatched and pending receipts stay in the intake. Ignored receipts require an explicit retention decision.

These completion steps are not implemented in milestone 1.

## OAuth client configuration

The CLI can store Xero client credentials. It cannot log in to Xero yet.

```sh
go run . oauth set --client-id CLIENT_ID --client-secret CLIENT_SECRET
go run . oauth get --name Xero
```

`oauth get` includes the client secret. Do not copy its output into logs or issue reports.

## Validation

```sh
go test ./...
go vet ./...
```

The focused CLI regression test uses real SQLite and a local HTTP server instead of OpenAI. It checks import, validation failure, repeat scans, persistence across connections, rejection of missing or changed source files, and successful retry after the original file is restored. It does not establish live OpenAI or WebDAV compatibility.
