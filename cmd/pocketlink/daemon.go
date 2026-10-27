package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adrg/xdg"

	"github.com/carnager/pocketlink/internal/clip"
	"github.com/carnager/pocketlink/internal/config"
	"github.com/carnager/pocketlink/internal/conn"
	"github.com/carnager/pocketlink/internal/control"
	"github.com/carnager/pocketlink/internal/davfs"
	"github.com/carnager/pocketlink/internal/media"
	"github.com/carnager/pocketlink/internal/notify"
	"github.com/carnager/pocketlink/internal/pair"
	"github.com/carnager/pocketlink/internal/proto"
	"github.com/carnager/pocketlink/internal/webui"
	"github.com/carnager/pocketlink/internal/xfer"
)

const pairTTL = 5 * time.Minute

func runDaemon(args []string) error {
	if err := config.MigrateLegacyDirs(); err != nil {
		slog.Warn("migrating old config", "err", err)
	}
	cfgPath := config.Path()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "address for phone connections")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "name shown on the phone")
	fs.BoolVar(&cfg.Clipboard, "clipboard", cfg.Clipboard, "send desktop clipboard changes to phones")
	fs.StringVar(&cfg.Downloads, "downloads", cfg.Downloads, "directory for received files")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: pocketlink daemon [flags]\n\nFlags override %s for this run.\n\n", cfgPath)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	// Flags given on the command line are not written back by `pocketlink config`.
	overridden := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { overridden[f.Name] = true })

	name, listen := &cfg.Name, &cfg.Listen
	host, port, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	cfgDir := filepath.Join(xdg.ConfigHome, "pocketlink")
	stateDir := filepath.Join(xdg.StateHome, "pocketlink")

	cert, err := pair.LoadOrCreateIdentity(cfgDir, *name)
	if err != nil {
		return fmt.Errorf("loading identity: %w", err)
	}
	fp := pair.Fingerprint(cert.Certificate[0])
	store, err := pair.OpenStore(filepath.Join(cfgDir, "devices.json"))
	if err != nil {
		return fmt.Errorf("loading devices: %w", err)
	}
	sink, err := notify.New()
	if err != nil {
		return fmt.Errorf("connecting to session bus: %w", err)
	}
	sink.SetFileManager(cfg.FileManager)
	clipboard := &clip.Clipboard{}
	images := &clip.Images{}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	incoming := xfer.NewIncoming(filepath.Join(stateDir, "incoming"), cfg.Downloads, store, func(dev pair.Device, path string) {
		// Images, e.g. a screenshot shared from the phone, can go straight
		// to the clipboard from the notification.
		var onCopy func() error
		if mime := clip.ImageFile(path); mime != "" {
			onCopy = func() error {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				return clipboard.SetImage(mime, data)
			}
		}
		if err := sink.FileReceived(dev, path, onCopy); err != nil {
			slog.Warn("file notification", "err", err)
		}
	})
	outgoing, err := xfer.OpenOutgoing(filepath.Join(stateDir, "outgoing.json"), store)
	if err != nil {
		return fmt.Errorf("loading outgoing files: %w", err)
	}
	go housekeeping(ctx, incoming, outgoing)

	var hub *conn.Hub
	players, err := media.New(func(st media.State) {
		if err := hub.Broadcast(proto.TypeMediaState, st); err != nil {
			slog.Warn("sending media state", "err", err)
		}
	})
	if err != nil {
		return fmt.Errorf("media: %w", err)
	}

	batteries := &batteryLevels{levels: map[string]proto.Battery{}}
	pairing := &pair.Pairing{}
	var phoneCalls *calls // set below, before any connection is accepted
	var files *davfs.Server
	hub = conn.NewHub(ctx, *name, store, pairing, filepath.Join(stateDir, "queue"), dispatch(sink, clipboard, outgoing, players, batteries, &phoneCalls, &files))
	files = davfs.New(hub, store)
	// A phone that just connected (or restarted) needs the current state,
	// which may not have changed since it last got it.
	hub.OnConnect = func(dev pair.Device) {
		// It may have missed telling us about changed files.
		files.Reset(dev.ID)
		if err := hub.Send(dev.ID, proto.TypeMediaState, players.State()); err != nil {
			slog.Warn("sending media state", "err", err)
		}
	}
	go players.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("GET /ws", hub)
	mux.HandleFunc("PUT "+proto.StreamPath+"{req}", hub.ServeStream)
	mux.HandleFunc("GET "+proto.StreamPath+"{req}", hub.ServeStream)
	mux.HandleFunc("PUT /files/in/{id}", incoming.ServePUT)
	mux.HandleFunc("HEAD /files/in/{id}", incoming.ServeHEAD)
	mux.Handle("GET /files/out/{id}", outgoing)
	mux.HandleFunc("GET /clip/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := store.Peer(r.TLS); !ok {
			http.Error(w, "not paired", http.StatusForbidden)
			return
		}
		mime, data, ok := images.Get(r.PathValue("id"))
		if !ok {
			http.Error(w, "clipboard has changed", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Write(data)
	})
	mux.HandleFunc("PUT /clip", func(w http.ResponseWriter, r *http.Request) {
		dev, ok := store.Peer(r.TLS)
		if !ok {
			http.Error(w, "not paired", http.StatusForbidden)
			return
		}
		mime := r.Header.Get("Content-Type")
		if !strings.HasPrefix(mime, "image/") {
			http.Error(w, "only images", http.StatusUnsupportedMediaType)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, clip.MaxImage+1))
		if err != nil || len(data) > clip.MaxImage {
			http.Error(w, "image too large or incomplete", http.StatusRequestEntityTooLarge)
			return
		}
		if err := clipboard.SetImage(mime, data); err != nil {
			slog.Warn("setting clipboard image", "err", err)
			http.Error(w, "could not set clipboard", http.StatusInternalServerError)
			return
		}
		slog.Info("clipboard image from phone", "device", dev.Name, "type", mime, "bytes", len(data))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /media/art/{key}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := store.Peer(r.TLS); !ok {
			http.Error(w, "not paired", http.StatusForbidden)
			return
		}
		data, err := players.Art(r.Context(), r.PathValue("key"))
		if err != nil {
			http.Error(w, "no art", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", http.DetectContentType(data))
		w.Write(data)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			// Any certificate is accepted at the TLS layer; the hub checks the
			// fingerprint against the trust store or a pairing token.
			ClientAuth: tls.RequireAnyClientCert,
			MinVersion: tls.VersionTLS13,
		},
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}

	watcher := &clipWatcher{ctx: ctx}
	watcher.enable(cfg.Clipboard)
	settings := &settingsState{path: cfgPath, cfg: cfg, overridden: overridden, apply: func(c config.Config) {
		incoming.SetDestDir(c.Downloads)
		watcher.enable(c.Clipboard)
		sink.SetFileManager(c.FileManager)
	}}
	phoneCalls = newCalls(settings, players, sink, hub)

	// Browsing phones' files from file managers, over WebDAV on localhost.
	var filesURL string
	filesSrv := &http.Server{Handler: files, ReadHeaderTimeout: 10 * time.Second}
	if cfg.Files != "off" {
		if fln, err := davfs.Listen(cfg.Files); err != nil {
			slog.Warn("phone file access disabled", "err", err)
		} else {
			filesURL = "http://" + fln.Addr().String() + "/"
			go func() {
				if err := filesSrv.Serve(fln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Warn("phone file access stopped", "err", err)
				}
			}()
		}
	}

	errc := make(chan error, 2)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	var page *webui.Page
	handle := func(req control.Request) (any, error) {
		switch req.Cmd {
		case "pair":
			tok, exp := pairing.Start(pairTTL)
			offer := pair.Offer{Addrs: localAddrs(host, port), FP: fp, Token: tok, Name: *name}
			return pairReply{URI: offer.URI(), Expires: exp}, nil
		case "status":
			devs := hub.Status()
			for i := range devs {
				if filesURL != "" {
					devs[i].Files = strings.TrimSuffix(filesURL, "/") + files.Folder(devs[i].ID)
				}
				if devs[i].Connected {
					devs[i].Battery = batteries.get(devs[i].ID)
				}
			}
			return statusReply{Name: *name, Fingerprint: fp, Listen: *listen, Files: filesURL, Devices: devs}, nil
		case "unpair":
			id, err := hub.Resolve(req.ID)
			if err != nil {
				return nil, err
			}
			if err := outgoing.ForgetDevice(id); err != nil {
				slog.Warn("withdrawing offers", "err", err)
			}
			return nil, hub.Forget(id)
		case "clip":
			// Manual pushes always go out; watcher events are dropped if
			// they only echo what a phone just sent us.
			if !clipboard.Changed(req.Text) && req.Watch {
				return nil, nil
			}
			return nil, hub.Broadcast(proto.TypeClipSet, proto.ClipSet{Text: req.Text})
		case "clip-image":
			if !clipboard.Changed(string(req.Data)) && req.Watch {
				return nil, nil
			}
			id := images.Put(req.Mime, req.Data)
			return nil, hub.Broadcast(proto.TypeClipImage, proto.ClipImage{ID: id, Mime: req.Mime, Size: len(req.Data)})
		case "send":
			return sendFiles(hub, store, outgoing, req)
		case "link":
			return sendLink(hub, store, req)
		case "find":
			return findPhones(ctx, hub, store, req)
		case "config":
			return settings.get(), nil
		case "set":
			return settings.set(req.Key, req.Value)
		case "settings":
			url, err := page.Open()
			if err != nil {
				return nil, fmt.Errorf("starting the settings page: %w", err)
			}
			return settingsReply{URL: url}, nil
		default:
			return nil, fmt.Errorf("unknown command %q", req.Cmd)
		}
	}
	// The settings page answers through the same commands as the CLI, and
	// runs only while it's used.
	page = webui.NewPage(handle)
	defer page.Close()
	go func() {
		errc <- control.Serve(ctx, control.SocketPath(), handle)
	}()

	slog.Info("pocketlink running", "name", *name, "listen", *listen, "fingerprint", pair.ShortID(fp))

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	filesSrv.Shutdown(shutdownCtx)
	return srv.Shutdown(shutdownCtx)
}

