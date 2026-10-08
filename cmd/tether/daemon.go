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
	"syscall"
	"time"

	"github.com/adrg/xdg"

	"tether/internal/clip"
	"tether/internal/conn"
	"tether/internal/control"
	"tether/internal/notify"
	"tether/internal/pair"
	"tether/internal/proto"
	"tether/internal/xfer"
)

const pairTTL = 5 * time.Minute

func runDaemon(args []string) error {
	hostname, _ := os.Hostname()
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	listen := fs.String("listen", ":1764", "address for phone connections")
	name := fs.String("name", hostname, "name shown on the phone")
	watchClip := fs.Bool("clipboard", true, "send desktop clipboard changes to phones")
	downloads := fs.String("downloads", defaultDownloads(), "directory for received files")
	fs.Parse(args)

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

	incoming := xfer.NewIncoming(filepath.Join(stateDir, "incoming"), *downloads, store, func(dev pair.Device, path string) {
		if err := sink.FileReceived(dev, path); err != nil {
			slog.Warn("file notification", "err", err)
		}
	})
	outgoing, err := xfer.OpenOutgoing(filepath.Join(stateDir, "outgoing.json"), store)
	if err != nil {
		return fmt.Errorf("loading outgoing files: %w", err)
	}
	go housekeeping(ctx, incoming, outgoing)

	pairing := &pair.Pairing{}
	hub := conn.NewHub(ctx, *name, store, pairing, filepath.Join(stateDir, "queue"), dispatch(sink, clipboard, outgoing))

	mux := http.NewServeMux()
	mux.Handle("GET /ws", hub)
	mux.HandleFunc("PUT /files/in/{id}", incoming.ServePUT)
	mux.HandleFunc("HEAD /files/in/{id}", incoming.ServeHEAD)
	mux.Handle("GET /files/out/{id}", outgoing)
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
			default:
				return nil, fmt.Errorf("unknown command %q", req.Cmd)
			}
		})
	}()
	if *watchClip {
		if exe, err := os.Executable(); err != nil {
			slog.Warn("clipboard sync disabled", "err", err)
		} else {
			go clip.Watch(ctx, exe)
		}
	}

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

func dispatch(sink *notify.Sink, clipboard *clip.Clipboard, outgoing *xfer.Outgoing) conn.Handler {
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

func defaultDownloads() string {
	if d := xdg.UserDirs.Download; d != "" {
		return d
	}
	return filepath.Join(xdg.Home, "Downloads")
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
