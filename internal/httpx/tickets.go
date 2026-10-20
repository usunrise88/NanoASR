package httpx

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Tickets let a browser fetch one resource without a bearer token.
//
// An <audio src> cannot send an Authorization header, so a page that wants to
// play a recording as it downloads — rather than fetch all of it first and
// play the copy — needs a URL that authenticates itself. The API key cannot go
// in that URL: the access log, the browser history and any proxy on the way
// would all keep it. A ticket is what goes instead: the path it is for, the key
// it was issued to by id, when it expires, and a signature over the three. It
// opens that one path, for reading, for a few hours, for the key that asked
// for it, and nothing else.
//
// The secret is made when the process starts and never stored: a restart
// voids every ticket, which costs a page a reload of its links and keeps
// nothing on disk that could open a recording.
type Tickets struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// TicketParam is the query parameter a ticket travels in.
const TicketParam = "ticket"

// NewTickets makes a signer with a fresh secret. ttl is how long a ticket is
// good for.
func NewTickets(ttl time.Duration) *Tickets {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &Tickets{secret: secret, ttl: ttl, now: time.Now}
}

// Sign returns path with a ticket for the caller, and when it expires.
func (t *Tickets) Sign(caller core.Caller, path string) (string, time.Time) {
	exp := t.now().Add(t.ttl).Truncate(time.Second)
	payload := strconv.FormatInt(exp.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString([]byte(caller.KeyID))
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + TicketParam + "=" + url.QueryEscape(payload+"."+t.mac(path, payload)), exp
}

func (t *Tickets) mac(path, payload string) string {
	m := hmac.New(sha256.New, t.secret)
	m.Write([]byte(path))
	m.Write([]byte{0})
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// verify returns the key id a request's ticket was issued to, if the ticket
// is for this path, unexpired and genuine. Only reads are opened by one.
func (t *Tickets) verify(r *http.Request) (string, bool) {
	if t == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return "", false
	}
	ticket := r.URL.Query().Get(TicketParam)
	parts := strings.Split(ticket, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload := parts[0] + "." + parts[1]
	// The path is what was signed: the ticket opens the resource it was made
	// for, whatever else the query asks of it.
	if !hmac.Equal([]byte(parts[2]), []byte(t.mac(r.URL.Path, payload))) {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || t.now().After(time.Unix(exp, 0)) {
		return "", false
	}
	id, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	return string(id), true
}
