package server

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/burmudar/go-receipt/receipt"
)

const receiptPageSize = 25

type receiptListPage struct {
	Items                                             []receiptItem
	filtered                                          []receiptItem
	Queue                                             string
	Query, StateFilter, ArchiveFilter                 string
	Attention                                         bool
	Total, Matching, NeedsReview, Completed, Failures int
	Page, Pages                                       int
	Previous, Next                                    string
}

func (item receiptItem) NeedsAttention() bool {
	return item.HasFailure() || (item.Status == receipt.Extracted && item.State != "approved" && item.State != "ignored" && item.State != "completed")
}
func (item receiptItem) HasFailure() bool {
	intervention := func(state string) bool { return state == "failed" || state == "uncertain" }
	return item.Status == receipt.Failed || item.LastError != "" || item.Attachment.Error != "" || item.Archive.Error != "" || item.Cleanup.Error != "" || intervention(item.Attachment.State) || intervention(item.Archive.State) || intervention(item.Cleanup.State)
}

// Filtering is server-side and shareable: it works without JavaScript, preserves
// combined filters across pages, and never changes receipt decisions.
func receiptList(items []receiptItem, query url.Values) receiptListPage {
	page := receiptListPage{Query: strings.TrimSpace(query.Get("q")), StateFilter: query.Get("state"), ArchiveFilter: query.Get("archive"), Attention: query.Get("attention") == "1", Total: len(items), Page: 1}
	var filtered []receiptItem
	for _, item := range items {
		if item.Complete() {
			page.Completed++
		}
		if item.HasFailure() {
			page.Failures++
		}
		if item.NeedsAttention() {
			page.NeedsReview++
		}
		if page.StateFilter != "" && item.State != page.StateFilter {
			continue
		}
		if page.Attention && !item.NeedsAttention() {
			continue
		}
		if page.ArchiveFilter != "" {
			state := item.Archive.State
			if state == "" || state == "uploading" || state == "consuming" {
				state = "pending"
			}
			if state != page.ArchiveFilter {
				continue
			}
		}
		text := item.Name + " " + item.SHA256 + " " + item.Review.TransactionID
		if a := item.Effective; a != nil {
			text += " " + a.Merchant + " " + a.Total + " " + a.Currency + " " + a.Date.Format("2006-01-02")
		}
		if !strings.Contains(strings.ToLower(text), strings.ToLower(page.Query)) {
			continue
		}
		filtered = append(filtered, item)
	}
	page.filtered = filtered
	page.Matching = len(filtered)
	page.Pages = max(1, (page.Matching+receiptPageSize-1)/receiptPageSize)
	if n, err := strconv.Atoi(query.Get("page")); err == nil {
		page.Page = max(1, min(n, page.Pages))
	}
	start := (page.Page - 1) * receiptPageSize
	page.Items = filtered[start:min(start+receiptPageSize, len(filtered))]
	link := func(n int) string {
		q := url.Values{}
		q.Set("page", strconv.Itoa(n))
		if page.Query != "" {
			q.Set("q", page.Query)
		}
		if page.StateFilter != "" {
			q.Set("state", page.StateFilter)
		}
		if page.ArchiveFilter != "" {
			q.Set("archive", page.ArchiveFilter)
		}
		if page.Attention {
			q.Set("attention", "1")
		}
		return "/receipts?" + q.Encode()
	}
	page.Queue = strings.TrimPrefix(link(page.Page), "/receipts?")
	if page.Page > 1 {
		page.Previous = link(page.Page - 1)
	}
	if page.Page < page.Pages {
		page.Next = link(page.Page + 1)
	}
	return page
}

func receiptURL(hash, queue string) string {
	path := "/receipts/" + url.PathEscape(hash)
	if queue != "" {
		path += "?" + url.Values{"queue": {queue}}.Encode()
	}
	return path
}

func (page receiptListPage) DetailURL(hash string) string { return receiptURL(hash, page.Queue) }

type receiptNavigation struct {
	Context, QueueURL, Previous, Next string
	Position, Total                   int
}

// Queue context contains filters, never a redirect URL or a client-supplied target.
// Neighbours are resolved from current server-side state before approving, so
// removing a receipt from a filtered queue cannot cause the next one to be skipped.
func queueNavigation(items []receiptItem, hash, raw string) receiptNavigation {
	query, err := url.ParseQuery(raw)
	if err != nil {
		query = url.Values{}
	}
	page := receiptList(items, query)
	nav := receiptNavigation{Context: page.Queue, Total: page.Matching}
	indices := map[string]int{}
	for i, item := range page.filtered {
		indices[item.SHA256] = i
	}
	if i, ok := indices[hash]; ok {
		nav.Position = i + 1
		q, _ := url.ParseQuery(nav.Context)
		q.Set("page", strconv.Itoa(i/receiptPageSize+1))
		nav.Context = q.Encode()
	}
	nav.QueueURL = "/receipts?" + nav.Context
	link := func(item receiptItem) string {
		q, _ := url.ParseQuery(nav.Context)
		q.Set("page", strconv.Itoa(indices[item.SHA256]/receiptPageSize+1))
		return receiptURL(item.SHA256, q.Encode())
	}
	found := false
	for _, item := range items {
		if item.SHA256 == hash {
			found = true
			continue
		}
		if _, ok := indices[item.SHA256]; !ok {
			continue
		}
		if !found {
			nav.Previous = link(item)
		} else {
			nav.Next = link(item)
			break
		}
	}
	if !found {
		nav.Previous = ""
	}
	return nav
}
