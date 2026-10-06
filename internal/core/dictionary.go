package core

import (
	"context"
	"time"
)

// Dictionary is a stored hotword list: the phrases a deployment wants
// recognised, kept under a name somebody can type.
//
// It exists because the alternative — the phrases travelling in every request —
// made the useful case the awkward one. A call centre biasing towards its own
// product names sends the same two hundred phrases on every call, and the list
// is maintained by whoever knows the products rather than by whoever writes the
// client. Here the client names the dictionary and the list is somebody else's
// to curate.
//
// Key is the identity. There is no separate numeric id, because every place a
// dictionary is named — a request parameter, a URL, a log line, a support
// conversation — reads better with "medical-terms" in it than with a number,
// and carrying both would mean two ways to say the same thing and a question
// about which one is canonical.
type Dictionary struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// Phrases is the list itself. A listing leaves it out — a page of
	// dictionaries is a page of names, not of their contents — so a client
	// reading it there must not take nil for "empty": PhraseCount is the one
	// that is always answered.
	Phrases     []string `json:"phrases,omitempty"`
	PhraseCount int      `json:"phrase_count"`

	// Matches are the phrases a search matched, capped. A listing that
	// answered only "this dictionary matches" would leave the reader to open
	// each one to find out why.
	Matches []string `json:"matches,omitempty"`

	// Score is this dictionary's bias strength, 0 meaning the server default
	// (postproc.hotwords.default_score). It belongs to the dictionary rather
	// than only to the request because how hard to push a list is a property
	// of the list: product codes nobody says by accident can take more than
	// surnames that collide with ordinary words.
	Score float32 `json:"score,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HotwordPolicy is what the server does with a dictionary once a request names
// one. It is reported alongside the dictionaries themselves so that a screen
// for managing them can say whether they will be applied at all.
type HotwordPolicy struct {
	// Enabled is postproc.hotwords.enabled.
	Enabled bool `json:"enabled"`
	// DefaultScore applies to a dictionary that states no score of its own.
	DefaultScore float32 `json:"default_score"`
	// MaxVariants is asr.variants.max. Biasing costs a second resident copy
	// of the model, so zero here means no request can be biased however many
	// dictionaries are stored.
	MaxVariants int `json:"max_variants"`
	// MaxPhrases bounds one dictionary.
	MaxPhrases int `json:"max_phrases"`
	// Defaults are the keys applied to every request on a model that can be
	// biased, named by postproc.hotwords.default_dictionaries. Reported so a
	// management screen can mark them: a dictionary that is already on every
	// request reads very differently from one nobody has asked for yet.
	Defaults []string `json:"default_dictionaries,omitempty"`
}

// Dictionaries stores hotword dictionaries.
//
// Separate from ModelService and from Service: a dialect that offers
// transcription need not offer dictionary management, and the queue has no
// business with either.
type Dictionaries interface {
	// List returns dictionaries without their phrases, newest first. query
	// matches the key, the name, the description and the phrases themselves;
	// empty returns everything up to limit.
	List(ctx context.Context, query string, limit int) ([]Dictionary, error)
	// Get returns one dictionary with its phrases.
	Get(ctx context.Context, key string) (Dictionary, error)
	// Save creates or replaces a dictionary wholesale. Replacing rather than
	// patching is deliberate: a phrase list is edited as a list, and a partial
	// update API for it would be a merge nobody asked for.
	Save(ctx context.Context, d Dictionary) (Dictionary, error)
	// Delete removes one. A dictionary a request still names afterwards is
	// reported to that request rather than silently ignored.
	Delete(ctx context.Context, key string) error
}
