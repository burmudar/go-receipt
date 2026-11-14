# Milestone 7: receipt-review workspace

## Framework and style

The UI uses the existing Go `html/template` server rendering with **Pico CSS
2.1.1**, a small semantic-HTML framework. Application styles add a light,
teal-accented workspace, bordered panels, readable tables, and textual status
badges. It deliberately has no SPA, JavaScript runtime dependency, frontend build
step, external font, or CDN request. All forms work as ordinary GET/POST requests.

Pico's pinned distribution is vendored in `server/static/pico.min.css`, embedded
in the Go binary, and served alongside `server/static/app.css`. Its MIT license is
included in `server/static/PICO-LICENSE.md`. Public static routes expose only the
two stylesheets (so login can render); receipt data and actions retain their
existing authentication and cross-origin protection.

Upstream sources:
- https://picocss.com/docs
- https://cdn.jsdelivr.net/npm/@picocss/pico@2.1.1/css/pico.min.css
- https://github.com/picocss/pico/blob/v2.1.1/LICENSE.md

Use `app.css` for design changes rather than editing the vendor file. Check theme
specificity: `:root[data-theme="light"]` intentionally overrides Pico's defaults.

## Screens and behavior

- **Receipt queue:** global overview counts, case-insensitive search by filename,
  merchant, amount, currency, receipt date, receipt hash or linked transaction ID;
  combined review/archive/attention filters; 25-item pages; shareable URLs that
  preserve filters during pagination. Recent imports appear first (hash is the
  deterministic tie-breaker for imports sharing the same timestamp).
- **Needs attention:** receipts requiring a review decision and receipts with
  processing errors or failed/uncertain delivery states. Approval alone is not
  completion. Overview counts describe the whole queue, not just current results.
- **Review workspace:** original document beside suggested matches and receipt
  details on desktop, stacked on mobile; a jump link takes reviewers to suggestions;
  a prominent correction action and visible form; separate extraction and matching confidence;
  suggested and manual matches, existing links, delivery
  stages, and audit history. No transaction is selected or approved automatically.
  All previous target-lock and provider-confirmation rules remain server-enforced.
- **Operations:** processing pause/resume, connected organisation, reconnect and
  refresh actions, and persistent worker health. Disconnect is separated from
  normal connected-state actions to reduce accidental clicks.
- **Transactions, login, and errors:** consistent navigation and styling, clear
  empty/error states, and links back into the workflow.

Tables scroll within named, keyboard-focusable regions on narrow screens instead
of overflowing the whole page. Labels, visible focus rings, skip navigation,
current-page navigation, semantic headings, and textual status indicators are
included. There are no animation-dependent interactions.

The original-document endpoint is unchanged. Native PDF preview depends on the
browser; an authenticated open-original link is always provided. After intake
cleanup, use the confirmed Paperless document reference to find the original.
No organisation-unsafe Xero or guessed Paperless deep links were added.

Pagination bounds rendered output, not database retrieval: this milestone still
loads the existing receipt list before filtering. Database-level pagination can
be introduced separately if dataset size warrants it.

## Moving through the queue

Open a receipt from the queue to retain its search and review/archive/attention
filters. **Previous receipt** and **Next receipt** appear at the top and bottom
of the review page, with the current position. They cross list-page boundaries
and never change a decision. Disabled controls mark the ends of the queue.

Both suggested-match and manual-selection forms offer **Approve & next** as
well as the existing approve-and-stay action. A transaction must be explicitly
selected. A successful approval advances to the next matching receipt; at the
end, **Approve & return to queue** returns to the filtered list. Failed approvals
never advance. Corrections and transaction searches retain the queue context.

This is a live filtered queue, not a frozen review session: receipts that no longer
match (for example, approved receipts in Needs review) drop out. Previous/Next
navigate the remaining matching receipts; browser Back can revisit an earlier
approved receipt. If a correction changes whether the current receipt matches,
its surrounding matching neighbours remain navigable. The server resolves the
next receipt before recording approval so removal from the queue cannot skip a
receipt. All destinations are locally generated, not client-supplied redirects.

## Validation

Standard regression checks:

```sh
go test ./...
go vet ./...
go test -race ./server
```

The optional browser test uses a fresh SQLite database, synthetic PDFs, a seeded
transaction, and the real HTTP handlers. It starts no workers and makes no
provider API calls. Browser dependencies stay outside the project:

```sh
npm install --prefix /tmp/go-receipt-ui-browser \
  playwright@1.65.0 @axe-core/playwright@4.14.0
/tmp/go-receipt-ui-browser/node_modules/.bin/playwright install chromium
NODE_PATH=/tmp/go-receipt-ui-browser/node_modules RECEIPT_UI_BROWSER=1 \
  go test ./server -run TestWorkspaceBrowser -v -count=1
```

It checks desktop (1440 px) and mobile (390 px) layouts, page overflow, automated
axe WCAG A/AA rules, search/pagination, correction, manual approval, pause/resume,
login/logout, and authenticated original-file retrieval. Screenshots are saved to
`receipt-ui-screenshots` under the OS temporary directory; set
`RECEIPT_UI_SCREENSHOTS` to override the directory. The browser-owned PDF viewer is
excluded from axe checks. Headless Chromium may not paint its native PDF viewer;
PDF retrieval is asserted separately. Passing automated checks is not a claim of
full accessibility conformance or a substitute for keyboard/screen-reader testing.

Go tests cover combined filters, page bounds, cross-page review navigation,
approval-and-next without skipping, final-item return, failed approvals, attention counts, escaping,
authentication, static-route allowlisting, and existing receipt workflow states.
No production database, real receipt, or external integration was changed.
