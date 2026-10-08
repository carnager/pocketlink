// Package control is the local CLI <-> daemon channel: one JSON request and
// one JSON response per connection on a unix socket.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/adrg/xdg"
)

type Request struct {
	Cmd   string   `json:"cmd"`
	ID    string   `json:"id,omitempty"`
	Text  string   `json:"text,omitempty"`
	Watch bool     `json:"watch,omitempty"`
	Paths []string `json:"paths,omitempty"`
	Mime  string   `json:"mime,omitempty"`
	Data  []byte   `json:"data,omitempty"`
	Key   string   `json:"key,omitempty"`
	Value string   `json:"value,omitempty"`
}

type response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type HandlerFunc func(Request) (any, error)

// SocketPath is $POCKETLINK_SOCKET if set, else $XDG_RUNTIME_DIR/pocketlink.sock.
func SocketPath() string {
	if p := os.Getenv("POCKETLINK_SOCKET"); p != "" {
		return p
	}
	return filepath.Join(xdg.RuntimeDir, "pocketlink.sock")
}

func Serve(ctx context.Context, path string, h HandlerFunc) error {
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return errors.New("another daemon is already running")
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go serveConn(c, h)
	}
}

func serveConn(c net.Conn, h HandlerFunc) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	var req Request
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		return
	}
	var resp response
	data, err := h(req)
	if err == nil && data != nil {
		resp.Data, err = json.Marshal(data)
	}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.OK = true
	}
	json.NewEncoder(c).Encode(resp)
}

// Call sends req to the running daemon and decodes the reply into out,
// which may be nil.
func Call(req Request, out any) error {
	c, err := net.DialTimeout("unix", SocketPath(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach daemon (is `pocketlink daemon` running?): %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	var resp response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}
