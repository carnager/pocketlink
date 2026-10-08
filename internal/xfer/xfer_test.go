package xfer

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tether/internal/pair"
)

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"photo.jpg":        "photo.jpg",
		"../../etc/passwd": "_.._etc_passwd",
		".bashrc":          "bashrc",
		"a\x00b\nc.txt":    "abc.txt",
		"  ":               "file",
		"..":               "file",
		`dir\name.txt`:     "dir_name.txt",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("ä", 150) + ".jpeg"
	got := sanitizeName(long)
	if len(got) > maxNameBytes || !strings.HasSuffix(got, ".jpeg") {
		t.Errorf("long name not truncated correctly: %d bytes, %q", len(got), got[len(got)-10:])
	}
}

type testEnv struct {
	in     *Incoming
	srv    *httptest.Server
	client *http.Client
	dest   string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := pair.OpenStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pair.LoadOrCreateIdentity(filepath.Join(dir, "phone"), "phone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(pair.Fingerprint(cert.Certificate[0]), "phone"); err != nil {
		t.Fatal(err)
	}

	env := &testEnv{dest: filepath.Join(dir, "dl")}
	env.in = NewIncoming(filepath.Join(dir, "incoming"), env.dest, store, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /files/in/{id}", env.in.ServePUT)
	mux.HandleFunc("HEAD /files/in/{id}", env.in.ServeHEAD)
	env.srv = httptest.NewUnstartedServer(mux)
	env.srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	env.srv.StartTLS()
	t.Cleanup(env.srv.Close)

	tr := env.srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.Certificates = []tls.Certificate{cert}
	env.client = &http.Client{Transport: tr}
	return env
}

func (env *testEnv) put(t *testing.T, id string, body io.Reader, length, size, offset int64) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, env.srv.URL+"/files/in/"+id, body)
	req.ContentLength = length
	req.Header.Set(HeaderName, "f.bin")
	req.Header.Set(HeaderSize, strconv.FormatInt(size, 10))
	req.Header.Set(HeaderOffset, strconv.FormatInt(offset, 10))
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// A resumed upload must take over from a request whose body has stalled
// (e.g. the phone switched networks), instead of waiting for TCP to time out.
func TestResumeSupersedesStalledUpload(t *testing.T) {
	env := newTestEnv(t)
	data := make([]byte, 64<<10)
	rand.Read(data)
	size := int64(len(data))
	const id = "transfer-1"

	// First attempt: send 1000 bytes, then stall forever.
	pr, pw := io.Pipe()
	defer pw.Close()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req, _ := http.NewRequest(http.MethodPut, env.srv.URL+"/files/in/"+id, pr)
		req.ContentLength = size
		req.Header.Set(HeaderName, "f.bin")
		req.Header.Set(HeaderSize, strconv.FormatInt(size, 10))
		req.Header.Set(HeaderOffset, "0")
		if resp, err := env.client.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	pw.Write(data[:1000])

	// Wait until the server has written those bytes.
	deadline := time.Now().Add(5 * time.Second)
	var have int64
	for have < 1000 {
		if time.Now().After(deadline) {
			t.Fatalf("server only has %d bytes", have)
		}
		time.Sleep(10 * time.Millisecond)
		req, _ := http.NewRequest(http.MethodHead, env.srv.URL+"/files/in/"+id, nil)
		if resp, err := env.client.Do(req); err == nil {
			resp.Body.Close()
			have, _ = strconv.ParseInt(resp.Header.Get(HeaderOffset), 10, 64)
		}
	}

	// Resume from a "new connection" with the rest of the file. It must not
	// hang behind the stalled request.
	start := time.Now()
	resp := env.put(t, id, bytes.NewReader(data[1000:]), size-1000, size, 1000)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("resume: got %s, want 201", resp.Status)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("resume took %s; stalled request was not superseded", d)
	}
	got, err := os.ReadFile(filepath.Join(env.dest, "f.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("saved file differs (err=%v, %d bytes)", err, len(got))
	}

	pw.Close()
	<-firstDone
}

func TestOffsetMismatchAndIdempotentCompletion(t *testing.T) {
	env := newTestEnv(t)
	data := []byte("hello world")
	size := int64(len(data))
	const id = "transfer-2"

	if r := env.put(t, id, bytes.NewReader(data[:5]), 5, size, 0); r.StatusCode != http.StatusNoContent {
		t.Fatalf("partial: got %s", r.Status)
	}
	r := env.put(t, id, bytes.NewReader(data), size, size, 0)
	if r.StatusCode != http.StatusConflict || r.Header.Get(HeaderOffset) != "5" {
		t.Fatalf("wrong offset: got %s offset=%s, want 409 offset=5", r.Status, r.Header.Get(HeaderOffset))
	}
	if r := env.put(t, id, bytes.NewReader(data[5:]), size-5, size, 5); r.StatusCode != http.StatusCreated {
		t.Fatalf("finish: got %s", r.Status)
	}
	// Repeating the last request (its response got lost) must not create a
	// second file.
	if r := env.put(t, id, bytes.NewReader(data[5:]), size-5, size, 5); r.StatusCode != http.StatusCreated {
		t.Fatalf("repeat: got %s", r.Status)
	}
	entries, _ := os.ReadDir(env.dest)
	if len(entries) != 1 {
		t.Fatalf("got %d files in destination, want 1", len(entries))
	}
}
