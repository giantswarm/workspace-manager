// Package grant is the workspace grant: a short-lived JWT the workspace-manager
// signs once a person may read every repository of a workspace, which kagent's
// Session admission verifies against the published keys (JWKSPath) before it
// mounts the workspace's volume into the Session. The grant names the person,
// the namespace whose Agents may start a Session with it, the workspace, its
// volume and the mounts of it a Session may have, the repositories pushed to
// since the last sync and the provider hosts whose credentials the Session
// gets. It carries no token.
//
// The claim names are kagent's JWT admission's: `agent_namespace`, `volume`
// {driver, handle}, `mounts` [{sub_path, read_only}], `changed` [owner/name]
// and `providers` [{hostname, audience, scheme, username, broker}].
package grant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// SessionDirectory is the read-write mount a grant allows: the Session's own
// directory, `${SESSION_ID}` standing for the Session kagent creates.
const SessionDirectory = "sessions/${SESSION_ID}"

// Mirrors is the read-only mount a grant allows: the workspace's mirrors.
const Mirrors = "mirrors"

// DefaultTTL is how long a grant is valid: long enough for the surface to
// pass it to agent-manager's start_session, short enough that a revoked access
// is not carried far.
const DefaultTTL = 5 * time.Minute

// Workspace names the workspace a grant is for.
type Workspace struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Volume is the workspace's volume as its CSI driver knows it.
type Volume struct {
	Driver string `json:"driver"`
	Handle string `json:"handle"`
}

// Mount is a sub-path of the volume a Session may mount.
type Mount struct {
	SubPath  string `json:"sub_path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Provider is one host, or the request paths under PathPrefix on it, the
// Session gets the person's credential for the provider instance Provider on,
// presented with Scheme. Audience, Username and Broker carry the same for a
// kagent that does not read Provider yet: Audience is Provider, and the
// credential comes from the token broker named Broker.
type Provider struct {
	Hostname   string `json:"hostname"`
	PathPrefix string `json:"pathPrefix,omitempty"`
	Scheme     string `json:"scheme"`
	Provider   string `json:"provider"`
	Audience   string `json:"audience"`
	Username   string `json:"username,omitempty"`
	Broker     string `json:"broker"`
}

// Grant is what a grant admits: everything but who and when.
type Grant struct {
	// AgentNamespace is the namespace whose Agents may start a Session with
	// the grant; kagent refuses one on an Agent elsewhere.
	AgentNamespace string     `json:"agent_namespace"`
	Workspace      Workspace  `json:"workspace"`
	Volume         Volume     `json:"volume"`
	Mounts         []Mount    `json:"mounts"`
	Changed        []string   `json:"changed,omitempty"`
	Providers      []Provider `json:"providers,omitempty"`
}

// Claims are a grant's payload.
type Claims struct {
	jwt.Claims
	Grant
}

// Signer signs grants with the key its KeySource names.
type Signer struct {
	Keys *KeySource
	// Issuer is the grant's iss: the manager's public base URL.
	Issuer string
	// Audience is the grant's aud: the installation's kagent.
	Audience string
	// TTL is how long a grant is valid; zero is DefaultTTL.
	TTL time.Duration
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Validate checks the signer's configuration.
func (s *Signer) Validate() error {
	var errs []error
	if s.Keys == nil {
		errs = append(errs, errors.New("grant: no signing key"))
	}
	if s.Issuer == "" || s.Audience == "" {
		errs = append(errs, errors.New("grant: an issuer and an audience are required"))
	}
	if s.TTL < 0 {
		errs = append(errs, fmt.Errorf("grant: TTL must not be negative, got %s", s.TTL))
	}
	return errors.Join(errs...)
}

// Sign returns the signed grant for subject, the person as the verifier names
// a Session's creator, and when it expires.
func (s *Signer) Sign(ctx context.Context, subject string, g Grant) (string, time.Time, error) {
	keys, err := s.Keys.Keys(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.SignatureAlgorithm(keys.Signing.Algorithm), Key: keys.Signing},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("grant signer: %w", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ttl := s.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	issued := now().UTC().Truncate(time.Second)
	expiry := issued.Add(ttl)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", time.Time{}, fmt.Errorf("grant id: %w", err)
	}
	claims := Claims{
		Claims: jwt.Claims{
			Issuer:    s.Issuer,
			Subject:   subject,
			Audience:  jwt.Audience{s.Audience},
			IssuedAt:  jwt.NewNumericDate(issued),
			NotBefore: jwt.NewNumericDate(issued),
			Expiry:    jwt.NewNumericDate(expiry),
			ID:        hex.EncodeToString(id),
		},
		Grant: g,
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign grant: %w", err)
	}
	return token, expiry, nil
}
