// Command tether is the desktop side of tether: a daemon that talks to
// paired phones, and a CLI that talks to the daemon.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdp/qrterminal/v3"
	"rsc.io/qr"

	"github.com/carnager/tether/internal/clip"
	"github.com/carnager/tether/internal/conn"
	"github.com/carnager/tether/internal/control"
	"github.com/carnager/tether/internal/pair"
)

const usage = `usage: tether <command> [args]

commands:
  daemon            run the daemon (normally via systemd --user)
  pair [-json] [-png FILE]
                    show a QR code for pairing a phone
  status [-json]    show paired devices
  unpair <device>   forget a device (ID prefix or name)
  clip [text]       send text, piped stdin, or else the desktop clipboard
                    to all phones' clipboards
  send [-to DEV] FILE...
                    send files to a phone (default: all paired phones)
  config [-json] [KEY VALUE]
                    show settings, or change one (name, listen, downloads,
                    clipboard, ring_action, talk_action, call_volume)
`

type statusReply struct {
	Name        string              `json:"name"`
	Fingerprint string              `json:"fingerprint"`
	Listen      string              `json:"listen"`
	Devices     []conn.DeviceStatus `json:"devices"`
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
	case "config":
		err = runConfig(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tether:", err)
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
	fmt.Printf("\nScan with the tether app, or pass to tether-sim:\n%s\n\nValid until %s.\n",
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

	fmt.Printf("%s listening on %s (fingerprint %s)\n\n", r.Name, r.Listen, pair.ShortID(r.Fingerprint))
	if len(r.Devices) == 0 {
		fmt.Println("No paired devices. Run `tether pair`.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tPENDING\tLAST SEEN")
	for _, d := range r.Devices {
		state, seen := "offline", "never"
		if d.Connected {
			state, seen = "connected", d.Addr
		} else if !d.LastSeen.IsZero() {
			seen = d.LastSeen.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", pair.ShortID(d.ID), d.Name, state, d.Pending, seen)
	}
	return tw.Flush()
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
		return errors.New("usage: tether config [-json] [KEY VALUE]")
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
		return errors.New("usage: tether unpair <device>")
	}
	return control.Call(control.Request{Cmd: "unpair", ID: args[0]}, nil)
}

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.String("to", "", "device ID prefix or name")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("usage: tether send [-to DEVICE] FILE...")
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
	if err := control.Call(control.Request{Cmd: "send", ID: *to, Paths: paths}, &sent); err != nil {
		return err
	}
	for _, name := range sent {
		fmt.Println("offered", name)
	}
	return nil
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func runClip(args []string) error {
	fs := flag.NewFlagSet("clip", flag.ExitOnError)
	watch := fs.Bool("watch", false, "internal: invoked by wl-paste --watch")
	fs.Parse(args)

	var text string
	if fs.NArg() > 0 {
		text = strings.Join(fs.Args(), " ")
	} else if stdinIsTerminal() {
		var err error
		if text, err = clip.Get(); err != nil {
			return err
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, clip.MaxText+1))
		if err != nil {
			return err
		}
		text = string(data)
	}

	if *watch {
		if text == "" || len(text) > clip.MaxText || clip.IsSensitive() {
			return nil
		}
	} else {
		if text == "" {
			return errors.New("nothing to send")
		}
		if len(text) > clip.MaxText {
			return fmt.Errorf("text larger than %d bytes", clip.MaxText)
		}
	}
	return control.Call(control.Request{Cmd: "clip", Text: text, Watch: *watch}, nil)
}
