package connect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/giantswarm/workspace-manager/internal/signin"
)

// Seal contexts keep each sealed value to its use: a state does not open as
// a session, a state for one instance does not open for another.
const (
	stateContext   = "workspace-manager/connect/state/"
	sessionContext = "workspace-manager/connect/session"
	signInContext  = "workspace-manager/connect/signin"
)

// flowState is a provider sign-in's state.
type flowState struct {
	Person   string `json:"p"`
	Instance string `json:"i"`
	Verifier string `json:"v"`
	Expiry   int64  `json:"e"`
}

func (c *Connector) sealState(st flowState) (string, error) {
	return seal(c.keyring, stateContext+st.Instance, st)
}

func (c *Connector) openState(instance, state string) (flowState, error) {
	var st flowState
	if err := open(c.keyring, stateContext+instance, state, &st); err != nil {
		return flowState{}, ErrInvalidState
	}
	if st.Instance != instance || st.Person == "" || st.Verifier == "" || !c.now().Before(time.Unix(st.Expiry, 0)) {
		return flowState{}, ErrInvalidState
	}
	return st, nil
}

// seal encodes v as JSON, sealed for context, in URL-safe base64.
func seal(k *signin.Keyring, context string, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sealed, err := k.Seal(plain, context)
	if err != nil {
		return "", fmt.Errorf("connect: seal: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open reverses seal; any failure is the same error, naming nothing.
func open(k *signin.Keyring, context, s string, v any) error {
	sealed, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return signin.ErrUnsealable
	}
	plain, err := k.Open(sealed, context)
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, v)
}
