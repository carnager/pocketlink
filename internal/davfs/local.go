package davfs

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
)

// Listen listens on a loopback address and accepts only connections from
// processes of the current user, which it looks up in /proc/net/tcp. File
// managers can't send a client certificate, and this way bookmarks need no
// password, while other users on the machine still can't get at the phone.
func Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if !IsLoopback(host) {
		return nil, fmt.Errorf("%s is not a loopback address", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return sameUser{ln}, nil
}

// IsLoopback reports whether host names this machine only.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type sameUser struct{ net.Listener }

func (l sameUser) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err == nil && uid == os.Getuid() {
			return c, nil
		}
		slog.Warn("refused file access connection", "from", c.RemoteAddr(), "uid", uid, "err", err)
		c.Close()
	}
}

// peerUID finds the user owning the other end of a loopback TCP connection:
// the socket whose local address is our remote one.
func peerUID(c net.Conn) (int, error) {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 || !remote.IP.IsLoopback() {
		return -1, errors.New("not a loopback TCP connection")
	}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		uid, err := findUID(table, remote, local)
		if err == nil {
			return uid, nil
		}
		if !errors.Is(err, errNoSocket) {
			return -1, err
		}
	}
	return -1, errNoSocket
}

var errNoSocket = errors.New("peer socket not found")

func findUID(table string, local, remote *net.TCPAddr) (int, error) {
	f, err := os.Open(table)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid ...
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 {
			continue
		}
		l, err1 := parseProcAddr(fields[1])
		r, err2 := parseProcAddr(fields[2])
		if err1 != nil || err2 != nil || !sameAddr(l, local) || !sameAddr(r, remote) {
			continue
		}
		return strconv.Atoi(fields[7])
	}
	if err := sc.Err(); err != nil {
		return -1, err
	}
	return -1, errNoSocket
}

func sameAddr(a, b *net.TCPAddr) bool {
	return a.Port == b.Port && a.IP.Equal(b.IP)
}

// parseProcAddr parses an address as /proc/net/tcp prints it: the IP in hex
// as 32-bit words in host (little-endian) order, then ":" and the port.
func parseProcAddr(s string) (*net.TCPAddr, error) {
	ipHex, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("bad address %q", s)
	}
	raw, err := hex.DecodeString(ipHex)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, fmt.Errorf("bad address %q", s)
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return nil, fmt.Errorf("bad port in %q", s)
	}
	return &net.TCPAddr{IP: net.IP(raw), Port: int(port)}, nil
}
