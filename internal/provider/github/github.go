// Package github is the GitHub provider kind: github.com or a GitHub
// Enterprise Server. A GitHub App is the provider on an installation: its
// installation token is the sync credential, and its user-to-server sign-in is
// the person's sign-in. Only internal/provider/kinds imports this package.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

// KindName is the kind's name in the provider configuration.
const KindName = "github"

// Values are an instance's settings.
type Values struct {
	// URL is the web URL: https://github.com (the default) or a GitHub
	// Enterprise Server's.
	URL string `json:"url,omitempty"`
	// APIURL is the REST API's base URL. Empty derives it from URL:
	// https://api.github.com for github.com, `<url>/api/v3` otherwise.
	APIURL string `json:"apiURL,omitempty"`
	// App is the GitHub App the sync authenticates as.
	App AppValues `json:"app"`
	// OAuth is the App's OAuth client, for the person's sign-in.
	OAuth OAuthValues `json:"oauth"`
}

// AppValues identify the GitHub App.
type AppValues struct {
	// ID is the App's ID or client ID, the issuer of its JWTs.
	ID string `json:"id"`
	// PrivateKey is the App's PEM private key.
	PrivateKey provider.SecretRef `json:"privateKey"`
}

// OAuthValues are the App's OAuth client.
type OAuthValues struct {
	ClientID     string             `json:"clientID"`
	ClientSecret provider.SecretRef `json:"clientSecret"`
}

// Factory builds a GitHub instance; http is the client the API calls use (nil
// for http.DefaultClient).
func Factory(hc *http.Client) provider.Factory {
	return func(raw json.RawMessage) (provider.Kind, error) {
		var v Values
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("values: %w", err)
		}
		return New(v, hc)
	}
}

// Kind is a configured GitHub instance.
type Kind struct {
	v    Values
	web  *url.URL
	api  *url.URL
	http *http.Client
	now  func() time.Time
}

// New validates v and builds the instance.
func New(v Values, hc *http.Client) (*Kind, error) {
	if v.URL == "" {
		v.URL = "https://github.com"
	}
	web, err := parseBase("url", v.URL)
	if err != nil {
		return nil, err
	}
	if v.APIURL == "" {
		if web.Host == "github.com" {
			v.APIURL = "https://api.github.com"
		} else {
			v.APIURL = strings.TrimSuffix(web.String(), "/") + "/api/v3"
		}
	}
	api, err := parseBase("apiURL", v.APIURL)
	if err != nil {
		return nil, err
	}
	var missing []string
	for field, empty := range map[string]bool{
		"app.id":                  v.App.ID == "",
		"app.privateKey.name":     v.App.PrivateKey.Name == "",
		"app.privateKey.key":      v.App.PrivateKey.Key == "",
		"oauth.clientID":          v.OAuth.ClientID == "",
		"oauth.clientSecret.name": v.OAuth.ClientSecret.Name == "",
		"oauth.clientSecret.key":  v.OAuth.ClientSecret.Key == "",
	} {
		if empty {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("values: %s required", strings.Join(missing, ", "))
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Kind{v: v, web: web, api: api, http: hc, now: time.Now}, nil
}

func parseBase(field, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("values: %s must be an absolute http(s) URL, got %q", field, raw)
	}
	return u, nil
}

// SecretRefs implements provider.Kind.
func (k *Kind) SecretRefs() []provider.SecretRef {
	return []provider.SecretRef{k.v.App.PrivateKey, k.v.OAuth.ClientSecret}
}

// SignIn implements provider.Kind: a GitHub App's user-to-server sign-in,
// which takes the App's permissions rather than scopes.
func (k *Kind) SignIn() provider.SignIn {
	web := k.web.String()
	return provider.SignIn{
		AuthURL:  web + "/login/oauth/authorize",
		TokenURL: web + "/login/oauth/access_token",
		// DELETE with the client's Basic credentials and the token in the
		// body revokes the person's grant.
		RevocationURL: k.api.String() + "/applications/" + url.PathEscape(k.v.OAuth.ClientID) + "/grant",
		Revocation:    provider.RevokeGrant,
		ClientID:      k.v.OAuth.ClientID,
		ClientSecret:  k.v.OAuth.ClientSecret,
	}
}

// Hosts implements provider.Kind. git takes a token as the password of any
// user; `x-access-token` is the name GitHub documents for App tokens.
func (k *Kind) Hosts() provider.Hosts {
	h := provider.Hosts{
		Git: []provider.Host{{Name: k.web.Host, Scheme: provider.SchemeBasic, User: "x-access-token"}},
		CLI: "gh",
	}
	h.API = []provider.Host{{Name: k.api.Host, Scheme: provider.SchemeBearer}}
	return h
}

// List implements provider.Kind over the owner's repositories, an
// organization's or a user account's.
func (k *Kind) List(ctx context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, error) {
	var account struct {
		Type string `json:"type"`
	}
	if err := k.get(ctx, cred, "/users/"+url.PathEscape(owner), &account); err != nil {
		return nil, fmt.Errorf("owner %s: %w", owner, err)
	}
	path := "/users/" + url.PathEscape(owner) + "/repos?type=owner&per_page=100"
	if account.Type == "Organization" {
		path = "/orgs/" + url.PathEscape(owner) + "/repos?type=all&per_page=100"
	}
	var items []provider.Item
	for next := k.api.String() + path; next != ""; {
		var page []repository
		var err error
		if next, err = k.getPage(ctx, cred, next, &page); err != nil {
			return nil, fmt.Errorf("listing %s: %w", owner, err)
		}
		for _, r := range page {
			items = append(items, provider.Item{
				Owner:         owner,
				Name:          r.Name,
				Language:      r.Language,
				Topics:        r.Topics,
				Archived:      r.Archived,
				Fork:          r.Fork,
				LastChange:    r.PushedAt,
				CloneURL:      r.CloneURL,
				DefaultBranch: r.DefaultBranch,
			})
		}
	}
	return items, nil
}

type repository struct {
	Name          string    `json:"name"`
	Language      string    `json:"language"`
	Topics        []string  `json:"topics"`
	Archived      bool      `json:"archived"`
	Fork          bool      `json:"fork"`
	PushedAt      time.Time `json:"pushed_at"`
	CloneURL      string    `json:"clone_url"`
	DefaultBranch string    `json:"default_branch"`
}

func (k *Kind) get(ctx context.Context, cred oauth2.TokenSource, path string, out any) error {
	_, err := k.getPage(ctx, cred, k.api.String()+path, out)
	return err
}

// getPage GETs one page and returns the URL of the next one, empty on the
// last.
func (k *Kind) getPage(ctx context.Context, cred oauth2.TokenSource, u string, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if cred != nil {
		tok, err := cred.Token()
		if err != nil {
			return "", fmt.Errorf("credential: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	header, err := k.do(req, http.StatusOK, out)
	if err != nil {
		return "", err
	}
	return nextLink(header.Get("Link")), nil
}

// do sends req and decodes a response of status want into out.
func (k *Kind) do(req *http.Request, want int, out any) (http.Header, error) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, &statusError{method: req.Method, path: req.URL.Path, status: resp.StatusCode, message: e.Message}
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

// nextLink extracts rel="next" from a Link header.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(segs[0]), "<>")
			}
		}
	}
	return ""
}

// statusError is an API answer other than the one expected.
type statusError struct {
	method, path string
	status       int
	message      string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: %d %s %s", e.method, e.path, e.status, http.StatusText(e.status), e.message)
}
