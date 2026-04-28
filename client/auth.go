package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const SigninPath = "/signin"
const LoginPath = "/login"

var csrfTagRE = regexp.MustCompile(`<input\b[^>]*name="_csrf"[^>]*>`)
var valueAttrRE = regexp.MustCompile(`value="([^"]*)"`)

// Login performs the email/password sign-in flow: GET /signin to capture the
// CSRF token, then POST /login. On success it updates c.Session and invokes
// c.OnRefresh (if set) so callers can persist the new value.
func (c *Client) Login(ctx context.Context) error {
	if !c.hasCreds() {
		return errors.New("login: KAGI_EMAIL/KAGI_PASSWORD not set")
	}
	csrf, err := c.fetchCSRF(ctx)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	form := url.Values{}
	form.Set("_csrf", csrf)
	form.Set("r", "/")
	form.Set("email", c.Email)
	form.Set("password", c.Password)

	req, err := c.newRequest(ctx, http.MethodPost, LoginPath, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("Referer", BaseURL+SigninPath)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("login: post: %w", err)
	}
	defer resp.Body.Close()

	// success = 302 to /assistant (or any non-auth path); a 200 means the
	// login form was re-rendered with an error.
	if resp.StatusCode == http.StatusOK {
		return errors.New("login: rejected (wrong credentials?)")
	}
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("login: status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	loc := resp.Header.Get("Location")
	if strings.Contains(loc, "/signin") || strings.Contains(loc, "/signup") {
		return errors.New("login: rejected (server redirected back to signin)")
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "kagi_session" && ck.Value != "" {
			c.Session = ck.Value
			if c.OnRefresh != nil {
				c.OnRefresh(ck.Value)
			}
			return nil
		}
	}
	return errors.New("login: no kagi_session cookie in response")
}

func (c *Client) fetchCSRF(ctx context.Context) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, SigninPath, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("get signin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get signin: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	tag := csrfTagRE.Find(body)
	if tag == nil {
		return "", errors.New("csrf input tag not found in signin form")
	}
	m := valueAttrRE.FindSubmatch(tag)
	if m == nil {
		return "", errors.New("csrf value attribute not found")
	}
	return string(m[1]), nil
}
