// Command pocketlink-sim pretends to be a phone, so the daemon can be exercised
// before the Android app exists.
//
//	pocketlink-sim pair 'pocketlink://pair?...'
//	pocketlink-sim run
//
// In run mode, each stdin line is sent to the desktop:
//
//	title | text       post a notification (prints its key)
//	/rm KEY            remove a notification
//	/clip TEXT         set the desktop clipboard
//	/send PATH         upload a file, resuming a previous partial upload
//	/sendcut PATH N    upload only the first N bytes, to test resuming
//	/call EVENT [NUMBER [NAME]]  report a phone call (ringing, talking, ended)
//
// Files offered by the desktop are downloaded to DIR/downloads.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/coder/websocket"

	"github.com/carnager/pocketlink/internal/pair"
	"github.com/carnager/pocketlink/internal/proto"
	"github.com/carnager/pocketlink/internal/xfer"
)

type server struct {
	Name  string   `json:"name"`
	FP    string   `json:"fp"`
	Addrs []string `json:"addrs"`
}

type client struct {
	dir  string
	name string
	hc   *http.Client // pinned to the desktop's certificate
	addr string       // address of the current connection
}

func main() {
	dir := flag.String("dir", filepath.Join(xdg.ConfigHome, "pocketlink-sim"), "state directory")
	name := flag.String("name", "pocketlink-sim", "device name")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pocketlink-sim [-dir DIR] [-name NAME] pair URI | run")
		flag.PrintDefaults()
	}
	flag.Parse()

	cert, err := pair.LoadOrCreateIdentity(*dir, *name)
	if err != nil {
		log.Fatal(err)
	}
	switch flag.Arg(0) {
	case "pair":
		err = doPair(cert, *dir, *name, flag.Arg(1))
	case "run":
		err = doRun(cert, *dir, *name)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func pinnedClient(cert tls.Certificate, fp string) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates:          []tls.Certificate{cert},
		InsecureSkipVerify:    true, // replaced by fingerprint pinning
		VerifyPeerCertificate: pair.PinnedVerifier(fp),
		MinVersion:            tls.VersionTLS13,
	}}}
}

func dial(ctx context.Context, hc *http.Client, addr string, q url.Values) (*websocket.Conn, error) {
	u := url.URL{Scheme: "wss", Host: addr, Path: "/ws", RawQuery: q.Encode()}
	c, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: hc})
	return c, err
}

func doPair(cert tls.Certificate, dir, name, uri string) error {
	offer, err := pair.ParseOffer(uri)
	if err != nil {
		return err
	}
	hc := pinnedClient(cert, offer.FP)
	q := url.Values{"pair": {offer.Token}, "name": {name}}
	var errs []error
	for _, addr := range offer.Addrs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := dial(ctx, hc, addr, q)
		if err == nil {
			// The server only sends hello once it has accepted us.
			_, _, err = c.Read(ctx)
			c.Close(websocket.StatusNormalClosure, "")
		}
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", addr, err))
			continue
		}
		data, _ := json.MarshalIndent(server{Name: offer.Name, FP: offer.FP, Addrs: offer.Addrs}, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "server.json"), data, 0o600); err != nil {
			return err
		}
		fmt.Printf("paired with %s via %s\n", offer.Name, addr)
		return nil
	}
	return errors.Join(errs...)
}

