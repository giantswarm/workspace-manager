package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/mirror"
)

// LabelSync labels a cycle's Job with the cycle's id.
const LabelSync = "workspace-manager.giantswarm.io/sync"

// FieldManager is the manager's name on the fields it applies.
const FieldManager = "workspace-manager"

// SyncContainer is the name of the sync Job template's container that runs
// `workspace-manager sync`; the cycle wires the volume, the selection and the
// credentials into it.
const SyncContainer = "sync"

// Where the sync Job finds its input: the sync subcommand's defaults. The
// selection ConfigMap carries the roots beside the selection.
const (
	volumeMountPath      = "/workspace"
	selectionMountPath   = "/etc/workspace-sync"
	credentialsMountPath = "/var/run/secrets/workspace-sync" //nolint:gosec // G101: a mount path, not a credential
	selectionFile        = "selection.json"
	rootsFile            = "ca.crt"
	reportsMountPath     = "/var/run/workspace-sync-owners"
)

// ownerContainerPrefix names the containers that run the session
// directories' part of the sync as their owners, before the sync container.
const ownerContainerPrefix = "owner-"

// The names of what a Workspace's sync cycle uses, in the Workspace's
// namespace.
func ManifestName(ws *v1alpha1.Workspace) string  { return "workspace-" + ws.Name + "-manifest" }
func SelectionName(ws *v1alpha1.Workspace) string { return "workspace-" + ws.Name + "-selection" }
func CredentialsName(ws *v1alpha1.Workspace) string {
	return "workspace-" + ws.Name + "-sync-credentials"
}
func JobName(ws *v1alpha1.Workspace, id string) string {
	return "workspace-" + ws.Name + "-sync-" + id
}

// Credential is a provider instance's sync credential for one owner, as git
// takes it over HTTPS.
type Credential struct {
	Provider, Owner, Username, Token string
}

// Jobs starts a cycle's sync Job, from the template the chart renders: the
// pod's shape (image, account, security contexts, resources, deadline, TTL)
// is the template's; the cycle adds the volume, the selection, the roots and
// the credentials.
type Jobs struct {
	Client kubernetes.Interface
	// Template is the Job every cycle's Job is made from; nil is
	// DefaultJobTemplate.
	Template *batchv1.Job
	// Image runs the Job, the manager's own; empty keeps the template's.
	Image string
	// ImagePullPolicy goes with Image.
	ImagePullPolicy corev1.PullPolicy
	// Roots is the PEM bundle the sync verifies the providers' git hosts
	// with, the manager's own: it travels in the cycle's input, since a
	// Secret of the manager's namespace is out of the Job's reach, and the
	// sync container gets it as its SSL_CERT_FILE. Empty hands none and the
	// image's roots apply.
	Roots []byte
}

// DefaultJobTemplate is the Job without the chart's template: the sync
// container alone, run as account.
func DefaultJobTemplate(account string) *batchv1.Job {
	return &batchv1.Job{
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: account,
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers:         []corev1.Container{{Name: SyncContainer, Args: []string{"sync"}}},
				},
			},
		},
	}
}

// LoadJobTemplate reads a Job template (YAML or JSON) and validates it.
func LoadJobTemplate(file string) (*batchv1.Job, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // the template path is the operator's own flag
	if err != nil {
		return nil, fmt.Errorf("sync job template: %w", err)
	}
	var job batchv1.Job
	if err := yaml.UnmarshalStrict(raw, &job); err != nil {
		return nil, fmt.Errorf("sync job template %s: %w", file, err)
	}
	if err := ValidateJobTemplate(&job); err != nil {
		return nil, fmt.Errorf("sync job template %s: %w", file, err)
	}
	return &job, nil
}

// ValidateJobTemplate refuses a template without the sync container or with
// a pod that restarts.
func ValidateJobTemplate(job *batchv1.Job) error {
	if _, err := syncContainer(&job.Spec.Template.Spec); err != nil {
		return err
	}
	if p := job.Spec.Template.Spec.RestartPolicy; p != corev1.RestartPolicyNever && p != corev1.RestartPolicyOnFailure {
		return fmt.Errorf("the pod's restartPolicy must be Never or OnFailure, got %q", p)
	}
	return nil
}