func dispatch(sink *notify.Sink, clipboard *clip.Clipboard, outgoing *xfer.Outgoing, players *media.Watcher, batteries *batteryLevels, phoneCalls **calls, files **davfs.Server) conn.Handler {
	return func(dev pair.Device, e proto.Envelope) error {
		switch e.Type {
		case proto.TypeNotifPosted:
			n, err := proto.Decode[proto.NotifPosted](e)
			if err != nil {
				return err
			}
			return sink.Posted(dev, n)
		case proto.TypeNotifRemoved:
			n, err := proto.Decode[proto.NotifRemoved](e)
			if err != nil {
				return err
			}
			return sink.Removed(dev, n.Key)
		case proto.TypeClipSet:
			c, err := proto.Decode[proto.ClipSet](e)
			if err != nil {
				return err
			}
			return clipboard.Set(c.Text)
		case proto.TypeBattery:
			b, err := proto.Decode[proto.Battery](e)
			if err != nil {
				return err
			}
			batteries.set(dev.ID, b)
			return nil
		case proto.TypeLinkOpen:
			l, err := proto.Decode[proto.LinkOpen](e)
			if err != nil {
				return err
			}
			if !webURL(l.URL) {
				slog.Warn("ignoring link that isn't http(s)", "device", dev.Name, "url", l.URL)
				return nil
			}
			slog.Info("opening link from phone", "device", dev.Name, "url", l.URL)
			// Browsers that aren't running yet take a moment to start; don't
			// hold up the connection meanwhile.
			go func() {
				if err := openBrowser(l.URL); err != nil {
					slog.Warn("opening link", "err", err)
					notify.Notice("dialog-information", "Link from "+dev.Name, l.URL)
				}
			}()
			return nil
		case proto.TypeFileDone:
			d, err := proto.Decode[proto.FileDone](e)
			if err != nil {
				return err
			}
			return outgoing.Done(dev.ID, d.ID)
		case proto.TypeCallState:
			ev, err := proto.Decode[proto.CallState](e)
			if err != nil {
				return err
			}
			return (*phoneCalls).handle(dev, ev)
		case proto.TypeFSChanged:
			c, err := proto.Decode[proto.FSChanged](e)
			if err != nil {
				return err
			}
			(*files).Changed(dev.ID, c.Path)
			return nil
		case proto.TypeMediaCmd:
			c, err := proto.Decode[media.Command](e)
			if err != nil {
				return err
			}
			return players.Command(c)
		default:
			slog.Debug("ignoring unknown frame type", "type", e.Type)
			return nil
		}
	}
}

