package signin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	// SecretType marks the Secrets that hold sealed sign-ins.
	SecretType corev1.SecretType = "workspace-manager.giantswarm.io/signin"
	// sealedKey is the Secret data entry with the sealed token.
	sealedKey = "sealed"

	labelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "workspace-manager"
	// LabelInstance names the provider instance a sign-in is for.
	LabelInstance = "workspace-manager.giantswarm.io/provider-instance"
	// LabelPerson is the person's hash (PersonHash), to find a person's
	// sign-ins without naming them.
	LabelPerson = "workspace-manager.giantswarm.io/person"

	// DefaultRefreshMargin is how long a returned access token stays valid
	// at least: an agent turn's length, with room to spare.
	DefaultRefreshMargin = 10 * time.Minute
	// DefaultLeaseDuration bounds a refresh: a replica that dies holding the
	// lease blocks the others this long. The token request gets half of it.
	DefaultLeaseDuration = 30 * time.Second
	defaultLeasePoll     = 100 * time.Millisecond
)

// Options configure a KubeStore.
type Options struct {
	// Client reaches the API server with the manager's ServiceAccount.
	Client kubernetes.Interface
	// Namespace is the manager's namespace, where sign-ins and leases live.
	Namespace string
	// Keyring seals and opens the tokens.
	Keyring *Keyring
	// OAuth2 resolves an instance's OAuth 2.0 client for a refresh.
	OAuth2 OAuth2Configs
	// Identity names this replica in refresh leases; default the host name.
	Identity string
	// RefreshMargin is how far ahead of expiry a token is refreshed;
	// default DefaultRefreshMargin.
	RefreshMargin time.Duration
	// LeaseDuration bounds a refresh; default DefaultLeaseDuration.
	LeaseDuration time.Duration
	// Logger receives the store's events; never a token or key value.
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// KubeStore is the Store on Kubernetes: one Secret per person and provider
// instance in the manager's namespace, its payload sealed by the Keyring.
// Refreshes are single-flighted within the replica and across replicas by a
// Lease per sign-in; the Secret's resourceVersion guards every write.
type KubeStore struct {
	opts   Options
	lease  *refreshLease
	flight singleflight.Group
}

var _ Store = (*KubeStore)(nil)

// NewKubeStore builds the store.
func NewKubeStore(opts Options) (*KubeStore, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("sign-in store: no Kubernetes client")
	case opts.Namespace == "":
		return nil, errors.New("sign-in store: no namespace")
	case opts.Keyring == nil:
		return nil, errors.New("sign-in store: no sealing keys")
	case opts.OAuth2 == nil:
		return nil, errors.New("sign-in store: no OAuth 2.0 clients")
	}
	if opts.Identity == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("sign-in store: replica identity: %w", err)
		}
		opts.Identity = host
	}
	if opts.RefreshMargin <= 0 {
		opts.RefreshMargin = DefaultRefreshMargin
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = DefaultLeaseDuration
	}
	if opts.LeaseDuration < 2*time.Second || opts.LeaseDuration > time.Hour {
		return nil, fmt.Errorf("sign-in store: lease duration %s is outside 2s to 1h", opts.LeaseDuration)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &KubeStore{
		opts: opts,
		lease: &refreshLease{
			client:    opts.Client,
			namespace: opts.Namespace,
			identity:  opts.Identity,
			duration:  opts.LeaseDuration,
			poll:      defaultLeasePoll,
			now:       opts.Now,
		},
	}, nil
}

// signIn addresses one stored sign-in.
type signIn struct {
	person, instance, name string
}

func (s *KubeStore) signIn(person, instance string) (signIn, error) {
	if person == "" {
		return signIn{}, errors.New("sign-in: no person")
	}
	if errs := validation.IsDNS1123Label(instance); len(errs) > 0 {
		return signIn{}, fmt.Errorf("sign-in: provider instance %q: %v", instance, errs)
	}
	return signIn{person: person, instance: instance, name: objectName(person, instance)}, nil
}

func (in signIn) labels() map[string]string {
	return map[string]string{
		labelManagedBy: managedBy,
		LabelInstance:  in.instance,
		LabelPerson:    PersonHash(in.person),
	}
}

// Get implements Store.
func (s *KubeStore) Get(ctx context.Context, person, instance string) (*oauth2.Token, error) {
	in, err := s.signIn(person, instance)
	if err != nil {
		return nil, err
	}
	tok, _, err := s.read(ctx, in)
	return tok, err
}

