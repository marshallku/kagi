package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const (
	ThreadListPath   = "/assistant/thread_list"
	ThreadModifyPath = "/assistant/thread_modify"
	ThreadDeletePath = "/assistant/thread_delete"
	ThreadSearchPath = "/assistant/search"
)

// ThreadCursor is the pagination handle returned by Kagi between thread_list
// pages. Pass the previous response's NextCursor verbatim to fetch the next
// page; pass nil to start from the most recent thread.
type ThreadCursor struct {
	Ack       string `json:"ack"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

// ThreadSummary is one entry from the sidebar list. Kagi returns this as
// HTML with attributes baked in; we parse it back into a struct.
type ThreadSummary struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Snippet string   `json:"snippet,omitempty"`
	Saved   bool     `json:"saved"`
	Shared  bool     `json:"shared"`
	TagIDs  []string `json:"tag_ids"`
	Group   string   `json:"group,omitempty"` // "Today", "Previous 7 days", "All time"
}

type ThreadListResult struct {
	Threads    []ThreadSummary `json:"threads"`
	NextCursor *ThreadCursor   `json:"next_cursor,omitempty"`
	HasMore    bool            `json:"has_more"`
	Count      int             `json:"count"`
}

// listThreadsRequest mirrors the JSON the browser sends to /assistant/thread_list.
type listThreadsRequest struct {
	Cursor *ThreadCursor `json:"cursor"`
	Limit  int           `json:"limit"`
}

// thread_list streams back this JSON event after `hi` and `tags.json`.
type threadListPayload struct {
	HTML        string        `json:"html"`
	NextCursor  *ThreadCursor `json:"next_cursor"`
	HasMore     bool          `json:"has_more"`
	Count       int           `json:"count"`
	TotalCounts any           `json:"total_counts"`
}

// ListThreads fetches one page of the thread sidebar. Pass cursor=nil for the
// first page; for subsequent pages pass the previous result's NextCursor.
// limit≤0 defaults to 100.
func (c *Client) ListThreads(ctx context.Context, cursor *ThreadCursor, limit int) (ThreadListResult, error) {
	if limit <= 0 {
		limit = 100
	}
	body, _ := json.Marshal(listThreadsRequest{Cursor: cursor, Limit: limit})
	rawEvents, err := c.streamJSON(ctx, http.MethodPost, ThreadListPath, body)
	if err != nil {
		return ThreadListResult{}, err
	}
	var payload *threadListPayload
	for _, ev := range rawEvents {
		if ev.Type == "thread_list.html" {
			var p threadListPayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return ThreadListResult{}, fmt.Errorf("decode thread_list.html: %w", err)
			}
			payload = &p
			break
		}
	}
	if payload == nil {
		return ThreadListResult{}, errors.New("thread_list: no thread_list.html event in stream")
	}
	threads := parseThreadHTML(payload.HTML)
	return ThreadListResult{
		Threads:    threads,
		NextCursor: payload.NextCursor,
		HasMore:    payload.HasMore,
		Count:      payload.Count,
	}, nil
}

// ListAllThreads pages through every saved thread until has_more=false.
// pageLimit caps each underlying request (default 100); maxThreads caps the
// total (≤0 = unlimited). Returns threads in the order Kagi served them
// (newest first, grouped by date label).
func (c *Client) ListAllThreads(ctx context.Context, pageLimit, maxThreads int) ([]ThreadSummary, error) {
	if pageLimit <= 0 {
		pageLimit = 100
	}
	var (
		out    []ThreadSummary
		cursor *ThreadCursor
	)
	for {
		page, err := c.ListThreads(ctx, cursor, pageLimit)
		if err != nil {
			return out, err
		}
		out = append(out, page.Threads...)
		if maxThreads > 0 && len(out) >= maxThreads {
			return out[:maxThreads], nil
		}
		if !page.HasMore || page.NextCursor == nil {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// streamJSON is the shared helper for endpoints that take a JSON body and
// stream back the same NUL-delimited Kagi protocol used by /assistant/prompt.
// We collect every event up front because the payloads are bounded (a list of
// threads, a search result page) — unlike chat streams which can be long.
func (c *Client) streamJSON(ctx context.Context, method, path string, body []byte) ([]Event, error) {
	return c.streamJSONRetry(ctx, method, path, body, false)
}

func (c *Client) streamJSONRetry(ctx context.Context, method, path string, body []byte, retried bool) ([]Event, error) {
	if c.Session == "" {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return nil, fmt.Errorf("auto-login: %w", err)
			}
		} else {
			return nil, errors.New("client: empty session")
		}
	}
	req, err := c.newRequest(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", StreamAccept)
	req.Header.Set("Cookie", "kagi_session="+c.Session)
	req.Header.Set("Referer", BaseURL+"/assistant")
	req.Header.Set("Origin", BaseURL)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	if isAuthFail(resp) {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return nil, fmt.Errorf("relogin after auth fail: %w", err)
			}
			return c.streamJSONRetry(ctx, method, path, body, true)
		}
		return nil, fmt.Errorf("auth failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	events := make(chan Event, 32)
	collected := make([]Event, 0, 8)
	done := make(chan error, 1)
	go func() {
		err := parseStream(resp.Body, events)
		close(events)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		done <- err
	}()
	for ev := range events {
		collected = append(collected, ev)
	}
	if err := <-done; err != nil {
		return collected, err
	}
	return collected, nil
}

// parseThreadHTML pulls thread metadata out of the server-rendered sidebar
// HTML. Each <li class="thread"> carries everything we need as data-* attrs.
// Group headers (<div class="thread-list-header">Today</div>) tag the
// following items with their bucket label.
var (
	threadGroupRE = regexp.MustCompile(`(?s)<div class="hide-if-no-threads"\s+data-group-name="([^"]*)">(.*?)</ul>\s*</div>`)
	threadItemRE  = regexp.MustCompile(`(?s)<li class="thread"\s+data-code="([^"]+)"\s+data-saved="([^"]*)"\s+data-public="([^"]*)"\s+data-tags="([^"]*)"\s+data-snippet="([^"]*)"\s*>(.*?)</li>`)
	threadTitleRE = regexp.MustCompile(`(?s)<div class="title">(.*?)</div>`)
)

