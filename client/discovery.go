package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
)

// AssistantProfile is one entry from the embedded `json-profile-list` on the
// /assistant page. Includes both base profiles (one per available model) and
// user-created Custom Assistants.
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

var profileListRE = regexp.MustCompile(`<div id="json-profile-list" hidden>(.+?)</div>`)

// FetchProfiles loads /assistant and parses the embedded profile list.
func (c *Client) FetchProfiles(ctx context.Context) ([]AssistantProfile, error) {
	return c.fetchProfiles(ctx, false)
}

func (c *Client) fetchProfiles(ctx context.Context, retried bool) ([]AssistantProfile, error) {
	if c.Session == "" {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return nil, fmt.Errorf("auto-login: %w", err)
			}
		} else {
			return nil, errors.New("client: empty session")
		}
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/assistant", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", "kagi_session="+c.Session)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch /assistant: %w", err)
	}
	defer resp.Body.Close()

	if isAuthFail(resp) {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return nil, fmt.Errorf("relogin after auth fail: %w", err)
			}
			return c.fetchProfiles(ctx, true)
		}
		return nil, fmt.Errorf("auth failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/assistant: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	m := profileListRE.FindSubmatch(body)
	if m == nil {
		return nil, errors.New("json-profile-list element not found in /assistant")
	}
	var wrapper struct {
		Profiles []AssistantProfile `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &wrapper); err != nil {
		return nil, fmt.Errorf("decode profile list: %w", err)
	}
	return wrapper.Profiles, nil
}
