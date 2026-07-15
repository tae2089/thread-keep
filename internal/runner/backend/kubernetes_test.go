package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote/server"
	"github.com/tae2089/thread-keep/internal/runner/artifact"
)

type testKubernetesStore struct {
	*artifact.FileStore
	root string
}

func TestKubernetesBackendCreatesRestrictedJobAndTemporarySecret(t *testing.T) {
	client := newFakeKubernetesClient()
	backend, store := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()
	handle, err := backend.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	jobName := kubernetesJobName(spec.AttemptID)
	job, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(Job) error = %v", err)
	}
	if handle.ResourceID != string(job.UID) || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 300 {
		t.Fatalf("Job/handle = %+v %+v", job.Spec, handle)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != "Never" || pod.ServiceAccountName != "thread-keep-runner" || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65532 {
		t.Fatalf("PodSpec security = %+v", pod)
	}
	container := pod.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem || container.Resources.Limits.Cpu().MilliValue() != 500 || container.Resources.Limits.Memory().Value() != 256<<20 {
		t.Fatalf("container security/resources = %+v", container)
	}
	secret, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), kubernetesSecretName(spec.AttemptID), metav1.GetOptions{})
	if err != nil || string(secret.Data[kubernetesCredentialKey]) != spec.Request.Credential {
		t.Fatalf("Secret = %+v, %v", secret, err)
	}
	request, err := store.ReadRequest(context.Background(), spec.AttemptID)
	if err != nil || strings.Contains(string(request), spec.Request.Credential) {
		t.Fatalf("request artifact leaked credential: %q, %v", request, err)
	}
}

func TestKubernetesBackendObservesPersistentResultAfterJobTTLDeletion(t *testing.T) {
	client := newFakeKubernetesClient()
	backend, store := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()
	handle, err := backend.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	envelope, err := EncodeResult(ResultEnvelope{Version: 1, Code: domain.CodeCoverageIncomplete, Message: "safe"})
	if err != nil {
		t.Fatalf("EncodeResult() error = %v", err)
	}
	if err := store.WriteResult(context.Background(), spec.AttemptID, envelope); err != nil {
		t.Fatalf("WriteResult() error = %v", err)
	}
	if err := client.BatchV1().Jobs("thread-keep-runners").Delete(context.Background(), kubernetesJobName(spec.AttemptID), metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete(Job) error = %v", err)
	}
	observation, err := backend.Observe(context.Background(), handle)
	if err != nil || observation.State != ObservationSucceeded || string(observation.ResultEnvelope) != string(envelope) {
		t.Fatalf("Observe() = %+v, %v", observation, err)
	}
}

func TestKubernetesBackendObservePreservesMismatchedSecret(t *testing.T) {
	client := newFakeKubernetesClient()
	backend, store := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()
	handle, err := backend.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	envelope, err := EncodeResult(ResultEnvelope{Version: 1, Code: domain.CodeCoverageIncomplete, Message: "safe"})
	if err != nil {
		t.Fatalf("EncodeResult() error = %v", err)
	}
	if err := store.WriteResult(context.Background(), spec.AttemptID, envelope); err != nil {
		t.Fatalf("WriteResult() error = %v", err)
	}
	secretName := kubernetesSecretName(spec.AttemptID)
	secret, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(Secret) error = %v", err)
	}
	secret.Labels[kubernetesLabelRequestDigest] = strings.Repeat("f", 64)
	if _, err := client.CoreV1().Secrets("thread-keep-runners").Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update(Secret) error = %v", err)
	}
	if _, err := backend.Observe(context.Background(), handle); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Observe() error = %v, want validation", err)
	}
	if _, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("mismatched Secret was deleted: %v", err)
	}
}

func TestKubernetesBackendObserveTerminalJobPreservesMismatchedSecret(t *testing.T) {
	client := newFakeKubernetesClient()
	backend, _ := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()
	handle, err := backend.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	job, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), kubernetesJobName(spec.AttemptID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(Job) error = %v", err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if _, err := client.BatchV1().Jobs("thread-keep-runners").UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("UpdateStatus(Job) error = %v", err)
	}
	secretName := kubernetesSecretName(spec.AttemptID)
	secret, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(Secret) error = %v", err)
	}
	secret.Labels[kubernetesLabelSpecDigest] = strings.Repeat("f", 64)
	if _, err := client.CoreV1().Secrets("thread-keep-runners").Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update(Secret) error = %v", err)
	}
	if _, err := backend.Observe(context.Background(), handle); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Observe() error = %v, want validation", err)
	}
	if _, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("mismatched Secret was deleted: %v", err)
	}
}

