// Package exchange is the token endpoint kagent calls on every turn of a
// Session with a workspace: OAuth 2.0 Token Exchange (RFC 8693) of the turn
// caller's Dex token for the person's access token at one provider instance,
// which the egress gateway sets on the provider's hosts. Only the
// installation's kagent client is answered. The response carries the access
// token the sign-in store holds, refreshed ahead of expiry, never a refresh
// token; a person without a sign-in is pointed at the instance's connect page.
package exchange

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

// Path is the token endpoint's path.
const Path = "/token"

// RFC 8693 identifiers: names of token types, not credentials.
const (
	GrantType            = "urn:ietf:params:oauth:grant-type:token-exchange"
	TokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec // G101: an RFC 8693 token type URN
	TokenTypeIDToken     = "urn:ietf:params:oauth:token-type:id_token"     //nolint:gosec // G101: an RFC 8693 token type URN
	TokenTypeJWT         = "urn:ietf:params:oauth:token-type:jwt"          //nolint:gosec // G101: an RFC 8693 token type URN
)

// subjectTokenTypes are what a Dex token is called by a client: an OpenID
// Connect id_token is a JWT, and a client that forwards the bearer it was
// called with calls it an access token.
var subjectTokenTypes = []string{TokenTypeIDToken, TokenTypeJWT, TokenTypeAccessToken}

// maxBody bounds the form a client posts: a Dex token and a few parameters.
const maxBody = 64 << 10

// Subjects validates a subject_token: the caller's Dex token, accepted on the
// same terms as the MCP endpoint's bearer. An error refuses it.
type Subjects interface {
	Subject(ctx context.Context, token string) (*identity.Identity, error)
}

// Tokens are the people's sign-ins (signin.Store).
type Tokens interface {
	Access(ctx context.Context, person, instance string) (signin.Access, error)
}

