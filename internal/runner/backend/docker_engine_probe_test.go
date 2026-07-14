package backend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	containerderrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	dockerProbeEnvironment = "THREAD_KEEP_DOCKER_PROBE"
	dockerProbeImage       = "THREAD_KEEP_DOCKER_PROBE_IMAGE"
	dockerProbeLabel       = "io.thread-keep.runner-probe"
	dockerProbeAttempt     = "io.thread-keep.runner-attempt"
	dockerProbeRequest     = `{"version":1}`
	dockerProbeResult      = `{"version":1,"status":"ok"}`
)

var (
	errDockerProbeFileUnavailable = errors.New("Docker probe file is unavailable")
	errDockerProbeOutputTooLarge  = errors.New("Docker probe output exceeds limit")
)

type boundedDockerProbeBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func TestDockerEngineCredentialFileTransport(t *testing.T) {
	if os.Getenv(dockerProbeEnvironment) != "1" {
		t.Skipf("set %s=1 to run the real Docker Engine probe", dockerProbeEnvironment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	imageReference := os.Getenv(dockerProbeImage)
	if imageReference == "" {
		imageReference = "thread-keep-e2e:local"
	}
	firstClient := newDockerProbeClient(t)
	image, err := firstClient.ImageInspect(ctx, imageReference)
	if err != nil {
		firstClient.Close()
		t.Fatalf("inspect probe image: %v", err)
	}
	if !strings.HasPrefix(image.ID, "sha256:") {
		firstClient.Close()
		t.Fatalf("probe image must resolve to an immutable image ID, got %q", image.ID)
	}

	attemptID := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	name := "thread-keep-" + attemptID
	created, err := firstClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:      image.ID,
			User:       "65532:65532",
			Entrypoint: []string{"/bin/sh", "-c"},
			Cmd: []string{`set -eu
while [ ! -s /run/thread-keep-request/request.json ]; do sleep 0.05; done
test "$(cat /run/thread-keep-request/request.json)" = '{"version":1}'
while [ ! -s /run/thread-keep-secret/token ]; do sleep 0.05; done
test "$(wc -c < /run/thread-keep-secret/token)" -gt 0
printf '%s' '{"version":1,"status":"ok"}' > /run/thread-keep-result/result.json.tmp
chmod 0400 /run/thread-keep-result/result.json.tmp
mv /run/thread-keep-result/result.json.tmp /run/thread-keep-result/result.json
while [ ! -e /run/thread-keep-result/completion-ack ]; do sleep 0.05; done`},
			Labels: map[string]string{
				dockerProbeLabel:   "true",
				dockerProbeAttempt: attemptID,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
			ReadonlyRootfs: true,
			SecurityOpt:    []string{"no-new-privileges"},
			CapDrop:        []string{"ALL"},
			Tmpfs: map[string]string{
				"/run/thread-keep-request": "rw,noexec,nosuid,nodev,mode=0700,size=1m,uid=65532,gid=65532",
				"/run/thread-keep-secret":  "rw,noexec,nosuid,nodev,mode=0700,size=64k,uid=65532,gid=65532",
				"/run/thread-keep-result":  "rw,noexec,nosuid,nodev,mode=0700,size=1m,uid=65532,gid=65532",
			},
			Resources: container.Resources{Memory: 64 << 20, NanoCPUs: 250_000_000},
		},
	})
	if err != nil {
		firstClient.Close()
		t.Fatalf("create stopped probe container: %v", err)
	}
	containerID := created.ID
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanupClient := newDockerProbeClient(t)
		defer cleanupClient.Close()
		if err := removeDockerProbeContainer(cleanupCtx, cleanupClient, containerID); err != nil {
			t.Errorf("cleanup probe container: %v", err)
		}
	})

	if _, err := firstClient.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		firstClient.Close()
		t.Fatalf("start probe container: %v", err)
	}
	if err := uploadDockerProbeFile(ctx, firstClient, containerID, []string{
		"/bin/sh", "-c",
		"umask 077; cat > /run/thread-keep-request/request.json.tmp && chmod 0400 /run/thread-keep-request/request.json.tmp && mv /run/thread-keep-request/request.json.tmp /run/thread-keep-request/request.json",
	}, []byte(dockerProbeRequest)); err != nil {
		firstClient.Close()
		t.Fatalf("upload retryable non-secret request into running tmpfs: %v", err)
	}

	credential := []byte("probe-credential")
	if err := uploadDockerProbeFile(ctx, firstClient, containerID, []string{
		"/bin/sh", "-c",
		"umask 077; cat > /run/thread-keep-secret/token.tmp && chmod 0400 /run/thread-keep-secret/token.tmp && mv /run/thread-keep-secret/token.tmp /run/thread-keep-secret/token",
	}, credential); err != nil {
		for index := range credential {
			credential[index] = 0
		}
		firstClient.Close()
		t.Fatalf("upload credential once into running tmpfs: %v", err)
	}
	for index := range credential {
		credential[index] = 0
	}
	if err := firstClient.Close(); err != nil {
		t.Fatalf("close first Docker client: %v", err)
	}

	secondClient := newDockerProbeClient(t)
	defer secondClient.Close()
	inspect, err := secondClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("re-observe running probe container: %v", err)
	}
	if inspect.Container.State == nil || !inspect.Container.State.Running {
		t.Fatalf("probe container did not preserve result for re-observation: %#v", inspect.Container.State)
	}
	resultBytes := waitForDockerProbeResult(t, ctx, secondClient, containerID)
	var result map[string]any
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if string(resultBytes) != dockerProbeResult {
		t.Fatalf("unexpected result: %s", resultBytes)
	}
	if err := uploadDockerProbeFile(ctx, secondClient, containerID, []string{
		"/bin/sh", "-c",
		"umask 077; : > /run/thread-keep-result/completion-ack.tmp && chmod 0400 /run/thread-keep-result/completion-ack.tmp && mv /run/thread-keep-result/completion-ack.tmp /run/thread-keep-result/completion-ack",
	}, nil); err != nil {
		t.Fatalf("upload completion ack: %v", err)
	}

	waitResult := secondClient.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-waitResult.Error:
		t.Fatalf("observe terminal probe container: %v", err)
	case result := <-waitResult.Result:
		if result.Error != nil || result.StatusCode != 0 {
			t.Fatalf("probe container exited unsuccessfully: status=%d error=%v", result.StatusCode, result.Error)
		}
	case <-ctx.Done():
		t.Fatalf("wait for probe container: %v", ctx.Err())
	}
	inspect, err = secondClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect terminal probe container: %v", err)
	}
	if inspect.Container.State == nil || inspect.Container.State.Running || inspect.Container.State.ExitCode != 0 {
		t.Fatalf("unexpected terminal state: %#v", inspect.Container.State)
	}

	if err := removeDockerProbeContainer(ctx, secondClient, containerID); err != nil {
		t.Fatalf("remove probe container: %v", err)
	}
	if err := removeDockerProbeContainer(ctx, secondClient, containerID); err != nil {
		t.Fatalf("remove probe container idempotently: %v", err)
	}
	remaining, err := secondClient.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", dockerProbeAttempt+"="+attemptID),
	})
	if err != nil {
		t.Fatalf("inventory labeled probe containers: %v", err)
	}
	if len(remaining.Items) != 0 {
		t.Fatalf("probe leaked %d labeled containers", len(remaining.Items))
	}
}

