package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ThreadCursor is retained for HTTP-API compatibility. The v2 list endpoint
// (/api/init) returns the full conversation list in one shot, so cursors are
// no longer used; the fields stay so existing callers compile unchanged.
type ThreadCursor struct {
	Ack       string `json:"ack"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

// ThreadSummary is one entry from the conversation list (the "threads"
// sidebar). Field names are preserved from v1 for output compatibility.
type ThreadSummary struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Snippet string   `json:"snippet,omitempty"`
	Saved   bool     `json:"saved"`
	Shared  bool     `json:"shared"`
	TagIDs  []string `json:"tag_ids"`
	Group   string   `json:"group,omitempty"`
}

type ThreadListResult struct {
	Threads    []ThreadSummary `json:"threads"`
	NextCursor *ThreadCursor   `json:"next_cursor,omitempty"`
	HasMore    bool            `json:"has_more"`
	Count      int             `json:"count"`
}

func conversationToSummary(c Conversation) ThreadSummary {
	return ThreadSummary{
		ID:     c.UUID,
		Title:  c.Title,
		Saved:  c.IsSaved,
		Shared: c.IsShared,
		TagIDs: []string{},
	}
}

// ListThreads returns the conversation list. v2 serves the whole list from
// /api/init, so the cursor argument is ignored and NextCursor is always nil;
// limit≤0 means no cap.
func (c *Client) ListThreads(ctx context.Context, cursor *ThreadCursor, limit int) (ThreadListResult, error) {
	_ = cursor
	data, err := c.FetchInit(ctx)
	if err != nil {
		return ThreadListResult{}, err
	}
	items := data.Conversations.Items
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	threads := make([]ThreadSummary, 0, len(items))
	for _, conv := range items {
		threads = append(threads, conversationToSummary(conv))
	}
	return ThreadListResult{Threads: threads, Count: len(threads)}, nil
}

// ListAllThreads returns every conversation. pageLimit is accepted for
// compatibility but unused (the list is not paginated in v2); maxThreads caps
// the total (≤0 = unlimited).
func (c *Client) ListAllThreads(ctx context.Context, pageLimit, maxThreads int) ([]ThreadSummary, error) {
	_ = pageLimit
	page, err := c.ListThreads(ctx, nil, 0)
	if err != nil {
		return nil, err
	}
	if maxThreads > 0 && len(page.Threads) > maxThreads {
		return page.Threads[:maxThreads], nil
	}
	return page.Threads, nil
}

// ThreadMessage is one turn (user prompt + assistant reply) of a conversation.
// v2 stores user and assistant messages as separate rows; we pair them here so
// the v1-shaped output (Prompt + Reply/Markdown per turn) is preserved.
type ThreadMessage struct {
	ID        string `json:"id"`
	ThreadID  string `json:"thread_id"`
	CreatedAt string `json:"created_at"`
	State     string `json:"state"`
	Prompt    string `json:"prompt"`
	Reply     string `json:"reply,omitempty"`
	Markdown  string `json:"md,omitempty"`
	Profile   struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Model             string `json:"model"`
		ModelName         string `json:"model_name"`
		ModelProvider     string `json:"model_provider"`
		ModelProviderName string `json:"model_provider_name"`
	} `json:"profile,omitempty"`
}

// ThreadDetail is the full conversation, mapped to the v1 shape.
type ThreadDetail struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	CreatedAt string          `json:"created_at,omitempty"`
	Saved     bool            `json:"saved"`
	Shared    bool            `json:"shared"`
	BranchID  string          `json:"branch_id,omitempty"`
	TagIDs    []string        `json:"tag_ids"`
	Messages  []ThreadMessage `json:"messages"`

	headMessageID string
}

// LastMessageID returns the conversation's current head message — the anchor a
// follow-up threads onto. v2 resolves the parent server-side, so this is used
// only as a "has any messages" signal and a follow-up hint.
func (d *ThreadDetail) LastMessageID() string {
	if d.headMessageID != "" {
		return d.headMessageID
	}
	if n := len(d.Messages); n > 0 {
		return d.Messages[n-1].ID
	}
	return ""
}

// conversationInit is GET /api/conversations/{uuid}/init.
type conversationInit struct {
	Conversation Conversation `json:"conversation"`
	Branches     []Branch     `json:"branches"`
	ActiveBranch Branch       `json:"active_branch"`
	Messages     struct {
		Items []Message `json:"items"`
	} `json:"messages"`
}

// ShowThread loads a full conversation and maps it to the v1 ThreadDetail.
func (c *Client) ShowThread(ctx context.Context, threadID string) (ThreadDetail, error) {
	if threadID == "" {
		return ThreadDetail{}, fmt.Errorf("ShowThread: thread id required")
	}
	var init conversationInit
	if err := c.apiDo(ctx, http.MethodGet, "/api/conversations/"+threadID+"/init", nil, &init); err != nil {
		return ThreadDetail{}, err
	}
	d := ThreadDetail{
		ID:            init.Conversation.UUID,
		Title:         init.Conversation.Title,
		CreatedAt:     init.Conversation.CreatedAt,
		Saved:         init.Conversation.IsSaved,
		Shared:        init.Conversation.IsShared,
		BranchID:      init.ActiveBranch.UUID,
		TagIDs:        []string{},
		headMessageID: init.ActiveBranch.HeadMessageUUID,
	}
	d.Messages = pairMessages(init.Messages.Items, init.Conversation.UUID)
	return d, nil
}

// pairMessages folds the flat user/assistant message list into per-turn
// records. A user message opens a turn; the next assistant message closes it.
func pairMessages(items []Message, convUUID string) []ThreadMessage {
	var out []ThreadMessage
	var cur *ThreadMessage
	for _, m := range items {
		switch m.Role {
		case "user":
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &ThreadMessage{
				ID:        m.UUID,
				ThreadID:  convUUID,
				CreatedAt: m.CreatedAt,
				State:     "done",
				Prompt:    m.Content,
			}
		case "assistant":
			if cur == nil {
				cur = &ThreadMessage{ID: m.UUID, ThreadID: convUUID, State: "done"}
			}
			cur.Reply = m.HTMLContent
			cur.Markdown = m.Content
			cur.Profile.Name = m.ProfileName
			cur.Profile.ID = m.ProfileUUID
			cur.Profile.Model = m.ModelName
			cur.Profile.ModelName = m.ModelDisplayName
			cur.Profile.ModelProvider = m.ModelProvider
			out = append(out, *cur)
			cur = nil
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// ThreadModification carries a desired conversation state. v2 applies a partial
// update, so only Title/Saved/Shared are sent (tags are not part of the v2
// conversation model).
type ThreadModification struct {
	ID     string   `json:"id"`
	Title  string   `json:"title,omitempty"`
	Saved  bool     `json:"saved"`
	Shared bool     `json:"shared"`
	TagIDs []string `json:"tag_ids"`
}

// ModifyThreads renames / saves / shares one or more conversations. Tags are
// not part of the v2 conversation model, so a non-empty TagIDs is rejected
// rather than silently dropped.
func (c *Client) ModifyThreads(ctx context.Context, mods ...ThreadModification) error {
	if len(mods) == 0 {
		return fmt.Errorf("ModifyThreads: at least one modification required")
	}
	for _, m := range mods {
		if len(m.TagIDs) > 0 {
			return fmt.Errorf("modify %s: tag editing is not supported by the Kagi v2 API", m.ID)
		}
	}
	for _, m := range mods {
		body := map[string]any{
			"is_saved":  m.Saved,
			"is_shared": m.Shared,
		}
		if m.Title != "" {
			body["title"] = m.Title
		}
		if err := c.apiDo(ctx, http.MethodPatch, "/api/conversations/"+m.ID, body, nil); err != nil {
			return fmt.Errorf("modify %s: %w", m.ID, err)
		}
	}
	return nil
}

// DeleteThreads deletes conversations by uuid. v2 accepts a direct DELETE — no
// pre-fetch of current state required.
func (c *Client) DeleteThreads(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return fmt.Errorf("DeleteThreads: at least one id required")
	}
	for _, id := range ids {
		if err := c.apiDo(ctx, http.MethodDelete, "/api/conversations/"+id, nil, nil); err != nil {
			return fmt.Errorf("delete %s: %w", id, err)
		}
	}
	return nil
}

// SearchOpts narrows a search. The v2 /api/search endpoint matches on the
// query only; Saved/Shared are applied client-side from each hit's
// conversation. Tags don't exist in v2, so a non-empty TagID is rejected.
type SearchOpts struct {
	TagID  string
	Saved  *bool
	Shared *bool
}

// SearchHit is one search result. v2 returns a hit per matching conversation
// (not per message), so MessageID/BranchID are left empty.
type SearchHit struct {
	Rank      float64 `json:"rank"`
	Snippet   string  `json:"snippet"`
	MessageID string  `json:"message_id,omitempty"`
	BranchID  string  `json:"branch_id,omitempty"`
	ThreadID  string  `json:"thread_id"`
}

// SearchThreads runs a full-text search across the user's conversations.
func (c *Client) SearchThreads(ctx context.Context, query string, opts SearchOpts) ([]SearchHit, error) {
	if opts.TagID != "" {
		return nil, fmt.Errorf("tag filtering is not supported by the Kagi v2 API")
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", strconv.Itoa(100))
	var resp struct {
		Items []struct {
			Conversation Conversation `json:"conversation"`
			Snippet      string       `json:"snippet"`
			Rank         float64      `json:"rank"`
		} `json:"items"`
	}
	if err := c.apiDo(ctx, http.MethodGet, "/api/search?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(resp.Items))
	for _, it := range resp.Items {
		// v2 search has no server-side saved/shared filter, so apply it here.
		if opts.Saved != nil && it.Conversation.IsSaved != *opts.Saved {
			continue
		}
		if opts.Shared != nil && it.Conversation.IsShared != *opts.Shared {
			continue
		}
		hits = append(hits, SearchHit{
			Rank:     it.Rank,
			Snippet:  it.Snippet,
			ThreadID: it.Conversation.UUID,
		})
	}
	return hits, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
