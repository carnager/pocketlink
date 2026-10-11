package phonefs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/carnager/pocketlink/internal/conn"
	"github.com/carnager/pocketlink/internal/pair"
	"github.com/carnager/pocketlink/internal/proto"
)

type testEnv struct {
	hub   *conn.Hub
	srv   *httptest.Server
	cert  tls.Certificate // the phone's
	devID string
	root  string // the phone's storage
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := pair.OpenStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	desk, err := pair.LoadOrCreateIdentity(filepath.Join(dir, "desk"), "desk")
	if err != nil {
		t.Fatal(err)
	}
	phone, err := pair.LoadOrCreateIdentity(filepath.Join(dir, "phone"), "phone")
	if err != nil {
		t.Fatal(err)
	}
	id := pair.Fingerprint(phone.Certificate[0])
	if _, err := store.Add(id, "phone"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hub := conn.NewHub(ctx, "desk", store, &pair.Pairing{}, filepath.Join(dir, "queue"), func(pair.Device, proto.Envelope) error { return nil })

	// httptest doesn't wait for hijacked (WebSocket) requests, which still
	// write to dir as they finish.
	var running sync.WaitGroup
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		running.Add(1)
		defer running.Done()
		hub.ServeHTTP(w, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{desk}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		running.Wait()
	})

	root := filepath.Join(dir, "storage")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return &testEnv{hub: hub, srv: srv, cert: phone, devID: id, root: root}
}

type fakePhone struct {
	c     *websocket.Conn
	stall atomic.Bool // swallow requests instead of answering
}

// connect dials the hub like the phone app and answers fs requests from
// env.root until the test ends or the connection is closed.
func (env *testEnv) connect(t *testing.T, features ...string) *fakePhone {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{env.cert}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
	}}}
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(env.srv.URL, "https")+"/ws", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(16 << 20)
	t.Cleanup(func() { c.CloseNow() })
	hello, _ := proto.NewUnacked(proto.TypeHello, proto.Hello{Name: "phone", Features: features})
	data, _ := json.Marshal(hello)
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}

	p := &fakePhone{c: c}
	srv := &Server{Root: env.root}
	go func() {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var e proto.Envelope
			var payload []byte
			if typ == websocket.MessageBinary {
				e, payload, err = proto.DecodeBinary(data)
			} else {
				err = json.Unmarshal(data, &e)
			}
			if err != nil || e.Req == "" || p.stall.Load() {
				continue
			}
			go func() {
				r, out := srv.Handle(e, payload)
				if out != nil {
					msg, _ := proto.EncodeBinary(r, out)
					c.Write(ctx, websocket.MessageBinary, msg)
				} else {
					msg, _ := json.Marshal(r)
					c.Write(ctx, websocket.MessageText, msg)
				}
			}()
		}
	}()

	// The hub learns the features from hello, which it reads asynchronously.
	deadline := time.Now().Add(5 * time.Second)
	for len(features) > 0 && !env.hub.Supports(env.devID, features[0]) {
		if time.Now().After(deadline) {
			t.Fatal("hub never saw the hello")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return p
}

func TestFileOps(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, proto.FeatureFS)
	cl := New(env.hub, env.devID)
	ctx := context.Background()

	if _, err := cl.Mkdir(ctx, "/DCIM"); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Mkdir(ctx, "/DCIM"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second mkdir: %v, want ErrExist", err)
	}
	if _, err := cl.Create(ctx, "/DCIM/a.txt", true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Create(ctx, "/DCIM/a.txt", true, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("exclusive create of existing file: %v", err)
	}
	if _, err := cl.WriteAt(ctx, "/DCIM/a.txt", []byte("hello world"), 0); err != nil {
		t.Fatal(err)
	}
	// An in-place write in the middle, then one past the end.
	if _, err := cl.WriteAt(ctx, "/DCIM/a.txt", []byte("WORLD"), 6); err != nil {
		t.Fatal(err)
	}
	ent, err := cl.WriteAt(ctx, "/DCIM/a.txt", []byte("!"), 13)
	if err != nil {
		t.Fatal(err)
	}
	if ent.Size != 14 || ent.Name != "a.txt" {
		t.Fatalf("entry after write: %+v", ent)
	}
	buf := make([]byte, 20)
	n, err := cl.ReadAt(ctx, "/DCIM/a.txt", buf, 0)
	if err != io.EOF || string(buf[:n]) != "hello WORLD\x00\x00!" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}

	size := int64(5)
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if ent, err = cl.SetAttr(ctx, "/DCIM/a.txt", &size, &mtime); err != nil {
		t.Fatal(err)
	}
	if ent.Size != 5 || ent.MTime != mtime.UnixMilli() {
		t.Fatalf("after setattr: %+v", ent)
	}

	if err := cl.Rename(ctx, "/DCIM/a.txt", "/b.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Create(ctx, "/c.txt", false, false); err != nil {
		t.Fatal(err)
	}
	if err := cl.Rename(ctx, "/c.txt", "/b.txt", true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("no-replace rename over a file: %v", err)
	}
	if _, err := cl.Stat(ctx, "/DCIM/a.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat of moved file: %v", err)
	}

	ents, err := cl.List(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"DCIM", "b.txt", "c.txt"}) {
		t.Fatalf("listing: %v", names)
	}
	if _, err := cl.List(ctx, "/b.txt"); err == nil {
		t.Fatal("listing a file succeeded")
	}

	if _, err := cl.Create(ctx, "/DCIM/x", false, false); err != nil {
		t.Fatal(err)
	}
	var pe *proto.Error
	if err := cl.Remove(ctx, "/DCIM", false); !errors.As(err, &pe) || pe.Code != proto.ErrNotEmpty {
		t.Fatalf("removing a non-empty dir: %v", err)
	}
	if err := cl.Remove(ctx, "/DCIM", true); err != nil {
		t.Fatal(err)
	}
	if err := cl.Remove(ctx, "/", true); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("removing the root: %v", err)
	}

	// Paths can't climb out of the storage root.
	if err := os.WriteFile(filepath.Join(filepath.Dir(env.root), "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Stat(ctx, "/../secret"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat outside root: %v", err)
	}

	if sp, err := cl.StatFS(ctx, "/"); err != nil || sp.Total <= 0 {
		t.Fatalf("statfs: %+v, %v", sp, err)
	}
}

