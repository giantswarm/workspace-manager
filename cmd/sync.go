package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/giantswarm/workspace-manager/internal/kube"
	"github.com/giantswarm/workspace-manager/internal/mirror"
)

type syncOptions struct {
	volume          string
	selection       string
	credentialsDir  string
	parallel        int
	resultConfigMap string
	namespace       string
	reportsDir      string
	ownerReport     string
	kube            kubeFlags
}

func newSyncCmd() *cobra.Command {
	o := &syncOptions{}
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mirror a workspace's repositories onto its volume (the sync Job)",
		Long: `Run one sync of a workspace's volume, as the Job the controller starts per
cycle: clean up the session directories under sessions/ from the selection's
cleanup (each Session's last turn as kagent lists it), clone the repositories
added to the selection as bare mirrors under mirrors/<owner>/<name>.git, fetch
those pushed to since the last sync, leave the others untouched, mark the
mirrors of repositories that left the selection and delete the marked ones
no session directory borrows from any more, and write the manifest (head
commits, push times, measured sizes, the session directories left) to the
volume and, with --result-configmap, as the Job's result. A stopped
selection, a deleted workspace's, touches no mirror.

The selection is the controller's ConfigMap mounted as a file. A provider
instance's credential for one owner is the directory <provider>/<owner> under
--credentials-dir with the files username and token, projected from a Secret;
git obtains it through this binary's credential helper, so no token reaches a
URL, the volume or a log. An owner without a directory is fetched anonymously.

While a session directory's clone borrows a mirror's objects, the sync keeps
every object of that mirror, reachable or not; a clone or fetch error fails
the Job and leaves the previous manifest in place.

A runtime creates a Session's directory as the Session's user, and a volume
that keeps every creator's uid and mode leaves it to that user: the sync
records such a directory's owner in the manifest, and the next cycle's Job
runs a container as each owner first, with --owner-report, which looks after
that owner's directories alone and writes what it did into the Job's reports
directory (--reports-dir) for the sync to take into the manifest. A directory
the sync can neither measure nor delete stays, with the reason; it never
fails the sync.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSync(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.volume, "volume", envOr("WORKSPACE_MANAGER_SYNC_VOLUME", "/workspace"), "Mount point of the workspace's volume (WORKSPACE_MANAGER_SYNC_VOLUME)")
	f.StringVar(&o.selection, "selection", envOr("WORKSPACE_MANAGER_SYNC_SELECTION", "/etc/workspace-sync/selection.json"), "The selection file: the repositories resolved for the workspace, with provider, owner, name, cloneUrl, defaultBranch and pushedAt each (WORKSPACE_MANAGER_SYNC_SELECTION)")
	f.StringVar(&o.credentialsDir, "credentials-dir", envOr("WORKSPACE_MANAGER_SYNC_CREDENTIALS_DIR", "/var/run/secrets/workspace-sync"), "Directory with one directory <provider>/<owner> per provider instance and owner holding the files username and token (WORKSPACE_MANAGER_SYNC_CREDENTIALS_DIR)")
	f.IntVar(&o.parallel, "parallel", envInt("WORKSPACE_MANAGER_SYNC_PARALLEL", mirror.DefaultParallel), "Clones and fetches run at once (WORKSPACE_MANAGER_SYNC_PARALLEL)")
	f.StringVar(&o.resultConfigMap, "result-configmap", envOr("WORKSPACE_MANAGER_SYNC_RESULT_CONFIGMAP", ""), "ConfigMap in --namespace to write the manifest into under manifest.json, the Job's result; empty writes it to the volume only (WORKSPACE_MANAGER_SYNC_RESULT_CONFIGMAP)")
	f.StringVar(&o.reportsDir, "reports-dir", envOr("WORKSPACE_MANAGER_SYNC_REPORTS_DIR", ""), "Directory the Job's owners' containers wrote their reports into, read before the session directories are looked after; empty reads none (WORKSPACE_MANAGER_SYNC_REPORTS_DIR)")
	f.StringVar(&o.ownerReport, "owner-report", "", "Look after the session directories the uid this runs as owns, and nothing else, and write what was done to this file: the run of the Job's container of a directory owner's")
	f.StringVar(&o.namespace, "namespace", envOr("WORKSPACE_MANAGER_NAMESPACE", "kagent"), "Namespace of the result ConfigMap (WORKSPACE_MANAGER_NAMESPACE)")
	o.kube.add(f)
	return cmd
}

func runSync(ctx context.Context, o *syncOptions) error {
	log := slog.Default()
	sel, err := mirror.ReadSelection(o.selection)
	if err != nil {
		return err
	}
	if o.ownerReport != "" {
		return runOwner(o, sel)
	}
	// A result that cannot be written is found out before any fetch.
	var clients *kube.Clients
	if o.resultConfigMap != "" {
		if clients, err = kube.New(o.kube.config()); err != nil {
			return fmt.Errorf("the result ConfigMap needs Kubernetes access: %w", err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the credential helper: %w", err)
	}
	s := &mirror.Syncer{
		Volume:           o.volume,
		CredentialsDir:   o.credentialsDir,
		CredentialHelper: []string{exe, "git-credential"},
		Parallel:         o.parallel,
		ReportsDir:       o.reportsDir,
		Log:              log,
	}
	log.Info("sync starting", "version", build.Version, "volume", o.volume, "repositories", len(sel.Repositories), "parallel", o.parallel)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := s.Run(ctx, sel)
	if err != nil {
		if clients != nil {
			// The error goes beside the previous manifest, which stays: the
			// controller reports it as the cycle's failure.
			if werr := kube.MergeConfigMap(context.WithoutCancel(ctx), clients.Typed(), o.namespace, o.resultConfigMap, map[string]string{mirror.ErrorKey: err.Error()}); werr != nil {
				log.Warn("sync error not written as the Job's result", "configmap", o.namespace+"/"+o.resultConfigMap, "error", werr)
			}
		}
		return err
	}
	if clients != nil {
		raw, err := res.Manifest.JSON()
		if err != nil {
			return err
		}
		removed, err := json.Marshal(res.Removal())
		if err != nil {
			return fmt.Errorf("encode what the sync removed: %w", err)
		}
		// Replaces the data, so a previous run's error and removals go.
		data := map[string]string{mirror.ManifestFile: string(raw), mirror.RemovedKey: string(removed)}
		if err := kube.WriteConfigMap(ctx, clients.Typed(), o.namespace, o.resultConfigMap, data); err != nil {
			return err
		}
		log.Info("manifest written as the Job's result", "configmap", o.namespace+"/"+o.resultConfigMap)
	}
	return nil
}

// runOwner is the sync of the Job's container of a directory owner's: the
// session directories that owner owns, reported to the sync that follows.
func runOwner(o *syncOptions, sel *mirror.Selection) error {
	s := &mirror.Syncer{Volume: o.volume, Log: slog.Default()}
	slog.Default().Info("sync of an owner's session directories starting", "version", build.Version, "volume", o.volume, "uid", os.Getuid(), "gid", os.Getgid())
	r, err := s.RunOwner(sel)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode the owner's report: %w", err)
	}
	if err := os.WriteFile(o.ownerReport, raw, 0o644); err != nil { //nolint:gosec // the sync, another uid, reads it
		return fmt.Errorf("write the owner's report: %w", err)
	}
	return nil
}

// newGitCredentialCmd is the credential helper the sync configures for git:
// `workspace-manager git-credential <credential dir> <operation>`.
func newGitCredentialCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-credential <credential-dir> <operation>",
		Short:  "Serve a provider's credential to git (the sync's credential helper)",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mirror.ServeCredential(args[0], args[1], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
