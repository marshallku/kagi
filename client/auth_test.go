package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeZitadel mimics Kagi's sign-in chain on one server: /signin bounces
// through /oauth2 to the login-name form, the SvelteKit form actions answer
// with JSON redirects, and /oauth2/callback sets kagi_session.
func fakeZitadel(t *testing.T, password string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/signin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/oauth2?service=zitadel&", http.StatusFound)
	})
	mux.HandleFunc("/oauth2", func(w http.ResponseWriter, r *http.Request) {
		// Real Kagi sends raw spaces in the authorize URL's scope.
		w.Header().Set("Location", "/oauth/v2/authorize?scope=openid email profile")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/oauth/v2/authorize", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "openid email profile" {
			t.Errorf("authorize scope = %q", r.URL.Query().Get("scope"))
		}
		http.Redirect(w, r, "/loginname?requestId=req1", http.StatusFound)
	})
	mux.HandleFunc("/loginname", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("sessionId") == "" {
				fmt.Fprint(w, `<form method="POST" class="x" action="/loginname?/email&amp;requestId=req1">
<input type="hidden" name="requestId" value="req1"/>
<input type="text" name="loginName" value=""/></form>`)
				return
			}
			fmt.Fprint(w, `<form method="POST" action="/loginname?/password&amp;requestId=req1">
<input type="password" name="password" value=""/>
<input type="hidden" name="sessionId" value="sess9"/>
<input name="loginName" value="me@example.com" type="text" readonly=""/>
<input type="hidden" name="requestId" value="req1"/></form>`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("form action Accept = %q, want application/json", r.Header.Get("Accept"))
		}
		switch r.URL.RawQuery {
		case "/email&requestId=req1":
			if r.PostForm.Get("requestId") != "req1" || r.PostForm.Get("loginName") != "me@example.com" {
				t.Errorf("email step form = %v", r.PostForm)
			}
			fmt.Fprint(w, `{"type":"redirect","status":303,"location":"/loginname?sessionId=sess9&requestId=req1"}`)
		case "/password&requestId=req1":
			if r.PostForm.Get("sessionId") != "sess9" || r.PostForm.Get("requestId") != "req1" {
				t.Errorf("password step lost hidden inputs: %v", r.PostForm)
			}
			if r.PostForm.Get("password") != password {
				fmt.Fprint(w, `{"type":"failure","status":400,"data":"{\"error\":\"bad password\"}"}`)
				return
			}
			fmt.Fprint(w, `{"type":"redirect","status":303,"location":"/oauth2/callback?code=c&state=s"}`)
		default:
			t.Errorf("unexpected action %q", r.URL.RawQuery)
		}
	})
	mux.HandleFunc("/oauth2/callback", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "kagi_session", Value: "fresh-session", Path: "/"})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>home</html>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLogin_ZitadelFlow(t *testing.T) {
	srv := fakeZitadel(t, "hunter2")
	c := New("")
	c.authBase = srv.URL
	c.SetCredentials("me@example.com", "hunter2")
	var refreshed string
	c.OnRefresh = func(s string) { refreshed = s }

	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if c.Session != "fresh-session" || refreshed != "fresh-session" {
		t.Fatalf("session = %q, refreshed = %q; want fresh-session", c.Session, refreshed)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	srv := fakeZitadel(t, "hunter2")
	c := New("")
	c.authBase = srv.URL
	c.SetCredentials("me@example.com", "nope")

	err := c.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Login err = %v, want a rejection", err)
	}
	if c.Session != "" {
		t.Fatalf("session set on failure: %q", c.Session)
	}
}
