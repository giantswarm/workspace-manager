// Package connect is a person's connection to each provider instance: the
// OAuth 2.0 authorization-code sign-in with PKCE that lands in the sign-in
// store, the list of instances with the person's connection state, and the
// disconnect that revokes the grant at the provider and forgets it.
//
// The flow's `state` is sealed (encrypted and authenticated by the sign-in
// store's keyring) and binds the person, the instance and the PKCE verifier
// for a short time; the verifier never leaves the manager in clear. A
// callback completes only for the person the state was made for, which the
// browser proves by signing in to the manager's own pages (Pages).
package connect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

// DefaultStateTTL bounds a sign-in from the link to the callback.
const DefaultStateTTL = 10 * time.Minute

var (
	// ErrUnknownProvider names an instance the installation does not
	// configure.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrInvalidState is a callback whose state is forged, expired or made
	// for another instance.
	ErrInvalidState = errors.New("invalid or expired sign-in state")
	// ErrOtherPerson is a callback whose state was made for someone else
	// than the person whose browser completes it.
	ErrOtherPerson = errors.New("the sign-in link belongs to another person")
)

// Clients resolves each instance's OAuth 2.0 client: the kind's endpoints,
// the client secret from its Secret, and the manager's callback for the
// instance. It is the sign-in store's signin.OAuth2Configs.
type Clients struct {
	instances map[string]provider.Instance
	order     []string
	secrets   provider.Secrets
	baseURL   string
}

var _ signin.OAuth2Configs = (*Clients)(nil)

// NewClients builds the clients of instances; baseURL is the manager's
// public URL, under which `/callback/<instance>` is each one's redirect URI.
func NewClients(instances []provider.Instance, secrets provider.Secrets, baseURL string) (*Clients, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("connect: base URL must be an absolute http(s) URL, got %q", baseURL)
	}
	if secrets == nil {
		return nil, errors.New("connect: no Secrets reader")
	}
	c := &Clients{instances: map[string]provider.Instance{}, secrets: secrets, baseURL: strings.TrimSuffix(baseURL, "/")}
	for _, in := range instances {
		c.instances[in.Name] = in
		c.order = append(c.order, in.Name)
	}
	return c, nil
}

func (c *Clients) instance(name string) (provider.Instance, error) {
	in, ok := c.instances[name]
	if !ok {
		return provider.Instance{}, fmt.Errorf("%w %q", ErrUnknownProvider, name)
	}
	return in, nil
}

// CallbackURL is the instance's redirect URI.
func (c *Clients) CallbackURL(instance string) string {
	return c.baseURL + "/callback/" + url.PathEscape(instance)
}

// ConnectURL is the instance's connect page, the link other surfaces show.
func (c *Clients) ConnectURL(instance string) string {
	return c.baseURL + "/connect/" + url.PathEscape(instance)
}

// OAuth2Config implements signin.OAuth2Configs.
func (c *Clients) OAuth2Config(ctx context.Context, instance string) (*oauth2.Config, error) {
	in, err := c.instance(instance)
	if err != nil {
		return nil, err
	}
	s := in.SignIn()
	secret, err := c.secrets.Value(ctx, s.ClientSecret)
	if err != nil {
		return nil, fmt.Errorf("provider %s: client secret: %w", instance, err)
	}
	return &oauth2.Config{
		ClientID:     s.ClientID,
		ClientSecret: string(secret),
		Endpoint:     oauth2.Endpoint{AuthURL: s.AuthURL, TokenURL: s.TokenURL},
		RedirectURL:  c.CallbackURL(instance),
		Scopes:       s.Scopes,
	}, nil
}

