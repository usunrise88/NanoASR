package httpx

import (
	"context"
	"strings"
	"testing"
)

const (
	goodKey  = "sk-test-0123456789abcdef"
	adminKey = "sk-admin-0123456789abcdef"
)

func newStore(t *testing.T, specs ...KeySpec) *StaticKeyStore {
	t.Helper()
	s, err := NewStaticKeyStore(specs)
	if err != nil {
		t.Fatalf("NewStaticKeyStore: %v", err)
	}
	return s
}

func TestVerifyAcceptsOnlyTheConfiguredKey(t *testing.T) {
	s := newStore(t, KeySpec{Name: "app", Secret: goodKey})
	ctx := context.Background()

	id, ok := s.Verify(ctx, goodKey)
	if !ok {
		t.Fatal("the configured key was rejected")
	}
	if id == "" {
		t.Error("a verified key must yield an id for logging")
	}

	for _, bad := range []string{"", "wrong", goodKey + "x", strings.ToUpper(goodKey)} {
		if _, ok := s.Verify(ctx, bad); ok {
			t.Errorf("Verify accepted %q", bad)
		}
	}
}

// An operator who does not want plaintext in a config file writes the digest
// instead; both forms must authenticate the same secret.
func TestPreHashedKeyIsEquivalentToPlaintext(t *testing.T) {
	plain := newStore(t, KeySpec{Secret: goodKey})
	hashed := newStore(t, KeySpec{Secret: Hash(goodKey)})

	plainID, ok := plain.Verify(context.Background(), goodKey)
	if !ok {
		t.Fatal("plaintext form failed")
	}
	hashedID, ok := hashed.Verify(context.Background(), goodKey)
	if !ok {
		t.Fatal("sha256 form failed")
	}
	if plainID != hashedID {
		t.Errorf("ids differ between forms: %q vs %q", plainID, hashedID)
	}
}

func TestKeyStoreRejectsUnusableConfiguration(t *testing.T) {
	cases := []struct {
		name  string
		specs []KeySpec
		want  string
	}{
		{"empty secret", []KeySpec{{Name: "a", Secret: "  "}}, "no value"},
		{"short secret", []KeySpec{{Secret: "sk-short"}}, "at least 16"},
		{"bad hex", []KeySpec{{Secret: "sha256:zzzz"}}, "hexadecimal"},
		{"wrong digest length", []KeySpec{{Secret: "sha256:abcd"}}, "digest is"},
		{
			name:  "duplicate keys",
			specs: []KeySpec{{Name: "a", Secret: goodKey}, {Name: "b", Secret: goodKey}},
			want:  "duplicates",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewStaticKeyStore(c.specs)
			if err == nil {
				t.Fatalf("expected an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestLookupReportsAdminRights(t *testing.T) {
	s := newStore(t,
		KeySpec{Name: "app", Secret: goodKey},
		KeySpec{Name: "ci", Secret: adminKey, Admin: true})

	appID, _ := s.Verify(context.Background(), goodKey)
	adminID, _ := s.Verify(context.Background(), adminKey)

	if k, ok := s.Lookup(appID); !ok || k.Admin {
		t.Errorf("app key: %+v, ok=%v — want a non-admin key", k, ok)
	}
	if k, ok := s.Lookup(adminID); !ok || !k.Admin {
		t.Errorf("ci key: %+v, ok=%v — want an admin key", k, ok)
	}
	if _, ok := s.Lookup("deadbeef"); ok {
		t.Error("Lookup invented a key")
	}
}

// Names goes into the startup log, so it must identify keys without revealing
// any part of them.
func TestNamesNeverLeakTheSecret(t *testing.T) {
	s := newStore(t, KeySpec{Name: "app", Secret: goodKey}, KeySpec{Secret: adminKey, Admin: true})

	for _, n := range s.Names() {
		if strings.Contains(n, goodKey) || strings.Contains(n, adminKey) {
			t.Fatalf("Names() leaked a secret: %q", n)
		}
	}
	joined := strings.Join(s.Names(), " ")
	if !strings.Contains(joined, "app") || !strings.Contains(joined, "admin") {
		t.Errorf("Names() = %v, want the name and the admin marker", s.Names())
	}
}

// An unnamed key still needs something to appear as in logs.
func TestUnnamedKeyGetsAGeneratedName(t *testing.T) {
	s := newStore(t, KeySpec{Secret: goodKey})
	if n := s.Names()[0]; !strings.HasPrefix(n, "key-") {
		t.Errorf("Names()[0] = %q, want a generated name", n)
	}
}
