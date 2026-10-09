package signin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testNamespace = "workspace-manager"
	testPerson    = "CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxZjU0NjYSBWxvY2Fs"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func newTestStore(t *testing.T, client kubernetes.Interface, ts *tokenServer, identity string, opts ...func(*Options)) (*KubeStore, *syncBuffer) {
	t.Helper()
	keys, err := NewKeyring(map[string][]byte{"k1": testKey}, "k1")
	require.NoError(t, err)
	log, logs := captureLogs()
	o := Options{
		Client:    client,
		Namespace: testNamespace,
		Keyring:   keys,
		OAuth2:    ts.configs(),
		Identity:  identity,
		Logger:    log,
	}
	for _, f := range opts {
		f(&o)
	}
	s, err := NewKubeStore(o)
	require.NoError(t, err)
	return s, logs
}

func expired(gen string) *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  "access-" + gen,
		RefreshToken: "refresh-" + gen,
		TokenType:    "bearer",
		Expiry:       time.Now().Add(-time.Minute),
	}
}

func TestPutGetDelete(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	s, logs := newTestStore(t, client, newTokenServer(t, 0), "replica-a")

	_, err := s.Get(ctx, testPerson, "github")
	require.ErrorIs(t, err, ErrNotSignedIn)

	tok := &oauth2.Token{AccessToken: "access-0", RefreshToken: "refresh-0", TokenType: "bearer", Expiry: time.Now().Add(time.Hour).Round(0).UTC()}
	require.NoError(t, s.Put(ctx, testPerson, "github", tok))
	got, err := s.Get(ctx, testPerson, "github")
	require.NoError(t, err)
	require.Equal(t, tok.AccessToken, got.AccessToken)
	require.Equal(t, tok.RefreshToken, got.RefreshToken)
	require.True(t, tok.Expiry.Equal(got.Expiry))

	secrets, err := client.CoreV1().Secrets(testNamespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 1)
	secret := secrets.Items[0]
	require.Equal(t, "signin-github-"+PersonHash(testPerson), secret.Name)
	require.Equal(t, SecretType, secret.Type)
	require.Equal(t, map[string]string{
		"app.kubernetes.io/managed-by":                      "workspace-manager",
		"workspace-manager.giantswarm.io/provider-instance": "github",
		"workspace-manager.giantswarm.io/person":            PersonHash(testPerson),
	}, secret.Labels)
	meta := secret.Name + strings.Join(mapValues(secret.Labels), " ") + strings.Join(mapValues(secret.Annotations), " ")
	require.NotContains(t, meta, testPerson, "nothing in names or labels names the person")
	assertNoSecret(t, string(secret.Data[sealedKey]), bytesOf("access-0", "refresh-0")...)

	require.NoError(t, s.Put(ctx, testPerson, "github", &oauth2.Token{AccessToken: "access-9"}))
	got, err = s.Get(ctx, testPerson, "github")
	require.NoError(t, err)
	require.Equal(t, "access-9", got.AccessToken)

	require.NoError(t, s.Delete(ctx, testPerson, "github"))
	require.NoError(t, s.Delete(ctx, testPerson, "github"), "deleting a missing sign-in is no error")
	_, err = s.Get(ctx, testPerson, "github")
	require.ErrorIs(t, err, ErrNotSignedIn)

	assertNoSecret(t, logs.String(), append(bytesOf("access-0", "refresh-0", "access-9"), testKey)...)
}