func syncContainer(pod *corev1.PodSpec) (int, error) {
	for i := range pod.Containers {
		if pod.Containers[i].Name == SyncContainer {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no container named %q", SyncContainer)
}

// Start writes the cycle's input and creates its Job: for each owner of the
// session directories the last manifest records, a container of the Job runs
// as that owner before the sync container (Jobs.build); the manifest ConfigMap
// the Job writes its result into (made ahead, owned by the Workspace so it
// stays with it), the selection ConfigMap (the selection and the roots), the
// credentials Secret (one per Workspace, replaced per cycle and emptied after
// it) and the Job itself, owned by the Workspace.
func (j Jobs) Start(ctx context.Context, ws *v1alpha1.Workspace, id string, sel *mirror.Selection, creds []Credential) (*batchv1.Job, error) {
	owner := *metav1.NewControllerRef(ws, v1alpha1.GroupVersion.WithKind("Workspace"))
	labels := map[string]string{LabelWorkspace: ws.Name}
	cms := j.Client.CoreV1().ConfigMaps(ws.Namespace)

	manifest := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ManifestName(ws), Namespace: ws.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}}}
	if _, err := cms.Create(ctx, manifest, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create the manifest configmap %s: %w", manifest.Name, err)
	}
	owners, err := j.owners(ctx, ws)
	if err != nil {
		return nil, err
	}

	raw, err := json.MarshalIndent(sel, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the selection: %w", err)
	}
	selection := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: SelectionName(ws), Namespace: ws.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
		Data: map[string]string{selectionFile: string(raw)}}
	if len(j.Roots) > 0 {
		selection.Data[rootsFile] = string(j.Roots)
	}
	if err := writeConfigMap(ctx, j.Client, selection); err != nil {
		return nil, err
	}

	items, err := j.applyCredentials(ctx, ws, owner, labels, creds)
	if err != nil {
		return nil, err
	}

	job, err := j.build(ws, id, owner, items, owners)
	if err != nil {
		return nil, err
	}
	created, err := j.Client.BatchV1().Jobs(ws.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create the sync job %s: %w", job.Name, err)
	}
	return created, nil
}

// Get returns the named sync Job, nil when it is gone.
func (j Jobs) Get(ctx context.Context, ws *v1alpha1.Workspace, name string) (*batchv1.Job, error) {
	job, err := j.Client.BatchV1().Jobs(ws.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get the sync job %s: %w", name, err)
	}
	return job, nil
}

// Finished is the condition a Job ended with, Complete or Failed; nil while
// it runs.
func Finished(job *batchv1.Job) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

// ClearCredentials empties the Workspace's credentials Secret once its Job
// ended: the credential lives only while a sync runs.
func (j Jobs) ClearCredentials(ctx context.Context, ws *v1alpha1.Workspace) error {
	owner := *metav1.NewControllerRef(ws, v1alpha1.GroupVersion.WithKind("Workspace"))
	_, err := j.applyCredentials(ctx, ws, owner, map[string]string{LabelWorkspace: ws.Name}, nil)
	return err
}

// applyCredentials writes the credentials Secret, server-side applied so the
// manager never reads a Secret of the namespace: the fields it applied last
// time and leaves out now go. It returns the items that project each
// credential to <provider>/<owner>/{username,token}.
func (j Jobs) applyCredentials(ctx context.Context, ws *v1alpha1.Workspace, owner metav1.OwnerReference, labels map[string]string, creds []Credential) ([]corev1.KeyToPath, error) {
	data := map[string][]byte{}
	var items []corev1.KeyToPath
	for i, c := range creds {
		if c.Username == "" || c.Token == "" {
			return nil, fmt.Errorf("the %s credential for %s is empty", c.Provider, c.Owner)
		}
		prefix := strconv.Itoa(i)
		data[prefix+".username"], data[prefix+".token"] = []byte(c.Username), []byte(c.Token)
		items = append(items,
			corev1.KeyToPath{Key: prefix + ".username", Path: path.Join(mirror.CredentialDir(c.Provider, c.Owner), mirror.CredentialUsernameFile)},
			corev1.KeyToPath{Key: prefix + ".token", Path: path.Join(mirror.CredentialDir(c.Provider, c.Owner), mirror.CredentialTokenFile)})
	}
	cfg := corev1apply.Secret(CredentialsName(ws), ws.Namespace).
		WithLabels(labels).
		WithOwnerReferences(metav1apply.OwnerReference().
			WithAPIVersion(owner.APIVersion).WithKind(owner.Kind).WithName(owner.Name).WithUID(owner.UID).
			WithController(true).WithBlockOwnerDeletion(true)).
		WithType(corev1.SecretTypeOpaque).
		WithData(data)
	if _, err := j.Client.CoreV1().Secrets(ws.Namespace).Apply(ctx, cfg, metav1.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
		return nil, fmt.Errorf("write the credentials secret %s: %w", CredentialsName(ws), err)
	}
	return items, nil
}

// owners are the owners of the session directories the last manifest
// records as left to another uid.
func (j Jobs) owners(ctx context.Context, ws *v1alpha1.Workspace) ([]mirror.Owner, error) {
	cm, err := j.Client.CoreV1().ConfigMaps(ws.Namespace).Get(ctx, ManifestName(ws), metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get the manifest configmap %s: %w", ManifestName(ws), err)
	}
	raw := cm.Data[mirror.ManifestFile]
	if raw == "" {
		return nil, nil
	}
	var m mirror.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parse the manifest configmap %s: %w", ManifestName(ws), err)
	}
	return m.Owners, nil
}

