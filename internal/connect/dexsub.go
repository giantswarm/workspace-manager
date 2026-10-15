package connect

import (
	"encoding/base64"
	"strings"

	"github.com/giantswarm/mcp-oauth/providers/oidc"
	"google.golang.org/protobuf/encoding/protowire"
)

// Dex's IDTokenSubject fields: the user's id at the connector, and the
// connector's id.
const (
	dexSubjectUserID    protowire.Number = 1
	dexSubjectConnector protowire.Number = 2
)

// dexConnector is the connector a Dex subject was signed in through. Dex's
// `sub` is the unpadded base64 of an IDTokenSubject protobuf, so one person
// signed in through two connectors of the same identity provider has two
// subjects. It is "" for a subject that is no Dex subject or names no valid
// connector id.
func dexConnector(sub string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(sub, "="))
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(sub, "=")); err != nil {
			return ""
		}
	}
	var user, connector string
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return ""
		}
		raw = raw[n:]
		if typ != protowire.BytesType {
			if n = protowire.ConsumeFieldValue(num, typ, raw); n < 0 {
				return ""
			}
			raw = raw[n:]
			continue
		}
		v, n := protowire.ConsumeBytes(raw)
		if n < 0 {
			return ""
		}
		raw = raw[n:]
		switch num {
		case dexSubjectUserID:
			user = string(v)
		case dexSubjectConnector:
			connector = string(v)
		}
	}
	if user == "" || oidc.ValidateConnectorID(connector) != nil {
		return ""
	}
	return connector
}
