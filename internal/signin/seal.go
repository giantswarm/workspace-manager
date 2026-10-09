package signin

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// envelopeVersion is the first byte of every sealed payload. A new layout
// gets a new version; Open refuses one it does not know.
const envelopeVersion byte = 1

// keySize is AES-256's key length.
const keySize = 32

var keyIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ErrUnsealable is returned when a payload does not open: it was tampered
// with, sealed for another Secret, or sealed under a key the keyring no
// longer holds. The error never carries the payload or a key.
var ErrUnsealable = errors.New("sealed sign-in does not open")

// Keyring seals sign-ins with AES-256-GCM. It writes with the current key and
// opens with any key it holds, so a key rotates by adding the new key,
// making it current and dropping the old one once every sign-in was written
// again (a refresh or a new sign-in re-seals under the current key).
type Keyring struct {
	current string
	aeads   map[string]cipher.AEAD
}

// NewKeyring builds a keyring from key ids to 32-byte keys, current naming
// the key that seals. A key id is a lower-case DNS label of at most 32
// characters; it is stored in clear in every payload.
func NewKeyring(keys map[string][]byte, current string) (*Keyring, error) {
	k, err := newKeyring(keys, current)
	if err != nil {
		return nil, fmt.Errorf("sealing keys: %w", err)
	}
	return k, nil
}

func newKeyring(keys map[string][]byte, current string) (*Keyring, error) {
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("current key %q is not among the keys (%v)", current, keyIDs(keys))
	}
	aeads := make(map[string]cipher.AEAD, len(keys))
	for id, key := range keys {
		if !keyIDPattern.MatchString(id) {
			return nil, fmt.Errorf("key id %q is not a lower-case DNS label of at most 32 characters", id)
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("key %q is %d bytes, AES-256 needs %d", id, len(key), keySize)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", id, err)
		}
		aeads[id] = aead
	}
	return &Keyring{current: current, aeads: aeads}, nil
}

// LoadKeyring reads the keys from a Secret: each data entry is a key id and
// its 32 raw bytes. Errors name the Secret and the key id, never a value.
func LoadKeyring(ctx context.Context, client kubernetes.Interface, namespace, name, current string) (*Keyring, error) {
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("sealing keys: Secret %s/%s not found", namespace, name)
		}
		return nil, fmt.Errorf("sealing keys: read Secret %s/%s: %w", namespace, name, err)
	}
	k, err := newKeyring(secret.Data, current)
	if err != nil {
		return nil, fmt.Errorf("sealing keys in Secret %s/%s: %w", namespace, name, err)
	}
	return k, nil
}

// Current is the id of the key that seals.
func (k *Keyring) Current() string { return k.current }

// Seal encrypts plaintext under the current key, bound to context (the
// Secret's name), so a payload copied into another Secret does not open.
func (k *Keyring) Seal(plaintext []byte, context string) ([]byte, error) {
	aead := k.aeads[k.current]
	header := k.header(k.current)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: nonce: %w", err)
	}
	out := make([]byte, 0, len(header)+len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, header...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, additionalData(header, context)), nil
}

// Open decrypts a payload Seal produced for the same context, under whichever
// key sealed it.
func (k *Keyring) Open(sealed []byte, context string) ([]byte, error) {
	if len(sealed) < 2 || sealed[0] != envelopeVersion {
		return nil, ErrUnsealable
	}
	idLen := int(sealed[1])
	if len(sealed) < 2+idLen {
		return nil, ErrUnsealable
	}
	id := string(sealed[2 : 2+idLen])
	aead, ok := k.aeads[id]
	if !ok {
		return nil, fmt.Errorf("%w: sealed under key %q, which the keyring does not hold", ErrUnsealable, id)
	}
	header := sealed[:2+idLen]
	rest := sealed[2+idLen:]
	if len(rest) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrUnsealable
	}
	nonce, ciphertext := rest[:aead.NonceSize()], rest[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, additionalData(header, context))
	if err != nil {
		return nil, ErrUnsealable
	}
	return plaintext, nil
}

// header is the envelope's clear prefix: version, key id length, key id.
func (k *Keyring) header(id string) []byte {
	h := make([]byte, 0, 2+len(id))
	h = append(h, envelopeVersion, byte(len(id)))
	return append(h, id...)
}

// additionalData authenticates the header (so the key id cannot be swapped)
// and the context.
func additionalData(header []byte, context string) []byte {
	ad := make([]byte, 0, len(header)+len(context))
	ad = append(ad, header...)
	return append(ad, context...)
}

func keyIDs(keys map[string][]byte) []string {
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
