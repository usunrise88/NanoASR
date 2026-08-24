package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// stubResolver answers whatever a test says, so the address check can be
// exercised without depending on DNS.
type stubResolver map[string][]net.IP

func (s stubResolver) LookupNetIP(_ context.Context, _, host string) ([]net.IP, error) {
	ips, ok := s[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return ips, nil
}

func job() core.Job {
	return core.Job{
		ID:     "job_1",
		Status: core.JobSucceeded,
		Result: &core.Result{Text: "привет мир"},
	}
}

// --- the address check ------------------------------------------------------

func TestAddressCheckRefusesEverythingButPublicHTTPS(t *testing.T) {
	resolver := stubResolver{
		"internal.example": {net.ParseIP("10.1.2.3")},
		"metadata.example": {net.ParseIP("169.254.169.254")},
		"public.example":   {net.ParseIP("93.184.216.34")},
		"v6only.example":   {net.ParseIP("::1")},
		"mapped.example":   {net.ParseIP("::ffff:127.0.0.1")},
		"nothing.example":  {},
		// One public answer and one private one is the standard way past a
		// check that stops at the head of the list.
		"mixed.example": {net.ParseIP("93.184.216.34"), net.ParseIP("192.168.0.5")},
	}

	refused := []struct {
		name string
		url  string
	}{
		{"plain http", "http://public.example/hook"},
		{"loopback literal", "https://127.0.0.1/hook"},
		{"loopback name", "https://v6only.example/hook"},
		{"rfc1918 literal", "https://10.0.0.1/hook"},
		{"rfc1918 name", "https://internal.example/hook"},
		{"cloud metadata", "https://metadata.example/hook"},
		{"link-local literal", "https://169.254.169.254/latest/meta-data/"},
		{"cgnat", "https://100.64.1.1/hook"},
		{"ipv4-mapped ipv6", "https://mapped.example/hook"},
		{"mixed answers", "https://mixed.example/hook"},
		{"unresolvable", "https://absent.example/hook"},
		{"resolves to nothing", "https://nothing.example/hook"},
		{"no host", "https:///hook"},
		{"not a url", "https://exa mple/hook"},
		{"ftp", "ftp://public.example/hook"},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			if err := checkURL(context.Background(), resolver, c.url, false); err == nil {
				t.Errorf("checkURL(%q) allowed it", c.url)
			}
		})
	}

	for _, allowed := range []string{
		"https://public.example/hook",
		"https://93.184.216.34/hook",
		"https://public.example:8443/hook?x=1",
	} {
		if err := checkURL(context.Background(), resolver, allowed, false); err != nil {
			t.Errorf("checkURL(%q) = %v, want it allowed", allowed, err)
		}
	}
}

// A developer's receiver on localhost is a real case, so the escape hatch
// exists — but only as an explicit setting, never as a fallback.
func TestAllowPrivateSkipsTheCheckButNotTheScheme(t *testing.T) {
	ctx := context.Background()
	if err := checkURL(ctx, stubResolver{}, "https://127.0.0.1:9000/hook", true); err != nil {
		t.Errorf("allowPrivate did not permit loopback: %v", err)
	}
	if err := checkURL(ctx, stubResolver{}, "http://127.0.0.1:9000/hook", true); err == nil {
		t.Error("allowPrivate permitted plain http")
	}
}

// --- delivery ---------------------------------------------------------------

// receiver is a TLS webhook endpoint whose behaviour a test controls.
type receiver struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	statuses []int
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	r := &receiver{statuses: statuses}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)

		r.mu.Lock()
		n := len(r.requests)
		r.requests = append(r.requests, req)
		r.bodies = append(r.bodies, body)
		status := http.StatusOK
		if n < len(r.statuses) {
			status = r.statuses[n]
		} else if len(r.statuses) > 0 {
			status = r.statuses[len(r.statuses)-1]
		}
		r.mu.Unlock()

		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// sender wires a Sender to a local TLS receiver: AllowPrivate because the
