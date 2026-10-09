package cmd

import (
	"context"
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
	kube            kubeFlags
}

func newSyncCmd() *cobra.Command {
	o := &syncOptions{}
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mirror a workspace's repositories onto its volume (the sync Job)",
		Long: `Run one sync of a workspace's volume, as the Job the controller starts per
cycle: clone the repositories added to the selection as bare mirrors under
mirrors/<owner>/<name>.git, fetch those pushed to since the last sync, leave
the others untouched, mark the mirrors of repositories that left the
selection, and write the manifest (head commits, push times, measured sizes)
to the volume and, with --result-configmap, as the Job's result.

The selection is the controller's ConfigMap mounted as a file. Each provider
instance's credential is a directory under --credentials-dir with the files
username and token, projected from a Secret; git obtains it through this
binary's credential helper, so no token reaches a URL, the volume or a log.
A provider without a directory is fetched anonymously.

While a session directory's clone borrows a mirror's objects, the sync keeps
every object of that mirror, reachable or not; a clone or fetch error fails
the Job and leaves the previous manifest in place.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSync(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.volume, "volume", envOr("WORKSPACE_MANAGER_SYNC_VOLUME", "/workspace"), "Mount point of the workspace's volume (WORKSPACE_MANAGER_SYNC_VOLUME)")
	f.StringVar(&o.selection, "selection", envOr("WORKSPACE_MANAGER_SYNC_SELECTION", "/etc/workspace-sync/selection.json"), "The selection file: the repositories resolved for the workspace, with provider, owner, name, cloneUrl, defaultBranch and pushedAt each (WORKSPACE_MANAGER_SYNC_SELECTION)")
	f.StringVar(&o.credentialsDir, "credentials-dir", envOr("WORKSPACE_MANAGER_SYNC_CREDENTIALS_DIR", "/var/run/secrets/workspace-sync"), "Directory with one directory per provider instance holding the files username and token (WORKSPACE_MANAGER_SYNC_CREDENTIALS_DIR)")
	f.IntVar(&o.parallel, "parallel", envInt("WORKSPACE_MANAGER_SYNC_PARALLEL", mirror.DefaultParallel), "Clones and fetches run at once (WORKSPACE_MANAGER_SYNC_PARALLEL)")
	f.StringVar(&o.resultConfigMap, "result-configmap", envOr("WORKSPACE_MANAGER_SYNC_RESULT_CONFIGMAP", ""), "ConfigMap in --namespace to write the manifest into under manifest.json, the Job's result; empty writes it to the volume only (WORKSPACE_MANAGER_SYNC_RESULT_CONFIGMAP)")
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
		Log:              log,
	}
	log.Info("sync starting", "version", build.Version, "volume", o.volume, "repositories", len(sel.Repositories), "parallel", o.parallel)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := s.Run(ctx, sel)
	if err != nil {
		return err
	}
	if clients != nil {
		raw, err := res.Manifest.JSON()
		if err != nil {
			return err
		}
		if err := kube.WriteConfigMap(ctx, clients.Typed(), o.namespace, o.resultConfigMap, map[string]string{mirror.ManifestFile: string(raw)}); err != nil {
			return err
		}
		log.Info("manifest written as the Job's result", "configmap", o.namespace+"/"+o.resultConfigMap)
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