func TestAccessTokenRefreshesAheadOfExpiry(t *testing.T) {
	ctx := context.Background()
	ts := newTokenServer(t, 0)
	s, logs := newTestStore(t, fake.NewClientset(), ts, "replica-a", func(o *Options) { o.RefreshMargin = 15 * time.Minute })

	// Valid for 20 more minutes: beyond the margin, returned as is.
	require.NoError(t, s.Put(ctx, testPerson, "github", &oauth2.Token{AccessToken: "access-0", RefreshToken: "refresh-0", Expiry: time.Now().Add(20 * time.Minute)}))
	at, err := s.AccessToken(ctx, testPerson, "github")
	require.NoError(t, err)
	require.Equal(t, "access-0", at)
	require.Zero(t, ts.total())

	// Valid for 10 more minutes: inside the margin, refreshed.
	require.NoError(t, s.Put(ctx, testPerson, "github", &oauth2.Token{AccessToken: "access-0", RefreshToken: "refresh-0", Expiry: time.Now().Add(10 * time.Minute)}))
	at, err = s.AccessToken(ctx, testPerson, "github")
	require.NoError(t, err)
	require.Equal(t, "access-1", at)
	got, err := s.Get(ctx, testPerson, "github")
	require.NoError(t, err)
	require.Equal(t, "refresh-1", got.RefreshToken, "the rotated refresh token is stored")
	require.WithinDuration(t, time.Now().Add(8*time.Hour), got.Expiry, time.Minute)

	at, err = s.AccessToken(ctx, testPerson, "github")
	require.NoError(t, err)
	require.Equal(t, "access-1", at)
	require.Equal(t, 1, ts.count("refresh-0"))
	require.Equal(t, 1, ts.total())

	leases, err := s.opts.Client.CoordinationV1().Leases(testNamespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, leases.Items, "the refresh lease is released")

	assertNoSecret(t, logs.String(), append(bytesOf(append(ts.tokens(), "access-0", "refresh-0", "client-secret")...), testKey)...)
}

func TestAccessTokenErrorsCarryNoSecret(t *testing.T) {
	ctx := context.Background()
	ts := newTokenServer(t, 0)
	client := fake.NewClientset()
	s, logs := newTestStore(t, client, ts, "replica-a")
	var errs []string

	_, err := s.AccessToken(ctx, testPerson, "github")
	require.ErrorIs(t, err, ErrNotSignedIn)
	errs = append(errs, err.Error())

	// A refresh token the provider already redeemed: it answers invalid_grant
	// and echoes the token in its description.
	require.NoError(t, s.Put(ctx, testPerson, "github", expired("0")))
	ts.redemptions["refresh-0"] = 1
	_, err = s.AccessToken(ctx, testPerson, "github")
	require.ErrorIs(t, err, ErrSignInExpired)
	require.Contains(t, err.Error(), "400 invalid_grant")
	errs = append(errs, err.Error())

	require.NoError(t, s.Put(ctx, testPerson, "github", &oauth2.Token{AccessToken: "access-x", Expiry: time.Now()}))
	_, err = s.AccessToken(ctx, testPerson, "github")
	require.ErrorIs(t, err, ErrSignInExpired)
	errs = append(errs, err.Error())

	// Tampered payload.
	name := objectName(testPerson, "github")
	secret, err := client.CoreV1().Secrets(testNamespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	secret.Data[sealedKey][len(secret.Data[sealedKey])-1] ^= 1
	_, err = client.CoreV1().Secrets(testNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = s.AccessToken(ctx, testPerson, "github")
	require.ErrorIs(t, err, ErrUnsealable)
	errs = append(errs, err.Error())

	// A payload copied into another person's Secret does not open there.
	require.NoError(t, s.Put(ctx, testPerson, "github", expired("5")))
	src, err := client.CoreV1().Secrets(testNamespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, s.Put(ctx, "someone-else", "github", expired("7")))
	dst, err := client.CoreV1().Secrets(testNamespace).Get(ctx, objectName("someone-else", "github"), metav1.GetOptions{})
	require.NoError(t, err)
	dst.Data = src.Data
	_, err = client.CoreV1().Secrets(testNamespace).Update(ctx, dst, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = s.Get(ctx, "someone-else", "github")
	require.ErrorIs(t, err, ErrUnsealable)
	errs = append(errs, err.Error())

	all := logs.String() + strings.Join(errs, "\n")
	assertNoSecret(t, all, append(bytesOf("access-0", "refresh-0", "access-x", "access-5", "refresh-5", "access-7", "refresh-7", "client-secret"), testKey)...)
	assertNoSecret(t, strings.Join(errs, "\n"), []byte(testPerson))
}

func TestStoreRefusesBadAddresses(t *testing.T) {
	s, _ := newTestStore(t, fake.NewClientset(), newTokenServer(t, 0), "replica-a")
	_, err := s.AccessToken(context.Background(), "", "github")
	require.EqualError(t, err, "sign-in: no person")
	_, err = s.AccessToken(context.Background(), testPerson, "GitHub.com")
	require.ErrorContains(t, err, `provider instance "GitHub.com"`)
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
