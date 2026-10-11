package grant

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// JWKSPath is where the grant's verification keys are published.
const JWKSPath = "/.well-known/jwks.json"

// JWKS serves the published keys, the signing key's and the others' in its
// Secret, without authentication: they verify grants and sign nothing.
func JWKS(keys *KeySource, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k, err := keys.Keys(r.Context())
		if err != nil {
			log.WarnContext(r.Context(), "grant keys not published", "error", err)
			http.Error(w, "the grant keys are unavailable", http.StatusServiceUnavailable)
			return
		}
		raw, err := json.Marshal(k.Public)
		if err != nil {
			http.Error(w, fmt.Sprintf("encode keys: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(KeysTTL.Seconds())))
		_, _ = w.Write(raw)
	})
}
