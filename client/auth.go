package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const SigninPath = "/signin"

// maxLoginHops bounds every redirect chain in the sign-in flow. The real chain
// is 3-4 hops (kagi.com/signin → /oauth2 → account.kagi.com/oauth/v2/authorize
// → /loginname); anything longer is a loop, not a slow server.
const maxLoginHops = 10

var (
	formActionRE = regexp.MustCompile(`<form\b[^>]*\baction="([^"]*)"`)
	inputTagRE   = regexp.MustCompile(`<input\b[^>]*>`)
	nameAttrRE   = regexp.MustCompile(`\bname="([^"]*)"`)
	valueAttrRE  = regexp.MustCompile(`\bvalue="([^"]*)"`)
	hiddenAttrRE = regexp.MustCompile(`\btype="hidden"`)
)

// Login performs the email/password sign-in flow against Kagi's Zitadel OIDC
// login (account.kagi.com, a SvelteKit app; see docs/api.md "Sign-in flow"):
//
//  1. GET kagi.com/signin, which redirects to the login-name form
//  2. POST the email to the form action → JSON redirect to the password form
//  3. POST the password → JSON redirect to kagi.com/oauth2/callback
//  4. GET the callback, which sets the kagi_session cookie
//
// On success it updates c.Session and invokes c.OnRefresh (if set) so callers
// can persist the new value.
func (c *Client) Login(ctx context.Context) error {
	if !c.hasCreds() {
		return errors.New("login: KAGI_EMAIL/KAGI_PASSWORD not set")
	}

	page, err := c.follow(ctx, c.signinBase()+SigninPath)
	if err != nil {
		return fmt.Errorf("login: get signin: %w", err)
	}
	if page.session != "" {
		// Already signed in (the jar still held a valid Zitadel session).
		return c.setSession(page.session)
	}

	next, err := c.submitForm(ctx, page, map[string]string{"loginName": c.Email})
	if err != nil {
		return fmt.Errorf("login: email step: %w", err)
	}
	page, err = c.follow(ctx, next)
	if err != nil {
		return fmt.Errorf("login: get password form: %w", err)
	}
	if !strings.Contains(page.body, `name="password"`) {
		return fmt.Errorf("login: no password field at %s (unknown account, or MFA/passkey required)", page.url.Path)
	}

	next, err = c.submitForm(ctx, page, map[string]string{
		"loginName":  c.Email,
		"password":   c.Password,
		"rememberMe": "on",
	})
	if err != nil {
		return fmt.Errorf("login: password step: %w", err)
	}
	page, err = c.follow(ctx, next)
	if err != nil {
		return fmt.Errorf("login: oauth callback: %w", err)
	}
	if page.session == "" {
		return fmt.Errorf("login: no kagi_session cookie after callback (ended at %s)", page.url.Path)
	}
	return c.setSession(page.session)
}

func (c *Client) setSession(session string) error {
	c.Session = session
	if c.OnRefresh != nil {
		c.OnRefresh(session)
	}
	return nil
}

// signinBase is the kagi.com origin the flow starts from; only tests override it.
func (c *Client) signinBase() string {
	if c.authBase != "" {
		return c.authBase
	}
	return BaseURL
}

// loginPage is where a redirect chain landed: the final URL (for resolving
// relative form actions), its HTML, and the kagi_session cookie if any hop set
// one.
type loginPage struct {
	url     *url.URL
	body    string
	session string
}

// follow GETs rawURL and walks redirects by hand — the client disables
// automatic redirects so API calls can read 3xx auth bounces — collecting any
// kagi_session cookie set along the way. The cookie jar carries the Zitadel
// cookies between hops.
func (c *Client) follow(ctx context.Context, rawURL string) (loginPage, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return loginPage{}, err
	}
	var session string
	for range maxLoginHops {
		req, err := c.newRequestURL(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return loginPage{}, err
		}
		req.Header.Set("Accept", "text/html")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return loginPage{}, err
		}
		for _, ck := range resp.Cookies() {
			if ck.Name == "kagi_session" && ck.Value != "" {
				session = ck.Value
			}
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			resp.Body.Close()
			// kagi.com/oauth2 sends the authorize URL with raw spaces in
			// `scope`; browsers percent-encode them, Go would put them on the
			// request line verbatim and get a 400.
			loc, err := u.Parse(strings.ReplaceAll(resp.Header.Get("Location"), " ", "%20"))
			if err != nil {
				return loginPage{}, fmt.Errorf("bad redirect from %s: %w", u.Path, err)
			}
			u = loc
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return loginPage{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return loginPage{}, fmt.Errorf("%s: status %d", u.Path, resp.StatusCode)
		}
		return loginPage{url: u, body: string(body), session: session}, nil
	}
	return loginPage{}, fmt.Errorf("more than %d redirects", maxLoginHops)
}

// actionResult is SvelteKit's form-action reply when the request asks for
// JSON: {"type":"redirect","location":…} on success, "failure" with data when
// the action rejected the input (e.g. wrong password).
type actionResult struct {
	Type     string          `json:"type"`
	Location string          `json:"location"`
	Data     json.RawMessage `json:"data"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}

// submitForm POSTs the page's first form — its hidden inputs plus fields —
// and returns the absolute URL the action redirected to.
func (c *Client) submitForm(ctx context.Context, page loginPage, fields map[string]string) (string, error) {
	m := formActionRE.FindStringSubmatch(page.body)
	if m == nil {
		return "", fmt.Errorf("no form at %s", page.url.Path)
	}
	action, err := page.url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		return "", fmt.Errorf("bad form action: %w", err)
	}

	form := hiddenInputs(page.body)
	for k, v := range fields {
		form.Set(k, v)
	}
	req, err := c.newRequestURL(ctx, http.MethodPost, action.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", action.Scheme+"://"+action.Host)
	req.Header.Set("Referer", page.url.String())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	var res actionResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("decode action result: %w", err)
	}
	switch res.Type {
	case "redirect":
		loc, err := action.Parse(res.Location)
		if err != nil {
			return "", fmt.Errorf("bad redirect location: %w", err)
		}
		return loc.String(), nil
	case "failure":
		return "", fmt.Errorf("rejected (wrong credentials?): %s", truncate(res.Data, 200))
	default:
		return "", fmt.Errorf("unexpected action result %q: %s", res.Type, res.Error.Message)
	}
}

// hiddenInputs collects every <input type="hidden"> on the page — requestId,
// sessionId and friends — so the flow tolerates Kagi adding or renaming them.
func hiddenInputs(body string) url.Values {
	form := url.Values{}
	for _, tag := range inputTagRE.FindAllString(body, -1) {
		if !hiddenAttrRE.MatchString(tag) {
			continue
		}
		name := nameAttrRE.FindStringSubmatch(tag)
		if name == nil {
			continue
		}
		var value string
		if v := valueAttrRE.FindStringSubmatch(tag); v != nil {
			value = html.UnescapeString(v[1])
		}
		form.Set(name[1], value)
	}
	return form
}
