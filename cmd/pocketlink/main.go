// Command pocketlink is the desktop side of pocketlink: a daemon that talks to
// paired phones, and a CLI that talks to the daemon.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdp/qrterminal/v3"
	"rsc.io/qr"

	"github.com/carnager/pocketlink/internal/clip"
	"github.com/carnager/pocketlink/internal/conn"
	"github.com/carnager/pocketlink/internal/control"
	"github.com/carnager/pocketlink/internal/notify"
	"github.com/carnager/pocketlink/internal/pair"
	"github.com/carnager/pocketlink/internal/proto"
)

const usage = `usage: pocketlink <command> [args]

commands:
  daemon            run the daemon (normally via systemd --user)
  pair [-json] [-png FILE]
                    show a QR code for pairing a phone
  status [-json]    show paired devices
  unpair <device>   forget a device (ID prefix or name)
  clip [text]       send text, piped stdin, or else the desktop clipboard
                    to all phones' clipboards
  send [-to DEV] [-notify] FILE|FOLDER...
                    send files and folders to a phone (default: all paired
                    phones); -notify reports the result as a notification
  link [-to DEV] [-notify] URL
                    open a link in a phone's browser (default: all paired
                    phones); offline phones open it when they connect
  find [-to DEV] [-stop]
                    make a phone ring loudly, even on silent (default: all
                    connected phones); -stop ends it
  settings [-print]  open the settings page in the browser (or print its address)
  config [-json] [KEY VALUE]
                    show settings, or change one (name, listen, downloads,
                    clipboard, files, file_manager, ring_action,
                    talk_action, call_volume)

Phones' files: open webdav://127.0.0.1:1765/ in Dolphin, or
dav://127.0.0.1:1765/ in Thunar or Nautilus (see the files setting).
`

type statusReply struct {
	Name        string              `json:"name"`
	Fingerprint string              `json:"fingerprint"`
	Listen      string              `json:"listen"`
	Files       string              `json:"files,omitempty"` // WebDAV URL for phones' files
	Devices     []conn.DeviceStatus `json:"devices"`
}

type settingsReply struct {
	URL string `json:"url"`
}