func newDockerProbeClient(t *testing.T) *client.Client {
	t.Helper()
	engineClient, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("create Docker client: %v", err)
	}
	return engineClient
}

func uploadDockerProbeFile(ctx context.Context, engineClient *client.Client, containerID string, command []string, content []byte) error {
	execution, err := engineClient.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          command,
	})
	if err != nil {
		return fmt.Errorf("create uploader exec: %w", err)
	}
	attached, err := engineClient.ExecAttach(ctx, execution.ID, client.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("attach uploader exec: %w", err)
	}
	if _, err := io.Copy(attached.Conn, bytes.NewReader(content)); err != nil {
		attached.Close()
		return fmt.Errorf("stream uploader input: %w", err)
	}
	if err := attached.CloseWrite(); err != nil {
		attached.Close()
		return fmt.Errorf("close uploader input: %w", err)
	}
	if _, err := io.Copy(io.Discard, attached.Reader); err != nil {
		attached.Close()
		return fmt.Errorf("wait for uploader output: %w", err)
	}
	attached.Close()
	inspection, err := engineClient.ExecInspect(ctx, execution.ID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect uploader exec: %w", err)
	}
	if inspection.Running || inspection.ExitCode != 0 {
		return fmt.Errorf("uploader exec failed: running=%t exit=%d", inspection.Running, inspection.ExitCode)
	}
	return nil
}

func waitForDockerProbeResult(t *testing.T, ctx context.Context, engineClient *client.Client, containerID string) []byte {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := downloadDockerProbeFile(ctx, engineClient, containerID, []string{
			"/bin/cat", "/run/thread-keep-result/result.json",
		}, 1<<20)
		if err == nil {
			return result
		}
		if !errors.Is(err, errDockerProbeFileUnavailable) {
			t.Fatalf("download bounded result: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("download bounded result: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func downloadDockerProbeFile(ctx context.Context, engineClient *client.Client, containerID string, command []string, maximum int) ([]byte, error) {
	execution, err := engineClient.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          command,
	})
	if err != nil {
		return nil, fmt.Errorf("create downloader exec: %w", err)
	}
	attached, err := engineClient.ExecAttach(ctx, execution.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("attach downloader exec: %w", err)
	}
	stdout := boundedDockerProbeBuffer{limit: maximum}
	stderr := boundedDockerProbeBuffer{limit: 4096}
	_, copyErr := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
	attached.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("read downloader output: %w", copyErr)
	}
	inspection, err := engineClient.ExecInspect(ctx, execution.ID, client.ExecInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect downloader exec: %w", err)
	}
	if inspection.Running || inspection.ExitCode != 0 {
		return nil, errDockerProbeFileUnavailable
	}
	if stderr.buffer.Len() != 0 {
		return nil, fmt.Errorf("downloader wrote stderr")
	}
	return bytes.Clone(stdout.buffer.Bytes()), nil
}

func (b *boundedDockerProbeBuffer) Write(content []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		return 0, errDockerProbeOutputTooLarge
	}
	if len(content) > remaining {
		written, _ := b.buffer.Write(content[:remaining])
		return written, errDockerProbeOutputTooLarge
	}
	return b.buffer.Write(content)
}

func removeDockerProbeContainer(ctx context.Context, engineClient *client.Client, containerID string) error {
	_, err := engineClient.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err == nil || containerderrdefs.IsNotFound(err) {
		return nil
	}
	return err
}
