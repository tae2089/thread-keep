package backend

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedbatchv1 "k8s.io/client-go/kubernetes/typed/batch/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/tae2089/thread-keep/internal/domain"
)

const (
	kubernetesCredentialKey       = "token"
	kubernetesLabelManaged        = "io.thread-keep.runner"
	kubernetesLabelExecutionID    = "io.thread-keep.runner.execution"
	kubernetesLabelAttemptID      = "io.thread-keep.runner.attempt"
	kubernetesLabelRequestDigest  = "io.thread-keep.runner.request"
	kubernetesLabelSpecDigest     = "io.thread-keep.runner.spec"
	kubernetesArtifactMountPath   = "/run/thread-keep-artifacts"
	kubernetesCredentialMountPath = "/run/thread-keep-secret"
	kubernetesRunnerPath          = "/usr/local/bin/thread-keep-runner"
)

type KubernetesArtifactStore interface {
	WriteRequest(context.Context, string, []byte) error
	ReadResult(context.Context, string) ([]byte, error)
	Cleanup(context.Context, string) error
}

type KubernetesClient interface {
	BatchV1() typedbatchv1.BatchV1Interface
	CoreV1() typedcorev1.CoreV1Interface
}

type KubernetesBackendConfig struct {
	Client                  KubernetesClient
	Artifacts               KubernetesArtifactStore
	Namespace               string
	Image                   string
	JobServiceAccount       string
	ArtifactClaim           string
	ArtifactFSGroup         int64
	CPURequestMillis        int64
	CPULimitMillis          int64
	MemoryRequestBytes      int64
	MemoryLimitBytes        int64
	TTLSecondsAfterFinished int32
}

type KubernetesBackend struct {
	client                  KubernetesClient
	artifacts               KubernetesArtifactStore
	namespace               string
	image                   string
	jobServiceAccount       string
	artifactClaim           string
	artifactFSGroup         int64
	cpuRequestMillis        int64
	cpuLimitMillis          int64
	memoryRequestBytes      int64
	memoryLimitBytes        int64
	ttlSecondsAfterFinished int32
}

func NewKubernetesBackend(config KubernetesBackendConfig) (*KubernetesBackend, error) {
	if config.Client == nil || config.Artifacts == nil || strings.TrimSpace(config.Namespace) == "" || !digestPinnedRunnerImage(config.Image) || strings.TrimSpace(config.JobServiceAccount) == "" || strings.TrimSpace(config.ArtifactClaim) == "" || config.ArtifactFSGroup <= 0 || config.CPULimitMillis <= 0 || config.MemoryLimitBytes <= 0 || config.TTLSecondsAfterFinished <= 0 {
		return nil, backendValidationError("Kubernetes backend configuration is invalid")
	}
	if config.CPURequestMillis < 0 || config.CPURequestMillis > config.CPULimitMillis || config.MemoryRequestBytes < 0 || config.MemoryRequestBytes > config.MemoryLimitBytes {
		return nil, backendValidationError("Kubernetes resource requests exceed limits")
	}
	return &KubernetesBackend{client: config.Client, artifacts: config.Artifacts, namespace: config.Namespace, image: config.Image, jobServiceAccount: config.JobServiceAccount, artifactClaim: config.ArtifactClaim, artifactFSGroup: config.ArtifactFSGroup, cpuRequestMillis: config.CPURequestMillis, cpuLimitMillis: config.CPULimitMillis, memoryRequestBytes: config.MemoryRequestBytes, memoryLimitBytes: config.MemoryLimitBytes, ttlSecondsAfterFinished: config.TTLSecondsAfterFinished}, nil
}

func NewInClusterKubernetesClient() (KubernetesClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, backendValidationError("Kubernetes in-cluster configuration is unavailable")
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, backendValidationError("Kubernetes client configuration is invalid")
	}
	return client, nil
}

func (b *KubernetesBackend) Name() BackendName { return BackendKubernetesJob }

