package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	CustomAssistantUpdatePath = "/settings/ast/profiles/update"
	CustomAssistantDeletePath = "/settings/ast/profiles/delete"
	CustomAssistantEditPath   = "/settings/custom_assistant"
)

// CustomAssistantSpec is the editable shape of a Custom Assistant. ID empty
// means "create"; ID set means "update". Fields with zero values are sent as
// such — there is no merge behaviour on the server, every update is a full
// rewrite.
type CustomAssistantSpec struct {
	ID               string // empty → create
	Name             string // required
	BaseModel        string // model id, e.g. "claude-4-sonnet"
	Instructions     string // system prompt
	BangTrigger      string // optional bang command (e.g. "code" → !code prompt)
	InternetAccess   bool
	Personalizations bool
	LensID           string // "0" or "" = no lens
}

// SaveCustomAssistant creates (when spec.ID is empty) or updates (when set)
// a Custom Assistant. Returns the resulting profile id — for creates this
// requires re-fetching the profile list because the form POST 302s back
// without echoing the new id.
func (c *Client) SaveCustomAssistant(ctx context.Context, spec CustomAssistantSpec) (string, error) {
	if spec.Name == "" {
		return "", errors.New("SaveCustomAssistant: name is required")
	}
	if spec.BaseModel == "" {
		return "", errors.New("SaveCustomAssistant: base_model is required")
	}
	form := url.Values{}
	form.Set("profile_id", spec.ID)
	form.Set("name", spec.Name)
	form.Set("base_model", spec.BaseModel)
	form.Set("custom_instructions", spec.Instructions)
	form.Set("bang_trigger", spec.BangTrigger)
	// The form has BOTH a checkbox (value="on", only sent when checked) and a
	// hidden fallback (value="false"). We replicate the on/off toggle by
	// emitting one value: the hidden "false" if disabled, "on" if enabled.
	form.Set("internet_access", boolField(spec.InternetAccess))
	form.Set("personalizations", boolField(spec.Personalizations))
	if spec.LensID == "" {
		form.Set("selected_lens", "0")
	} else {
		form.Set("selected_lens", spec.LensID)
	}

	if err := c.postForm(ctx, CustomAssistantUpdatePath, form); err != nil {
		return "", err
	}
	if spec.ID != "" {
		return spec.ID, nil
	}
	// Create case: look up the new id by name. Kagi names are unique within
	// an account so this is safe; if it isn't, the most recently created
	// assistant wins (FetchProfiles returns them in catalog order).
	profiles, err := c.FetchProfiles(ctx)
	if err != nil {
		return "", fmt.Errorf("created but couldn't resolve id: %w", err)
	}
	for _, p := range profiles {
		if p.ID != "" && p.Name == spec.Name {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("created but no profile with name %q found", spec.Name)
}

// DeleteCustomAssistant removes the profile by UUID. The settings endpoint
// always replies with a 302 — both on success and on rejected requests
// (e.g. trying to delete the default profile, which the server refuses with
// an error flash on the redirected page). We can't tell those apart from the
// HTTP response alone, so we bracket the POST with two FetchProfiles calls:
// the id has to exist beforehand and be gone afterward. If it never existed,
// surface that as an error too — silently succeeding on a typo'd id would
// be more confusing than helpful.
func (c *Client) DeleteCustomAssistant(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("DeleteCustomAssistant: id required")
	}
	before, err := c.FetchProfiles(ctx)
	if err != nil {
		return fmt.Errorf("verify-before failed: %w", err)
	}
	if !containsProfileID(before, id) {
		return fmt.Errorf("no custom assistant with id %s", id)
	}
	form := url.Values{}
	form.Set("profile_id", id)
	if err := c.postForm(ctx, CustomAssistantDeletePath, form); err != nil {
		return err
	}
	after, err := c.FetchProfiles(ctx)
	if err != nil {
		return fmt.Errorf("delete posted but verify-after failed: %w", err)
	}
	if containsProfileID(after, id) {
		return fmt.Errorf("delete rejected by server (profile %s still present — likely the default profile, or in use)", id)
	}
	return nil
}

func containsProfileID(profiles []AssistantProfile, id string) bool {
	for _, p := range profiles {
		if p.ID == id {
			return true
		}
	}
	return false
}

// FetchCustomAssistant loads the per-assistant edit page and parses the form
// values back into a spec. Used by the CLI's `assistants update` to support
// partial updates: the user can change `--name X` alone without losing the
// existing system prompt, since the server's update form overwrites every
// field.
//
// The edit page isn't a JSON island — it's the rendered settings form.
// Parsing is regex-based; the form structure has been stable across the
// captures we have (2026-04 → 2026-05), but a server tweak could break it.
func (c *Client) FetchCustomAssistant(ctx context.Context, id string) (CustomAssistantSpec, error) {
	return c.fetchCustomAssistant(ctx, id, false)
}

func (c *Client) fetchCustomAssistant(ctx context.Context, id string, retried bool) (CustomAssistantSpec, error) {
	if id == "" {
		return CustomAssistantSpec{}, errors.New("FetchCustomAssistant: id required")
	}
	if c.Session == "" {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return CustomAssistantSpec{}, fmt.Errorf("auto-login: %w", err)
			}
		} else {
			return CustomAssistantSpec{}, errors.New("client: empty session")
		}
	}
	req, err := c.newRequest(ctx, http.MethodGet, CustomAssistantEditPath+"?id="+url.QueryEscape(id), nil)
	if err != nil {
		return CustomAssistantSpec{}, err
	}
	req.Header.Set("Cookie", "kagi_session="+c.Session)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return CustomAssistantSpec{}, fmt.Errorf("fetch edit page: %w", err)
	}
	defer resp.Body.Close()
	if isAuthFail(resp) {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return CustomAssistantSpec{}, fmt.Errorf("relogin after auth fail: %w", err)
			}
			return c.fetchCustomAssistant(ctx, id, true)
		}
		return CustomAssistantSpec{}, fmt.Errorf("auth failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return CustomAssistantSpec{}, fmt.Errorf("%s?id=%s: status %d", CustomAssistantEditPath, id, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return CustomAssistantSpec{}, err
	}
	spec, err := parseCustomAssistantForm(body)
	if err != nil {
		return CustomAssistantSpec{}, err
	}
	spec.ID = id
	return spec, nil
}

