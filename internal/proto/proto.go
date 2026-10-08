// Package proto defines the wire format shared by the desktop daemon and
// phone clients. Every frame is one JSON-encoded Envelope sent as a
// WebSocket text message.
package proto

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
)

const Version = 1

const (
	TypeHello        = "hello"
	TypeAck          = "ack"
	TypeNotifPosted  = "notif.posted"
	TypeNotifRemoved = "notif.removed"
	TypeClipSet      = "clip.set"
	TypeClipImage    = "clip.image"
	TypeFileOffer    = "file.offer"
	TypeFileDone     = "file.done"
	TypeMediaState   = "media.state"
	TypeMediaCmd     = "media.cmd"
	TypeCallState    = "call.state"
	TypeCallMute     = "call.mute"
)

// Envelope is a single protocol frame. A frame with an ID must be
// acknowledged by the receiver with an ack frame whose Ref is that ID.
// Senders retransmit unacknowledged frames after reconnecting, so receivers
// must tolerate duplicates.
type Envelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id,omitempty"`
	Type string          `json:"type"`
	Ref  string          `json:"ref,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
}

type Hello struct {
	Name     string `json:"name"`
	Platform string `json:"platform,omitempty"`
}

type NotifPosted struct {
	Key     string `json:"key"`      // stable per notification; reused for updates and removal
	App     string `json:"app"`      // package name, e.g. org.thoughtcrime.securesms
	AppName string `json:"app_name"` // display name, e.g. Signal
	Title   string `json:"title"`
	Text    string `json:"text"`
	Time    int64  `json:"time"` // unix millis
}

type NotifRemoved struct {
	Key string `json:"key"`
}

type ClipSet struct {
	Text string `json:"text"`
}

// CallState reports a phone call. Event is "ringing" (incoming), "talking"
// (answered, or an outgoing call) or "ended".
type CallState struct {
	Event  string `json:"event"`
	Number string `json:"number,omitempty"`
	Name   string `json:"name,omitempty"` // contact name, if known
	Time   int64  `json:"time"`           // unix millis on the phone
}

// ClipImage tells the phone the desktop clipboard holds an image, to be
// fetched from GET /clip/{id}. Phones send images to the desktop clipboard
// with PUT /clip, the image's type as Content-Type.
type ClipImage struct {
	ID   string `json:"id"`
	Mime string `json:"mime"`
	Size int    `json:"size"`
}

// FileOffer tells the phone a file is ready at GET /files/out/{id}.
type FileOffer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Mime string `json:"mime"`
}

// FileDone tells the desktop the phone has finished with an offer, whether
// it downloaded the file or declined it.
type FileDone struct {
	ID string `json:"id"`
}

// New builds a frame that the receiver must acknowledge.
func New(typ string, body any) (Envelope, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{V: Version, ID: rand.Text(), Type: typ, Body: raw}, nil
}

// NewUnacked builds a fire-and-forget frame, used for per-connection
// messages such as hello.
func NewUnacked(typ string, body any) (Envelope, error) {
	e, err := New(typ, body)
	e.ID = ""
	return e, err
}

func Ack(id string) Envelope {
	return Envelope{V: Version, Type: TypeAck, Ref: id}
}

func Decode[T any](e Envelope) (T, error) {
	var v T
	if err := json.Unmarshal(e.Body, &v); err != nil {
		return v, fmt.Errorf("decode %s: %w", e.Type, err)
	}
	return v, nil
}

// Coalesces reports whether a newer pending frame of this type makes older
// pending frames of the same type obsolete.
func Coalesces(typ string) bool {
	return typ == TypeClipSet || typ == TypeClipImage || typ == TypeMediaState || typ == TypeCallState
}
