package server

import (
	"net/http"
	"sort"
)

// GET /models  → deduplicated model catalog (mirrors `kagi models`).
func (s *Server) handleModelsList(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.newClient().FetchProfiles(r.Context())
	if err != nil {
		httpClientError(w, err)
		return
	}
	type modelEntry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Provider    string `json:"provider"`
		Recommended bool   `json:"recommended"`
	}
	seen := map[string]modelEntry{}
	for _, p := range profiles {
		if !p.Accessible || p.Deprecate || p.Retired || p.Model == "" {
			continue
		}
		if _, ok := seen[p.Model]; ok {
			continue
		}
		seen[p.Model] = modelEntry{
			ID: p.Model, Name: p.ModelName, Provider: p.ModelProvider, Recommended: p.Recommended,
		}
	}
	out := make([]modelEntry, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	writeJSON(w, http.StatusOK, out)
}

// GET /profiles  → user-selectable profiles only (mirrors `kagi profiles`).
// Empty-id base profiles, retired models, and inaccessible entries are
// already filtered; no query params are honoured today.
func (s *Server) handleProfilesList(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.newClient().FetchProfiles(r.Context())
	if err != nil {
		httpClientError(w, err)
		return
	}
	out := profiles[:0]
	for _, p := range profiles {
		if p.ID == "" || !p.Accessible || p.Deprecate || p.Retired {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// GET /assistants  is an alias for the profile list. Convention: the CLI uses
// "profiles" and "assistants" interchangeably for the same data; the HTTP API
// exposes both spellings so callers don't have to guess.
