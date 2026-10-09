package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// gitEnv makes every git call hermetic: no system or global configuration
// (a credential store or URL rewrite of the image's git would otherwise
// apply) and no prompt, so a missing credential fails instead of hanging.
var gitEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_TERMINAL_PROMPT=0",
	"LC_ALL=C",
}

// gitBaseArgs precede every subcommand. The volume is a network filesystem
// whose uid mapping may differ from the Job's, which git would otherwise
// refuse as dubious ownership; the volume is the sync's own.
var gitBaseArgs = []string{"-c", "safe.directory=*"}

type git struct {
	bin string
	log *slog.Logger
}

func newGit(bin string, log *slog.Logger) (*git, error) {
	if bin == "" {
		bin = "git"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	return &git{bin: path, log: log}, nil
}

// run executes git in dir and returns its combined output. The output never
// carries a credential: git prints URLs without one, and the credential
// helper's command line names only the credential directory.
func (g *git) run(ctx context.Context, dir string, args ...string) (string, error) {
	full := append(append([]string{}, gitBaseArgs...), args...)
	cmd := exec.CommandContext(ctx, g.bin, full...) //nolint:gosec // the arguments are the sync's own
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	g.log.Debug("git", "dir", dir, "args", args, "output", strings.TrimSpace(out.String()), "error", err)
	if err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", subcommand(args), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// subcommand names the git subcommand among the arguments, past the -c
// options that precede it.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
}

// output runs git and returns its trimmed stdout.
func (g *git) output(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := g.run(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// revParse resolves a revision to a commit, "" when there is none (an empty
// repository, or a branch the mirror does not have).
func (g *git) revParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := g.output(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	return out, err
}

// setHead points the mirror's HEAD at the default branch, which a mirror
// clone fixes at clone time and a provider may change later.
func (g *git) setHead(ctx context.Context, dir, branch string) error {
	_, err := g.run(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return err
}