func TestKubernetesBackendObserveDeletesOwnedSecretWithUIDPrecondition(t *testing.T) {
	client := newFakeKubernetesClient()
	var deletedSecretUID types.UID
	client.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(k8stesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions != nil && options.Preconditions.UID != nil {
			deletedSecretUID = *options.Preconditions.UID
		}
		return false, nil, nil
	})
	backend, store := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()
	handle, err := backend.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	envelope, err := EncodeResult(ResultEnvelope{Version: 1, Code: domain.CodeCoverageIncomplete, Message: "safe"})
	if err != nil {
		t.Fatalf("EncodeResult() error = %v", err)
	}
	if err := store.WriteResult(context.Background(), spec.AttemptID, envelope); err != nil {
		t.Fatalf("WriteResult() error = %v", err)
	}
	observation, err := backend.Observe(context.Background(), handle)
	if err != nil || observation.State != ObservationSucceeded {
		t.Fatalf("Observe() = %+v, %v", observation, err)
	}
	if deletedSecretUID != types.UID("uid-"+kubernetesSecretName(spec.AttemptID)) {
		t.Fatalf("Secret delete UID precondition = %q", deletedSecretUID)
	}
}

func TestKubernetesBackendCleanupDeletesJobSecretAndArtifactsIdempotently(t *testing.T) {
	client := newFakeKubernetesClient()
	backend, store := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()
	handle, err := backend.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if err := backend.Cleanup(context.Background(), handle); err != nil {
		t.Fatalf("Cleanup(first) error = %v", err)
	}
	if err := backend.Cleanup(context.Background(), handle); err != nil {
		t.Fatalf("Cleanup(second) error = %v", err)
	}
	if _, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), kubernetesJobName(spec.AttemptID), metav1.GetOptions{}); err == nil {
		t.Fatal("Job remained after cleanup")
	}
	if _, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), kubernetesSecretName(spec.AttemptID), metav1.GetOptions{}); err == nil {
		t.Fatal("Secret remained after cleanup")
	}
	if _, err := os.Stat(filepath.Join(store.root, spec.AttemptID)); !os.IsNotExist(err) {
		t.Fatalf("artifact directory remained: %v", err)
	}
}

func TestKubernetesBackendDiscoveryCleanupDeletesValidatedUIDsAndIsNotFoundIdempotent(t *testing.T) {
	client := newFakeKubernetesClient()
	var deletedJobUID types.UID
	var deletedSecretUID types.UID
	client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(k8stesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions != nil && options.Preconditions.UID != nil {
			deletedJobUID = *options.Preconditions.UID
		}
		return false, nil, nil
	})
	client.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(k8stesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions != nil && options.Preconditions.UID != nil {
			deletedSecretUID = *options.Preconditions.UID
		}
		return false, nil, nil
	})
	backend, _ := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()
	if _, err := backend.Ensure(context.Background(), spec); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	identity := CleanupIdentity{ExecutionID: spec.ExecutionID, AttemptID: spec.AttemptID, RequestDigest: spec.RequestDigest, SpecDigest: spec.SpecDigest}
	if err := backend.CleanupDiscovered(context.Background(), identity); err != nil {
		t.Fatalf("CleanupDiscovered(first) error = %v", err)
	}
	if deletedJobUID != types.UID("uid-"+kubernetesJobName(spec.AttemptID)) || deletedSecretUID != types.UID("uid-"+kubernetesSecretName(spec.AttemptID)) {
		t.Fatalf("delete UID preconditions = %q/%q", deletedJobUID, deletedSecretUID)
	}
	if err := backend.CleanupDiscovered(context.Background(), identity); err != nil {
		t.Fatalf("CleanupDiscovered(second) error = %v", err)
	}
}

