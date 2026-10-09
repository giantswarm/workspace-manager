package signin

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, keySize) }

func TestSealRoundTrip(t *testing.T) {
	k, err := NewKeyring(map[string][]byte{"k1": key(1)}, "k1")
	require.NoError(t, err)

	plain := []byte(`{"accessToken":"a"}`)
	sealed, err := k.Seal(plain, "signin-github-abc")
	require.NoError(t, err)
	require.NotContains(t, string(sealed), "accessToken")

	opened, err := k.Open(sealed, "signin-github-abc")
	require.NoError(t, err)
	require.Equal(t, plain, opened)

	again, err := k.Seal(plain, "signin-github-abc")
	require.NoError(t, err)
	require.NotEqual(t, sealed, again, "every seal draws a new nonce")
}

func TestSealRotatedOutKeyStillOpens(t *testing.T) {
	old, err := NewKeyring(map[string][]byte{"k1": key(1)}, "k1")
	require.NoError(t, err)
	sealed, err := old.Seal([]byte("payload"), "ctx")
	require.NoError(t, err)

	rotated, err := NewKeyring(map[string][]byte{"k1": key(1), "k2": key(2)}, "k2")
	require.NoError(t, err)
	opened, err := rotated.Open(sealed, "ctx")
	require.NoError(t, err)
	require.Equal(t, []byte("payload"), opened)

	resealed, err := rotated.Seal(opened, "ctx")
	require.NoError(t, err)
	onlyNew, err := NewKeyring(map[string][]byte{"k2": key(2)}, "k2")
	require.NoError(t, err)
	_, err = onlyNew.Open(resealed, "ctx")
	require.NoError(t, err, "the new key writes")

	_, err = onlyNew.Open(sealed, "ctx")
	require.ErrorIs(t, err, ErrUnsealable, "a dropped key no longer opens its payloads")
	require.Contains(t, err.Error(), `"k1"`)
}

func TestSealTamperingFails(t *testing.T) {
	k, err := NewKeyring(map[string][]byte{"k1": key(1), "k2": key(2)}, "k1")
	require.NoError(t, err)
	sealed, err := k.Seal([]byte("payload"), "ctx")
	require.NoError(t, err)

	for i := range sealed {
		tampered := bytes.Clone(sealed)
		tampered[i] ^= 0x01
		_, err := k.Open(tampered, "ctx")
		require.ErrorIs(t, err, ErrUnsealable, "flipped bit at byte %d", i)
	}

	swapped := bytes.Clone(sealed)
	swapped[3] = '2' // key id k1 -> k2
	_, err = k.Open(swapped, "ctx")
	require.ErrorIs(t, err, ErrUnsealable, "the key id is authenticated")

	_, err = k.Open(sealed, "another-secret")
	require.ErrorIs(t, err, ErrUnsealable, "a payload is bound to its Secret")

	for _, short := range [][]byte{nil, {envelopeVersion}, sealed[:4], sealed[:20]} {
		_, err = k.Open(short, "ctx")
		require.ErrorIs(t, err, ErrUnsealable)
	}
}

func TestNewKeyringRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		keys    map[string][]byte
		current string
		want    string
	}{
		"current missing": {map[string][]byte{"k1": key(1)}, "k2", `current key "k2" is not among the keys ([k1])`},
		"short key":       {map[string][]byte{"k1": key(1)[:16]}, "k1", `key "k1" is 16 bytes, AES-256 needs 32`},
		"bad id":          {map[string][]byte{"K_1": key(1)}, "K_1", `key id "K_1" is not a lower-case DNS label`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewKeyring(tc.keys, tc.current)
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, err.Error(), string(key(1)))
		})
	}
}

func TestLoadKeyring(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sealing", Namespace: "ns"},
		Data:       map[string][]byte{"k1": key(1), "k2": key(2)},
	})
	k, err := LoadKeyring(context.Background(), client, "ns", "sealing", "k2")
	require.NoError(t, err)
	require.Equal(t, "k2", k.Current())

	_, err = LoadKeyring(context.Background(), client, "ns", "missing", "k1")
	require.EqualError(t, err, "sealing keys: Secret ns/missing not found")

	_, err = LoadKeyring(context.Background(), client, "ns", "sealing", "k3")
	require.ErrorContains(t, err, "sealing keys in Secret ns/sealing: current key \"k3\"")
	require.False(t, errors.Is(err, ErrUnsealable))
}