// Put implements Store.
func (s *KubeStore) Put(ctx context.Context, person, instance string, token *oauth2.Token) error {
	in, err := s.signIn(person, instance)
	if err != nil {
		return err
	}
	if token == nil || token.AccessToken == "" {
		return errors.New("sign-in: no access token to store")
	}
	sealed, err := s.seal(in, token)
	if err != nil {
		return err
	}
	secrets := s.opts.Client.CoreV1().Secrets(s.opts.Namespace)
	for {
		existing, err := secrets.Get(ctx, in.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: in.name, Namespace: s.opts.Namespace, Labels: in.labels()},
				Type:       SecretType,
				Data:       map[string][]byte{sealedKey: sealed},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("store sign-in %s: %w", in.name, err)
			}
			s.opts.Logger.InfoContext(ctx, "sign-in stored", "signin", in.name, "instance", in.instance)
			return nil
		}
		if err != nil {
			return fmt.Errorf("read sign-in %s: %w", in.name, err)
		}
		err = s.write(ctx, existing, sealed)
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return err
		}
		s.opts.Logger.InfoContext(ctx, "sign-in replaced", "signin", in.name, "instance", in.instance)
		return nil
	}
}

// Delete implements Store.
func (s *KubeStore) Delete(ctx context.Context, person, instance string) error {
	in, err := s.signIn(person, instance)
	if err != nil {
		return err
	}
	err = s.opts.Client.CoreV1().Secrets(s.opts.Namespace).Delete(ctx, in.name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete sign-in %s: %w", in.name, err)
	}
	s.opts.Logger.InfoContext(ctx, "sign-in deleted", "signin", in.name, "instance", in.instance)
	return nil
}

// AccessToken implements Store.
func (s *KubeStore) AccessToken(ctx context.Context, person, instance string) (string, error) {
	a, err := s.Access(ctx, person, instance)
	return a.Token, err
}

// Access implements Store. Within the replica, concurrent calls for one
// sign-in share a single call; across replicas, the refresh lease lets one
// replica redeem the refresh token while the others wait and read the token
// it wrote.
func (s *KubeStore) Access(ctx context.Context, person, instance string) (Access, error) {
	in, err := s.signIn(person, instance)
	if err != nil {
		return Access{}, err
	}
	v, err, _ := s.flight.Do(in.name, func() (any, error) {
		return s.access(ctx, in)
	})
	if err != nil {
		return Access{}, err
	}
	return v.(Access), nil
}

func (s *KubeStore) access(ctx context.Context, in signIn) (Access, error) {
	tok, _, err := s.read(ctx, in)
	if err != nil {
		return Access{}, err
	}
	if s.fresh(tok) {
		return accessOf(tok), nil
	}
	held, err := s.lease.acquire(ctx, in.name, in.labels(), func() (bool, error) {
		tok, _, err = s.read(ctx, in)
		return err == nil && s.fresh(tok), err
	})
	if err != nil {
		return Access{}, err
	}
	if !held {
		// Another replica refreshed while this one waited.
		return accessOf(tok), nil
	}
	defer func() {
		// The lease goes even when the caller's context ended.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.LeaseDuration)
		defer cancel()
		if err := s.lease.release(releaseCtx, in.name); err != nil {
			s.opts.Logger.WarnContext(ctx, "refresh lease not released; it expires", "signin", in.name, "error", err)
		}
	}()
	return s.refresh(ctx, in)
}