func doRun(cert tls.Certificate, dir, name string) error {
	data, err := os.ReadFile(filepath.Join(dir, "server.json"))
	if err != nil {
		return fmt.Errorf("not paired yet: %w", err)
	}
	var srv server
	if err := json.Unmarshal(data, &srv); err != nil {
		return err
	}
	cl := &client{dir: dir, name: name, hc: pinnedClient(cert, srv.FP)}

	// Unbuffered: while disconnected, stdin simply isn't read, so lines wait.
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	backoff := time.Second
	for {
		c, err := cl.connect(srv.Addrs)
		if err != nil {
			log.Printf("connect failed, retrying in %s: %v", backoff, err)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		log.Printf("connected to %s at %s", srv.Name, cl.addr)
		err = cl.session(c, lines)
		if errors.Is(err, io.EOF) {
			return nil
		}
		log.Printf("disconnected: %v", err)
	}
}

func (cl *client) connect(addrs []string) (*websocket.Conn, error) {
	var errs []error
	for _, addr := range addrs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := dial(ctx, cl.hc, addr, nil)
		cancel()
		if err == nil {
			cl.addr = addr
			return c, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", addr, err))
	}
	return nil, errors.Join(errs...)
}

func (cl *client) session(c *websocket.Conn, lines <-chan string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.CloseNow()

	hello, _ := proto.NewUnacked(proto.TypeHello, proto.Hello{Name: cl.name, Platform: "sim"})
	if err := write(ctx, c, hello); err != nil {
		return err
	}
	readErr := make(chan error, 1)
	go func() { readErr <- cl.readLoop(ctx, c) }()

	n := 0
	for {
		select {
		case err := <-readErr:
			return err
		case line, ok := <-lines:
			if !ok {
				c.Close(websocket.StatusNormalClosure, "bye")
				return io.EOF
			}
			e, err := cl.handleLine(line, &n)
			if err != nil {
				log.Print(err)
				continue
			}
			if e.Type == "" {
				continue
			}
			if err := write(ctx, c, e); err != nil {
				return err
			}
		}
	}
}

func (cl *client) readLoop(ctx context.Context, c *websocket.Conn) error {
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		var e proto.Envelope
		if err := json.Unmarshal(data, &e); err != nil {
			log.Printf("malformed frame: %v", err)
			continue
		}
		switch e.Type {
		case proto.TypeAck:
			log.Printf("acked %s", e.Ref)
		case proto.TypeHello:
			log.Printf("hello: %s", e.Body)
		case proto.TypeFileOffer:
			log.Printf("<- %s %s", e.Type, e.Body)
			if off, err := proto.Decode[proto.FileOffer](e); err == nil {
				go cl.fetchOffer(ctx, c, off)
			}
		default:
			log.Printf("<- %s %s", e.Type, e.Body)
		}
		if e.ID != "" {
			if err := write(ctx, c, proto.Ack(e.ID)); err != nil {
				return err
			}
		}
	}
}

// handleLine turns a stdin line into a frame to send, or performs an upload.
func (cl *client) handleLine(line string, n *int) (proto.Envelope, error) {
	line = strings.TrimSpace(line)
	cmd, arg, _ := strings.Cut(line, " ")
	switch {
	case line == "":
		return proto.Envelope{}, nil
	case cmd == "/clip":
		return proto.New(proto.TypeClipSet, proto.ClipSet{Text: arg})
	case cmd == "/rm":
		return proto.New(proto.TypeNotifRemoved, proto.NotifRemoved{Key: arg})
	case cmd == "/send":
		return proto.Envelope{}, cl.upload(arg, -1)
	case cmd == "/sendcut":
		path, nstr, _ := strings.Cut(arg, " ")
		limit, err := strconv.ParseInt(nstr, 10, 64)
		if err != nil {
			return proto.Envelope{}, fmt.Errorf("usage: /sendcut PATH BYTES")
		}
		return proto.Envelope{}, cl.upload(path, limit)
	case cmd == "/call":
		f := strings.SplitN(arg, " ", 3)
		ev := proto.CallState{Event: f[0], Time: time.Now().UnixMilli()}
		if len(f) > 1 {
			ev.Number = f[1]
		}
		if len(f) > 2 {
			ev.Name = f[2]
		}
		return proto.New(proto.TypeCallState, ev)
	case strings.HasPrefix(cmd, "/"):
		return proto.Envelope{}, fmt.Errorf("unknown command %q", cmd)
	}
	*n++
	key := fmt.Sprintf("sim-%d", *n)
	title, text, _ := strings.Cut(line, "|")
	log.Printf("posting notification %s", key)
	return proto.New(proto.TypeNotifPosted, proto.NotifPosted{
		Key: key, App: "dev.pocketlink.sim", AppName: "pocketlink-sim",
		Title: strings.TrimSpace(title), Text: strings.TrimSpace(text), Time: time.Now().UnixMilli(),
	})
}

