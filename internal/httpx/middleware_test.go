package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func authedServer(t *testing.T, public ...string) http.Handler {
	t.Helper()
	store := newStore(t,
		KeySpec{Name: "app", Secret: goodKey},
		KeySpec{Name: "ci", Secret: adminKey, Admin: true})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/ui/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/uisecret", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(APIKeyID(r.Context())))
	})
	mux.Handle("/v1/admin", RequireAdmin(nil)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))

	return Chain(mux, Auth(store, public...))
}

func request(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthProtectsTheAPI(t *testing.T) {
	h := authedServer(t, "/healthz", "/readyz", "/ui")

	cases := []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{"no credential", "/v1/models", "", http.StatusUnauthorized},
		{"wrong credential", "/v1/models", "sk-not-a-real-key-0123", http.StatusUnauthorized},
		{"valid credential", "/v1/models", goodKey, http.StatusOK},
		// A readiness probe cannot present a credential.
		{"health probe", "/healthz", "", http.StatusOK},
		// A browser does not send a bearer token for a script tag.
		{"ui asset", "/ui/app.js", "", http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := request(t, h, c.path, c.token).Code; got != c.want {
				t.Errorf("status = %d, want %d", got, c.want)
			}
		})
	}
}

// The exemption is a path segment, not a string prefix: "/ui" must not open
// "/uisecret".
func TestPublicPrefixMatchesOnSegmentBoundary(t *testing.T) {
	h := authedServer(t, "/ui")

	if got := request(t, h, "/uisecret", "").Code; got != http.StatusUnauthorized {
		t.Errorf("/uisecret status = %d, want 401 — the prefix leaked past a segment boundary", got)
	}
	if got := request(t, h, "/ui", "").Code; got == http.StatusUnauthorized {
		t.Error("/ui itself should be exempt")
	}
}

// A prefix of "/" (or one that trims to it) would match every request and
// silently disable authentication; it must be ignored.
func TestRootPublicPrefixExemptsNothing(t *testing.T) {
	h := authedServer(t, "/")

	if got := request(t, h, "/ui", "").Code; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — a root public prefix must exempt nothing", got)
	}
	if got := request(t, h, "/v1/models", "").Code; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — a root public prefix must exempt nothing", got)
	}
}

func TestAuthChallengesWithBearerScheme(t *testing.T) {
	h := authedServer(t)
	w := request(t, h, "/v1/models", "")

	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("a 401 must carry WWW-Authenticate so clients know what to send")
	}
}

func TestAuthPutsKeyIdentityInTheContext(t *testing.T) {
	h := authedServer(t)
	w := request(t, h, "/v1/models", goodKey)

	if w.Body.Len() == 0 {
		t.Fatal("the handler saw no key id: requests cannot be attributed")
	}
}

func TestRequireAdminSeparatesReadFromAdminister(t *testing.T) {
	h := authedServer(t)

	if got := request(t, h, "/v1/admin", goodKey).Code; got != http.StatusForbidden {
		t.Errorf("non-admin key on an admin route = %d, want 403", got)
	}
	if got := request(t, h, "/v1/admin", adminKey).Code; got != http.StatusOK {
		t.Errorf("admin key on an admin route = %d, want 200", got)
	}
}

// Open mode installs no Auth middleware at all, so nothing restricts a request
// that never passed through it.
func TestIsAdminDefaultsToTrueWithoutAuthentication(t *testing.T) {
	handler := RequireAdmin(nil)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: open mode has no keys and no restrictions", w.Code)
	}
}

func TestBearerParsing(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"BEARER  abc": "abc",
		"Basic abc":   "",
		"abc":         "",
		"Bearer":      "",
		"Bearer ":     "",
		"":            "",
	}
	for header, want := range cases {
		if got := bearer(header); got != want {
			t.Errorf("bearer(%q) = %q, want %q", header, got, want)
		}
	}
}
