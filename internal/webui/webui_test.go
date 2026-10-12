package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/carnager/pocketlink/internal/control"
)

func newTestServer(t *testing.T) (*Server, *[]control.Request) {
	var got []control.Request
	s := &Server{addr: "127.0.0.1:4242", token: "secret", handle: func(r control.Request) (any, error) {
		got = append(got, r)
		if r.Cmd == "pair" {
			return map[string]any{"uri": "pocketlink://pair?x=1", "expires": time.Now()}, nil
		}
		return map[string]string{"ok": r.Cmd}, nil
	}}
	return s, &got
}

func do(s *Server, method, host, path, origin, ctype, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestPage(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(s, "GET", s.addr, "/secret/", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pocketlink settings") {
		t.Fatalf("page: %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("missing security headers: %v", w.Header())
	}
	if w := do(s, "GET", s.addr, "/secret/app.js", "", "", ""); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("app.js: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := do(s, "GET", s.addr, "/secret", "", "", ""); w.Code != http.StatusFound {
		t.Errorf("no trailing slash: %d", w.Code)
	}
}

func TestRefusals(t *testing.T) {
	s, got := newTestServer(t)
	origin := "http://" + s.addr
	for _, tc := range []struct {
		name, method, host, path, origin, ctype string
		want                                    int
	}{
		{"no token", "GET", s.addr, "/", "", "", 404},
		{"wrong token", "GET", s.addr, "/secrex/", "", "", 404},
		{"rebound DNS name", "GET", "evil.example:4242", "/secret/", "", "", http.StatusMisdirectedRequest},
		{"localhost alias", "GET", "localhost:4242", "/secret/", "", "", http.StatusMisdirectedRequest},
		{"API by GET", "GET", s.addr, "/secret/api/status", origin, "application/json", 405},
		{"no origin", "POST", s.addr, "/secret/api/status", "", "application/json", 403},
		{"other site", "POST", s.addr, "/secret/api/set", "https://evil.example", "application/json", 403},
		{"form post", "POST", s.addr, "/secret/api/set", origin, "application/x-www-form-urlencoded", 415},
		{"CLI-only command", "POST", s.addr, "/secret/api/send", origin, "application/json", 405},
		{"path escape", "GET", s.addr, "/secret/../webui.go", "", "", 404},
	} {
		if w := do(s, tc.method, tc.host, tc.path, tc.origin, tc.ctype, `{}`); w.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.want)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("refused requests reached the daemon: %v", *got)
	}
}

func TestAPI(t *testing.T) {
	s, got := newTestServer(t)
	origin := "http://" + s.addr
	w := do(s, "POST", s.addr, "/secret/api/set", origin, "application/json", `{"key":"clipboard","value":"false","cmd":"send","paths":["/etc/passwd"]}`)
	if w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	// Only key, value and id come from the page; the command from the URL.
	if r := (*got)[0]; r.Cmd != "set" || r.Key != "clipboard" || r.Value != "false" || len(r.Paths) != 0 {
		t.Fatalf("daemon got %+v", r)
	}
	w = do(s, "POST", s.addr, "/secret/api/pair", origin, "application/json; charset=utf-8", `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"qr":"data:image/png;base64,`) {
		t.Fatalf("pair: %d %s", w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}
}