func parseThreadHTML(s string) []ThreadSummary {
	var out []ThreadSummary
	for _, gm := range threadGroupRE.FindAllStringSubmatch(s, -1) {
		group := html.UnescapeString(gm[1])
		for _, im := range threadItemRE.FindAllStringSubmatch(gm[2], -1) {
			id := im[1]
			saved := im[2] == "true"
			shared := im[3] == "true"
			var tags []string
			_ = json.Unmarshal([]byte(html.UnescapeString(im[4])), &tags)
			snippet := html.UnescapeString(im[5])
			title := snippet
			if t := threadTitleRE.FindStringSubmatch(im[6]); t != nil {
				title = strings.TrimSpace(html.UnescapeString(t[1]))
			}
			out = append(out, ThreadSummary{
				ID: id, Title: title, Snippet: snippet,
				Saved: saved, Shared: shared, TagIDs: tags, Group: group,
			})
		}
	}
	return out
}

// ThreadModification is the JSON shape /assistant/thread_modify expects per
// item in its `threads` array. Only fields present here are sent.
type ThreadModification struct {
	ID     string   `json:"id"`
	Title  string   `json:"title,omitempty"`
	Saved  bool     `json:"saved"`
	Shared bool     `json:"shared"`
	TagIDs []string `json:"tag_ids"`
}

// ModifyThreads renames / saves / shares / re-tags one or more threads in a
// single request. Title left empty leaves the existing title alone.
func (c *Client) ModifyThreads(ctx context.Context, mods ...ThreadModification) error {
	if len(mods) == 0 {
		return errors.New("ModifyThreads: at least one modification required")
	}
	body, _ := json.Marshal(struct {
		Threads []ThreadModification `json:"threads"`
	}{Threads: mods})
	_, err := c.streamJSON(ctx, http.MethodPost, ThreadModifyPath, body)
	return err
}