// receiver is on loopback, and the test server's own client for its certificate.
func sender(t *testing.T, rec *receiver, secret string) *Sender {
	t.Helper()
	s := New(Options{Secret: secret, AllowPrivate: true, Client: rec.srv.Client()})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Close(ctx)
	})
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDeliverPostsTheJobAndSignsIt(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	s := sender(t, rec, "s3cret")

	s.Notify(job(), rec.srv.URL+"/hook")
	waitFor(t, "the delivery", func() bool { return rec.count() == 1 })

	rec.mu.Lock()
	req, body := rec.requests[0], rec.bodies[0]
	rec.mu.Unlock()

	var got core.Job
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not a job: %v", err)
	}
	if got.ID != "job_1" || got.Result == nil || got.Result.Text != "привет мир" {
		t.Errorf("delivered %+v", got)
	}
	if req.Header.Get("X-NanoASR-Delivery") != "job_1" {
		t.Errorf("delivery header = %q", req.Header.Get("X-NanoASR-Delivery"))
	}

	timestamp := req.Header.Get("X-NanoASR-Timestamp")
	want := "sha256=" + Sign("s3cret", timestamp, body)
	if got := req.Header.Get("X-NanoASR-Signature"); got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
}

// Signing the body alone would let anyone who saw one delivery replay it
// forever, so the timestamp has to be inside the signed material.
func TestSignatureCoversTheTimestamp(t *testing.T) {
	body := []byte(`{"id":"job_1"}`)
	if Sign("k", "100", body) == Sign("k", "200", body) {
		t.Fatal("the signature ignores the timestamp")
	}
	if Sign("k", "100", body) == Sign("other", "100", body) {
		t.Fatal("the signature ignores the secret")
	}
}

func TestNoSecretMeansNoSignatureHeader(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	s := sender(t, rec, "")

	s.Notify(job(), rec.srv.URL+"/hook")
	waitFor(t, "the delivery", func() bool { return rec.count() == 1 })

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := rec.requests[0].Header.Get("X-NanoASR-Signature"); got != "" {
		t.Errorf("unsigned delivery carried %q", got)
	}
}

func TestDeliveryRetriesAServerError(t *testing.T) {
	rec := newReceiver(t, http.StatusInternalServerError, http.StatusOK)
	s := New(Options{AllowPrivate: true, Client: rec.srv.Client()})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Close(ctx)
	}()

	s.Notify(job(), rec.srv.URL+"/hook")
	waitFor(t, "the retry", func() bool { return rec.count() == 2 })
}

// A 4xx is the receiver saying the request itself is wrong. Sending it again
// unchanged cannot help, and doing so three times is just noise on their end.
func TestDeliveryDoesNotRetryAClientError(t *testing.T) {
	rec := newReceiver(t, http.StatusBadRequest)
	s := sender(t, rec, "")

	s.Notify(job(), rec.srv.URL+"/hook")
	waitFor(t, "the delivery", func() bool { return rec.count() == 1 })

	time.Sleep(1500 * time.Millisecond) // past the first backoff
	if n := rec.count(); n != 1 {
		t.Errorf("attempted %d times, want 1", n)
	}
}

// Checking the address and then following a redirect to wherever it points is
// the same as not checking at all.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var reached bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer target.Close()

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/hook", http.StatusFound)
	}))
	defer redirector.Close()

	rec := &receiver{srv: redirector}
	s := New(Options{AllowPrivate: true, Client: redirector.Client()})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Close(ctx)
	}()

	s.Notify(job(), rec.srv.URL+"/hook")
	time.Sleep(300 * time.Millisecond)
	if reached {
		t.Fatal("the delivery followed a redirect")
	}
}

func TestNotifyIgnoresAnEmptyURL(t *testing.T) {
	s := New(Options{})
	s.Notify(job(), "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNotifyDoesNotBlockTheCaller(t *testing.T) {
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer slow.Close()

	s := New(Options{AllowPrivate: true, Client: slow.Client()})
	start := time.Now()
	s.Notify(job(), slow.URL+"/hook")
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Notify blocked for %v", elapsed)
	}

	// Close abandons the delivery rather than waiting out the receiver.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = s.Close(ctx)
}

func TestDeliveryToARefusedAddressNeverConnects(t *testing.T) {
	var reached bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer target.Close()

	// AllowPrivate off: the receiver is on loopback and must be refused.
	s := New(Options{Client: target.Client()})
	s.Notify(job(), target.URL+"/hook")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Close(ctx)

	if reached {
		t.Fatal("a loopback webhook was delivered")
	}
}
