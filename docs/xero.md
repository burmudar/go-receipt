# Xero connection and attachment targets

## Milestone 2 boundary

This milestone reads accounting records. It does not create transactions, upload attachments, import bank statement lines, or reconcile accounts.

The supported targets are:

| Target | Read endpoint | Included type | Stable identity | Future attachment path |
| --- | --- | --- | --- | --- |
| Spend-money bank transaction | `GET /BankTransactions` | `SPEND` | Tenant ID + `BankTransactionID` | `/BankTransactions/{BankTransactionID}/Attachments/{FileName}` |
| Purchase bill | `GET /Invoices` | `ACCPAY` | Tenant ID + `InvoiceID` | `/Invoices/{InvoiceID}/Attachments/{FileName}` |

The API base is `https://api.xero.com/api.xro/2.0`. Requests include the selected tenant in `xero-tenant-id`. Deleted and voided records are excluded from the target snapshot. Other states remain visible for future review; inclusion is not approval to attach.

Receive-money transactions, sales invoices, bank transfers, credit notes, and other Xero attachment targets are outside this version. Bill payments are not separate attachment targets here. Select the bill, not its payment record.

**Bank transactions are not bank statement lines.** These endpoints do not provide the unreconciled bank-feed inbox. The application cannot offer matches to records that Xero has not yet created as supported accounting records. Bank feed access is a separate API and is not requested.

`IsReconciled` is stored only when Xero reports it for a bank transaction. A bill has no inferred reconciliation state. An attachment must never be described as accounting reconciliation. No accounting write scope is requested.

## OAuth and permissions

Create an OAuth web app in the Xero developer portal. Register the exact redirect URI used by `oauth set`. The default is `http://localhost:11211/callback`. The current CLI supports only HTTP loopback callbacks with an explicit port. It does not expose a remote callback server.

The authorization request uses these granular scopes:

- `offline_access`: request a refresh token.
- `accounting.banktransactions.read`: read bank transactions.
- `accounting.invoices.read`: read bills through the Invoices endpoint.

The application does not request identity claims or use an ID token for authentication. The local operator chooses the organisation from the connections returned by Xero. API access still depends on that user's Xero permissions.

Xero introduced granular scopes for new apps on 2 March 2026. Use these scopes rather than the older broad `accounting.transactions.read` scope. Milestone 4 must request `accounting.attachments` before uploading. Read-only attachment access uses `accounting.attachments.read`; neither attachment scope is needed by this milestone.

Endpoints:

- Authorization: `https://login.xero.com/identity/connect/authorize`
- Token exchange and refresh: `https://identity.xero.com/connect/token`
- Organisation connections: `GET https://api.xero.com/connections`

Login creates a random state value and accepts one valid callback within five minutes. The callback checks the method, host, path, and state. Token requests authenticate the OAuth client with HTTP Basic authentication. The browser receives no code, token, or upstream error body in the response.

Access tokens normally expire after 30 minutes. Refresh tokens rotate and expire after 60 days without use. Each refresh saves both replacement tokens before making another API request. If a refresh response is lost, Xero permits retry with the previous refresh token for a 30-minute grace period. Retry promptly; outside that window, login may be required.

An `invalid_grant` refresh response or a missing selected connection sets a durable reconnect requirement. A second HTTP 401 after refresh does the same. HTTP 403 reports a permissions or scope problem. Login replaces the grant and clears the selection, so the operator must select the intended organisation again.

Credentials and tokens remain in SQLite, not in command output. The database is not encrypted. Use restrictive file permissions and protect backups. Client credential changes invalidate the stored grant. Only one Xero command may use a database at a time; cross-process refresh coordination is not implemented.

## Transaction snapshots

Sync checks the selected connection, then reads both endpoints in pages of 100. It continues until an empty page and rejects repeated or missing identifiers. It publishes the new snapshot and sync timestamp in one SQLite transaction only after every page succeeds.

The snapshot contains the tenant, record kind, Xero ID, contact ID and name, date, decimal total, currency, reference, bill number, status, attachment flag, and reported reconciliation state. Monetary values never pass through a floating-point representation. Snapshot keys include the tenant to prevent cross-organisation matches.

