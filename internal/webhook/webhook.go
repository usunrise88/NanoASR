package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Delivery limits. A webhook is a courtesy, not part of the transcription, so it
// gets a short leash: a slow endpoint must not hold a worker or a connection.
const (
	requestTimeout = 10 * time.Second
	maxAttempts    = 3
	// maxBodyBytes is what is read from the receiver's response purely to keep
	// the connection reusable; the content is discarded.
	maxBodyBytes = 4 << 10
)

// backoff is the wait before attempt n+1. Wide gaps on purpose: a receiver that
// just failed is usually still failing a second later.
var backoff = []time.Duration{time.Second, 5 * time.Second, 25 * time.Second}

// Options configures a Sender.
type Options struct {
	// Secret signs the payload. Empty means unsigned, which the caller is
	// warned about at startup rather than here.
	Secret string
	// AllowPrivate disables the address check. It exists for a developer whose
	// receiver is on localhost, and config.Validate refuses it off loopback.
	AllowPrivate bool
	Logger       *slog.Logger
	Resolver     Resolver
	// Client is substituted by tests. Its redirect policy is replaced either
	// way: see New.
	Client *http.Client
}

// Sender delivers finished jobs.
type Sender struct {
	opt    Options
	client *http.Client
	log    *slog.Logger

	wg sync.WaitGroup
	// stop makes Close stop retrying rather than wait out a backoff.
	stop chan struct{}
	once sync.Once
}

func New(opt Options) *Sender {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Resolver == nil {
		opt.Resolver = netResolver{}
	}

	client := opt.Client
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout: 5 * time.Second,
			},
		}
	}
	client.Timeout = requestTimeout
	// Redirects are refused outright. Checking the address and then following
	// a 302 to wherever it points is the same as not checking at all.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Sender{opt: opt, client: client, log: opt.Logger, stop: make(chan struct{})}
}

// Notify delivers a job in the background. It never blocks the caller: the queue
// worker that finished this job has other work waiting.
func (s *Sender) Notify(job core.Job, url string) {
	if url == "" {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.deliver(job, url); err != nil {
			// A failed webhook is not a failed job. The transcript is stored
			// and fetchable; say what happened and move on.
			s.log.Warn("webhook delivery failed", "job", job.ID, "err", err)
		}
	}()
}

func (s *Sender) deliver(job core.Job, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := checkURL(ctx, s.opt.Resolver, url, s.opt.AllowPrivate); err != nil {
		return err
	}

	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("encode job: %w", err)
	}

	var last error
	for attempt := range maxAttempts {
		if attempt > 0 {
			select {
			case <-time.After(backoff[attempt-1]):
			case <-s.stop:
				return fmt.Errorf("shutting down after %d attempts: %w", attempt, last)
			case <-ctx.Done():
				return fmt.Errorf("gave up after %d attempts: %w", attempt, last)
			}
		}
		status, err := s.post(ctx, url, body, job.ID, attempt+1)
		if err == nil {
			return nil
		}
		last = err
		// 4xx other than 429 is the receiver saying the request itself is
		// wrong. Repeating it unchanged cannot help.
		if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
			return err
		}
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, last)
}

func (s *Sender) post(ctx context.Context, url string, body []byte, jobID string, attempt int) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nanoasr-webhook/1")
	req.Header.Set("X-NanoASR-Delivery", jobID)
	req.Header.Set("X-NanoASR-Attempt", strconv.Itoa(attempt))
	req.Header.Set("X-NanoASR-Timestamp", timestamp)
	if s.opt.Secret != "" {
		req.Header.Set("X-NanoASR-Signature", "sha256="+Sign(s.opt.Secret, timestamp, body))
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, maxBodyBytes)

	// The redirect policy turns a 3xx into a returned response rather than a
	// followed hop, so it has to be rejected here rather than silently counted
	// as success.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("receiver answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// Sign is the HMAC a receiver verifies.
//
// The timestamp is inside the signed material, not beside it: signing the body
// alone lets anyone who once saw a delivery replay it forever.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Close waits for deliveries in flight, abandoning retries once ctx expires.
func (s *Sender) Close(ctx context.Context) error {
	s.once.Do(func() { close(s.stop) })

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