func TestKubernetesBackendPersistedCleanupPreservesReplacementCollisions(t *testing.T) {
	tests := []struct {
		name                  string
		replaceJobUID         bool
		mismatchJobRequest    bool
		mismatchSecret        bool
		mismatchSecretRequest bool
	}{
		{name: "job UID", replaceJobUID: true},
		{name: "job request label", mismatchJobRequest: true},
		{name: "secret labels", mismatchSecret: true},
		{name: "secret request label", mismatchSecretRequest: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newFakeKubernetesClient()
			backend, _ := newTestKubernetesBackend(t, client)
			spec := testDockerExecutionSpec()
			handle, err := backend.Ensure(context.Background(), spec)
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			jobName := kubernetesJobName(spec.AttemptID)
			secretName := kubernetesSecretName(spec.AttemptID)
			if test.replaceJobUID {
				job, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), jobName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Get(Job) error = %v", err)
				}
				job.UID = types.UID("replacement-job")
				if _, err := client.BatchV1().Jobs("thread-keep-runners").Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Update(Job) error = %v", err)
				}
			}
			if test.mismatchJobRequest {
				job, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), jobName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Get(Job) error = %v", err)
				}
				job.Labels[kubernetesLabelRequestDigest] = strings.Repeat("f", 64)
				if _, err := client.BatchV1().Jobs("thread-keep-runners").Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Update(Job) error = %v", err)
				}
			}
			if test.mismatchSecret || test.mismatchSecretRequest {
				secret, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Get(Secret) error = %v", err)
				}
				if test.mismatchSecret {
					secret.Labels[kubernetesLabelSpecDigest] = strings.Repeat("f", 64)
				} else {
					secret.Labels[kubernetesLabelRequestDigest] = strings.Repeat("f", 64)
				}
				if _, err := client.CoreV1().Secrets("thread-keep-runners").Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Update(Secret) error = %v", err)
				}
			}
			if err := backend.Cleanup(context.Background(), handle); domain.CodeOf(err) != domain.CodeValidation {
				t.Fatalf("Cleanup() error = %v, want validation", err)
			}
			if _, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), jobName, metav1.GetOptions{}); err != nil {
				t.Fatalf("replacement-collision Job was deleted: %v", err)
			}
			if _, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{}); err != nil {
				t.Fatalf("replacement-collision Secret was deleted: %v", err)
			}
		})
	}
}

func TestKubernetesBackendCancelPreservesReplacementCollision(t *testing.T) {
	client := newFakeKubernetesClient()
	backend, _ := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()
	handle, err := backend.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	jobName := kubernetesJobName(spec.AttemptID)
	secretName := kubernetesSecretName(spec.AttemptID)
	job, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(Job) error = %v", err)
	}
	job.UID = types.UID("replacement-job")
	if _, err := client.BatchV1().Jobs("thread-keep-runners").Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update(Job) error = %v", err)
	}
	if err := backend.Cancel(context.Background(), handle); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Cancel() error = %v, want validation", err)
	}
	if _, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), jobName, metav1.GetOptions{}); err != nil {
		t.Fatalf("replacement Job was deleted: %v", err)
	}
	if _, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("Secret was deleted after replacement collision: %v", err)
	}
}

func TestKubernetesBackendRejectsJobSpecCollision(t *testing.T) {
	client := newFakeKubernetesClient(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: kubernetesJobName(strings.Repeat("c", 64)), Namespace: "thread-keep-runners", UID: "foreign", Labels: map[string]string{kubernetesLabelManaged: "true", kubernetesLabelSpecDigest: strings.Repeat("f", 64)}}})
	backend, _ := newTestKubernetesBackend(t, client)
	if _, err := backend.Ensure(context.Background(), testDockerExecutionSpec()); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Ensure() error = %v, want validation", err)
	}
}

func TestKubernetesBackendReturnsCleanupHandleAfterAmbiguousCreate(t *testing.T) {
	client := newFakeKubernetesClient()
	created := false
	recoveryReadUnavailable := true
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		job := create.GetObject().(*batchv1.Job).DeepCopy()
		job.UID = types.UID("uid-" + job.Name)
		if err := client.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), job, create.GetNamespace()); err != nil {
			return true, nil, err
		}
		created = true
		return true, nil, errors.New("create response lost")
	})
	client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !created || !recoveryReadUnavailable {
			return false, nil, nil
		}
		return true, nil, errors.New("recovery read unavailable")
	})
	backend, store := newTestKubernetesBackend(t, client)
	spec := testDockerExecutionSpec()

	handle, err := backend.Ensure(context.Background(), spec)
	if domain.CodeOf(err) != domain.CodeBusy || handle.ResourceID == "" {
		t.Fatalf("Ensure() = %+v, %v; want retryable error with cleanup handle", handle, err)
	}
	if _, err := client.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), "thread-keep-runners", kubernetesJobName(spec.AttemptID)); err != nil {
		t.Fatalf("accepted Job disappeared before cleanup: %v", err)
	}
	if _, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), "thread-keep-runners", kubernetesSecretName(spec.AttemptID)); err != nil {
		t.Fatalf("credential Secret disappeared before cleanup: %v", err)
	}
	if err := backend.Cleanup(context.Background(), handle); domain.CodeOf(err) != domain.CodeBusy {
		t.Fatalf("Cleanup(read unavailable) error = %v, want busy", err)
	}
	if _, err := client.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), "thread-keep-runners", kubernetesJobName(spec.AttemptID)); err != nil {
		t.Fatalf("accepted Job disappeared after unverified cleanup: %v", err)
	}
	recoveryReadUnavailable = false
	if err := backend.Cleanup(context.Background(), handle); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, spec.AttemptID)); !os.IsNotExist(err) {
		t.Fatalf("artifact directory remained: %v", err)
	}
}

