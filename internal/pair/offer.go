package pair

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Offer is what the desktop shows as a QR code: where to reach it, which
// certificate to pin, and a one-time token that authorizes the phone's
// certificate to be added to the trust store.
type Offer struct {
	Addrs []string
	FP    string
	Token string
	Name  string
}

func (o Offer) URI() string {
	v := url.Values{}
	v.Set("a", strings.Join(o.Addrs, ","))
	v.Set("fp", o.FP)
	v.Set("t", o.Token)
	v.Set("n", o.Name)
	return "tether://pair?" + v.Encode()
}

func ParseOffer(s string) (Offer, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Offer{}, err
	}
	if u.Scheme != "tether" || u.Host != "pair" {
		return Offer{}, errors.New("not a tether pairing URI")
	}
	q := u.Query()
	o := Offer{FP: q.Get("fp"), Token: q.Get("t"), Name: q.Get("n")}
	if a := q.Get("a"); a != "" {
		o.Addrs = strings.Split(a, ",")
	}
	if len(o.Addrs) == 0 || o.FP == "" || o.Token == "" {
		return Offer{}, errors.New("incomplete pairing URI")
	}
	return o, nil
}

// Pairing holds the single currently valid pairing token, if any.
type Pairing struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}

// Start issues a new token, invalidating any previous one.
func (p *Pairing) Start(ttl time.Duration) (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = rand.Text()
	p.expires = time.Now().Add(ttl)
	return p.token, p.expires
}

// Consume reports whether tok is the current unexpired token, and if so
// invalidates it so it can only be used once.
func (p *Pairing) Consume(tok string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token == "" || tok == "" || time.Now().After(p.expires) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(p.token)) != 1 {
		return false
	}
	p.token = ""
	return true
}
