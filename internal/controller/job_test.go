package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/workspace-manager/internal/mirror"
)

// chartTemplate is a Job template as the chart renders it: the pod's shape,
// the sync container with its own mounts; the cycle adds the rest.
const chartTemplate = `
apiVersion: batch/v1
kind: Job
metadata:
  labels:
    app.kubernetes.io/name: workspace-manager
    app.kubernetes.io/component: sync
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 7200
  ttlSecondsAfterFinished: 86400
  template:
    metadata:
      labels:
        app.kubernetes.io/component: sync
    spec:
      serviceAccountName: workspace-manager-sync
      restartPolicy: Never
      securityContext:
        fsGroup: 1000
      containers:
        - name: sync
          image: gsoci.azurecr.io/giantswarm/workspace-manager:1.2.3
          args: [sync, --parallel=4]
          env:
            - name: HOME
              value: /tmp
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
`

// TestLoadJobTemplate: the chart's template loads; one without the sync
// container, with a restarting pod or with an unknown field is refused.
func TestLoadJobTemplate(t *testing.T) {
	dir := t.TempDir()
	write := func(name, doc string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
		return path
	}
	job, err := LoadJobTemplate(write("job.yaml", chartTemplate))
	require.NoError(t, err)
	assert.Equal(t, int64(7200), ptr.Deref(job.Spec.ActiveDeadlineSeconds, 0))
	assert.Equal(t, "workspace-manager-sync", job.Spec.Template.Spec.ServiceAccountName)

	_, err = LoadJobTemplate(write("missing.yaml", ""))
	assert.ErrorContains(t, err, `no container named "sync"`)
	_, err = LoadJobTemplate(write("restarts.yaml", `
spec:
  template:
    spec:
      restartPolicy: Always
      containers: [{name: sync}]`))
	assert.ErrorContains(t, err, "restartPolicy must be Never or OnFailure")
	_, err = LoadJobTemplate(write("unknown.yaml", `
spec:
  templat:
    spec: {}`))
	assert.ErrorContains(t, err, "unknown field")
	_, err = LoadJobTemplate(filepath.Join(dir, "absent.yaml"))
	assert.ErrorContains(t, err, "sync job template")
}

// TestJobsBuildFromTemplate: the cycle's Job keeps the template's shape and
// adds its name, labels, owner, the image, the input and the volume.
func TestJobsBuildFromTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.yaml")
	require.NoError(t, os.WriteFile(path, []byte(chartTemplate), 0o600))
	tpl, err := LoadJobTemplate(path)
	require.NoError(t, err)
	ws := workspaceNamed("platform")
	owner := *metav1.NewControllerRef(ws, ws.GroupVersionKind())
	j := Jobs{Template: tpl}
	items := []corev1.KeyToPath{{Key: "0.username", Path: "github/acme/username"}, {Key: "0.token", Path: "github/acme/token"}}

	job, err := j.build(ws, "ab12", owner, items)
	require.NoError(t, err)
	assert.Equal(t, "workspace-platform-sync-ab12", job.Name)
	assert.Equal(t, testNamespace, job.Namespace)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "workspace-manager", "app.kubernetes.io/component": "sync", LabelWorkspace: "platform", LabelSync: "ab12"}, job.Labels)
	assert.Equal(t, map[string]string{"app.kubernetes.io/component": "sync", LabelWorkspace: "platform", LabelSync: "ab12"}, job.Spec.Template.Labels)
	assert.Equal(t, []metav1.OwnerReference{owner}, job.OwnerReferences)
	assert.Equal(t, int64(7200), ptr.Deref(job.Spec.ActiveDeadlineSeconds, 0))
	assert.Equal(t, int32(86400), ptr.Deref(job.Spec.TTLSecondsAfterFinished, 0))
	pod := job.Spec.Template.Spec
	assert.Equal(t, int64(1000), ptr.Deref(pod.SecurityContext.FSGroup, 0))
	c := pod.Containers[0]
	assert.Equal(t, "gsoci.azurecr.io/giantswarm/workspace-manager:1.2.3", c.Image, "the template's image without one of the cycle's")
	assert.Equal(t, []string{"sync", "--parallel=4"}, c.Args)
	assert.Equal(t, "HOME", c.Env[0].Name)
	assert.Contains(t, c.Env, corev1.EnvVar{Name: "WORKSPACE_MANAGER_NAMESPACE", Value: testNamespace})
	assert.Contains(t, c.Env, corev1.EnvVar{Name: "KUBERNETES_IN_CLUSTER", Value: "true"})
	for _, e := range c.Env {
		assert.NotEqual(t, "SSL_CERT_FILE", e.Name, "no roots: the image's apply")
	}
	assert.Equal(t, "tmp", c.VolumeMounts[0].Name, "the template's mounts stay")
	assert.Len(t, c.VolumeMounts, 4)
	assert.Len(t, pod.Volumes, 4)
	assert.Equal(t, items, pod.Volumes[3].Secret.Items)
	assert.Equal(t, int32(0o400), ptr.Deref(pod.Volumes[3].Secret.DefaultMode, 0))
	assert.Empty(t, tpl.Spec.Template.Spec.Volumes[0].Name == "", "the template is not changed")
	assert.Len(t, tpl.Spec.Template.Spec.Volumes, 1)

	j.Image, j.ImagePullPolicy = "workspace-manager:dev", corev1.PullNever
	j.Roots = []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	job, err = j.build(ws, "cd34", owner, nil)
	require.NoError(t, err)
	assert.Equal(t, "workspace-manager:dev", job.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, corev1.PullNever, job.Spec.Template.Spec.Containers[0].ImagePullPolicy)
	assert.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: "/etc/workspace-sync/ca.crt"},
		"the roots the manager verifies with, where the selection ConfigMap puts them")

	_, err = Jobs{}.build(ws, "x", owner, nil)
	assert.ErrorContains(t, err, "no sync job template")
}