var (
	// Inputs span multiple lines (newlines+spaces inside the tag) for
	// bang_trigger and others; (?s) so . matches newlines.
	nameInputRE             = regexp.MustCompile(`(?s)<input[^>]*name="name"[^>]*value="([^"]*)"`)
	bangTriggerInputRE      = regexp.MustCompile(`(?s)<input[^>]*name="bang_trigger"[^>]*value="([^"]*)"`)
	customInstructionsRE    = regexp.MustCompile(`(?s)<textarea[^>]*name="custom_instructions"[^>]*>(.*?)</textarea>`)
	checkedBaseModelRE      = regexp.MustCompile(`(?s)<input[^>]*checked[^>]*name="base_model"[^>]*value="([^"]*)"|<input[^>]*name="base_model"[^>]*value="([^"]*)"[^>]*checked`)
	checkedSelectedLensRE   = regexp.MustCompile(`(?s)<input[^>]*checked[^>]*name="selected_lens"[^>]*value="([^"]*)"|<input[^>]*name="selected_lens"[^>]*value="([^"]*)"[^>]*checked`)
	internetAccessCheckRE   = regexp.MustCompile(`(?s)<input[^>]*name="internet_access"[^>]*type="checkbox"[^>]*>`)
	personalizationsCheckRE = regexp.MustCompile(`(?s)<input[^>]*name="personalizations"[^>]*type="checkbox"[^>]*>`)
)

func parseCustomAssistantForm(body []byte) (CustomAssistantSpec, error) {
	spec := CustomAssistantSpec{}
	if m := nameInputRE.FindSubmatch(body); m != nil {
		spec.Name = html.UnescapeString(string(m[1]))
	} else {
		return spec, errors.New("custom_assistant edit page: no name input — wrong id, or form layout changed")
	}
	if m := bangTriggerInputRE.FindSubmatch(body); m != nil {
		spec.BangTrigger = html.UnescapeString(string(m[1]))
	}
	if m := customInstructionsRE.FindSubmatch(body); m != nil {
		spec.Instructions = html.UnescapeString(string(m[1]))
	}
	if m := checkedBaseModelRE.FindSubmatch(body); m != nil {
		spec.BaseModel = string(firstNonEmpty(m[1], m[2]))
	}
	if m := checkedSelectedLensRE.FindSubmatch(body); m != nil {
		spec.LensID = string(firstNonEmpty(m[1], m[2]))
	}
	// Checkboxes: presence of `checked` (with or without value) means on.
	// `value=on` alone (without `checked`) means the checkbox renders unchecked.
	if m := internetAccessCheckRE.Find(body); m != nil {
		spec.InternetAccess = bytes.Contains(m, []byte("checked"))
	}
	if m := personalizationsCheckRE.Find(body); m != nil {
		spec.Personalizations = bytes.Contains(m, []byte("checked"))
	}
	return spec, nil
}

func firstNonEmpty(b ...[]byte) []byte {
	for _, x := range b {
		if len(x) > 0 {
			return x
		}
	}
	return nil
}

func boolField(on bool) string {
	if on {
		return "on"
	}
	return "false"
}

// postForm submits an application/x-www-form-urlencoded request to a settings
// endpoint. These endpoints respond with a 302 redirect back to the listing
// page on success, or a 4xx/302-to-signin on failure. We treat any non-error
// response (200, 302 to a non-auth path) as success.
func (c *Client) postForm(ctx context.Context, path string, form url.Values) error {
	return c.postFormRetry(ctx, path, form, false)
}

func (c *Client) postFormRetry(ctx context.Context, path string, form url.Values, retried bool) error {
	if c.Session == "" {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return fmt.Errorf("auto-login: %w", err)
			}
		} else {
			return errors.New("client: empty session")
		}
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", "kagi_session="+c.Session)
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("Referer", BaseURL+"/settings/assistant")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()
	if isAuthFail(resp) {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return fmt.Errorf("relogin after auth fail: %w", err)
			}
			return c.postFormRetry(ctx, path, form, true)
		}
		return fmt.Errorf("auth failed (status %d)", resp.StatusCode)
	}
	// 302 to /settings/assistant or similar = success. The form re-rendered
	// (200) with an error message would mean a validation failure.
	if resp.StatusCode == http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("settings POST rejected (form re-rendered): %s", bytes.TrimSpace(b))
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}