// Reads and writes larger than one payload are split into chunks.
func TestLargeTransfer(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, proto.FeatureFS)
	cl := New(env.hub, env.devID)
	ctx := context.Background()

	data := make([]byte, 3*proto.MaxPayload+12345)
	rand.Read(data)
	if _, err := cl.Create(ctx, "/big", false, false); err != nil {
		t.Fatal(err)
	}
	ent, err := cl.WriteAt(ctx, "/big", data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ent.Size != int64(len(data)) {
		t.Fatalf("size after write: %d, want %d", ent.Size, len(data))
	}
	got, _ := os.ReadFile(filepath.Join(env.root, "big"))
	if !bytes.Equal(got, data) {
		t.Fatal("file on the phone differs")
	}

	buf := make([]byte, len(data)+100)
	n, err := cl.ReadAt(ctx, "/big", buf, 0)
	if err != io.EOF || n != len(data) || !bytes.Equal(buf[:n], data) {
		t.Fatalf("read back %d bytes, %v", n, err)
	}
	buf = buf[:proto.MaxPayload+10]
	if n, err := cl.ReadAt(ctx, "/big", buf, 100); err != nil || !bytes.Equal(buf[:n], data[100:100+len(buf)]) {
		t.Fatalf("read at offset: %d, %v", n, err)
	}
}

func TestOffline(t *testing.T) {
	env := newTestEnv(t)
	cl := New(env.hub, env.devID)
	if _, err := cl.Stat(context.Background(), "/"); !errors.Is(err, conn.ErrOffline) {
		t.Fatalf("stat while not connected: %v", err)
	}
	if env.hub.Supports(env.devID, proto.FeatureFS) {
		t.Fatal("offline device supports fs")
	}
}

func TestOldApp(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t) // no fs feature in hello
	deadline := time.Now().Add(5 * time.Second)
	for !env.hub.Connected(env.devID) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if _, err := New(env.hub, env.devID).Stat(context.Background(), "/"); !errors.Is(err, ErrNoFS) {
		t.Fatalf("got %v, want ErrNoFS", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("waited for an app that will never answer")
	}
}

func TestDisconnectDuringCall(t *testing.T) {
	env := newTestEnv(t)
	p := env.connect(t, proto.FeatureFS)
	p.stall.Store(true)
	cl := New(env.hub, env.devID)

	errc := make(chan error, 1)
	go func() {
		_, err := cl.Stat(context.Background(), "/")
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	p.c.CloseNow()
	select {
	case err := <-errc:
		if !errors.Is(err, conn.ErrOffline) {
			t.Fatalf("got %v, want ErrOffline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call still waiting after the connection dropped")
	}
}

func TestTimeout(t *testing.T) {
	env := newTestEnv(t)
	p := env.connect(t, proto.FeatureFS)
	p.stall.Store(true)
	cl := New(env.hub, env.devID)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := cl.Stat(ctx, "/"); !Offline(err) {
		t.Fatalf("got %v, want a timeout", err)
	}
	// The connection itself still answers pings, so it stays up.
	p.stall.Store(false)
	if _, err := cl.Stat(context.Background(), "/"); err != nil {
		t.Fatalf("after timeout: %v", err)
	}
}
