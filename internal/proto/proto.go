// Package proto defines the wire format shared by the desktop daemon and
// phone clients. Every frame is one JSON-encoded Envelope sent as a
// WebSocket text message.
package proto

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	TypeReply        = "reply"
)

// Envelope is a single protocol frame. A frame with an ID must be
// acknowledged by the receiver with an ack frame whose Ref is that ID.
// Senders retransmit unacknowledged frames after reconnecting, so receivers
// must tolerate duplicates.
//
// A frame with Req set instead is a live request. It is never queued, acked
// or retransmitted: the receiver answers with exactly one reply frame whose
// Ref is the Req and which carries either a body or an Error. If the
// connection drops first, the request is lost and the caller fails.
type Envelope struct {
	V     int             `json:"v"`
	ID    string          `json:"id,omitempty"`
	Req   string          `json:"req,omitempty"`
	Type  string          `json:"type"`
	Ref   string          `json:"ref,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
	Error *Error          `json:"error,omitempty"`
}

// Features a peer lists in its hello.
const (
	FeatureFS      = "fs"       // answers fs.* requests
	FeatureFSWatch = "fs.watch" // sends fs.changed for listed directories
)

type Hello struct {
	Name     string   `json:"name"`
	Platform string   `json:"platform,omitempty"`
	Features []string `json:"features,omitempty"`
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
	Name   string `json:"name,omitempty"`  // contact name, if known
	Photo  []byte `json:"photo,omitempty"` // contact photo (JPEG/PNG, small), if any
	Time   int64  `json:"time"`            // unix millis on the phone
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

// NewRequest builds a live request frame.
func NewRequest(typ string, body any) (Envelope, error) {
	e, err := NewUnacked(typ, body)
	e.Req = rand.Text()
	return e, err
}

// Reply builds the successful answer to a request.
func Reply(req string, body any) (Envelope, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{V: Version, Type: TypeReply, Ref: req, Body: raw}, nil
}

// ErrorReply builds the failed answer to a request.
func ErrorReply(req, code, msg string) Envelope {
	return Envelope{V: Version, Type: TypeReply, Ref: req, Error: &Error{Code: code, Message: msg}}
}

// MaxPayload bounds the payload of one binary frame.
const MaxPayload = 512 << 10

// EncodeBinary builds a binary WebSocket message: a 4-byte big-endian
// length of the JSON envelope, the envelope, then the raw payload. Frames
// that carry bulk data (file contents) use this instead of a text message.
func EncodeBinary(e Envelope, payload []byte) ([]byte, error) {
	hdr, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4, 4+len(hdr)+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(hdr)))
	out = append(out, hdr...)
	return append(out, payload...), nil
}

// DecodeBinary splits a binary WebSocket message into envelope and payload.
func DecodeBinary(data []byte) (Envelope, []byte, error) {
	var e Envelope
	if len(data) < 4 {
		return e, nil, errors.New("binary frame too short")
	}
	n := binary.BigEndian.Uint32(data)
	if uint64(n) > uint64(len(data)-4) {
		return e, nil, errors.New("binary frame header overruns message")
	}
	if err := json.Unmarshal(data[4:4+n], &e); err != nil {
		return e, nil, err
	}
	return e, data[4+n:], nil
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