// DeleteThreads bulk-deletes by UUID. The endpoint takes the same shape as
// thread_modify ({threads:[{id,title,saved,shared,tag_ids}]}), so we fetch
// each thread first to build a valid envelope (the server returns 500 for a
// bare {id}). Any lookup failure is propagated — including ErrNotFound for
// bogus ids — so callers can distinguish "doesn't exist" from "delete
// failed". For bulk requests this means fail-fast: one bad id rejects the
// whole batch. Pre-check ids if you want partial success.
func (c *Client) DeleteThreads(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return errors.New("DeleteThreads: at least one id required")
	}
	envelopes := make([]ThreadModification, 0, len(ids))
	for _, id := range ids {
		d, err := c.ShowThread(ctx, id)
		if err != nil {
			return fmt.Errorf("delete %s: %w", id, err)
		}
		tags := d.TagIDs
		if tags == nil {
			tags = []string{}
		}
		envelopes = append(envelopes, ThreadModification{
			ID: id, Title: d.Title, Saved: d.Saved, Shared: d.Shared, TagIDs: tags,
		})
	}
	body, _ := json.Marshal(struct {
		Threads []ThreadModification `json:"threads"`
	}{Threads: envelopes})
	_, err := c.streamJSON(ctx, http.MethodPost, ThreadDeletePath, body)
	return err
}

// SearchOpts narrows /assistant/search. Zero values mean "no filter".
type SearchOpts struct {
	TagID  string
	Saved  *bool
	Shared *bool
}

// SearchHit is one match returned by /assistant/search. The endpoint returns
// a flat JSON array (not the streamed thread_list HTML), with each hit
// pointing at the specific message inside a thread that matched the query.
type SearchHit struct {
	Rank      float64 `json:"rank"`
	Snippet   string  `json:"snippet"` // HTML — search terms wrapped in <b>
	MessageID string  `json:"message_id"`
	BranchID  string  `json:"branch_id"`
	ThreadID  string  `json:"thread_id"`
}

// SearchThreads runs a full-text search across the user's threads.
func (c *Client) SearchThreads(ctx context.Context, query string, opts SearchOpts) ([]SearchHit, error) {
	req := map[string]any{"q": query}
	if opts.TagID != "" {
		req["tag_id"] = opts.TagID
	}
	if opts.Saved != nil {
		req["saved"] = *opts.Saved
	}
	if opts.Shared != nil {
		req["shared"] = *opts.Shared
	}
	body, _ := json.Marshal(req)
	resp, err := c.doJSON(ctx, http.MethodPost, ThreadSearchPath, body)
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	if err := json.Unmarshal(resp, &hits); err != nil {
		return nil, fmt.Errorf("decode search result: %w (got: %s)", err, truncate(resp, 200))
	}
	return hits, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// doJSON is a one-shot JSON request helper for endpoints that return a plain
// (non-streamed) response. Auth-failure handling mirrors streamJSON.
func (c *Client) doJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return c.doJSONRetry(ctx, method, path, body, false)
}

func (c *Client) doJSONRetry(ctx context.Context, method, path string, body []byte, retried bool) ([]byte, error) {
	if c.Session == "" {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return nil, fmt.Errorf("auto-login: %w", err)
			}
		} else {
			return nil, errors.New("client: empty session")
		}
	}
	req, err := c.newRequest(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", "kagi_session="+c.Session)
	req.Header.Set("Referer", BaseURL+"/assistant")
	req.Header.Set("Origin", BaseURL)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	if isAuthFail(resp) {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return nil, fmt.Errorf("relogin after auth fail: %w", err)
			}
			return c.doJSONRetry(ctx, method, path, body, true)
		}
		return nil, fmt.Errorf("auth failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// ThreadMessage is one turn (user prompt + assistant reply) of a conversation.
// In Kagi's data model these are paired into a single record — the `id` is
// what you pass as `focus.message_id` to continue the thread.
type ThreadMessage struct {
	ID         string   `json:"id"`
	ThreadID   string   `json:"thread_id"`
	CreatedAt  string   `json:"created_at"`
	State      string   `json:"state"`           // "done", "waiting"
	Prompt     string   `json:"prompt"`          // user input (markdown)
	Reply      string   `json:"reply,omitempty"` // assistant output (HTML)
	Markdown   string   `json:"md,omitempty"`    // assistant output (markdown)
	Documents  []any    `json:"documents,omitempty"`
	BranchList []string `json:"branch_list,omitempty"`
	// Profile snapshot used for this turn (model, name, etc.). We don't
	// expose the full Kagi schema — only the fields callers usually care about.
	Profile struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Model             string `json:"model"`
		ModelName         string `json:"model_name"`
		ModelProvider     string `json:"model_provider"`
		ModelProviderName string `json:"model_provider_name"`
	} `json:"profile,omitempty"`
}