// Config configures the endpoint.
type Config struct {
	// ClientID is the installation's kagent client: the only client
	// answered.
	ClientID string
	// ClientSecret names the Secret key holding the client's secret, read
	// on every request so a rotated secret applies without a restart.
	ClientSecret provider.SecretRef
	// Secrets reads ClientSecret.
	Secrets provider.Secrets
	// Instances are the provider instances: an audience is one of their
	// names.
	Instances []provider.Instance
	// Tokens are the people's sign-ins.
	Tokens Tokens
	// ConnectURL is the page where a person signs in to an instance:
	// error_uri of a refusal for a missing sign-in.
	ConnectURL func(instance string) string
	// Logger receives one line per exchange, never a token or secret.
	Logger *slog.Logger
	// Meter counts the exchanges; default the global MeterProvider's.
	Meter metric.Meter
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Validate checks required fields.
func (c Config) Validate() error {
	var errs []error
	if c.ClientID == "" {
		errs = append(errs, errors.New("token exchange: the kagent client ID is required"))
	}
	if c.ClientSecret.Name == "" || c.ClientSecret.Key == "" {
		errs = append(errs, errors.New("token exchange: the kagent client secret needs a Secret name and key"))
	}
	if c.Secrets == nil || c.Tokens == nil || c.ConnectURL == nil {
		errs = append(errs, errors.New("token exchange: secrets, sign-in store and connect URL are required"))
	}
	return errors.Join(errs...)
}

// Handler serves POST /token.
type Handler struct {
	cfg       Config
	subjects  Subjects
	instances map[string]bool
	exchanges metric.Int64Counter
}

// New builds the endpoint; subjects validates the subject tokens.
func New(cfg Config, subjects Subjects) (*Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if subjects == nil {
		return nil, errors.New("token exchange: needs OAuth, which validates the subject token")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Meter == nil {
		cfg.Meter = otel.Meter("github.com/giantswarm/workspace-manager/internal/exchange")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	exchanges, err := cfg.Meter.Int64Counter("workspace_manager.token_exchange.requests",
		metric.WithDescription("Token exchanges answered, by provider instance and outcome (released, or the OAuth error code)"),
		metric.WithUnit("{exchange}"))
	if err != nil {
		otel.Handle(err)
	}
	instances := make(map[string]bool, len(cfg.Instances))
	for _, in := range cfg.Instances {
		instances[in.Name] = true
	}
	return &Handler{cfg: cfg, subjects: subjects, instances: instances, exchanges: exchanges}, nil
}

// Register adds POST /token to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST "+Path, h)
}

// oauthError is an RFC 6749 §5.2 error response.
type oauthError struct {
	status      int
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	URI         string `json:"error_uri,omitempty"`
	// basic asks for HTTP Basic client authentication (401 invalid_client).
	basic bool
}

// response is the RFC 8693 §2.2.1 success response. No refresh_token, ever.
type response struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in,omitempty"`
}

// exchange is one request's outcome, for the log line and the counter.
type exchange struct {
	client   string
	person   string
	instance string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var x exchange
	res, oe := h.exchange(ctx, r, &x)
	if oe != nil {
		h.record(ctx, x, oe.Code)
		writeError(w, oe)
		return
	}
	h.record(ctx, x, "released")
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) exchange(ctx context.Context, r *http.Request, x *exchange) (*response, *oauthError) {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/x-www-form-urlencoded" {
		return nil, invalidRequest("the request must be application/x-www-form-urlencoded")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		return nil, invalidRequest("the request body is not a valid form")
	}
	form := r.PostForm
	if oe := single(form, "client_id", "client_secret", "grant_type", "subject_token", "subject_token_type", "requested_token_type", "audience"); oe != nil {
		return nil, oe
	}

	// The client first: nothing about the subject or the audience is
	// revealed to anyone but the kagent client.
	client, oe := h.authenticate(ctx, r, form)
	x.client = client
	if oe != nil {
		return nil, oe
	}

	if gt := form.Get("grant_type"); gt != GrantType {
		return nil, &oauthError{status: http.StatusBadRequest, Code: "unsupported_grant_type", Description: "only " + GrantType + " is supported"}
	}
	if rt := form.Get("requested_token_type"); rt != "" && rt != TokenTypeAccessToken {
		return nil, invalidRequest("requested_token_type must be " + TokenTypeAccessToken)
	}
	if form.Has("actor_token") {
		return nil, invalidRequest("delegation (actor_token) is not supported")
	}
	subjectToken := form.Get("subject_token")
	if subjectToken == "" {
		return nil, invalidRequest("subject_token is required")
	}
	if !slices.Contains(subjectTokenTypes, form.Get("subject_token_type")) {
		return nil, invalidRequest("subject_token_type must be one of " + strings.Join(subjectTokenTypes, ", "))
	}
	audience := form.Get("audience")
	if audience == "" {
		return nil, invalidRequest("audience is required: a provider instance's name")
	}

	id, err := h.subjects.Subject(ctx, subjectToken)
	if err != nil || id == nil || id.Subject == "" {
		return nil, invalidRequest("subject_token is not a valid Dex token for this installation")
	}
	x.person = id.String()

	if !h.instances[audience] {
		return nil, &oauthError{status: http.StatusBadRequest, Code: "invalid_target", Description: fmt.Sprintf("audience %q is not a provider instance", audience)}
	}
	x.instance = audience

	access, err := h.cfg.Tokens.Access(ctx, id.Subject, audience)
	switch {
	case errors.Is(err, signin.ErrNotSignedIn):
		return nil, h.notSignedIn(audience, "the person has not signed in to "+audience)
	case errors.Is(err, signin.ErrSignInExpired):
		return nil, h.notSignedIn(audience, "the person's sign-in to "+audience+" expired or was revoked")
	case err != nil:
		h.cfg.Logger.ErrorContext(ctx, "token exchange failed", "person", x.person, "instance", audience, "client", client, "error", err)
		return nil, &oauthError{status: http.StatusInternalServerError, Code: "server_error", Description: "the sign-in could not be read or refreshed"}
	}
	res := &response{AccessToken: access.Token, IssuedTokenType: TokenTypeAccessToken, TokenType: "Bearer"}
	if !access.Expiry.IsZero() {
		res.ExpiresIn = max(int64(access.Expiry.Sub(h.cfg.Now()).Seconds()), 0)
	}
	return res, nil
}

