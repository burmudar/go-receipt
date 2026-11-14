# go-receipt

Read receipts from WebDAV or a local directory, extract structured data with OpenAI, and preserve the results in SQLite. Original files remain in the intake during processing. The `serve` command imports local receipts in the background, provides receipt pages, and connects Xero through the browser. Attachment upload, Paperless archiving, intake cleanup, and the review UI are planned in [milestone.md](milestone.md). Xero API findings are in [docs/xero-api.md](docs/xero-api.md).

## Setup

Use the Nix development shell or the Go version specified in `go.mod`. Run commands from the repository root.

Create the database and apply all migrations before using the CLI:

```sh
umask 077
go tool goose -dir . sqlite3 ./app.db up
mkdir -p inbox
```

All commands accept `--db` to select the database. The default is `app.db`.

The database contains receipt hashes, source locations, extracted data, OAuth client secrets, and Xero tokens. It does not contain original file bytes. Restrict access to the database, intake files, and their backups. The default inbox, database files, and default secret files are excluded from Git.

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

Retries read the saved source locations and verify each file against its recorded hash before extraction. Local retries do not need `--dir`, but the original folder must remain accessible. WebDAV retries need the connection settings shown below.

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
- Run only one receipt processor per database at a time. Stop the service before running `scan` or `scan retry`; concurrent worker coordination is not implemented.

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

## Service and Xero connection

Save the Xero app client credentials. This is the only CLI setup step:

```sh
go run . oauth set --client-id CLIENT_ID --client-secret CLIENT_SECRET
go run . oauth get --name Xero
```

`oauth get` does not show the client secret. Restart the service after you change the credentials.

Run the service on the media PC:

```sh
go run . serve --dir ./inbox
```

Set `OPENAI_API_KEY` or use `--api-key-file` as for `scan`. The service imports and extracts pending local receipts immediately and then every minute; `--receipt-interval` changes the interval. Without `--dir`, receipt import is disabled. Failed extractions remain visible and are not retried automatically; stop the service and use `scan retry` to retry them. View originals and extraction results under **View receipts**. The document is read from the retained source and is refused if its hash has changed. The service currently supports local sources for background import and preview; WebDAV remains available through the scan CLI.

The service listens on `127.0.0.1:11211`. The default public URL is `http://localhost:11211`. Open the status page, select **Connect to Xero**, and authorise the organisation. The service uses one organisation, so authorise exactly one. If more than one organisation is connected to the Xero app, the connection fails. Disconnect the app from the other organisations in Xero, then connect again. To change the organisation, select **Disconnect** and connect again.

Register the redirect URI on the status page for the Xero app. It is the public URL followed by `/xero/callback`. The service requests the scopes `offline_access accounting.banktransactions.read accounting.attachments`.

The service checks the connection every 24 hours. Each check refreshes the tokens, because an unused Xero refresh token expires after 60 days. If Xero rejects the connection, the status page shows **Reconnect required**.

### Bank transactions

The service refreshes bank transactions in the background at startup and every 15 minutes while connected; use `--transaction-interval` to change the interval. **Refresh transactions** also starts a manual refresh. The first refresh fetches all transactions. Each later refresh fetches only the transactions that changed since the last complete refresh. The service stores `SPEND` and `RECEIVE` transactions, including deleted and voided ones. Only reconciled, authorised transactions can receive a receipt. Select **View transactions** to see the latest 500.

A refresh saves each page when it arrives. If a refresh fails, the status page shows the error, and the next refresh starts again from the last complete refresh. A short rate-limit response delays the refresh. The daily limit stops it.

### Home-network access through Caddy

Xero accepts plain `http` redirect URIs only for `localhost`. For other devices on the home network, use Caddy on the media PC for HTTPS:

```caddyfile
receipts.home.arpa {
	tls internal
	reverse_proxy 127.0.0.1:11211
}
```

- Make the host name resolve to the media PC on the home network, for example in the router DNS.
- `tls internal` uses the Caddy local certificate authority. Install the Caddy root certificate on each device that uses the service.
- Register `https://receipts.home.arpa/xero/callback` as a redirect URI for the Xero app. An app can have more than one redirect URI.

Set a password, then start the service with the Caddy address:

```sh
umask 077
printf '%s\n' 'a long password' > .rcpt-password
go run . serve --dir ./inbox --public-url https://receipts.home.arpa
```

`RCPT_PASSWORD` takes precedence over `--password-file`. The service does not start without a password if the public URL or the listen address is not local. A restart ends all login sessions. Do not expose the service to the internet.

`go run . serve --help` shows the timeout and check-interval flags.

## Validation

```sh
go test ./...
go vet ./...
```

The server test uses real SQLite and a controlled Xero server. It checks login, the browser connection flow, OAuth state validation, rejection of more than one organisation, token refresh after a restart, and the reconnect requirement after revocation. It does not establish live Xero compatibility.

The focused CLI regression test uses real SQLite and a local HTTP server instead of OpenAI. It checks import, validation failure, repeat scans, persistence across connections, rejection of missing or changed source files, and successful retry after the original file is restored. It does not establish live OpenAI or WebDAV compatibility.
