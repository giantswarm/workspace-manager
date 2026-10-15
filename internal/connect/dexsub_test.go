package connect

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/giantswarm/workspace-manager/internal/connect/connecttest"
)

func TestDexConnector(t *testing.T) {
	for name, tc := range map[string]struct{ sub, want string }{
		// The example from Dex's documentation: user 08a8684b-… at "local".
		"dex subject":          {"CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxZjU0NjYSBWxvY2Fs", "local"},
		"padded subject":       {connecttest.Subject("u1", "giantswarm-ad") + "==", "giantswarm-ad"},
		"standard base64":      {base64.RawStdEncoding.EncodeToString([]byte("\n\x02u1\x12\x11giantswarm-github")), "giantswarm-github"},
		"connector of a sub":   {connecttest.Subject("1234567", "giantswarm-github"), "giantswarm-github"},
		"plain subject":        {"alice", ""},
		"empty":                {"", ""},
		"no connector":         {base64.RawURLEncoding.EncodeToString([]byte("\n\x02u1")), ""},
		"no user":              {connecttest.Subject("", "giantswarm-ad"), ""},
		"invalid connector id": {connecttest.Subject("u1", "ad&prompt=none"), ""},
		"truncated protobuf":   {base64.RawURLEncoding.EncodeToString([]byte("\n\x09u1")), ""},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, dexConnector(tc.sub))
		})
	}
}
