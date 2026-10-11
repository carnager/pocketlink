package davfs

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carnager/pocketlink/internal/pair"
	"github.com/carnager/pocketlink/internal/phonefs"
	"github.com/carnager/pocketlink/internal/proto"
)

type devList []pair.Device

func (d devList) List() []pair.Device { return d }

type env struct {
	url   string // of the phone's folder
	root  string // the phone's storage
	phone *phonefs.Local
}

func newEnv(t *testing.T, features ...string) *env {
	t.Helper()
	root := t.TempDir()
	features = append([]string{proto.FeatureFS, proto.FeatureFSWatch}, features...)
	phone := &phonefs.Local{Server: &phonefs.Server{Root: root}, Features: features}
	srv := httptest.NewServer(New(phone, devList{{ID: "dev", Name: "My/Phone"}}))
	t.Cleanup(srv.Close)
	return &env{url: srv.URL + "/My_Phone", root: root, phone: phone}
}

func (e *env) do(t *testing.T, method, path string, body io.Reader, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(data)
}

func (e *env) write(t *testing.T, name, content string) {
	t.Helper()
	p := filepath.Join(e.root, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPropfindIsOneRequest(t *testing.T) {
	e := newEnv(t)
	for i := range 50 {
		e.write(t, fmt.Sprintf("DCIM/img%02d.weird", i), "x")
	}
	e.write(t, "DCIM/sub/deep.txt", "x")

	resp, body := e.do(t, "PROPFIND", "/DCIM/", nil, "Depth", "1")
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: %s\n%s", resp.Status, body)
	}
	if n := strings.Count(body, "<D:response>"); n != 52 {
		t.Errorf("%d responses, want 52 (the dir, 50 files, sub)", n)
	}
	if strings.Contains(body, "deep.txt") {
		t.Error("depth 1 listed a grandchild")
	}
	if !strings.Contains(body, "application/octet-stream") {
		t.Error("no content type for an unknown extension")
	}
	lists, stats, reads := e.phone.Calls(proto.TypeFSList), e.phone.Calls(proto.TypeFSStat), e.phone.Calls(proto.TypeFSRead)
	// The directory itself is stat'ed via its parent, which costs one list.
	if lists > 2 || stats > 1 || reads != 0 {
		t.Errorf("requests to the phone: %d list, %d stat, %d read; want ≤2, ≤1, 0", lists, stats, reads)
	}
}

func TestRefusesInfiniteDepth(t *testing.T) {
	e := newEnv(t)
	for _, depth := range []string{"", "infinity"} {
		var hdr []string
		if depth != "" {
			hdr = []string{"Depth", depth}
		}
		if resp, _ := e.do(t, "PROPFIND", "/", nil, hdr...); resp.StatusCode != http.StatusForbidden {
			t.Errorf("PROPFIND depth %q: %s", depth, resp.Status)
		}
	}
}

func TestTopLevel(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(t, "PROPFIND", "/..", nil, "Depth", "1")
	if resp.StatusCode != http.StatusMultiStatus || !strings.Contains(body, "/My_Phone/") {
		t.Fatalf("top level: %s\n%s", resp.Status, body)
	}
	if resp, _ := e.do(t, "DELETE", "", nil); resp.StatusCode < 400 {
		t.Errorf("deleting a phone's folder: %s", resp.Status)
	}
}

func TestPutGetMove(t *testing.T) {
	t.Run("chunks", func(t *testing.T) { testPutGetMove(t, newEnv(t)) })
	t.Run("streams", func(t *testing.T) {
		e := newEnv(t, proto.FeatureFSStream, proto.FeatureFSWriteCreate)
		testPutGetMove(t, e)
		// A big file goes in one stream each way.
		if in, out := e.phone.Calls(proto.TypeFSWriteStream), e.phone.Calls(proto.TypeFSReadStream); in != 1 || out < 1 {
			t.Errorf("%d write streams, %d read streams", in, out)
		}
		// Only the small overwrite goes as a plain write, which creates it.
		if n, c := e.phone.Calls(proto.TypeFSWrite), e.phone.Calls(proto.TypeFSCreate); n != 1 || c != 1 {
			t.Errorf("%d plain writes and %d creates, want 1 and 1 (for the big file's stream)", n, c)
		}
	})
}

func testPutGetMove(t *testing.T, e *env) {
	data := make([]byte, 5*proto.MaxPayload+777)
	rand.Read(data)

	if resp, body := e.do(t, "MKCOL", "/Music", nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL: %s %s", resp.Status, body)
	}
	if resp, body := e.do(t, "PUT", "/Music/song.flac", bytes.NewReader(data)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: %s %s", resp.Status, body)
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, "Music/song.flac")); !bytes.Equal(got, data) {
		t.Fatalf("file on the phone has %d bytes, differs from the %d sent", len(got), len(data))
	}

	resp, body := e.do(t, "GET", "/Music/song.flac", nil)
	if resp.StatusCode != http.StatusOK || body != string(data) {
		t.Fatalf("GET: %s, %d bytes", resp.Status, len(body))
	}
	resp, body = e.do(t, "GET", "/Music/song.flac", nil, "Range", "bytes=1000000-1000099")
	if resp.StatusCode != http.StatusPartialContent || body != string(data[1000000:1000100]) {
		t.Fatalf("ranged GET: %s, %q", resp.Status, body)
	}

	// Overwriting a file truncates it.
	if resp, _ := e.do(t, "PUT", "/Music/song.flac", strings.NewReader("short")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("second PUT: %s", resp.Status)
	}
	if _, body := e.do(t, "GET", "/Music/song.flac", nil); body != "short" {
		t.Fatalf("after overwrite: %d bytes", len(body))
	}

	if resp, body := e.do(t, "MOVE", "/Music/song.flac", nil, "Destination", e.url+"/song.flac"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("MOVE: %s %s", resp.Status, body)
	}
	if resp, _ := e.do(t, "GET", "/Music/song.flac", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET of moved file: %s", resp.Status)
	}
	if resp, _ := e.do(t, "DELETE", "/Music", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %s", resp.Status)
	}
	if _, err := os.Stat(filepath.Join(e.root, "Music")); !os.IsNotExist(err) {
		t.Fatal("directory still there")
	}
	if resp, _ := e.do(t, "PUT", "/nodir/x", strings.NewReader("x")); resp.StatusCode != http.StatusConflict {
		t.Fatalf("PUT into missing dir: %s", resp.Status)
	}
}

