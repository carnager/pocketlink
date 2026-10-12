// Package webui is the settings page: phones, pairing and settings in a
// browser, for desktops without the DMS plugin. `pocketlink settings`
// opens it.
//
// It runs on a random loopback port, for this user only (davfs.Listen),
// but a browser is a local program too: any website could make it send
// requests to 127.0.0.1. So the page lives under a random token that only
// `pocketlink settings` hands out, requests must be addressed to
// 127.0.0.1:port (against DNS rebinding), and calls must come from the
// page itself (Origin) as JSON.
package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/carnager/pocketlink/internal/control"
	"github.com/carnager/pocketlink/internal/davfs"
)

//go:embed static
var static embed.FS

// Handler answers a control command, as for the CLI.
type Handler func(control.Request) (any, error)

// What the page may do; everything else stays CLI-only.
var allowed = map[string]bool{"status": true, "config": true, "set": true, "pair": true, "unpair": true}

type Server struct {
	handle Handler
	addr   string
	token  string
	srv    *http.Server
}

// Start serves the page until the daemon stops.
func Start(h Handler) (*Server, error) {
	ln, err := davfs.Listen("127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{handle: h, addr: ln.Addr().String(), token: rand.Text()}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Warn("settings page stopped", "err", err)
		}
	}()
	return s, nil
}

// URL is the page's address, token included.
func (s *Server) URL() string {
	return "http://" + s.addr + "/" + s.token + "/"
}

func (s *Server) Close() error { return s.srv.Close() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer") // the token is in the URL
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")

	// A rebound DNS name would arrive with its own Host.
	if r.Host != s.addr {
		http.Error(w, "wrong host", http.StatusMisdirectedRequest)
		return
	}
	tok, rest, slash := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !slash { // the page's relative links need the trailing slash
		http.Redirect(w, r, "/"+s.token+"/", http.StatusFound)
		return
	}

	if cmd, ok := strings.CutPrefix(rest, "api/"); ok {
		s.api(w, r, cmd)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if rest == "" {
		rest = "index.html"
	}
	data, err := static.ReadFile(path.Join("static", path.Clean("/"+rest)))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.Set("Content-Type", mime.TypeByExtension(path.Ext(rest)))
	w.Write(data)
}

// api runs one control command for the page: POST api/{cmd} with a JSON
// control.Request (key, value, id).
func (s *Server) api(w http.ResponseWriter, r *http.Request, cmd string) {
	if r.Method != http.MethodPost || !allowed[cmd] {
		http.Error(w, "not allowed", http.StatusMethodNotAllowed)
		return
	}
	// From the page itself, and JSON, which a form on another site can't
	// send without the browser asking us first.
	if r.Header.Get("Origin") != "http://"+s.addr {
		http.Error(w, "wrong origin", http.StatusForbidden)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "JSON only", http.StatusUnsupportedMediaType)
		return
	}
	var req control.Request
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req = control.Request{Cmd: cmd, ID: req.ID, Key: req.Key, Value: req.Value}

	reply, err := s.handle(req)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if cmd == "pair" {
		reply = withQR(reply)
	}
	json.NewEncoder(w).Encode(reply)
}

// withQR adds the pairing link's QR code to a pair reply, as a data URL.
func withQR(reply any) any {
	data, _ := json.Marshal(reply)
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return reply
	}
	uri, _ := m["uri"].(string)
	code, err := qr.Encode(uri, qr.L)
	if err != nil {
		return reply
	}
	code.Scale = 8
	m["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
	return m
}
