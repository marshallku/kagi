package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/marshallku/kagi/client"
)

// GET /threads
//
//	?limit=N      (default 100)
//	?all=true     (page through every thread)
//	?cursor_id=…&cursor_ack=…&cursor_created=…  (resume mid-pagination)
//
// Returns: {"threads":[...], "next_cursor":{...}|null, "has_more":bool, "count":int}
// In all-mode, next_cursor is always null and has_more is always false.
func (s *Server) handleThreadsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	c := s.newClient()
	if q.Get("all") == "true" {
		threads, err := c.ListAllThreads(r.Context(), limit, 0)
		if err != nil {
			httpClientError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, client.ThreadListResult{
			Threads: threads, Count: len(threads),
		})
		return
	}
	var cursor *client.ThreadCursor
	if id := q.Get("cursor_id"); id != "" {
		cursor = &client.ThreadCursor{
			ID: id, Ack: q.Get("cursor_ack"), CreatedAt: q.Get("cursor_created"),
		}
	}
	res, err := c.ListThreads(r.Context(), cursor, limit)
	if err != nil {
		httpClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// GET /threads/{id}
func (s *Server) handleThreadShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, err := s.newClient().ShowThread(r.Context(), id)
	if err != nil {
		httpClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type threadModifyRequest struct {
	Title  *string   `json:"title,omitempty"`
	Saved  *bool     `json:"saved,omitempty"`
	Shared *bool     `json:"shared,omitempty"`
	TagIDs *[]string `json:"tag_ids,omitempty"`
}

// PATCH /threads/{id} — partial update. Server fetches current state, applies
// only the fields the caller passed, then re-submits the full envelope. This
// matches the CLI's `kagi threads rename/save/share` semantics.
func (s *Server) handleThreadModify(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req threadModifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c := s.newClient()
	d, err := c.ShowThread(r.Context(), id)
	if err != nil {
		httpClientError(w, err)
		return
	}
	mod := client.ThreadModification{
		ID: id, Title: d.Title, Saved: d.Saved, Shared: d.Shared, TagIDs: d.TagIDs,
	}
	if req.Title != nil {
		mod.Title = *req.Title
	}
	if req.Saved != nil {
		mod.Saved = *req.Saved
	}
	if req.Shared != nil {
		mod.Shared = *req.Shared
	}
	if req.TagIDs != nil {
		mod.TagIDs = *req.TagIDs
	}
	if mod.TagIDs == nil {
		mod.TagIDs = []string{}
	}
	if err := c.ModifyThreads(r.Context(), mod); err != nil {
		httpClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mod)
}

// DELETE /threads/{id}
func (s *Server) handleThreadDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.newClient().DeleteThreads(r.Context(), id); err != nil {
		httpClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /threads/delete  body: {"ids":[...]}
//
// Bulk variant. Using POST instead of DELETE-with-body because most HTTP
// clients (and intermediaries) don't handle DELETE bodies cleanly.
func (s *Server) handleThreadsBulkDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		http.Error(w, "ids: at least one required", http.StatusBadRequest)
		return
	}
	if err := s.newClient().DeleteThreads(r.Context(), req.IDs...); err != nil {
		httpClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": len(req.IDs)})
}

type searchRequest struct {
	Q      string `json:"q"`
	TagID  string `json:"tag_id,omitempty"`
	Saved  *bool  `json:"saved,omitempty"`
	Shared *bool  `json:"shared,omitempty"`
}

// POST /threads/search  body: {"q":"...", "tag_id":"...", "saved":bool, "shared":bool}
func (s *Server) handleThreadsSearch(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Q == "" {
		http.Error(w, "q: search query required", http.StatusBadRequest)
		return
	}
	hits, err := s.newClient().SearchThreads(r.Context(), req.Q, client.SearchOpts{
		TagID: req.TagID, Saved: req.Saved, Shared: req.Shared,
	})
	if err != nil {
		httpClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

// httpClientError maps a client error to an HTTP status. The order matters:
// not-found first (so a typo'd id doesn't get masked as auth failure on
// Kagi's 404-as-auth-fail quirk), then auth (401, callers can re-auth
// out-of-band), then upstream (502, we're proxying Kagi).
func httpClientError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if errors.Is(err, client.ErrNotFound) {
		http.Error(w, msg, http.StatusNotFound)
		return
	}
	if isAuthErr(err) {
		http.Error(w, msg, http.StatusUnauthorized)
		return
	}
	http.Error(w, msg, http.StatusBadGateway)
}

// isAuthErr recognises the auth-failure messages the client wraps with
// fmt.Errorf. Substring-check is the simplest path that doesn't require
// restructuring the client API around a typed error.
func isAuthErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, needle := range []string{"auth failed", "empty session", "auto-login", "relogin"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		fmt.Fprintln(w, err.Error())
	}
}