func TestDurableSourceRunnerPreservesForeignKubernetesResourcesDuringDiscoveryCleanup(t *testing.T) {
	tests := []struct {
		name           string
		mismatchJob    bool
		mismatchSecret bool
	}{
		{name: "job", mismatchJob: true},
		{name: "secret", mismatchSecret: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
			if err != nil {
				t.Fatalf("OpenGormRefStore() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			claim := claimLifecycleJob(t, store, "worker-1")
			spec := testDockerExecutionSpec()
			seed := server.RunnerExecutionSeed{ExecutionID: spec.ExecutionID, JobID: claim.JobID, RequestDigest: spec.RequestDigest, SpecDigest: spec.SpecDigest, Backend: string(BackendKubernetesJob), AttemptID: spec.AttemptID, OwnerInstance: "coordinator-1"}
			execution, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
			if err != nil {
				t.Fatalf("PrepareRunnerExecution() error = %v", err)
			}
			if err := store.FailRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy); err != nil {
				t.Fatalf("FailRunnerExecution() error = %v", err)
			}
			if err := store.MarkRunnerCleanupPending(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
				t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
			}
			client := newFakeKubernetesClient()
			backend, _ := newTestKubernetesBackend(t, client)
			if _, err := backend.Ensure(context.Background(), spec); err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			jobName := kubernetesJobName(spec.AttemptID)
			secretName := kubernetesSecretName(spec.AttemptID)
			if test.mismatchJob {
				job, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), jobName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Get(Job) error = %v", err)
				}
				job.Labels[kubernetesLabelSpecDigest] = strings.Repeat("f", 64)
				if _, err := client.BatchV1().Jobs("thread-keep-runners").Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Update(Job) error = %v", err)
				}
			}
			if test.mismatchSecret {
				secret, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Get(Secret) error = %v", err)
				}
				secret.Labels[kubernetesLabelSpecDigest] = strings.Repeat("f", 64)
				if _, err := client.CoreV1().Secrets("thread-keep-runners").Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Update(Secret) error = %v", err)
				}
			}
			runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: spec.SpecDigest})
			if err != nil {
				t.Fatalf("NewDurableSourceRunner() error = %v", err)
			}
			if err := runner.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if _, err := client.BatchV1().Jobs("thread-keep-runners").Get(context.Background(), jobName, metav1.GetOptions{}); err != nil {
				t.Fatalf("foreign-collision Job was deleted: %v", err)
			}
			if _, err := client.CoreV1().Secrets("thread-keep-runners").Get(context.Background(), secretName, metav1.GetOptions{}); err != nil {
				t.Fatalf("foreign-collision Secret was deleted: %v", err)
			}
			stored, err := store.RunnerExecution(context.Background(), execution.ExecutionID)
			if err != nil {
				t.Fatalf("RunnerExecution() error = %v", err)
			}
			if stored.CleanupState != server.RunnerCleanupFailed || stored.CleanupAttempts != 1 {
				t.Fatalf("RunnerExecution() cleanup = %q attempts=%d, want failed/1", stored.CleanupState, stored.CleanupAttempts)
			}
		})
	}
}

func newTestKubernetesBackend(t *testing.T, client *fake.Clientset) (*KubernetesBackend, testKubernetesStore) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := artifact.NewFileStore(artifact.FileStoreConfig{Root: root, MaxRequestBytes: 1 << 20, MaxResultBytes: 16 << 20})
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	backend, err := NewKubernetesBackend(KubernetesBackendConfig{
		Client:                  client,
		Artifacts:               store,
		Namespace:               "thread-keep-runners",
		Image:                   "registry.invalid/thread-keep-runner@sha256:" + strings.Repeat("a", 64),
		JobServiceAccount:       "thread-keep-runner",
		ArtifactClaim:           "thread-keep-runner-artifacts",
		ArtifactFSGroup:         65532,
		CPURequestMillis:        250,
		CPULimitMillis:          500,
		MemoryRequestBytes:      128 << 20,
		MemoryLimitBytes:        256 << 20,
		TTLSecondsAfterFinished: 300,
	})
	if err != nil {
		t.Fatalf("NewKubernetesBackend() error = %v", err)
	}
	return backend, testKubernetesStore{FileStore: store, root: root}
}

func newFakeKubernetesClient(objects ...runtime.Object) *fake.Clientset {
	client := fake.NewClientset(objects...)
	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		secret := create.GetObject().(*corev1.Secret).DeepCopy()
		secret.UID = types.UID("uid-" + secret.Name)
		err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("secrets"), secret, create.GetNamespace())
		return true, secret, err
	})
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		job := create.GetObject().(*batchv1.Job).DeepCopy()
		job.UID = types.UID("uid-" + job.Name)
		err := client.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), job, create.GetNamespace())
		return true, job, err
	})
	return client
}