// sendFiles offers files to one device (req.ID) or all paired devices.
// Several files or folders go as one tar offer to phones that unpack them,
// which is much faster for many files; others get one offer per file.
func sendFiles(hub *conn.Hub, store *pair.Store, outgoing *xfer.Outgoing, req control.Request) (any, error) {
	var devs []string
	if req.ID != "" {
		id, err := hub.Resolve(req.ID)
		if err != nil {
			return nil, err
		}
		devs = []string{id}
	} else {
		for _, d := range store.List() {
			devs = append(devs, d.ID)
		}
	}
	if len(devs) == 0 {
		return nil, errors.New("no paired devices")
	}
	if len(req.Paths) == 0 {
		return nil, errors.New("no files given")
	}

	folders := false
	for _, p := range req.Paths {
		if fi, err := os.Stat(p); err != nil {
			return nil, err
		} else if fi.IsDir() {
			folders = true
		}
	}
	var tarDevs, plainDevs []string
	for _, id := range devs {
		d, _ := store.Get(id)
		if (len(req.Paths) > 1 || folders) && slices.Contains(d.Features, proto.FeatureFilesTar) {
			tarDevs = append(tarDevs, id)
		} else {
			plainDevs = append(plainDevs, id)
		}
	}

	var sent []string
	if len(tarDevs) > 0 {
		name, entries, files, err := xfer.Collect(req.Paths)
		if err != nil {
			return nil, err
		}
		off, err := outgoing.AddTar(name, entries, files, tarDevs)
		if err != nil {
			return nil, err
		}
		body := proto.FileOffer{ID: off.ID, Name: off.Name, Size: off.Size, Mime: off.Mime, Files: off.Files}
		for _, d := range tarDevs {
			if err := hub.Send(d, proto.TypeFileOffer, body); err != nil {
				return nil, err
			}
		}
		label := fmt.Sprintf("%d files", files)
		if files == 1 {
			label = "1 file"
		}
		if name != "" {
			label = name + " (" + label + ")"
		}
		sent = append(sent, label)
	}
	if len(plainDevs) == 0 {
		return sent, nil
	}
	if folders {
		return sent, errors.New("folders can't be sent to a phone whose app is older than 0.5; update it")
	}
	for _, p := range req.Paths {
		off, err := outgoing.Add(p, plainDevs)
		if err != nil {
			return sent, err
		}
		body := proto.FileOffer{ID: off.ID, Name: off.Name, Size: off.Size, Mime: off.Mime}
		for _, d := range plainDevs {
			if err := hub.Send(d, proto.TypeFileOffer, body); err != nil {
				return sent, err
			}
		}
		if len(tarDevs) == 0 {
			sent = append(sent, off.Name)
		}
	}
	return sent, nil
}