// refresh runs under the lease: it reads the sign-in once more (a replica
// that held the lease before may have refreshed it), redeems the refresh
// token and writes the result guarded by the resourceVersion it read.
func (s *KubeStore) refresh(ctx context.Context, in signIn) (Access, error) {
	tok, secret, err := s.read(ctx, in)
	if err != nil {
		return Access{}, err
	}
	if s.fresh(tok) {
		return accessOf(tok), nil
	}
	if tok.RefreshToken == "" {
		return Access{}, fmt.Errorf("sign-in %s: %w: no refresh token", in.name, ErrSignInExpired)
	}
	cfg, err := s.opts.OAuth2.OAuth2Config(ctx, in.instance)
	if err != nil {
		return Access{}, fmt.Errorf("sign-in %s: OAuth 2.0 client: %w", in.name, err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, s.opts.LeaseDuration/2)
	defer cancel()
	// A token with only the refresh token makes the source redeem it.
	refreshed, err := cfg.TokenSource(reqCtx, &oauth2.Token{RefreshToken: tok.RefreshToken}).Token()
	if err != nil {
		return Access{}, refreshError(in, err)
	}
	if refreshed.RefreshToken == "" {
		// A provider that does not rotate keeps the refresh token valid.
		refreshed.RefreshToken = tok.RefreshToken
	}
	sealed, err := s.seal(in, refreshed)
	if err != nil {
		return Access{}, err
	}
	for {
		err = s.write(ctx, secret, sealed)
		if err == nil {
			s.opts.Logger.InfoContext(ctx, "sign-in refreshed", "signin", in.name, "instance", in.instance,
				"expiry", refreshed.Expiry.UTC().Format(time.RFC3339))
			return accessOf(refreshed), nil
		}
		if !apierrors.IsConflict(err) {
			return Access{}, err
		}
		// Written meanwhile: a new sign-in (Put) wins over the refresh;
		// otherwise the redeemed refresh token is gone, so write the result
		// over whatever changed.
		var current *oauth2.Token
		current, secret, err = s.read(ctx, in)
		if err != nil {
			return Access{}, err
		}
		if current.RefreshToken != tok.RefreshToken {
			return accessOf(current), nil
		}
	}
}

// refreshError reports a failed refresh with the provider's status and error
// code only: oauth2's error carries the response body, which may echo a token.
func refreshError(in signIn, err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		if re.ErrorCode == "invalid_grant" {
			return fmt.Errorf("sign-in %s: %w: the provider refused the refresh token (%d %s)", in.name, ErrSignInExpired, status, re.ErrorCode)
		}
		return fmt.Errorf("sign-in %s: refresh failed: the provider answered %d %s", in.name, status, re.ErrorCode)
	}
	return fmt.Errorf("sign-in %s: refresh failed: %w", in.name, err)
}

func (s *KubeStore) fresh(tok *oauth2.Token) bool {
	if tok.AccessToken == "" {
		return false
	}
	return tok.Expiry.IsZero() || tok.Expiry.After(s.opts.Now().Add(s.opts.RefreshMargin))
}

// payload is the sealed content of a sign-in Secret.
type payload struct {
	AccessToken  string    `json:"accessToken"`
	TokenType    string    `json:"tokenType,omitempty"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
}

func (s *KubeStore) seal(in signIn, tok *oauth2.Token) ([]byte, error) {
	plain, err := json.Marshal(payload{ //nolint:gosec // the tokens are marshaled to be sealed, never stored or logged in clear
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
	})
	if err != nil {
		return nil, fmt.Errorf("sign-in %s: encode: %w", in.name, err)
	}
	sealed, err := s.opts.Keyring.Seal(plain, in.name)
	if err != nil {
		return nil, fmt.Errorf("sign-in %s: %w", in.name, err)
	}
	return sealed, nil
}

func (s *KubeStore) read(ctx context.Context, in signIn) (*oauth2.Token, *corev1.Secret, error) {
	secret, err := s.opts.Client.CoreV1().Secrets(s.opts.Namespace).Get(ctx, in.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("sign-in %s: %w", in.name, ErrNotSignedIn)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read sign-in %s: %w", in.name, err)
	}
	plain, err := s.opts.Keyring.Open(secret.Data[sealedKey], in.name)
	if err != nil {
		return nil, nil, fmt.Errorf("sign-in %s: %w", in.name, err)
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, nil, fmt.Errorf("sign-in %s: decode: %w", in.name, ErrUnsealable)
	}
	return &oauth2.Token{
		AccessToken:  p.AccessToken,
		TokenType:    p.TokenType,
		RefreshToken: p.RefreshToken,
		Expiry:       p.Expiry,
	}, secret, nil
}

// write replaces the sealed payload, guarded by the Secret's resourceVersion.
func (s *KubeStore) write(ctx context.Context, secret *corev1.Secret, sealed []byte) error {
	next := secret.DeepCopy()
	next.Data = map[string][]byte{sealedKey: sealed}
	_, err := s.opts.Client.CoreV1().Secrets(s.opts.Namespace).Update(ctx, next, metav1.UpdateOptions{})
	if err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("write sign-in %s: %w", secret.Name, err)
	}
	return err
}
