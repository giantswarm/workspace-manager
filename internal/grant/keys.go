package grant

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// KeysTTL is how long the keys read from their Secret are used before the
// Secret is read again: a key added to it is published within it, and the
// public JWKS endpoint costs the API server one read per KeysTTL.
const KeysTTL = 30 * time.Second

// minRSABits is the smallest RSA key accepted.
const minRSABits = 2048

// Keys is the signing key and every key published with it.
type Keys struct {
	// Signing signs new grants.
	Signing jose.JSONWebKey
	// Public are the verification keys: the signing key's and those of every
	// other key in the Secret, so a key is rotated by adding the next one
	// beside it, switching the reference to it, and removing the old one once
	// its last grant expired.
	Public jose.JSONWebKeySet
}

// KeySource reads the keys from one Secret: the entry Key names signs, and
// every entry holding a PEM private key is published.
type KeySource struct {
	Client    kubernetes.Interface
	Namespace string
	Secret    string
	Key       string
	// Now is the clock; default time.Now.
	Now func() time.Time

	mu     sync.Mutex
	keys   *Keys
	readAt time.Time
}

// Keys returns the keys, read again once KeysTTL passed. A Secret that cannot
// be read, or whose signing entry is no usable key, is an error.
func (s *KeySource) Keys(ctx context.Context) (*Keys, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys != nil && now().Sub(s.readAt) < KeysTTL {
		return s.keys, nil
	}
	secret, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, s.Secret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("grant signing key: secret %s/%s not found", s.Namespace, s.Secret)
	}
	if err != nil {
		return nil, fmt.Errorf("grant signing key: read secret %s/%s: %w", s.Namespace, s.Secret, err)
	}
	keys, err := ParseKeys(secret.Data, s.Key)
	if err != nil {
		return nil, fmt.Errorf("grant signing key %s/%s: %w", s.Secret, s.Key, err)
	}
	s.keys, s.readAt = keys, now()
	return keys, nil
}

// ParseKeys reads the keys from a Secret's data: the entry signing names
// must be a PEM private key (EC P-256 or P-384, or RSA of at least 2048
// bits); every other entry holding one is published beside it, and entries
// that hold none are left alone. Errors name entries, never a value.
func ParseKeys(data map[string][]byte, signing string) (*Keys, error) {
	raw, ok := data[signing]
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("no entry %q", signing)
	}
	key, err := parsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("entry %q: %w", signing, err)
	}
	sk, err := webKey(key)
	if err != nil {
		return nil, fmt.Errorf("entry %q: %w", signing, err)
	}
	keys := &Keys{Signing: sk}
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for _, name := range names {
		k := sk
		if name != signing {
			pk, err := parsePrivateKey(data[name])
			if err != nil {
				continue
			}
			if k, err = webKey(pk); err != nil {
				continue
			}
		}
		if seen[k.KeyID] {
			continue
		}
		seen[k.KeyID] = true
		keys.Public.Keys = append(keys.Public.Keys, k.Public())
	}
	return keys, nil
}

func parsePrivateKey(raw []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("not a PEM private key")
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("PEM block %q is not a private key", block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", block.Type, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%T is not a signing key", key)
	}
	return signer, nil
}

// webKey is key as a JSON Web Key: its algorithm, `use: sig` and its RFC 7638
// thumbprint as the key id.
func webKey(key crypto.Signer) (jose.JSONWebKey, error) {
	var alg jose.SignatureAlgorithm
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		switch k.Curve {
		case elliptic.P256():
			alg = jose.ES256
		case elliptic.P384():
			alg = jose.ES384
		default:
			return jose.JSONWebKey{}, fmt.Errorf("EC curve %s: want P-256 or P-384", k.Curve.Params().Name)
		}
	case *rsa.PrivateKey:
		if k.N.BitLen() < minRSABits {
			return jose.JSONWebKey{}, fmt.Errorf("RSA key of %d bits: want at least %d", k.N.BitLen(), minRSABits)
		}
		alg = jose.RS256
	default:
		return jose.JSONWebKey{}, fmt.Errorf("%T: want an EC or RSA key", key)
	}
	jwk := jose.JSONWebKey{Key: key, Algorithm: string(alg), Use: "sig"}
	public := jwk.Public()
	thumb, err := public.Thumbprint(crypto.SHA256)
	if err != nil {
		return jose.JSONWebKey{}, fmt.Errorf("key id: %w", err)
	}
	jwk.KeyID = base64.RawURLEncoding.EncodeToString(thumb)
	return jwk, nil
}