// sendLink queues a link for one device (req.ID) or all paired devices
// whose app opens links. Offline phones open it when they next connect.
func sendLink(hub *conn.Hub, store *pair.Store, req control.Request) (any, error) {
	if !webURL(req.Text) {
		return nil, errors.New("only http:// and https:// links can be sent")
	}
	var devs []pair.Device
	if req.ID != "" {
		id, err := hub.Resolve(req.ID)
		if err != nil {
			return nil, err
		}
		d, _ := store.Get(id)
		devs = []pair.Device{d}
	} else {
		devs = store.List()
	}
	if len(devs) == 0 {
		return nil, errors.New("no paired devices")
	}
	var sent, old []string
	for _, d := range devs {
		if !slices.Contains(d.Features, proto.FeatureLinkOpen) {
			old = append(old, d.Name)
			continue
		}
		if err := hub.Send(d.ID, proto.TypeLinkOpen, proto.LinkOpen{URL: req.Text}); err != nil {
			return sent, err
		}
		sent = append(sent, d.Name)
	}
	if len(old) > 0 {
		return sent, fmt.Errorf("%s can't open links; update the app", strings.Join(old, ", "))
	}
	return sent, nil
}

// findPhones rings one device (req.ID) or every connected one, or with
// req.Value "stop" ends the ringing. Only connected phones can ring.
func findPhones(ctx context.Context, hub *conn.Hub, store *pair.Store, req control.Request) (any, error) {
	var devs []pair.Device
	if req.ID != "" {
		id, err := hub.Resolve(req.ID)
		if err != nil {
			return nil, err
		}
		d, _ := store.Get(id)
		devs = []pair.Device{d}
	} else {
		for _, d := range store.List() {
			if hub.Connected(d.ID) {
				devs = append(devs, d)
			}
		}
		if len(devs) == 0 {
			return nil, errors.New("no phone is connected")
		}
	}
	var rang []string
	var errs []error
	for _, d := range devs {
		switch {
		case !hub.Connected(d.ID):
			errs = append(errs, fmt.Errorf("%s is not connected", d.Name))
		case !hub.Supports(d.ID, proto.FeatureFind):
			errs = append(errs, fmt.Errorf("%s can't ring; update the app", d.Name))
		default:
			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			_, err := hub.Call(cctx, d.ID, proto.TypeFind, proto.Find{Stop: req.Value == "stop"}, nil)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", d.Name, err))
			} else {
				rang = append(rang, d.Name)
			}
		}
	}
	return rang, errors.Join(errs...)
}