// authenticate checks the client: client_secret_basic or client_secret_post,
// not both (RFC 6749 §2.3.1). The secret is compared in constant time.
func (h *Handler) authenticate(ctx context.Context, r *http.Request, form url.Values) (string, *oauthError) {
	id, secret, basic := r.BasicAuth()
	if basic {
		if form.Has("client_secret") {
			return id, invalidRequest("more than one client authentication method")
		}
		// RFC 6749 §2.3.1: Basic credentials are form-urlencoded.
		var err1, err2 error
		id, err1 = url.QueryUnescape(id)
		secret, err2 = url.QueryUnescape(secret)
		if err1 != nil || err2 != nil {
			return "", invalidClient(true)
		}
		if formID := form.Get("client_id"); formID != "" && formID != id {
			return id, invalidRequest("client_id differs from the authenticated client")
		}
	} else {
		id, secret = form.Get("client_id"), form.Get("client_secret")
	}
	if id == "" || secret == "" {
		return id, invalidClient(basic)
	}
	if id != h.cfg.ClientID {
		return id, invalidClient(basic)
	}
	want, err := h.cfg.Secrets.Value(ctx, h.cfg.ClientSecret)
	if err != nil || len(want) == 0 {
		h.cfg.Logger.ErrorContext(ctx, "token exchange: the kagent client secret is not readable", "secret", h.cfg.ClientSecret.String(), "error", err)
		return id, &oauthError{status: http.StatusInternalServerError, Code: "server_error", Description: "the client credentials cannot be checked"}
	}
	if subtle.ConstantTimeCompare([]byte(secret), want) != 1 {
		return id, invalidClient(basic)
	}
	return id, nil
}

func (h *Handler) notSignedIn(instance, why string) *oauthError {
	return &oauthError{
		status:      http.StatusBadRequest,
		Code:        "invalid_target",
		Description: why + "; sign in at error_uri",
		URI:         h.cfg.ConnectURL(instance),
	}
}

// record logs the exchange and counts it: the person, the instance, the
// client and the outcome, never a token.
func (h *Handler) record(ctx context.Context, x exchange, outcome string) {
	attrs := []any{"person", x.person, "instance", x.instance, "client", x.client, "outcome", outcome}
	if outcome == "released" {
		h.cfg.Logger.InfoContext(ctx, "token released", attrs...)
	} else {
		h.cfg.Logger.WarnContext(ctx, "token exchange refused", attrs...)
	}
	if h.exchanges != nil {
		h.exchanges.Add(ctx, 1, metric.WithAttributes(
			attribute.String("provider_instance", x.instance),
			attribute.String("outcome", outcome),
		))
	}
}

// single refuses a parameter given more than once (RFC 6749 §3.2).
func single(form url.Values, names ...string) *oauthError {
	for _, n := range names {
		if len(form[n]) > 1 {
			if n == "audience" {
				return &oauthError{status: http.StatusBadRequest, Code: "invalid_target", Description: "exactly one audience: one provider instance per exchange"}
			}
			return invalidRequest(n + " is given more than once")
		}
	}
	return nil
}

func invalidRequest(desc string) *oauthError {
	return &oauthError{status: http.StatusBadRequest, Code: "invalid_request", Description: desc}
}

func invalidClient(basic bool) *oauthError {
	return &oauthError{status: http.StatusUnauthorized, Code: "invalid_client", Description: "client authentication failed", basic: basic}
}

func writeError(w http.ResponseWriter, oe *oauthError) {
	if oe.basic {
		w.Header().Set("WWW-Authenticate", `Basic realm="token"`)
	}
	writeJSON(w, oe.status, oe)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
