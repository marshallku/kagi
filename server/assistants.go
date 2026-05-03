package server

import (
	"encoding/json"
	"net/http"

	"github.com/marshallku/kagi/client"
)

// assistantSpecRequest is the body shape for POST/PATCH /assistants. Pointer
// fields distinguish "not set" from "set to zero" so PATCH can preserve
// untouched fields.
type assistantSpecRequest struct {
	Name             *string `json:"name,omitempty"`
	BaseModel        *string `json:"base_model,omitempty"`
	Instructions     *string `json:"instructions,omitempty"`
	BangTrigger      *string `json:"bang_trigger,omitempty"`
	InternetAccess   *bool   `json:"internet_access,omitempty"`
	Personalizations *bool   `json:"personalizations,omitempty"`
	LensID           *string `json:"lens_id,omitempty"`
}

// GET /assistants/{id}  — current spec (uses FetchCustomAssistant under the
// hood, scrapes /settings/custom_assistant?id=…).
func (s *Server) handleAssistantShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spec, err := s.newClient().FetchCustomAssistant(r.Context(), id)
	if err != nil {
		httpClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

// POST /assistants  body: full spec; name and base_model are required.
// Returns 201 with the resulting spec (id populated).
func (s *Server) handleAssistantCreate(w http.ResponseWriter, r *http.Request) {
	var req assistantSpecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == nil || *req.Name == "" {
		http.Error(w, "name: required", http.StatusBadRequest)
		return
	}
	if req.BaseModel == nil || *req.BaseModel == "" {
		http.Error(w, "base_model: required", http.StatusBadRequest)
		return
	}
	spec := client.CustomAssistantSpec{
		Name: *req.Name, BaseModel: *req.BaseModel,
		// All booleans default to true on create — matches the UI's defaults
		// and what most callers expect ("create a useful assistant").
		InternetAccess: true, Personalizations: true,
	}
	if req.Instructions != nil {
		spec.Instructions = *req.Instructions
	}
	if req.BangTrigger != nil {
		spec.BangTrigger = *req.BangTrigger
	}
	if req.InternetAccess != nil {
		spec.InternetAccess = *req.InternetAccess
	}
	if req.Personalizations != nil {
		spec.Personalizations = *req.Personalizations
	}
	if req.LensID != nil {
		spec.LensID = *req.LensID
	}
	c := s.newClient()
	id, err := c.SaveCustomAssistant(r.Context(), spec)
	if err != nil {
		httpClientError(w, err)
		return
	}
	spec.ID = id
	writeJSON(w, http.StatusCreated, spec)
}

// PATCH /assistants/{id} — partial update; we fetch the live spec first so
// fields the caller didn't pass stay intact. This is the same fetch+merge
// dance `kagi assistants update` does on the CLI side.
func (s *Server) handleAssistantUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req assistantSpecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c := s.newClient()
	spec, err := c.FetchCustomAssistant(r.Context(), id)
	if err != nil {
		httpClientError(w, err)
		return
	}
	if req.Name != nil {
		spec.Name = *req.Name
	}
	if req.BaseModel != nil {
		spec.BaseModel = *req.BaseModel
	}
	if req.Instructions != nil {
		spec.Instructions = *req.Instructions
	}
	if req.BangTrigger != nil {
		spec.BangTrigger = *req.BangTrigger
	}
	if req.InternetAccess != nil {
		spec.InternetAccess = *req.InternetAccess
	}
	if req.Personalizations != nil {
		spec.Personalizations = *req.Personalizations
	}
	if req.LensID != nil {
		spec.LensID = *req.LensID
	}
	// Validate at the HTTP boundary: SaveCustomAssistant would error on
	// these too, but as a generic upstream error → 502. They're really
	// client-input errors, so 400 is the right status.
	if spec.Name == "" {
		http.Error(w, "name: must not be empty", http.StatusBadRequest)
		return
	}
	if spec.BaseModel == "" {
		http.Error(w, "base_model: must not be empty", http.StatusBadRequest)
		return
	}
	if _, err := c.SaveCustomAssistant(r.Context(), spec); err != nil {
		httpClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

// DELETE /assistants/{id}
func (s *Server) handleAssistantDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.newClient().DeleteCustomAssistant(r.Context(), id); err != nil {
		httpClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
