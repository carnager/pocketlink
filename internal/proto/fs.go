package proto

import "io/fs"

// File access on the phone. The desktop sends these as live requests to a
// phone whose hello lists FeatureFS.
//
// Paths are slash-separated and rooted at the phone's shared storage: "/" is
// the storage root, "/DCIM/Camera" a folder in it. The phone must refuse any
// path that resolves outside the root.
//
// Every request is self-contained; there are no open file handles, so a
// dropped connection leaves nothing behind on the phone.
const (
	TypeFSStat    = "fs.stat"    // FSPath → FSEntry
	TypeFSList    = "fs.list"    // FSPath → FSList
	TypeFSRead    = "fs.read"    // FSRead → empty body, file data as payload
	TypeFSWrite   = "fs.write"   // FSWrite + data as payload → FSEntry
	TypeFSCreate  = "fs.create"  // FSCreate → FSEntry
	TypeFSMkdir   = "fs.mkdir"   // FSPath → FSEntry
	TypeFSRemove  = "fs.remove"  // FSRemove → empty body
	TypeFSRename  = "fs.rename"  // FSRename → empty body
	TypeFSSetAttr = "fs.setattr" // FSSetAttr → FSEntry
	TypeFSStatFS  = "fs.statfs"  // FSPath → FSSpace

	// Streams (FeatureFSStream), for sequential bulk transfers. The data
	// goes over its own HTTPS connection to /stream/{req} rather than in
	// frames, like file transfers do.
	TypeFSReadStream  = "fs.readstream"  // FSStream → phone PUTs the file from Offset to its end, then replies (empty body)
	TypeFSWriteStream = "fs.writestream" // FSStream → phone GETs the data and writes it at Offset into an existing file, then replies FSEntry

	// TypeFSChanged (FSChanged) is a plain unacked frame from the phone,
	// not a request.
	TypeFSChanged = "fs.changed"
)

// FSChanged tells the desktop that something in directory Path changed:
// an entry appeared, vanished or was modified. A phone whose hello lists
// FeatureFSWatch watches each directory it has recently listed and sends
// this, so the desktop can cache listings for longer. Notifications can be
// lost with the connection; the desktop forgets its cache on reconnect.
type FSChanged struct {
	Path string `json:"path"`
}

// FSEntry describes a file or directory. Name is the last path element
// ("" for the root).
type FSEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir,omitempty"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix millis
}

type FSPath struct {
	Path string `json:"path"`
}

// FSList is a directory's entries, without "." and "..", in no particular
// order.
type FSList struct {
	Entries []FSEntry `json:"entries"`
}

// FSRead asks for up to Length (≤ MaxPayload) bytes at Offset. A payload
// shorter than Length means the read reached the end of the file.
type FSRead struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

// FSWrite writes the payload (≤ MaxPayload bytes) at Offset into an
// existing file, extending it if needed. The reply is the file afterwards.
type FSWrite struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

// FSStream names where a stream starts.
type FSStream struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

// FSCreate creates an empty file; its parent must exist. An existing file
// is kept, emptied if Truncate is set, or refused with ErrExist if
// Exclusive is set.
type FSCreate struct {
	Path      string `json:"path"`
	Exclusive bool   `json:"exclusive,omitempty"`
	Truncate  bool   `json:"truncate,omitempty"`
}

// FSRemove deletes a file or an empty directory, or a directory with its
// contents if Recursive is set.
type FSRemove struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// FSRename moves From to To, replacing an existing file (or empty
// directory) at To unless NoReplace is set.
type FSRename struct {
	From      string `json:"from"`
	To        string `json:"to"`
	NoReplace bool   `json:"no_replace,omitempty"`
}

// FSSetAttr changes a file's size (truncating or extending it) and/or its
// modification time; nil fields are left alone.
type FSSetAttr struct {
	Path  string `json:"path"`
	Size  *int64 `json:"size,omitempty"`
	MTime *int64 `json:"mtime,omitempty"` // unix millis
}

// FSSpace is the size of the volume holding a path, in bytes.
type FSSpace struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
}

// Error codes in a reply's Error.
const (
	ErrNotFound    = "notfound"
	ErrExist       = "exists"
	ErrNotDir      = "notdir"
	ErrIsDir       = "isdir"
	ErrNotEmpty    = "notempty"
	ErrPerm        = "perm" // also: the user hasn't granted file access
	ErrInvalid     = "invalid"
	ErrNoSpace     = "nospace"
	ErrIO          = "io"
	ErrUnsupported = "unsupported" // unknown request type
)

// Error is a failed request's reason.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Is lets errors.Is match the io/fs sentinels, e.g. fs.ErrNotExist.
func (e *Error) Is(target error) bool {
	switch target {
	case fs.ErrNotExist:
		return e.Code == ErrNotFound
	case fs.ErrExist:
		return e.Code == ErrExist
	case fs.ErrPermission:
		return e.Code == ErrPerm
	case fs.ErrInvalid:
		return e.Code == ErrInvalid
	}
	return false
}
