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
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

// SyncCredential implements provider.Kind: the App's installation token on
// owner, renewed shortly before it expires. The private key is read once, here,
// so a missing Secret fails the first sync of the owner with its name.
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
	src := &installationTokens{k: k, signer: signer, owner: owner, ctx: context.WithoutCancel(ctx)}
	return oauth2.ReuseTokenSourceWithExpiry(nil, src, time.Minute), nil
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

// installationTokens mints an installation token per call; ReuseTokenSource
// calls it only when the last one is about to expire.
type installationTokens struct {
	k      *Kind
	signer jose.Signer
	owner  string
	ctx    context.Context
}

func (s *installationTokens) Token() (*oauth2.Token, error) {
	now := s.k.now()
	// Backdated against clock drift; GitHub accepts at most ten minutes.
	appJWT, err := jwt.Signed(s.signer).Claims(jwt.Claims{
		Issuer:   s.k.v.App.ID,
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
		Expiry:   jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).Serialize()
	if err != nil {
		return nil, err
	}
	app := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: appJWT})

	var inst struct {
		ID int64 `json:"id"`
	}
	if err := s.k.get(s.ctx, app, "/users/"+url.PathEscape(s.owner)+"/installation", &inst); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusNotFound {
			return nil, fmt.Errorf("the GitHub App %s is not installed on %s", s.k.v.App.ID, s.owner)
		}
		return nil, fmt.Errorf("installation on %s: %w", s.owner, err)
	}

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost,
		s.k.api.String()+"/app/installations/"+strconv.FormatInt(inst.ID, 10)+"/access_tokens", nil)
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
	return &oauth2.Token{AccessToken: tok.Token, TokenType: "Bearer", Expiry: tok.ExpiresAt}, nil
}