func (b *KubernetesBackend) Adoptable() bool { return true }

func (b *KubernetesBackend) Ensure(ctx context.Context, spec ExecutionSpec) (BackendHandle, error) {
	if err := validateExecutionSpec(spec); err != nil {
		return BackendHandle{}, err
	}
	request := spec.Request
	credential := []byte(request.Credential)
	request.Credential = ""
	if len(credential) == 0 {
		return BackendHandle{}, domain.NewError(domain.CodeAuth, errors.New("Kubernetes runner credential is empty"))
	}
	defer clear(credential)
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return BackendHandle{}, domain.NewError(domain.CodeCoverageIncomplete, errors.New("serialize Kubernetes runner request"))
	}
	if err := b.artifacts.WriteRequest(ctx, spec.AttemptID, requestBytes); err != nil {
		return BackendHandle{}, err
	}
	jobName := kubernetesJobName(spec.AttemptID)
	job, err := b.client.BatchV1().Jobs(b.namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err == nil {
		return b.handleForJob(spec, job)
	}
	if !apierrors.IsNotFound(err) {
		return BackendHandle{}, kubernetesEngineError("inspect Kubernetes runner Job", err)
	}
	if err := b.ensureSecret(ctx, spec, credential); err != nil {
		return BackendHandle{}, err
	}
	job, err = b.client.BatchV1().Jobs(b.namespace).Create(ctx, b.jobForSpec(spec), metav1.CreateOptions{})
	if err != nil {
		job, getErr := b.client.BatchV1().Jobs(b.namespace).Get(ctx, jobName, metav1.GetOptions{})
		if getErr != nil {
			return kubernetesHandleForSpec(spec, jobName), kubernetesEngineError("create Kubernetes runner Job", err)
		}
		return b.handleForJob(spec, job)
	}
	return b.handleForJob(spec, job)
}

func (b *KubernetesBackend) Observe(ctx context.Context, handle BackendHandle) (Observation, error) {
	if err := validateKubernetesHandle(handle); err != nil {
		return Observation{}, err
	}
	result, resultErr := b.artifacts.ReadResult(ctx, handle.AttemptID)
	if resultErr == nil {
		if _, err := DecodeResult(result); err != nil {
			return Observation{}, err
		}
		_ = b.deleteSecret(ctx, handle.AttemptID)
		return Observation{State: ObservationSucceeded, ResultEnvelope: result}, nil
	}
	if !errors.Is(resultErr, os.ErrNotExist) {
		return Observation{}, resultErr
	}
	job, err := b.client.BatchV1().Jobs(b.namespace).Get(ctx, kubernetesJobName(handle.AttemptID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Observation{State: ObservationLost, FailureCode: domain.CodeCoverageIncomplete}, nil
	}
	if err != nil {
		return Observation{}, kubernetesEngineError("inspect Kubernetes runner Job", err)
	}
	if err := validateKubernetesJobHandle(job, handle); err != nil {
		return Observation{}, err
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobFailed:
			_ = b.deleteSecret(ctx, handle.AttemptID)
			return Observation{State: ObservationFailed, FailureCode: domain.CodeCoverageIncomplete}, nil
		case batchv1.JobComplete:
			_ = b.deleteSecret(ctx, handle.AttemptID)
			return Observation{State: ObservationLost, FailureCode: domain.CodeCoverageIncomplete}, nil
		}
	}
	return Observation{State: ObservationRunning}, nil
}

func (b *KubernetesBackend) Cancel(ctx context.Context, handle BackendHandle) error {
	if err := validateKubernetesHandle(handle); err != nil {
		return err
	}
	if err := b.deleteJob(ctx, handle.AttemptID); err != nil {
		return err
	}
	return b.deleteSecret(ctx, handle.AttemptID)
}

