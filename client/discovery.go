package client

import (
	"context"
	"net/http"
)

// InitData is the assistant app bootstrap blob (GET /api/init). It carries the
// entire client state; we decode only the parts the CLI/HTTP surface needs.
type InitData struct {
	Conversations struct {
		Items []Conversation `json:"items"`
	} `json:"conversations"`
	CustomAssistants []CustomAssistant `json:"custom_assistants"`
	Models           ModelsData        `json:"models"`
}

// ModelsData is the `models` block of /api/init.
type ModelsData struct {
	Models  []Model `json:"models"`
	Default string  `json:"default"`
}

// Model is one entry from the model catalogue. The v2 schema is far richer
// than v1's flat list; we keep the fields the CLI surfaces plus a couple used
// for filtering.
type Model struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ProviderLabel string `json:"provider_label"`
	DisplayName   string `json:"display_name"`
	Recommended   bool   `json:"recommended"`
	Supported     bool   `json:"supported"`
	Deprecated    bool   `json:"deprecated"`
	Retired       bool   `json:"retired"`
}

// CustomAssistant is a user-defined assistant (the v2 replacement for a
// "profile"): a system prompt, default model, and toggles.
type CustomAssistant struct {
	UUID             string `json:"uuid"`
	Name             string `json:"name"`
	LLMID            string `json:"llm_id"`
	Instructions     string `json:"instructions"`
	BangTrigger      string `json:"bang_trigger"`
	InternetAccess   bool   `json:"internet_access"`
	Personalizations bool   `json:"personalizations"`
	LensID           string `json:"lens_id"`
	Deprecated       bool   `json:"deprecated"`
	Retired          bool   `json:"retired"`
}

// FetchInit loads the assistant app bootstrap state.
func (c *Client) FetchInit(ctx context.Context) (InitData, error) {
	var data InitData
	if err := c.apiDo(ctx, http.MethodGet, "/api/init", nil, &data); err != nil {
		return InitData{}, err
	}
	return data, nil
}

// AssistantProfile is the compatibility view the CLI and HTTP discovery
// handlers consume. v1 returned a single list mixing base profiles (one per
// model, empty id) and custom assistants; we reconstruct the same shape from
// the v2 `models` + `custom_assistants` blocks so those call sites stay
// unchanged. Entries with ID=="" are base models (used by `kagi models`);
// entries with a UUID are custom assistants (used by `kagi profiles`).
type AssistantProfile struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Accessible        bool   `json:"accessible"`
	Model             string `json:"model"`
	Deprecate         bool   `json:"deprecate"`
	Retired           bool   `json:"retired"`
	ModelName         string `json:"model_name"`
	ModelProvider     string `json:"model_provider"`
	ModelProviderName string `json:"model_provider_name"`
	Recommended       bool   `json:"recommended"`
	IsDefaultProfile  bool   `json:"is_default_profile"`
}

// FetchProfiles returns the combined base-model + custom-assistant catalogue
// in the v1 AssistantProfile shape. See AssistantProfile for why.
func (c *Client) FetchProfiles(ctx context.Context) ([]AssistantProfile, error) {
	data, err := c.FetchInit(ctx)
	if err != nil {
		return nil, err
	}

	// Index models by id for assistant model-name lookups.
	modelByID := make(map[string]Model, len(data.Models.Models))
	out := make([]AssistantProfile, 0, len(data.Models.Models)+len(data.CustomAssistants))

	for _, m := range data.Models.Models {
		modelByID[m.ID] = m
		out = append(out, AssistantProfile{
			ID:               "", // base model — not user-selectable as a profile
			Name:             m.DisplayName,
			Accessible:       m.Supported,
			Model:            m.ID,
			Deprecate:        m.Deprecated,
			Retired:          m.Retired,
			ModelName:        m.DisplayName,
			ModelProvider:    m.Provider,
			Recommended:      m.Recommended,
			IsDefaultProfile: m.ID == data.Models.Default,
		})
	}

	for _, ca := range data.CustomAssistants {
		modelName := ca.LLMID
		provider := ""
		if m, ok := modelByID[ca.LLMID]; ok {
			modelName = m.DisplayName
			provider = m.Provider
		}
		out = append(out, AssistantProfile{
			ID:            ca.UUID,
			Name:          ca.Name,
			Accessible:    !ca.Retired,
			Model:         ca.LLMID,
			Deprecate:     ca.Deprecated,
			Retired:       ca.Retired,
			ModelName:     modelName,
			ModelProvider: provider,
		})
	}

	return out, nil
}

// findAssistant resolves a custom assistant by uuid from /api/init. Returns
// ErrNotFound if no assistant with that uuid exists.
func (c *Client) findAssistant(ctx context.Context, uuid string) (CustomAssistant, error) {
	data, err := c.FetchInit(ctx)
	if err != nil {
		return CustomAssistant{}, err
	}
	for _, ca := range data.CustomAssistants {
		if ca.UUID == uuid {
			return ca, nil
		}
	}
	return CustomAssistant{}, ErrNotFound
}
