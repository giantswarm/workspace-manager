package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

// revoke calls the instance's revocation endpoint the way its kind says,
// authenticated with the client's credentials. A grant that is already gone
// (404) counts as revoked. Errors carry the status only: a body may echo the
// token.
func (c *Connector) revoke(ctx context.Context, instance string, s provider.SignIn, token, hint string) error {
	secret, err := c.secrets.Value(ctx, s.ClientSecret)
	if err != nil {
		return fmt.Errorf("provider %s: client secret: %w", instance, err)
	}
	var req *http.Request
	switch s.Revocation {
	case provider.RevokeGrant:
		body, err := json.Marshal(map[string]string{"access_token": token})
		if err != nil {
			return err
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodDelete, s.RevocationURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")
	case "", provider.RevokeRFC7009:
		form := url.Values{"token": {token}, "token_type_hint": {hint}}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, s.RevocationURL, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	default:
		return fmt.Errorf("provider %s: unknown revocation method %q", instance, s.Revocation)
	}
	// RFC 6749 §2.3.1: the client credentials are form-encoded first.
	req.SetBasicAuth(url.QueryEscape(s.ClientID), url.QueryEscape(string(secret)))
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("provider %s: revoke: %w", instance, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode/100 == 2, resp.StatusCode == http.StatusNotFound && s.Revocation == provider.RevokeGrant:
		return nil
	default:
		return fmt.Errorf("provider %s refused the revocation: status %d", instance, resp.StatusCode)
	}
}