// TestJobsStartCarriesRoots: the roots travel in the selection ConfigMap of
// the Workspace's namespace, where the Job reads them; without roots the
// ConfigMap carries the selection alone.
func TestJobsStartCarriesRoots(t *testing.T) {
	ws := workspaceNamed("platform")
	roots := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	j := Jobs{Client: fake.NewClientset(), Template: DefaultJobTemplate("workspace-manager-sync"), Roots: roots}
	job, err := j.Start(context.Background(), ws, "ab12", &mirror.Selection{}, nil)
	require.NoError(t, err)
	assert.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: "/etc/workspace-sync/ca.crt"})
	cm, err := j.Client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), SelectionName(ws), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, string(roots), cm.Data[rootsFile])
	assert.NotEmpty(t, cm.Data[selectionFile])

	j.Roots = nil
	_, err = j.Start(context.Background(), ws, "cd34", &mirror.Selection{}, nil)
	require.NoError(t, err)
	cm, err = j.Client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), SelectionName(ws), metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, cm.Data, rootsFile, "the roots of the last cycle go with the selection's rewrite")
}

// TestJobsStartProjectsEachOwnersCredential: the Job's Secret projects each
// credential to <provider>/<owner>/{username,token}, the directory the sync
// reads an owner's credential from, so two owners of one provider coexist.
func TestJobsStartProjectsEachOwnersCredential(t *testing.T) {
	ws := workspaceNamed("platform")
	j := Jobs{Client: fake.NewClientset(), Template: DefaultJobTemplate("workspace-manager-sync")}
	creds := []Credential{
		{Provider: "github", Owner: "acme", Username: "x-access-token", Token: "t1"},
		{Provider: "github", Owner: "globex", Username: "x-access-token", Token: "t2"},
	}
	job, err := j.Start(context.Background(), ws, "ab12", &mirror.Selection{}, creds)
	require.NoError(t, err)
	var items []corev1.KeyToPath
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "credentials" {
			items = v.Secret.Items
		}
	}
	assert.Equal(t, []corev1.KeyToPath{
		{Key: "0.username", Path: "github/acme/username"},
		{Key: "0.token", Path: "github/acme/token"},
		{Key: "1.username", Path: "github/globex/username"},
		{Key: "1.token", Path: "github/globex/token"},
	}, items)
	assert.Equal(t, "github/acme", mirror.CredentialDir("github", "acme"), "the sync reads where the Job projects")
}