func (b *KubernetesBackend) Cleanup(ctx context.Context, handle BackendHandle) error {
	if err := validateKubernetesHandle(handle); err != nil {
		return err
	}
	if err := b.deleteJob(ctx, handle.AttemptID); err != nil {
		return err
	}
	if err := b.deleteSecret(ctx, handle.AttemptID); err != nil {
		return err
	}
	return b.artifacts.Cleanup(ctx, handle.AttemptID)
}

func (b *KubernetesBackend) ensureSecret(ctx context.Context, spec ExecutionSpec, credential []byte) error {
	name := kubernetesSecretName(spec.AttemptID)
	secret, err := b.client.CoreV1().Secrets(b.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return validateKubernetesSecret(secret, spec, credential)
	}
	if !apierrors.IsNotFound(err) {
		return kubernetesEngineError("inspect Kubernetes runner Secret", err)
	}
	immutable := true
	secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: kubernetesLabels(spec)}, Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{kubernetesCredentialKey: append([]byte(nil), credential...)}}
	secret.StringData = nil
	if _, err := b.client.CoreV1().Secrets(b.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		secret, getErr := b.client.CoreV1().Secrets(b.namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return kubernetesEngineError("create Kubernetes runner Secret", err)
		}
		return validateKubernetesSecret(secret, spec, credential)
	}
	clear(secret.Data[kubernetesCredentialKey])
	return nil
}

func (b *KubernetesBackend) handleForJob(spec ExecutionSpec, job *batchv1.Job) (BackendHandle, error) {
	if err := validateKubernetesJobSpec(job, spec); err != nil {
		return BackendHandle{}, err
	}
	if job.UID == "" {
		return BackendHandle{}, kubernetesEngineError("read Kubernetes runner Job UID", errors.New("Job UID is empty"))
	}
	return kubernetesHandleForSpec(spec, string(job.UID)), nil
}

func (b *KubernetesBackend) jobForSpec(spec ExecutionSpec) *batchv1.Job {
	backoff := int32(0)
	ttl := b.ttlSecondsAfterFinished
	deadline := max(int64(1), int64(spec.Timeout/time.Second))
	falseValue := false
	trueValue := true
	mode := int32(0o400)
	labels := kubernetesLabels(spec)
	requestPath := fmt.Sprintf("%s/%s/request.json", kubernetesArtifactMountPath, spec.AttemptID)
	resultPath := fmt.Sprintf("%s/%s/result.json", kubernetesArtifactMountPath, spec.AttemptID)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: kubernetesJobName(spec.AttemptID), Namespace: b.namespace, Labels: labels},
		Spec: batchv1.JobSpec{BackoffLimit: &backoff, TTLSecondsAfterFinished: &ttl, ActiveDeadlineSeconds: &deadline, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			ServiceAccountName:           b.jobServiceAccount,
			AutomountServiceAccountToken: &falseValue,
			RestartPolicy:                corev1.RestartPolicyNever,
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: &trueValue, FSGroup: &b.artifactFSGroup, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers:                   []corev1.Container{{Name: "runner", Image: b.image, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{kubernetesRunnerPath}, Args: []string{"execute", "--request-file=" + requestPath, "--credential-file=" + kubernetesCredentialMountPath + "/" + kubernetesCredentialKey, "--result-file=" + resultPath, "--credential-wait-timeout=30s"}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &falseValue, ReadOnlyRootFilesystem: &trueValue, RunAsNonRoot: &trueValue, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(b.cpuRequestMillis, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(b.memoryRequestBytes, resource.BinarySI)}, Limits: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(b.cpuLimitMillis, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(b.memoryLimitBytes, resource.BinarySI)}}, VolumeMounts: []corev1.VolumeMount{{Name: "artifacts", MountPath: kubernetesArtifactMountPath}, {Name: "credential", MountPath: kubernetesCredentialMountPath, ReadOnly: true}}}},
			Volumes:                      []corev1.Volume{{Name: "artifacts", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: b.artifactClaim}}}, {Name: "credential", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: kubernetesSecretName(spec.AttemptID), DefaultMode: &mode}}}},
		}}},
	}
}

