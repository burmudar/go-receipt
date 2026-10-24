# Xero API findings

Checked on 2026-10-19 for milestone 2a. The Xero developer pages use JavaScript rendering, and a direct fetch could not read them. The findings come from these sources:

- **Spec:** the Xero OpenAPI specification, [`xero_accounting.yaml`](https://github.com/XeroAPI/Xero-OpenAPI/blob/master/xero_accounting.yaml), version 19.1.0, and [`xero-finance.yaml`](https://github.com/XeroAPI/Xero-OpenAPI/blob/master/xero-finance.yaml).
- **FAQ:** the Xero developer FAQ pages for [OAuth 2.0](https://developer.xero.com/faq/oauth2), [limits](https://developer.xero.com/faq/limits), and [granular scopes](https://developer.xero.com/faq/granular-scopes).
- **Search:** web search summaries that cite Xero documentation pages. Confirm these findings in the Xero developer portal before you depend on them.

## Attachment targets

- The Accounting API does not expose bank statement lines (bank feed lines). An unreconciled statement line can have no matching `BankTransaction` record. (Spec, Search)
- The Finance API endpoint `GET /BankStatementsPlus/statements` returns statement lines with `isReconciled`. It requires the `finance.bankstatementsplus.read` scope. Xero gives Finance API access only to partners. Do not use it. (Spec, Search)
- `GET /BankTransactions` returns spend-money and receive-money records that exist in Xero. A record exists after the user creates it in Xero, usually during bank reconciliation. A user can also create a record manually, before reconciliation. Each record has `IsReconciled`. (Spec)
- `Type` values are `SPEND`, `RECEIVE`, `SPEND-TRANSFER`, `RECEIVE-TRANSFER`, `SPEND-PREPAYMENT`, `RECEIVE-PREPAYMENT`, `SPEND-OVERPAYMENT`, and `RECEIVE-OVERPAYMENT`. Use only `SPEND` and `RECEIVE`. (Spec)
- `Status` values are `AUTHORISED`, `DELETED`, and `VOIDED`. Link receipts only to `AUTHORISED` records. (Spec)
- Useful fields: `BankTransactionID`, `Type`, `Contact`, `BankAccount`, `Date`, `Reference`, `CurrencyCode`, `Total`, `Status`, `IsReconciled`, `HasAttachments`, and `UpdatedDateUTC`. (Spec)

**Conclusion:** A receipt can match only a transaction that exists in Xero. For a bank feed workflow, the transaction exists after reconciliation. The service must not depend on statement lines.

## Reading bank transactions

- **Pagination:** the `page` query parameter returns up to 100 records for each page. A `pageSize` parameter is also available. (Spec)
- **Incremental refresh:** the `If-Modified-Since` header returns only records that were created or changed after that time. (Spec)
- **Filter:** the `where` query parameter accepts expressions such as `Status=="AUTHORISED"`. (Spec)
- **Organisation:** each request must include the `xero-tenant-id` header. (Spec)
- **Open question:** it is not confirmed whether `If-Modified-Since` returns records that changed to `DELETED` or `VOIDED`. Test this with a real organisation in milestone 2b.
- **Response format:** request `Accept: application/json`. Dates use the format `/Date(1476316800000+0000)/`: milliseconds since the Unix epoch in UTC. `Total` is a JSON number. (Spec)
- **Rate-limit response:** HTTP 429 has a `Retry-After` header in seconds and an `X-Rate-Limit-Problem` header that names the limit. (FAQ, Search)

### How the service refreshes

Milestone 2b uses these choices. Each one avoids a failure that a live organisation can cause:

- The service requests all types without a `where` filter and keeps only `SPEND` and `RECEIVE`. A `Status` filter would let a voided record leave the result during a refresh and move later records to an earlier page.
- The service orders by `BankTransactionID`. A change during a refresh does not move a record in this order. A record can be read twice, but no record is skipped. It is not confirmed that Xero accepts this order.
- The next `If-Modified-Since` is the HTTP `Date` header of the first page, which is the Xero clock. The service saves it only after all pages are saved.
- The service waits for a `Retry-After` of up to one minute, at most three times for each page. A longer wait stops the refresh.

## Attachments

- **Upload:** `PUT /BankTransactions/{BankTransactionID}/Attachments/{FileName}` creates an attachment. The request body contains the file bytes as `application/octet-stream`. (Spec)
- **Replace:** `POST` on the same path updates the attachment that has that file name. Use a file name that is unique for each receipt, for example one that includes the content hash. (Spec)
- **List:** `GET /BankTransactions/{BankTransactionID}/Attachments` lists existing attachments. Use it to check an uncertain upload. (Spec)
- **Retry:** attachment requests accept an `Idempotency-Key` header of up to 128 characters. Use a stable key for each receipt and target for safe retries in milestone 4. (Spec)
- **Limits:** the search summaries give 10 attachments for each document. They give 10 MB or 25 MB for each file, and these values are not consistent. Confirm the limit for bank transactions before milestone 4. (Search)

## OAuth

- **Endpoints:** authorisation at `https://login.xero.com/identity/connect/authorize` and tokens at `https://identity.xero.com/connect/token`. (Spec)
- **Access token:** expires after 30 minutes. (FAQ)
- **Refresh token:**
  - The token rotates. Replace the stored refresh token with the new one after each refresh.
  - If a refresh response is lost, you can retry with the previous refresh token for 30 minutes.
  - An unused refresh token expires after 60 days. After that, the user must connect again. (FAQ)
- **Redirect URI:**
  - Each redirect URI must use HTTPS. The only exception is `http://localhost`.
  - A redirect URI cannot contain wildcards.
  - An app can register up to 50 redirect URIs. The `redirect_uri` in the request must exactly match a registered URI. (FAQ, Search)
- **Home-network access:** a connection flow from another device needs an HTTPS redirect URI with a certificate that the browser trusts. Xero sends only a browser redirect to this URI, so the host does not need to be reachable from the internet. This project uses Caddy on the media PC for HTTPS.

## Scopes

- Apps created on or after 2 March 2026 use granular scopes. Older apps can use broad scopes until September 2027. (FAQ)
- **Granular scopes:** `offline_access accounting.banktransactions.read accounting.attachments`. (Search; confirmed with live Xero on 24 October 2026: Xero granted all three.) This project uses these scopes, because its Xero app was created after 2 March 2026.
- Xero can grant more scopes than the app requests. On 24 October 2026, Xero also granted `openid profile email accounting.banktransactions`, the scopes of an earlier CLI login for the same app. (Observed)
- **Broad scopes:** `offline_access accounting.transactions.read accounting.attachments`. (Spec) Not used by this project.
- `accounting.attachments` gives read and write access to attachments. `offline_access` is necessary to get refresh tokens.
- A change of scopes requires the user to authorise the app again.

## Rate limits

Limits for each organisation (FAQ):

- 5 calls in progress at the same time.
- 60 calls each minute.
- 5000 calls each day.

A call over a limit returns HTTP 429. Responses include `X-DayLimit-Remaining`, `X-MinLimit-Remaining`, and `X-AppMinLimit-Remaining`.

## Accounting reconciliation

The user reconciles transactions in Xero. The service reads `IsReconciled` but never changes it. An attachment upload does not change the reconciliation status.
