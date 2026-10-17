# go-receipt

Import receipt files, extract structured data with OpenAI, and preserve the results in SQLite. Xero attachment upload and the review UI are planned in [milestone.md](milestone.md).

## Setup

Use the Nix development shell or the Go version specified in `go.mod`. Run commands from the repository root.

Create the database and apply all migrations before using the CLI:

```sh
umask 077
go tool goose -dir . sqlite3 ./app.db up
mkdir -p inbox
```

The database contains original receipt files, extracted data, and OAuth client secrets. Restrict access to the database and its backups. The default inbox, database files, and default secret files are excluded from Git.

## Local receipt workflow

Place JPEG, PNG, WebP, GIF, or PDF receipts in `inbox`. The importer reads top-level regular files. It skips directories and symlinks.

Set `OPENAI_API_KEY` or save the key in `.openai-password`. Then import and extract:

```sh
go run . scan --dir ./inbox
```

Use `--api-key-file` to select a different secret file. The environment variable takes precedence. Use `--model` to change the extraction model and `--timeout` to change the per-receipt timeout.

To save originals without an OpenAI key or network access:

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

Retries use the saved originals. They do not need the local folder or WebDAV connection. All scan commands accept `--db` to select a different migrated database.

### Processing rules

- A SHA-256 content hash identifies each receipt across input sources. Importing identical bytes does not create another record or reset an existing result.
- SQLite stores original bytes and the first source location together. Deleting a source file does not remove the saved receipt.
- `pending` means the receipt is saved but has no completed extraction.
- `extracted` means the extraction passed field validation. It does not mean approved, matched, attached, or reconciled.
- `failed` means extraction or validation failed. `scan list` includes the error and attempt count.
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

Live WebDAV validation is blocked until service access is restored.

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

The focused CLI regression test uses real SQLite and a local HTTP server instead of OpenAI. It checks import, validation failure, repeat scans, persistence across connections, and retry after the source folder is removed. It does not establish live OpenAI or WebDAV compatibility.
