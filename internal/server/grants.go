package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

// Grants makes the MCP endpoint the registration muster connects to with the
// person's provider App tokens (MCPServer auth.providerApps, auth.forwardToken):
// the bearer stays the person's Dex ID token, and each App the person granted
// arrives in muster.GrantHeader(<instance>), verified with the instance's
// provider and handed to the tools on the request's context. An App the
// person has not granted sends no header: the tools name it, and muster
// answers that with the App's sign-in link.
type Grants struct {
	// Apps are the provider instances whose App muster attaches the person's
	// token of, each App named after its instance.
	Apps []provider.Instance
	// CacheTTL bounds how long a verified token is trusted without asking the
	// provider again (zero: DefaultPinCacheTTL).
	CacheTTL time.Duration
}

// grantGuard reads the person's App tokens muster attaches to the call.
type grantGuard struct {
	apps     []provider.Instance
	verifier *tokenVerifier
	log      *slog.Logger
}

func newGrantGuard(cfg Grants, log *slog.Logger) (*grantGuard, error) {
	if len(cfg.Apps) == 0 {
		return nil, errors.New("muster grants: at least one provider instance's App is required")
	}
	names := make([]string, len(cfg.Apps))
	for i, in := range cfg.Apps {
		if in.Name == "" || in.Kind == nil {
			return nil, errors.New("muster grants: every App needs its provider instance and its provider")
		}
		names[i] = in.Name
	}
	v := newTokenVerifier(cfg.CacheTTL)
	log.Info("muster grants enabled: the person's provider tokens are muster's grants of the instances' Apps", "apps", names, "cacheTTL", v.ttl)
	return &grantGuard{apps: cfg.Apps, verifier: v, log: log}, nil
}

// protect puts every App token the provider accepts on the request's context
// and drops every grant header, so nothing downstream reads one unverified. A
// token its provider refuses (revoked there) is left off, as if not granted;
// one the provider could not be asked about is kept, and the tool's own call
// to the provider answers.
func (g *grantGuard) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tokens := make(map[string]string, len(g.apps))
		for _, in := range g.apps {
			token := strings.TrimSpace(r.Header.Get(muster.GrantHeader(in.Name)))
			if token == "" {
				continue
			}
			switch err := g.verifier.verify(ctx, in.Kind, token); {
			case errors.Is(err, provider.ErrCredentialRefused):
				g.log.Info("provider refused the person's App token: treated as not granted", "provider", in.Name)
				continue
			case err != nil:
				g.log.Warn("App token not verified: kept for the tool's own call to the provider", "provider", in.Name, "error", err)
			}
			tokens[in.Name] = token
		}
		r = r.Clone(ctx)
		for name := range r.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(name), http.CanonicalHeaderKey(muster.GrantHeaderPrefix)) {
				r.Header.Del(name)
			}
		}
		for name, token := range tokens {
			ctx = identity.ContextWithProviderToken(ctx, name, token)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tokenVerifier asks a token's provider whether it accepts it, and remembers
// a yes for a while: a revoked token must stop working soon.
type tokenVerifier struct {
	ttl time.Duration

	mu    sync.Mutex
	cache map[[sha256.Size]byte]time.Time
}

func newTokenVerifier(ttl time.Duration) *tokenVerifier {
	if ttl <= 0 {
		ttl = DefaultPinCacheTTL
	}
	return &tokenVerifier{ttl: ttl, cache: map[[sha256.Size]byte]time.Time{}}
}

// verify asks kind whose token this is, unless the cache already trusts it.
func (v *tokenVerifier) verify(ctx context.Context, kind provider.Kind, token string) error {
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	v.mu.Lock()
	until, ok := v.cache[key]
	v.mu.Unlock()
	if ok && now.Before(until) {
		return nil
	}
	if _, err := kind.Person(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) >= pinCacheMax {
		for k, until := range v.cache {
			if !now.Before(until) {
				delete(v.cache, k)
			}
		}
	}
	if len(v.cache) < pinCacheMax {
		v.cache[key] = now.Add(v.ttl)
	}
	return nil
}