func (b *KubernetesBackend) deleteJob(ctx context.Context, attemptID string) error {
	policy := metav1.DeletePropagationBackground
	err := b.client.BatchV1().Jobs(b.namespace).Delete(ctx, kubernetesJobName(attemptID), metav1.DeleteOptions{PropagationPolicy: &policy})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return kubernetesEngineError("delete Kubernetes runner Job", err)
}

func (b *KubernetesBackend) deleteSecret(ctx context.Context, attemptID string) error {
	err := b.client.CoreV1().Secrets(b.namespace).Delete(ctx, kubernetesSecretName(attemptID), metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return kubernetesEngineError("delete Kubernetes runner Secret", err)
}

func validateKubernetesJobSpec(job *batchv1.Job, spec ExecutionSpec) error {
	if job == nil || job.Labels[kubernetesLabelManaged] != "true" || job.Labels[kubernetesLabelExecutionID] != spec.ExecutionID || job.Labels[kubernetesLabelAttemptID] != spec.AttemptID || job.Labels[kubernetesLabelRequestDigest] != spec.RequestDigest || job.Labels[kubernetesLabelSpecDigest] != spec.SpecDigest {
		return backendValidationError("Kubernetes runner Job identity does not match the execution spec")
	}
	return nil
}

func validateKubernetesJobHandle(job *batchv1.Job, handle BackendHandle) error {
	if job == nil || string(job.UID) != handle.ResourceID || job.Labels[kubernetesLabelManaged] != "true" || job.Labels[kubernetesLabelExecutionID] != handle.ExecutionID || job.Labels[kubernetesLabelAttemptID] != handle.AttemptID || job.Labels[kubernetesLabelSpecDigest] != handle.SpecDigest {
		return backendValidationError("Kubernetes runner Job identity does not match the stored handle")
	}
	return nil
}

func validateKubernetesSecret(secret *corev1.Secret, spec ExecutionSpec, credential []byte) error {
	if secret == nil || secret.Labels[kubernetesLabelManaged] != "true" || secret.Labels[kubernetesLabelAttemptID] != spec.AttemptID || secret.Labels[kubernetesLabelSpecDigest] != spec.SpecDigest || subtle.ConstantTimeCompare(secret.Data[kubernetesCredentialKey], credential) != 1 {
		return backendValidationError("Kubernetes runner Secret identity does not match the execution spec")
	}
	return nil
}

func validateKubernetesHandle(handle BackendHandle) error {
	if handle.Version != 1 || handle.Backend != BackendKubernetesJob || strings.TrimSpace(handle.ResourceID) == "" || !validDigest(handle.ExecutionID) || !validDigest(handle.AttemptID) || !validDigest(handle.SpecDigest) {
		return backendValidationError("Kubernetes runner handle is invalid")
	}
	return nil
}

func kubernetesLabels(spec ExecutionSpec) map[string]string {
	return map[string]string{kubernetesLabelManaged: "true", kubernetesLabelExecutionID: spec.ExecutionID, kubernetesLabelAttemptID: spec.AttemptID, kubernetesLabelRequestDigest: spec.RequestDigest, kubernetesLabelSpecDigest: spec.SpecDigest}
}

func kubernetesJobName(attemptID string) string { return "thread-keep-runner-" + attemptID[:24] }

func kubernetesSecretName(attemptID string) string { return "thread-keep-secret-" + attemptID[:24] }

func kubernetesHandleForSpec(spec ExecutionSpec, resourceID string) BackendHandle {
	return BackendHandle{Version: 1, Backend: BackendKubernetesJob, ResourceID: resourceID, ExecutionID: spec.ExecutionID, AttemptID: spec.AttemptID, SpecDigest: spec.SpecDigest}
}

func kubernetesEngineError(operation string, err error) error {
	return domain.NewError(domain.CodeBusy, fmt.Errorf("%s: %w", operation, err))
}
