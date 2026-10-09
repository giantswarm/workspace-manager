package mirror

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Credential files a provider instance's directory holds, projected from
// the Secret the controller gives the Job.
const (
	credentialUsernameFile = "username"
	credentialTokenFile    = "token"
)

// ServeCredential answers git's credential helper protocol for one
// provider's credential directory: on "get" it reads the request git writes
// and prints the directory's username and token; "store" and "erase" do
// nothing, so git never keeps the credential anywhere. git runs the helper as
// `<command> <directory> <operation>`; the token travels only through this
// pipe, never on a command line, in a URL or in a log.
func ServeCredential(dir, operation string, stdin io.Reader, stdout io.Writer) error {
	if operation != "get" {
		return nil
	}
	if _, err := io.Copy(io.Discard, stdin); err != nil {
		return fmt.Errorf("read credential request: %w", err)
	}
	username, token, err := readCredential(dir)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "username=%s\npassword=%s\n", username, token)
	return err
}

func readCredential(dir string) (username, token string, err error) {
	username, err = readCredentialFile(filepath.Join(dir, credentialUsernameFile))
	if err != nil {
		return "", "", err
	}
	token, err = readCredentialFile(filepath.Join(dir, credentialTokenFile))
	if err != nil {
		return "", "", err
	}
	return username, token, nil
}

func readCredentialFile(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the credential directory is the Job's own
	if err != nil {
		return "", fmt.Errorf("credential: %w", err)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", fmt.Errorf("credential: %s is empty", path)
	}
	return v, nil
}

// hasCredential reports whether the provider's credential directory exists;
// a provider without one is fetched anonymously.
func hasCredential(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("credential: %w", err)
	}
	return info.IsDir(), nil
}

// credentialArgs are git's global options that make the helper the only
// credential source: the helper list is reset, so nothing stores or serves a
// credential besides it. The helper is a shell snippet (the "!" form), each
// word quoted, and git appends the operation to it.
func credentialArgs(helper []string, dir string) []string {
	words := make([]string, 0, len(helper)+1)
	for _, w := range helper {
		words = append(words, shellQuote(w))
	}
	words = append(words, shellQuote(dir))
	return []string{"-c", "credential.helper=", "-c", "credential.helper=!" + strings.Join(words, " ")}
}

// shellQuote quotes one word for the shell git runs a credential helper with.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
