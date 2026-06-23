package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// CustomAssistantSpec is the editable shape of a custom assistant. ID empty
// means "create"; ID set means "update". JSON tags use snake_case so HTTP API
// responses are consumable by non-Go callers without case-conversion glue.
type CustomAssistantSpec struct {
	ID               string `json:"id,omitempty"` // empty → create
	Name             string `json:"name"`         // required
	BaseModel        string `json:"base_model"`   // model id, e.g. "claude-4-sonnet"
	Instructions     string `json:"instructions"` // system prompt
	BangTrigger      string `json:"bang_trigger"` // optional bang command
	InternetAccess   bool   `json:"internet_access"`
	Personalizations bool   `json:"personalizations"`
	LensID           string `json:"lens_id"` // "0" or "" = no lens
}

// assistantBody is the POST/PATCH /api/assistants payload.
type assistantBody struct {
	Name             string  `json:"name"`
	LLMID            string  `json:"llm_id"`
	Instructions     string  `json:"instructions"`
	BangTrigger      *string `json:"bang_trigger"`
	InternetAccess   bool    `json:"internet_access"`
	Personalizations bool    `json:"personalizations"`
	LensID           *string `json:"lens_id"`
}

func (spec CustomAssistantSpec) body() assistantBody {
	b := assistantBody{
		Name:             spec.Name,
		LLMID:            spec.BaseModel,
		Instructions:     spec.Instructions,
		InternetAccess:   spec.InternetAccess,
		Personalizations: spec.Personalizations,
	}
	if spec.BangTrigger != "" {
		bt := spec.BangTrigger
		b.BangTrigger = &bt
	}
	if spec.LensID != "" && spec.LensID != "0" {
		l := spec.LensID
		b.LensID = &l
	}
	return b
}

// SaveCustomAssistant creates (spec.ID empty) or updates (spec.ID set) a custom
// assistant and returns the resulting uuid.
func (c *Client) SaveCustomAssistant(ctx context.Context, spec CustomAssistantSpec) (string, error) {
	if spec.Name == "" {
		return "", errors.New("SaveCustomAssistant: name is required")
	}
	if spec.BaseModel == "" {
		return "", errors.New("SaveCustomAssistant: base_model is required")
	}

	if spec.ID != "" {
		if err := c.apiDo(ctx, http.MethodPatch, "/api/assistants/"+spec.ID, spec.body(), nil); err != nil {
			return "", err
		}
		return spec.ID, nil
	}

	var resp struct {
		UUID      string          `json:"uuid"`
		Assistant CustomAssistant `json:"assistant"`
	}
	if err := c.apiDo(ctx, http.MethodPost, "/api/assistants", spec.body(), &resp); err != nil {
		return "", err
	}
	if resp.UUID != "" {
		return resp.UUID, nil
	}
	if resp.Assistant.UUID != "" {
		return resp.Assistant.UUID, nil
	}
	// Fallback: resolve the new id by name (names are unique within an account).
	data, err := c.FetchInit(ctx)
	if err != nil {
		return "", fmt.Errorf("created but couldn't resolve id: %w", err)
	}
	for _, ca := range data.CustomAssistants {
		if ca.Name == spec.Name {
			return ca.UUID, nil
		}
	}
	return "", fmt.Errorf("created but no assistant with name %q found", spec.Name)
}

// DeleteCustomAssistant removes a custom assistant by uuid.
func (c *Client) DeleteCustomAssistant(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("DeleteCustomAssistant: id required")
	}
	return c.apiDo(ctx, http.MethodDelete, "/api/assistants/"+id, nil, nil)
}

// FetchCustomAssistant returns the current spec for an assistant by uuid.
func (c *Client) FetchCustomAssistant(ctx context.Context, id string) (CustomAssistantSpec, error) {
	if id == "" {
		return CustomAssistantSpec{}, errors.New("FetchCustomAssistant: id required")
	}
	ca, err := c.findAssistant(ctx, id)
	if err != nil {
		return CustomAssistantSpec{}, err
	}
	return CustomAssistantSpec{
		ID:               ca.UUID,
		Name:             ca.Name,
		BaseModel:        ca.LLMID,
		Instructions:     ca.Instructions,
		BangTrigger:      ca.BangTrigger,
		InternetAccess:   ca.InternetAccess,
		Personalizations: ca.Personalizations,
		LensID:           ca.LensID,
	}, nil
}
