package server

import (
	"context"
	"errors"

	"github.com/giantswarm/mcp-oauth/providers"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

// errNoDexToken refuses a token that proves no Dex identity.
var errNoDexToken = errors.New("no Dex identity token for the subject")

// Subject validates a token on the MCP bearer's terms (the token exchange's
// subject_token): mcp-oauth validates it (signature, expiry, issuer and a
// trusted audience for a forwarded Dex id_token; the store for this server's
// own), and it must yield the caller's Dex token. A forged, expired or foreign
// token is refused.
func (o *oauthRuntime) Subject(ctx context.Context, token string) (*identity.Identity, error) {
	info, err := o.server.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if info == nil || info.ID == "" {
		return nil, errNoDexToken
	}
	if o.dexToken(ctx, token, info) == "" {
		return nil, errNoDexToken
	}
	return identityOf(info), nil
}

// identityOf is the request identity of a validated mcp-oauth user.
func identityOf(info *providers.UserInfo) *identity.Identity {
	id := &identity.Identity{Subject: info.ID, Email: info.Email, Name: info.Name, Groups: info.Groups, Source: identity.SourceOAuth}
	if info.IsSSO() {
		id.Source = identity.SourceSSO
	}
	return id
}