// batteryLevels holds what each phone last said about its battery.
type batteryLevels struct {
	mu     sync.Mutex
	levels map[string]proto.Battery
}

func (b *batteryLevels) set(id string, v proto.Battery) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.levels[id] = v
}

func (b *batteryLevels) get(id string) *proto.Battery {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.levels[id]; ok {
		return &v
	}
	return nil
}

// webURL reports whether s is an absolute http or https URL, the only kind
// passed to a browser: other schemes could open local files or programs.
func webURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

const offerMaxAge = 7 * 24 * time.Hour

func housekeeping(ctx context.Context, incoming *xfer.Incoming, outgoing *xfer.Outgoing) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		incoming.Cleanup()
		if err := outgoing.Expire(offerMaxAge); err != nil {
			slog.Warn("expiring offers", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// settingsState is the live configuration, changed via `pocketlink config`.
type settingsState struct {
	path       string
	overridden map[string]bool
	apply      func(config.Config)

	mu  sync.Mutex
	cfg config.Config
}

type configReply struct {
	Config     config.Config `json:"config"`
	Restart    bool          `json:"restart,omitempty"`    // a changed setting applies after a restart
	Overridden []string      `json:"overridden,omitempty"` // settings fixed by daemon flags
}

func (s *settingsState) get() configReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := configReply{Config: s.cfg}
	for _, k := range config.Keys {
		if s.overridden[k] {
			r.Overridden = append(r.Overridden, k)
		}
	}
	return r
}

func (s *settingsState) set(key, value string) (configReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overridden[key] {
		return configReply{}, fmt.Errorf("%s is set by a daemon flag; remove the flag to change it here", key)
	}
	next := s.cfg
	if err := next.Set(key, value); err != nil {
		return configReply{}, err
	}
	// Save what's on disk plus this one change, so flag overrides of other
	// settings don't leak into the file.
	onDisk, err := config.Load(s.path)
	if err != nil {
		return configReply{}, err
	}
	if err := onDisk.Set(key, value); err != nil {
		return configReply{}, err
	}
	if err := onDisk.Save(s.path); err != nil {
		return configReply{}, err
	}
	s.cfg = next
	s.apply(next)
	slog.Info("setting changed", "key", key, "value", value)
	return configReply{Config: next, Restart: config.NeedsRestart(key)}, nil
}

// clipWatcher runs clip.Watch while clipboard sync is enabled.
type clipWatcher struct {
	ctx    context.Context
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (w *clipWatcher) enable(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if on == (w.cancel != nil) {
		return
	}
	if !on {
		w.cancel()
		w.cancel = nil
		return
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("clipboard sync disabled", "err", err)
		return
	}
	ctx, cancel := context.WithCancel(w.ctx)
	w.cancel = cancel
	go clip.Watch(ctx, exe)
}

// localAddrs lists the addresses a phone could reach us on: the listen
// address if it's specific, otherwise all IPv4 interface addresses,
// including VPNs such as Tailscale or WireGuard.
func localAddrs(host, port string) []string {
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return []string{net.JoinHostPort(host, port)}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || isVirtualBridge(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip := ipn.IP.To4(); ip != nil && !ip.IsLinkLocalUnicast() {
				out = append(out, net.JoinHostPort(ip.String(), port))
			}
		}
	}
	return out
}

func isVirtualBridge(name string) bool {
	for _, p := range []string{"docker", "veth", "br-", "virbr", "podman", "cni"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