A failed request retains the previous snapshot and timestamp. HTTP 429 returns the `Retry-After` delay. Wait for that delay before rerunning sync. Long-term scheduling, background refresh, and automatic backoff belong to later milestones. Local status and cached lists do not establish that a connection is still valid.

## Attachment constraints for milestone 4

Xero's attachment endpoints accept raw file bytes with the file's MIME type in `Content-Type`. The parent accounting record must already exist. List existing attachments before an upload retry when the first result is uncertain.

The generic [Attachments documentation](https://developer.xero.com/documentation/api/accounting/attachments/) reports a maximum of 10 attachments per document and 10 MB per file. The [Invoices documentation](https://developer.xero.com/documentation/api/accounting/invoices/) reports 10 attachments and a larger 25 MB per-file limit. These pages are not consistent about size. Use the lower documented limit when designing the shared uploader, and confirm actual limits and accepted receipt formats before enabling uploads. This milestone has no uploader and does not validate upload compatibility.

Keep upload confirmation separate from approval, accounting reconciliation, Paperless consumption, and intake cleanup. An attachment alone must not permit intake deletion.

## Validation

The automated workflow uses a controlled Xero server and real SQLite. It covers login state rejection, selection, pagination, exact totals, restart and refresh rotation, a one-time HTTP 401 refresh, rate-limit snapshot retention, tenant isolation, revoked access, refresh expiry, and secret redaction.

Commands run for this milestone:

```text
$ go test ./xero -run TestConnectionSurvivesRestartAndSyncFailure -count=1 -v
=== RUN   TestConnectionSurvivesRestartAndSyncFailure
--- PASS: TestConnectionSurvivesRestartAndSyncFailure (0.02s)
PASS
ok      github.com/burmudar/go-receipt/xero  0.356s

$ go vet ./...
(no output; exit 0)

$ go test -race ./xero
ok      github.com/burmudar/go-receipt/xero  1.460s
```

`go test ./...` also passed. A temporary database passed migration up, down, and up again. A manual CLI check produced:

```text
$ XERO_CLIENT_SECRET=controlled-secret go run . oauth --db "$tmp/app.db" set --client-id controlled-client
(no output; exit 0)

$ go run . oauth --db "$tmp/app.db" get --name Xero
[{"ID":1,"Name":"Xero","ClientID":"controlled-client","RedirectURI":"http://localhost:11211/callback","CreatedAt":"2026-10-17T01:22:49Z"}]

$ go run . xero --db "$tmp/app.db" status
{"connected":false,"reconnect_required":true}
```

### Live validation still required

Use a test organisation with known bank transactions and purchase bills:

1. Configure the real app, run `xero login`, and complete consent.
2. Run `xero connections`. Confirm the intended organisation by name and tenant ID.
3. Run `xero select --tenant-id ID`, then `xero sync` and `xero list`.
4. Compare returned IDs, exact amounts, dates, contacts, and statuses with Xero.
5. Exit the application. Wait for access-token expiry. Run sync again and confirm refresh succeeds without consent.
6. Revoke the connection in Xero. Run sync and confirm the reconnect requirement. Complete login and selection again.
7. Record the commands and redacted results before marking the live milestone checks complete.

No live Xero login or transaction retrieval was performed for this implementation. Do not infer external compatibility from the controlled endpoint test.

## API sources

- [Bank Transactions](https://developer.xero.com/documentation/api/accounting/banktransactions/)
- [Invoices](https://developer.xero.com/documentation/api/accounting/invoices/)
- [Attachments](https://developer.xero.com/documentation/api/accounting/attachments/)
- [Scopes](https://developer.xero.com/documentation/guides/oauth2/scopes/) and [granular-scope migration](https://developer.xero.com/faq/granular-scopes)
- [Authorization code flow](https://developer.xero.com/documentation/guides/oauth2/auth-flow/)
- [Token types](https://developer.xero.com/documentation/guides/oauth2/token-types/)
- [Rate limits](https://developer.xero.com/documentation/best-practices/api-call-efficiencies/rate-limits/)
- [Xero Accounting OpenAPI](https://github.com/XeroAPI/Xero-OpenAPI/blob/master/xero_accounting.yaml)
