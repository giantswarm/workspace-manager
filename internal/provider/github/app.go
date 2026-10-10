package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

// tokenRenewal is how long before its expiry an installation token is
// replaced, so no call goes out on one about to expire.
const tokenRenewal = 5 * time.Minute

// SyncCredential implements provider.Kind: the App's installation token on
// owner, minted from the App's private key and renewed tokenRenewal before it
// expires. The key is read once, here, so a missing Secret fails the first
// sync of the owner with its name.
func (k *Kind) SyncCredential(ctx context.Context, secrets provider.Secrets, owner string) (oauth2.TokenSource, error) {
	pemKey, err := secrets.Value(ctx, k.v.App.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("the App's private key (Secret %s): %w", k.v.App.PrivateKey, err)
	}
	key, err := parsePrivateKey(pemKey)
	if err != nil {
		return nil, fmt.Errorf("the App's private key (Secret %s): %w", k.v.App.PrivateKey, err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return nil, err
	}
	// The token source outlives the call that built it.
	return &installationTokens{k: k, signer: signer, owner: owner, ctx: context.WithoutCancel(ctx)}, nil
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("neither a PKCS#1 nor a PKCS#8 key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return key, nil
}

// installationTokens is the token source of one owner: it holds the current
// installation token and mints the next one when the current nears expiry.
type installationTokens struct {
	k      *Kind
	signer jose.Signer
	owner  string
	ctx    context.Context

	mu    sync.Mutex
	token *oauth2.Token
}

// Token implements oauth2.TokenSource.
func (s *installationTokens) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.k.clock.Now()
	if s.token != nil && now.Add(tokenRenewal).Before(s.token.Expiry) {
		return s.token, nil
	}
	// The App's own JWT, backdated against clock drift; GitHub accepts at
	// most ten minutes.
	appJWT, err := jwt.Signed(s.signer).Claims(jwt.Claims{
		Issuer:   s.k.v.App.ID,
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
		Expiry:   jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).Serialize()
	if err != nil {
		return nil, err
	}
	id, err := s.installation(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: appJWT}))
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost,
		s.k.api.String()+"/app/installations/"+strconv.FormatInt(id, 10)+"/access_tokens", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := s.k.do(req, http.StatusCreated, &tok); err != nil {
		return nil, fmt.Errorf("installation token on %s: %w", s.owner, err)
	}
	s.token = &oauth2.Token{AccessToken: tok.Token, TokenType: "Bearer", Expiry: tok.ExpiresAt}
	return s.token, nil
}

// installation looks the App's installation on the owner up: an
// organization's, else a user account's.
func (s *installationTokens) installation(app oauth2.TokenSource) (int64, error) {
	owner := url.PathEscape(s.owner)
	for _, path := range []string{"/orgs/" + owner + "/installation", "/users/" + owner + "/installation"} {
		var inst struct {
			ID int64 `json:"id"`
		}
		err := s.k.get(s.ctx, app, path, &inst)
		if err == nil {
			return inst.ID, nil
		}
		var se *statusError
		if !errors.As(err, &se) || se.status != http.StatusNotFound {
			return 0, fmt.Errorf("installation on %s: %w", s.owner, err)
		}
	}
	return 0, fmt.Errorf("the GitHub App %s is not installed on %s", s.k.v.App.ID, s.owner)
}