// ThreadDetail is what the embedded `json-thread` + `json-message-list`
// islands deserialise into — full structured fidelity, no HTML scraping.
type ThreadDetail struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	Ack       string           `json:"ack,omitempty"`
	CreatedAt string           `json:"created_at,omitempty"`
	Saved     bool             `json:"saved"`
	Shared    bool             `json:"shared"`
	BranchID  string           `json:"branch_id,omitempty"`
	TagIDs    []string         `json:"tag_ids"`
	Profile   AssistantProfile `json:"profile,omitempty"`
	Messages  []ThreadMessage  `json:"messages"`
}

// LastMessageID returns the id of the most recent turn — the value to pass
// as `focus.message_id` to continue the conversation. Returns "" if the
// thread has no messages yet.
func (d *ThreadDetail) LastMessageID() string {
	if n := len(d.Messages); n > 0 {
		return d.Messages[n-1].ID
	}
	return ""
}

// ShowThread loads /assistant/<id> and decodes the embedded `json-thread`
// and `json-message-list` islands. We use these instead of scraping the chat
// bubbles because the bubbles are populated client-side from the same JSON
// — the islands are the source of truth.
func (c *Client) ShowThread(ctx context.Context, threadID string) (ThreadDetail, error) {
	return c.showThread(ctx, threadID, false)
}

func (c *Client) showThread(ctx context.Context, threadID string, retried bool) (ThreadDetail, error) {
	if threadID == "" {
		return ThreadDetail{}, errors.New("ShowThread: thread id required")
	}
	if c.Session == "" {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return ThreadDetail{}, fmt.Errorf("auto-login: %w", err)
			}
		} else {
			return ThreadDetail{}, errors.New("client: empty session")
		}
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/assistant/"+threadID, nil)
	if err != nil {
		return ThreadDetail{}, err
	}
	req.Header.Set("Cookie", "kagi_session="+c.Session)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ThreadDetail{}, fmt.Errorf("fetch thread: %w", err)
	}
	defer resp.Body.Close()
	if isAuthFail(resp) {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return ThreadDetail{}, fmt.Errorf("relogin after auth fail: %w", err)
			}
			return c.showThread(ctx, threadID, true)
		}
		// Kagi serves 404 both for "no such thread" and "unauthenticated".
		// We only retry once; a second 404 after a fresh login is almost
		// certainly a real not-found, so map it to ErrNotFound rather than
		// reporting an auth failure.
		if retried && resp.StatusCode == http.StatusNotFound {
			return ThreadDetail{}, fmt.Errorf("thread %s: %w", threadID, ErrNotFound)
		}
		return ThreadDetail{}, fmt.Errorf("auth failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return ThreadDetail{}, fmt.Errorf("/assistant/%s: status %d", threadID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return ThreadDetail{}, err
	}
	return parseThreadDetail(body)
}

var (
	jsonThreadRE      = regexp.MustCompile(`(?s)<div id="json-thread"[^>]*>(.+?)</div>`)
	jsonMessageListRE = regexp.MustCompile(`(?s)<div id="json-message-list"[^>]*>(.+?)</div>`)
)

func parseThreadDetail(body []byte) (ThreadDetail, error) {
	d := ThreadDetail{}
	tm := jsonThreadRE.FindSubmatch(body)
	if tm == nil {
		// Kagi serves a 200 page with no json-thread island for unknown
		// thread ids (the layout still has <div id="chat_box"></div> but no
		// island). Treat that as not-found so the HTTP API can return 404.
		return d, fmt.Errorf("no json-thread island in /assistant/<id>: %w", ErrNotFound)
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(string(tm[1]))), &d); err != nil {
		return d, fmt.Errorf("decode json-thread: %w", err)
	}
	mm := jsonMessageListRE.FindSubmatch(body)
	if mm == nil {
		return d, errors.New("json-message-list island not found")
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(string(mm[1]))), &d.Messages); err != nil {
		return d, fmt.Errorf("decode json-message-list: %w", err)
	}
	return d, nil
}