// upload sends a file, continuing wherever a previous attempt stopped. If
// limit >= 0, it stops after that many bytes of the file.
func (cl *client) upload(path string, limit int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	id := transferID(path, fi)
	u := "https://" + cl.addr + "/files/in/" + id

	// Ask how much of this transfer already arrived.
	var offset int64
	resp, err := cl.hc.Head(u)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
	case http.StatusOK:
		if resp.Header.Get(xfer.HeaderComplete) != "" {
			log.Printf("upload %s: already complete", fi.Name())
			return nil
		}
		offset, _ = strconv.ParseInt(resp.Header.Get(xfer.HeaderOffset), 10, 64)
	default:
		return fmt.Errorf("upload %s: HEAD: %s", fi.Name(), resp.Status)
	}

	end := size
	if limit >= 0 && limit < end {
		end = limit
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, u, io.LimitReader(f, end-offset))
	if err != nil {
		return err
	}
	req.ContentLength = end - offset
	req.Header.Set(xfer.HeaderName, url.QueryEscape(fi.Name()))
	req.Header.Set(xfer.HeaderSize, strconv.FormatInt(size, 10))
	req.Header.Set(xfer.HeaderOffset, strconv.FormatInt(offset, 10))
	resp, err = cl.hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	log.Printf("upload %s: sent bytes %d-%d of %d: %s (server has %s, complete=%q)",
		fi.Name(), offset, end, size, resp.Status, resp.Header.Get(xfer.HeaderOffset), resp.Header.Get(xfer.HeaderComplete))
	return nil
}

// transferID is stable for an unchanged file, so an upload can be resumed
// even after the app restarts.
func transferID(path string, fi os.FileInfo) string {
	abs, _ := filepath.Abs(path)
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d", abs, fi.Size(), fi.ModTime().UnixNano()))
	return hex.EncodeToString(sum[:12])
}

// fetchOffer downloads an offered file, resuming a partial download, then
// tells the desktop it's done with the offer.
func (cl *client) fetchOffer(ctx context.Context, c *websocket.Conn, off proto.FileOffer) {
	if err := cl.download(ctx, off); err != nil {
		log.Printf("download %s: %v", off.Name, err)
		return
	}
	done, _ := proto.New(proto.TypeFileDone, proto.FileDone{ID: off.ID})
	if err := write(ctx, c, done); err != nil {
		log.Printf("sending file.done: %v", err)
	}
}

func (cl *client) download(ctx context.Context, off proto.FileOffer) error {
	dir := filepath.Join(cl.dir, "downloads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	part := filepath.Join(dir, off.ID+".part")
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	have, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+cl.addr+"/files/out/"+off.ID, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := cl.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		log.Printf("download %s: resuming at %d", off.Name, have)
	case http.StatusOK:
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	case http.StatusRequestedRangeNotSatisfiable:
		if have != off.Size {
			return fmt.Errorf("server rejected range at %d", have)
		}
	default:
		return fmt.Errorf("GET: %s", resp.Status)
	}
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		if _, err := io.Copy(f, resp.Body); err != nil {
			return err
		}
	}
	if fi, err := f.Stat(); err != nil || fi.Size() != off.Size {
		return fmt.Errorf("incomplete download")
	}
	dst := filepath.Join(dir, filepath.Base(off.Name))
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	log.Printf("download %s: saved to %s", off.Name, dst)
	return nil
}

func write(ctx context.Context, c *websocket.Conn, e proto.Envelope) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, data)
}
