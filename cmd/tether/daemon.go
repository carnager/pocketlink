package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adrg/xdg"

	"tether/internal/clip"
	"tether/internal/config"
	"tether/internal/conn"
	"tether/internal/control"
	"tether/internal/media"
	"tether/internal/notify"
	"tether/internal/pair"
	"tether/internal/proto"
	"tether/internal/xfer"
)

const pairTTL = 5 * time.Minute

func runDaemon(args []string) error {
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
		fmt.Fprintf(fs.Output(), "usage: tether daemon [flags]\n\nFlags override %s for this run.\n\n", cfgPath)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	// Flags given on the command line are not written back by `tether config`.
	overridden := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { overridden[f.Name] = true })

	name, listen := &cfg.Name, &cfg.Listen
	host, port, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	cfgDir := filepath.Join(xdg.ConfigHome, "tether")
	stateDir := filepath.Join(xdg.StateHome, "tether")

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
	clipboard := &clip.Clipboard{}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	incoming := xfer.NewIncoming(filepath.Join(stateDir, "incoming"), cfg.Downloads, store, func(dev pair.Device, path string) {
		if err := sink.FileReceived(dev, path); err != nil {
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

	pairing := &pair.Pairing{}
	var phoneCalls *calls // set below, before any connection is accepted
	hub = conn.NewHub(ctx, *name, store, pairing, filepath.Join(stateDir, "queue"), dispatch(sink, clipboard, outgoing, players, &phoneCalls))
	// A phone that just connected (or restarted) needs the current state,
	// which may not have changed since it last got it.
	hub.OnConnect = func(dev pair.Device) {
		if err := hub.Send(dev.ID, proto.TypeMediaState, players.State()); err != nil {
			slog.Warn("sending media state", "err", err)
		}
	}
	go players.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("GET /ws", hub)
	mux.HandleFunc("PUT /files/in/{id}", incoming.ServePUT)
	mux.HandleFunc("HEAD /files/in/{id}", incoming.ServeHEAD)
	mux.Handle("GET /files/out/{id}", outgoing)
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
	}}
	phoneCalls = newCalls(settings, players, sink, hub)

	errc := make(chan error, 2)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	go func() {
		errc <- control.Serve(ctx, control.SocketPath(), func(req control.Request) (any, error) {
			switch req.Cmd {
			case "pair":
				tok, exp := pairing.Start(pairTTL)
				offer := pair.Offer{Addrs: localAddrs(host, port), FP: fp, Token: tok, Name: *name}
				return pairReply{URI: offer.URI(), Expires: exp}, nil
			case "status":
				return statusReply{Name: *name, Fingerprint: fp, Listen: *listen, Devices: hub.Status()}, nil
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
			case "send":
				return sendFiles(hub, store, outgoing, req)
			case "config":
				return settings.get(), nil
			case "set":
				return settings.set(req.Key, req.Value)
			default:
				return nil, fmt.Errorf("unknown command %q", req.Cmd)
			}
		})
	}()

	slog.Info("tether running", "name", *name, "listen", *listen, "fingerprint", pair.ShortID(fp))

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func dispatch(sink *notify.Sink, clipboard *clip.Clipboard, outgoing *xfer.Outgoing, players *media.Watcher, phoneCalls **calls) conn.Handler {
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

	var sent []string
	for _, p := range req.Paths {
		off, err := outgoing.Add(p, devs)
		if err != nil {
			return sent, err
		}
		body := proto.FileOffer{ID: off.ID, Name: off.Name, Size: off.Size, Mime: off.Mime}
		for _, d := range devs {
			if err := hub.Send(d, proto.TypeFileOffer, body); err != nil {
				return sent, err
			}
		}
		sent = append(sent, off.Name)
	}
	return sent, nil
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

// settingsState is the live configuration, changed via `tether config`.
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