// A small file is read in one request, not a whole window and a prefetch.
func TestSmallRead(t *testing.T) {
	e := newEnv(t)
	e.write(t, "note.txt", "hi")
	e.do(t, "PROPFIND", "/", nil, "Depth", "1")
	e.phone.Calls(proto.TypeFSRead)
	if _, body := e.do(t, "GET", "/note.txt", nil); body != "hi" {
		t.Fatalf("GET: %q", body)
	}
	if n := e.phone.Calls(proto.TypeFSRead); n != 1 {
		t.Errorf("%d reads for a 2-byte file", n)
	}
}

func TestOffline(t *testing.T) {
	e := newEnv(t)
	e.phone.Offline = true
	if resp, body := e.do(t, "PROPFIND", "/", nil, "Depth", "1"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PROPFIND while offline: %s %s", resp.Status, body)
	}
	// The top level still lists the phone, without asking it.
	resp, body := e.do(t, "PROPFIND", "/..", nil, "Depth", "1")
	if resp.StatusCode != http.StatusMultiStatus || !strings.Contains(body, "/My_Phone/") || !strings.HasSuffix(strings.TrimSpace(body), "</D:multistatus>") {
		t.Fatalf("top level while offline: %s\n%s", resp.Status, body)
	}
}

func TestSameUserListener(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("own connection refused: %v", err)
	}
	if _, err := Listen("0.0.0.0:0"); err == nil {
		t.Fatal("listening on all interfaces was allowed")
	}
}

func TestParseProcAddr(t *testing.T) {
	for in, want := range map[string]string{
		"0100007F:1F90":                         "127.0.0.1:8080",
		"00000000000000000000000001000000:06E5": "[::1]:1765",
	} {
		a, err := parseProcAddr(in)
		if err != nil || a.String() != want {
			t.Errorf("parseProcAddr(%q) = %v, %v; want %s", in, a, err, want)
		}
	}
}

// A folder the phone refuses to list is an error, not an empty folder.
func TestNoAccess(t *testing.T) {
	e := newEnv(t)
	e.write(t, "secret/x", "x")
	os.Chmod(filepath.Join(e.root, "secret"), 0)
	defer os.Chmod(filepath.Join(e.root, "secret"), 0o755)
	if resp, body := e.do(t, "PROPFIND", "/secret/", nil, "Depth", "1"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PROPFIND of unreadable dir: %s\n%s", resp.Status, body)
	}
}

func TestFolder(t *testing.T) {
	s := New(&phonefs.Local{}, devList{{ID: "a", Name: "Pixel 8"}, {ID: "b", Name: "x/y"}})
	if got := s.Folder("a"); got != "/Pixel%208/" {
		t.Errorf("Folder(a) = %q", got)
	}
	if got := s.Folder("b"); got != "/x_y/" {
		t.Errorf("Folder(b) = %q", got)
	}
}

// Uploading many small files into a folder costs one request per file,
// plus one listing of the folder.
func TestSmallUploads(t *testing.T) {
	e := newEnv(t, proto.FeatureFSStream, proto.FeatureFSWriteCreate)
	e.do(t, "MKCOL", "/up", nil)
	e.phone.Calls(proto.TypeFSList)
	e.phone.Calls(proto.TypeFSStat)
	for i := range 30 {
		name := fmt.Sprintf("/up/f%02d.txt", i)
		// What clients do: check the target, then PUT.
		if resp, _ := e.do(t, "PROPFIND", name, nil, "Depth", "0"); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("PROPFIND %s: %s", name, resp.Status)
		}
		if resp, _ := e.do(t, "PUT", name, strings.NewReader(name)); resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT %s: %s", name, resp.Status)
		}
	}
	if resp, _ := e.do(t, "PUT", "/up/empty", strings.NewReader("")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT of an empty file: %s", resp.Status)
	}
	got, _ := os.ReadFile(filepath.Join(e.root, "up/f07.txt"))
	if string(got) != "/up/f07.txt" {
		t.Fatalf("f07 holds %q", got)
	}
	if fi, err := os.Stat(filepath.Join(e.root, "up/empty")); err != nil || fi.Size() != 0 {
		t.Fatalf("empty file: %v", err)
	}
	total := 0
	for _, typ := range []string{proto.TypeFSList, proto.TypeFSStat, proto.TypeFSCreate, proto.TypeFSWrite} {
		total += e.phone.Calls(typ)
	}
	if total > 31+2 {
		t.Errorf("%d requests for 31 files", total)
	}
	if resp, _ := e.do(t, "PUT", "/nodir/x", strings.NewReader("x")); resp.StatusCode != http.StatusConflict {
		t.Fatalf("PUT into a missing folder: %s", resp.Status)
	}
	if resp, _ := e.do(t, "PUT", "/up", strings.NewReader("x")); resp.StatusCode < 400 {
		t.Fatalf("PUT over a folder: %s", resp.Status)
	}
}