// build is the cycle's Job from the template.
func (j Jobs) build(ws *v1alpha1.Workspace, id string, owner metav1.OwnerReference, credentialItems []corev1.KeyToPath, owners []mirror.Owner) (*batchv1.Job, error) {
	tpl := j.Template
	if tpl == nil {
		return nil, errors.New("no sync job template")
	}
	job := tpl.DeepCopy()
	job.TypeMeta = metav1.TypeMeta{}
	job.ObjectMeta = metav1.ObjectMeta{
		Name:            JobName(ws, id),
		Namespace:       ws.Namespace,
		Labels:          withLabels(tpl.Labels, ws.Name, id),
		Annotations:     tpl.Annotations,
		OwnerReferences: []metav1.OwnerReference{owner},
	}
	pod := &job.Spec.Template
	pod.Labels = withLabels(pod.Labels, ws.Name, id)
	i, err := syncContainer(&pod.Spec)
	if err != nil {
		return nil, err
	}
	c := &pod.Spec.Containers[i]
	if j.Image != "" {
		c.Image = j.Image
		c.ImagePullPolicy = j.ImagePullPolicy
	}
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "WORKSPACE_MANAGER_NAMESPACE", Value: ws.Namespace},
		corev1.EnvVar{Name: "WORKSPACE_MANAGER_SYNC_RESULT_CONFIGMAP", Value: ManifestName(ws)},
		corev1.EnvVar{Name: "KUBERNETES_IN_CLUSTER", Value: "true"},
	)
	if len(j.Roots) > 0 {
		// The sync hands SSL_CERT_FILE to git (mirror.gitEnviron).
		c.Env = append(c.Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: path.Join(selectionMountPath, rootsFile)})
	}
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{Name: "workspace", MountPath: volumeMountPath},
		corev1.VolumeMount{Name: "selection", MountPath: selectionMountPath, ReadOnly: true},
		corev1.VolumeMount{Name: "credentials", MountPath: credentialsMountPath, ReadOnly: true},
	)
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: ClaimName(ws)}}},
		corev1.Volume{Name: "selection", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: SelectionName(ws)}}}},
		corev1.Volume{Name: "credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: CredentialsName(ws), Items: credentialItems, DefaultMode: ptr.To[int32](0o400)}}},
	)
	if len(owners) > 0 {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, ownerContainers(*c, owners)...)
		c.Env = append(c.Env, corev1.EnvVar{Name: "WORKSPACE_MANAGER_SYNC_REPORTS_DIR", Value: reportsMountPath})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "owners", MountPath: reportsMountPath, ReadOnly: true})
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "owners", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1Mi"))}}})
	}
	return job, nil
}

// ownerContainers run the session directories' part of the sync as each
// owner, the uid and gid of its directories; the pod's fsGroup stays a
// supplementary group, which writes sessions/. Each is the sync container's
// image, resources and security context with the owner's uid and gid, the
// volume and the selection, and no credential or Kubernetes access: it
// reports into the owners' directory, which the sync container reads.
func ownerContainers(sync corev1.Container, owners []mirror.Owner) []corev1.Container {
	out := make([]corev1.Container, 0, len(owners))
	for _, o := range owners {
		c := corev1.Container{
			Name:            fmt.Sprintf("%s%d-%d", ownerContainerPrefix, o.UID, o.GID),
			Image:           sync.Image,
			ImagePullPolicy: sync.ImagePullPolicy,
			Args:            []string{"sync", "--owner-report=" + path.Join(reportsMountPath, mirror.ReportFile(o))},
			Resources:       sync.Resources,
			SecurityContext: sync.SecurityContext.DeepCopy(),
		}
		for _, e := range sync.Env {
			if e.Name != "SSL_CERT_FILE" && !strings.HasPrefix(e.Name, "WORKSPACE_MANAGER_") && e.Name != "KUBERNETES_IN_CLUSTER" {
				c.Env = append(c.Env, e)
			}
		}
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		c.SecurityContext.RunAsUser, c.SecurityContext.RunAsGroup = ptr.To(o.UID), ptr.To(o.GID)
		for _, m := range sync.VolumeMounts {
			switch m.Name {
			case "credentials":
			case "owners":
			default:
				c.VolumeMounts = append(c.VolumeMounts, m)
			}
		}
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "owners", MountPath: reportsMountPath})
		out = append(out, c)
	}
	return out
}

func withLabels(base map[string]string, workspace, id string) map[string]string {
	out := make(map[string]string, len(base)+2)
	for k, v := range base {
		out[k] = v
	}
	out[LabelWorkspace], out[LabelSync] = workspace, id
	return out
}

// writeConfigMap creates the ConfigMap or replaces an existing one's data,
// keeping its metadata.
func writeConfigMap(ctx context.Context, cs kubernetes.Interface, cm *corev1.ConfigMap) error {
	cms := cs.CoreV1().ConfigMaps(cm.Namespace)
	existing, err := cms.Get(ctx, cm.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create configmap %s: %w", cm.Name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get configmap %s: %w", cm.Name, err)
	}
	existing.Data, existing.BinaryData = cm.Data, nil
	if _, err := cms.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update configmap %s: %w", cm.Name, err)
	}
	return nil
}