type pairReply struct {
	URI     string    `json:"uri"`
	Expires time.Time `json:"expires"`
	PNG     string    `json:"png,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "daemon":
		err = runDaemon(args)
	case "pair":
		err = runPair(args)
	case "status":
		err = runStatus(args)
	case "unpair":
		err = runUnpair(args)
	case "clip":
		err = runClip(args)
	case "send":
		err = runSend(args)
	case "link":
		err = runLink(args)
	case "find":
		err = runFind(args)
	case "config":
		err = runConfig(args)
	case "settings":
		err = runSettings(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pocketlink:", err)
		os.Exit(1)
	}
}

func runPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON instead of a terminal QR code")
	png := fs.String("png", "", "also write the QR code as a PNG image to `FILE`")
	fs.Parse(args)

	var r pairReply
	if err := control.Call(control.Request{Cmd: "pair"}, &r); err != nil {
		return err
	}
	if *png != "" {
		code, err := qr.Encode(r.URI, qr.L)
		if err != nil {
			return err
		}
		code.Scale = 8
		if err := os.WriteFile(*png, code.PNG(), 0o600); err != nil {
			return err
		}
		r.PNG = *png
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	qrterminal.GenerateHalfBlock(r.URI, qrterminal.L, os.Stdout)
	fmt.Printf("\nScan with the pocketlink app, or pass to pocketlink-sim:\n%s\n\nValid until %s.\n",
		r.URI, r.Expires.Format("15:04:05"))
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)

	var r statusReply
	if err := control.Call(control.Request{Cmd: "status"}, &r); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}

	fmt.Printf("%s listening on %s (fingerprint %s)\n", r.Name, r.Listen, pair.ShortID(r.Fingerprint))
	if host, ok := strings.CutPrefix(r.Files, "http://"); ok {
		fmt.Printf("Phones' files: webdav://%s (Dolphin), dav://%s (Thunar, Nautilus)\n", host, host)
	}
	fmt.Println()
	if len(r.Devices) == 0 {
		fmt.Println("No paired devices. Run `pocketlink pair`.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tBATTERY\tPENDING\tLAST SEEN")
	for _, d := range r.Devices {
		state, seen := "offline", "never"
		if d.Connected {
			state, seen = "connected", d.Addr
		} else if !d.LastSeen.IsZero() {
			seen = d.LastSeen.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", pair.ShortID(d.ID), d.Name, state, batteryText(d.Battery), d.Pending, seen)
	}
	return tw.Flush()
}

func batteryText(b *proto.Battery) string {
	switch {
	case b == nil:
		return "-"
	case b.Charging:
		return fmt.Sprintf("%d%%, charging", b.Level)
	default:
		return fmt.Sprintf("%d%%", b.Level)
	}
}

func runFind(args []string) error {
	fs := flag.NewFlagSet("find", flag.ExitOnError)
	to := fs.String("to", "", "device ID prefix or name")
	stop := fs.Bool("stop", false, "stop the ringing")
	fs.Parse(args)
	req := control.Request{Cmd: "find", ID: *to}
	if *stop {
		req.Value = "stop"
	}
	var rang []string
	err := control.Call(req, &rang)
	for _, name := range rang {
		if *stop {
			fmt.Println("stopped", name)
		} else {
			fmt.Println("ringing", name)
		}
	}
	return err
}

func runSettings(args []string) error {
	fs := flag.NewFlagSet("settings", flag.ExitOnError)
	print := fs.Bool("print", false, "print the page's address instead of opening it")
	fs.Parse(args)
	var r settingsReply
	err := control.Call(control.Request{Cmd: "settings"}, &r)
	if *print || err != nil {
		if err == nil {
			fmt.Println(r.URL)
		} else if !isTerminal() {
			notify.Notice("dialog-error", "pocketlink settings", err.Error())
		}
		return err
	}
	if err := openBrowser(r.URL); err != nil {
		// From a launcher there's no terminal to print to. The address
		// stays off the clipboard: clipboard sync would send it to phones.
		if !isTerminal() {
			notify.Notice("dialog-information", "Open pocketlink settings in your browser", r.URL)
		}
		fmt.Printf("Couldn't open a browser (%v).\nOpen this address in one:\n%s\n\nSet a default browser with xdg-settings set default-web-browser NAME.desktop, or set $BROWSER.\n", err, r.URL)
		return nil
	}
	if isTerminal() {
		fmt.Println("Opened", r.URL)
	}
	return nil
}

// openBrowser tries $BROWSER (a colon-separated list, by convention), then
// xdg-open. A program that exits with an error within a few seconds
// failed; one still running by then, or done without error, opened it.
func openBrowser(url string) error {
	var cmds [][]string
	for _, b := range strings.Split(os.Getenv("BROWSER"), ":") {
		if f := strings.Fields(b); len(f) > 0 {
			cmds = append(cmds, append(f, url))
		}
	}
	cmds = append(cmds, []string{"xdg-open", url})

	var errs []error
	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		if err := cmd.Start(); err != nil {
			errs = append(errs, err)
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil {
				return nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", c[0], err))
		case <-time.After(3 * time.Second):
			return nil // still running: a browser that stays in front
		}
	}
	return errors.Join(errs...)
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func runConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)

	var r configReply
	var err error
	switch fs.NArg() {
	case 0:
		err = control.Call(control.Request{Cmd: "config"}, &r)
	case 2:
		err = control.Call(control.Request{Cmd: "set", Key: fs.Arg(0), Value: fs.Arg(1)}, &r)
	default:
		return errors.New("usage: pocketlink config [-json] [KEY VALUE]")
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	c := r.Config
	fmt.Fprintf(tw, "name\t%s\nlisten\t%s\ndownloads\t%s\nclipboard\t%t\nring_action\t%s\ntalk_action\t%s\ncall_volume\t%d\n",
		c.Name, c.Listen, c.Downloads, c.Clipboard, c.RingAction, c.TalkAction, c.CallVolume)
	tw.Flush()
	if r.Restart {
		fmt.Println("\nRestart the daemon for this change to take effect.")
	}
	return nil
}

func runUnpair(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: pocketlink unpair <device>")
	}
	return control.Call(control.Request{Cmd: "unpair", ID: args[0]}, nil)
}

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.String("to", "", "device ID prefix or name")
	notice := fs.Bool("notify", false, "report the result as a desktop notification (for file manager actions)")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("usage: pocketlink send [-to DEVICE] [-notify] FILE|FOLDER...")
	}
	paths := make([]string, fs.NArg())
	for i, p := range fs.Args() {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		paths[i] = abs
	}
	var sent []string
	err := control.Call(control.Request{Cmd: "send", ID: *to, Paths: paths}, &sent)
	if *notice {
		notifySent(*to, sent, err)
	}
	if err != nil {
		return err
	}
	for _, name := range sent {
		fmt.Println("offered", name)
	}
	return nil
}

// notifySent reports a send as a desktop notification. Files are offered
// even to a phone that's offline; it fetches them when it connects.
func notifySent(to string, sent []string, err error) {
	target := to
	if target == "" {
		var st statusReply
		if control.Call(control.Request{Cmd: "status"}, &st) == nil && len(st.Devices) == 1 {
			target = st.Devices[0].Name
		} else {
			target = "your phones"
		}
	}
	files := strings.Join(sent, ", ")
	if len(sent) > 3 {
		files = fmt.Sprintf("%s and %d more", strings.Join(sent[:3], ", "), len(sent)-3)
	}
	var nerr error
	switch {
	case err != nil && len(sent) > 0:
		nerr = notify.Notice("dialog-warning", "Sent only some files to "+target, files+"\n"+err.Error())
	case err != nil:
		nerr = notify.Notice("dialog-error", "Could not send to "+target, err.Error())
	default:
		nerr = notify.Notice("document-send", "Sending to "+target, files)
	}
	if nerr != nil {
		fmt.Fprintln(os.Stderr, "pocketlink: notification:", nerr)
	}
}

func runLink(args []string) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	to := fs.String("to", "", "device ID prefix or name")
	notice := fs.Bool("notify", false, "report the result as a desktop notification")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: pocketlink link [-to DEVICE] [-notify] URL")
	}
	link := strings.TrimSpace(fs.Arg(0))
	var sent []string
	err := control.Call(control.Request{Cmd: "link", ID: *to, Text: link}, &sent)
	if *notice {
		var nerr error
		switch {
		case err != nil && len(sent) > 0:
			nerr = notify.Notice("dialog-warning", "Sent the link only to "+strings.Join(sent, ", "), err.Error())
		case err != nil:
			nerr = notify.Notice("dialog-error", "Could not send the link", err.Error())
		default:
			nerr = notify.Notice("web-browser", "Opening on "+strings.Join(sent, ", "), link)
		}
		if nerr != nil {
			fmt.Fprintln(os.Stderr, "pocketlink: notification:", nerr)
		}
	}
	for _, name := range sent {
		fmt.Println("sent to", name)
	}
	return err
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func runClip(args []string) error {
	fs := flag.NewFlagSet("clip", flag.ExitOnError)
	watch := fs.Bool("watch", false, "internal: invoked by wl-paste --watch")
	fs.Parse(args)

	switch {
	case *watch:
		// wl-paste writes the selection to us; drain it so it doesn't fail
		// writing, then read it in whatever form we sync.
		io.Copy(io.Discard, os.Stdin)
		return sendSelection(true)
	case fs.NArg() > 0:
		return sendText(strings.Join(fs.Args(), " "), false)
	case stdinIsTerminal():
		return sendSelection(false)
	default:
		data, err := io.ReadAll(io.LimitReader(os.Stdin, clip.MaxText+1))
		if err != nil {
			return err
		}
		return sendText(string(data), false)
	}
}

// sendSelection sends the desktop clipboard, as an image if that's all it
// holds, otherwise as text. From the watcher (watch), anything unsuitable
// is skipped quietly.
func sendSelection(watch bool) error {
	types := clip.Types()
	if clip.IsSensitive(types) {
		if watch {
			return nil
		}
		return errors.New("the clipboard is marked as a password, not sending it")
	}
	if mime := clip.ImageType(types); mime != "" {
		data, err := clip.GetImage(mime)
		switch {
		case err != nil && watch, len(data) == 0 && watch, len(data) > clip.MaxImage && watch:
			return nil
		case err != nil:
			return err
		case len(data) > clip.MaxImage:
			return fmt.Errorf("image larger than %d MB", clip.MaxImage>>20)
		}
		return control.Call(control.Request{Cmd: "clip-image", Mime: mime, Data: data, Watch: watch}, nil)
	}
	text, err := clip.Get()
	if err != nil {
		if watch {
			return nil
		}
		return err
	}
	return sendText(text, watch)
}

func sendText(text string, watch bool) error {
	switch {
	case (text == "" || len(text) > clip.MaxText) && watch:
		return nil
	case text == "":
		return errors.New("nothing to send")
	case len(text) > clip.MaxText:
		return fmt.Errorf("text larger than %d bytes", clip.MaxText)
	}
	return control.Call(control.Request{Cmd: "clip", Text: text, Watch: watch}, nil)
}