// Options configure a Connector.
type Options struct {
	Clients *Clients
	// Store keeps the sign-ins.
	Store signin.Store
	// Keyring seals the flow's state.
	Keyring *signin.Keyring
	// HTTPClient makes the token and revocation requests; default
	// http.DefaultClient.
	HTTPClient *http.Client
	// StateTTL bounds a sign-in; default DefaultStateTTL.
	StateTTL time.Duration
	// Logger receives the connector's events; never a token or code.
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Connector connects, lists and disconnects a person's providers.
type Connector struct {
	*Clients
	store   signin.Store
	keyring *signin.Keyring
	http    *http.Client
	ttl     time.Duration
	log     *slog.Logger
	now     func() time.Time
}

// New builds the connector.
func New(opts Options) (*Connector, error) {
	switch {
	case opts.Clients == nil:
		return nil, errors.New("connect: no OAuth 2.0 clients")
	case opts.Store == nil:
		return nil, errors.New("connect: no sign-in store")
	case opts.Keyring == nil:
		return nil, errors.New("connect: no sealing keys")
	}
	c := &Connector{Clients: opts.Clients, store: opts.Store, keyring: opts.Keyring, http: opts.HTTPClient,
		ttl: opts.StateTTL, log: opts.Logger, now: opts.Now}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	if c.ttl <= 0 {
		c.ttl = DefaultStateTTL
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Status is one instance and the person's connection to it.
type Status struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Connected  bool   `json:"connected"`
	ConnectURL string `json:"connectURL"`
}

// Providers lists every configured instance, in configuration order, with
// whether person has a sign-in for it.
func (c *Connector) Providers(ctx context.Context, person string) ([]Status, error) {
	out := make([]Status, 0, len(c.order))
	for _, name := range c.order {
		_, err := c.store.Get(ctx, person, name)
		switch {
		case err == nil:
		case errors.Is(err, signin.ErrNotSignedIn):
		default:
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		out = append(out, Status{Name: name, Kind: c.instances[name].KindName, Connected: err == nil, ConnectURL: c.ConnectURL(name)})
	}
	return out, nil
}

// AuthorizationURL starts person's sign-in to instance: the provider's
// authorization URL, carrying a sealed state and the PKCE challenge, valid
// until the returned time.
func (c *Connector) AuthorizationURL(ctx context.Context, person, instance string) (string, time.Time, error) {
	if person == "" {
		return "", time.Time{}, errors.New("connect: no person")
	}
	cfg, err := c.OAuth2Config(ctx, instance)
	if err != nil {
		return "", time.Time{}, err
	}
	verifier := oauth2.GenerateVerifier()
	expiry := c.now().Add(c.ttl)
	state, err := c.sealState(flowState{Person: person, Instance: instance, Verifier: verifier, Expiry: expiry.Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), expiry, nil
}

// CheckState opens a callback's state for instance: ErrInvalidState unless
// it is one this manager sealed for that instance and has not expired.
func (c *Connector) CheckState(instance, state string) (person string, err error) {
	st, err := c.openState(instance, state)
	if err != nil {
		return "", err
	}
	return st.Person, nil
}

// Complete finishes a sign-in at instance's callback for person, the one
// whose browser carries it: the state must be valid and made for person,
// and the provider must accept the code with the state's PKCE verifier.
// Only then is the token stored.
func (c *Connector) Complete(ctx context.Context, person, instance, state, code string) error {
	st, err := c.openState(instance, state)
	if err != nil {
		return err
	}
	if st.Person != person {
		c.log.WarnContext(ctx, "sign-in refused: state of another person", "instance", instance,
			"person", signin.PersonHash(person), "statePerson", signin.PersonHash(st.Person))
		return ErrOtherPerson
	}
	if code == "" {
		return errors.New("connect: the callback carries no code")
	}
	cfg, err := c.OAuth2Config(ctx, instance)
	if err != nil {
		return err
	}
	tok, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, c.http), code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return fmt.Errorf("provider %s refused the sign-in: %w", instance, providerError(err))
	}
	if err := c.store.Put(ctx, person, instance, tok); err != nil {
		return err
	}
	c.log.InfoContext(ctx, "provider connected", "instance", instance, "person", signin.PersonHash(person))
	return nil
}

// Disconnect revokes person's grant at instance and forgets the sign-in.
// revoked is false when there was nothing left to revoke (no sign-in, or one
// that can no longer be refreshed). A revocation the provider refuses keeps
// the sign-in, so the person can try again.
func (c *Connector) Disconnect(ctx context.Context, person, instance string) (revoked bool, err error) {
	in, err := c.instance(instance)
	if err != nil {
		return false, err
	}
	tok, err := c.store.Get(ctx, person, instance)
	if errors.Is(err, signin.ErrNotSignedIn) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s := in.SignIn()
	token, hint := tok.RefreshToken, "refresh_token"
	if s.Revocation == provider.RevokeGrant || token == "" {
		// A grant is revoked with a live access token.
		hint = "access_token"
		token, err = c.store.AccessToken(ctx, person, instance)
		if err != nil && !errors.Is(err, signin.ErrSignInExpired) {
			return false, err
		}
	}
	if token != "" {
		if err := c.revoke(ctx, instance, s, token, hint); err != nil {
			return false, err
		}
		revoked = true
	}
	if err := c.store.Delete(ctx, person, instance); err != nil {
		return revoked, err
	}
	c.log.InfoContext(ctx, "provider disconnected", "instance", instance, "person", signin.PersonHash(person), "revoked", revoked)
	return revoked, nil
}

// providerError keeps a token endpoint's status and error code only: the
// response body may echo a code or token.
func providerError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		if re.ErrorCode != "" {
			return fmt.Errorf("status %d: %s", status, re.ErrorCode)
		}
		return fmt.Errorf("status %d", status)
	}
	return err
}
