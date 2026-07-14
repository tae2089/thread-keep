package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	containerderrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	dockerCredentialTmpfsBytes = 64 << 10
	dockerRunnerPIDsLimit      = 256
)

type mobyClient interface {
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type MobyDockerEngine struct {
	client mobyClient
}

type limitedDockerBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func NewMobyDockerEngine(engineClient *client.Client) (*MobyDockerEngine, error) {
	if engineClient == nil {
		return nil, backendValidationError("Docker Engine client is required")
	}
	return newMobyDockerEngine(engineClient), nil
}

func NewMobyDockerEngineFromEndpoint(endpoint string) (*MobyDockerEngine, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, backendValidationError("Docker Engine endpoint is required")
	}
	engineClient, err := client.New(client.WithHost(endpoint), client.WithTLSClientConfigFromEnv(), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, backendValidationError("Docker Engine endpoint is invalid")
	}
	return NewMobyDockerEngine(engineClient)
}

func newMobyDockerEngine(engineClient mobyClient) *MobyDockerEngine {
	return &MobyDockerEngine{client: engineClient}
}

func (e *MobyDockerEngine) Inspect(ctx context.Context, resourceID string) (DockerContainer, error) {
	result, err := e.client.ContainerInspect(ctx, resourceID, client.ContainerInspectOptions{})
	if containerderrdefs.IsNotFound(err) {
		return DockerContainer{}, ErrDockerResourceNotFound
	}
	if err != nil {
		return DockerContainer{}, err
	}
	state := DockerContainerExited
	exitCode := 0
	if result.Container.State != nil {
		exitCode = result.Container.State.ExitCode
		switch result.Container.State.Status {
		case container.StateCreated:
			state = DockerContainerCreated
		case container.StateRunning:
			state = DockerContainerRunning
		}
	}
	return DockerContainer{ID: result.Container.ID, State: state, ExitCode: exitCode, Labels: maps.Clone(result.Container.Config.Labels)}, nil
}

func (e *MobyDockerEngine) Create(ctx context.Context, spec DockerCreateSpec) (DockerContainer, error) {
	if spec.MaxRequestBytes <= 0 || spec.MaxResultBytes <= 0 || spec.CPULimitMillis > 1000_000 || spec.MemoryLimitBytes <= 0 || spec.WorkspaceLimitBytes <= 0 {
		return DockerContainer{}, backendValidationError("Docker create bounds are invalid")
	}
	pidsLimit := int64(dockerRunnerPIDsLimit)
	created, err := e.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:      spec.Image,
			User:       spec.User,
			Entrypoint: []string{"/bin/sh", "-c"},
			Cmd:        []string{dockerRunnerWrapper(spec.CredentialWaitTimeout)},
			Env:        []string{"TMPDIR=/tmp"},
			Labels:     maps.Clone(spec.Labels),
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(spec.Network),
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
			ReadonlyRootfs: spec.ReadonlyRootFS,
			SecurityOpt:    []string{"no-new-privileges"},
			CapDrop:        []string{"ALL"},
			Tmpfs: map[string]string{
				"/run/thread-keep-request": dockerTmpfsOptions(spec.MaxRequestBytes),
				"/run/thread-keep-secret":  dockerTmpfsOptions(dockerCredentialTmpfsBytes),
				"/run/thread-keep-result":  dockerTmpfsOptions(spec.MaxResultBytes),
				"/tmp":                     dockerWorkspaceTmpfsOptions(spec.WorkspaceLimitBytes),
			},
			Resources: container.Resources{
				Memory:    spec.MemoryLimitBytes,
				NanoCPUs:  spec.CPULimitMillis * 1_000_000,
				PidsLimit: &pidsLimit,
			},
		},
	})
	if err != nil {
		return DockerContainer{}, err
	}
	return DockerContainer{ID: created.ID, State: DockerContainerCreated, Labels: maps.Clone(spec.Labels)}, nil
}

func (e *MobyDockerEngine) Start(ctx context.Context, resourceID string) error {
	_, err := e.client.ContainerStart(ctx, resourceID, client.ContainerStartOptions{})
	return err
}

func (e *MobyDockerEngine) Upload(ctx context.Context, resourceID string, upload DockerUpload) error {
	command, err := dockerUploadCommand(upload.Kind)
	if err != nil {
		return err
	}
	execution, err := e.client.ExecCreate(ctx, resourceID, client.ExecCreateOptions{
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          command,
	})
	if err != nil {
		return err
	}
	attached, err := e.client.ExecAttach(ctx, execution.ID, client.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer attached.Close()
	if _, err := io.Copy(attached.Conn, bytes.NewReader(upload.Content)); err != nil {
		return err
	}
	if err := attached.CloseWrite(); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, attached.Reader); err != nil {
		return err
	}
	inspection, err := e.client.ExecInspect(ctx, execution.ID, client.ExecInspectOptions{})
	if err != nil {
		return err
	}
	if inspection.Running || inspection.ExitCode != 0 {
		return errors.New("Docker uploader exec failed")
	}
	return nil
}

func (e *MobyDockerEngine) Download(ctx context.Context, resourceID string, maximum int) ([]byte, error) {
	if maximum <= 0 {
		return nil, backendValidationError("Docker result bound is invalid")
	}
	execution, err := e.client.ExecCreate(ctx, resourceID, client.ExecCreateOptions{
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          []string{"/bin/cat", dockerResultPath},
	})
	if err != nil {
		return nil, err
	}
	attached, err := e.client.ExecAttach(ctx, execution.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	defer attached.Close()
	stdout := limitedDockerBuffer{limit: maximum}
	stderr := limitedDockerBuffer{limit: 4096}
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader); err != nil {
		return nil, err
	}
	inspection, err := e.client.ExecInspect(ctx, execution.ID, client.ExecInspectOptions{})
	if err != nil {
		return nil, err
	}
	if inspection.Running || inspection.ExitCode != 0 {
		return nil, ErrDockerResultUnavailable
	}
	if stderr.buffer.Len() != 0 {
		return nil, errors.New("Docker downloader exec wrote stderr")
	}
	return bytes.Clone(stdout.buffer.Bytes()), nil
}

func (e *MobyDockerEngine) Stop(ctx context.Context, resourceID string) error {
	timeout := 0
	_, err := e.client.ContainerStop(ctx, resourceID, client.ContainerStopOptions{Timeout: &timeout})
	if containerderrdefs.IsNotFound(err) || containerderrdefs.IsNotModified(err) {
		return ErrDockerResourceNotFound
	}
	return err
}

func (e *MobyDockerEngine) Remove(ctx context.Context, resourceID string) error {
	_, err := e.client.ContainerRemove(ctx, resourceID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if containerderrdefs.IsNotFound(err) {
		return ErrDockerResourceNotFound
	}
	return err
}

func (b *limitedDockerBuffer) Write(content []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		return 0, errors.New("Docker output exceeds the configured limit")
	}
	if len(content) > remaining {
		written, _ := b.buffer.Write(content[:remaining])
		return written, errors.New("Docker output exceeds the configured limit")
	}
	return b.buffer.Write(content)
}

func dockerRunnerWrapper(credentialWaitTimeout time.Duration) string {
	return fmt.Sprintf(`while [ ! -s %s ]; do sleep 0.05; done
status=0
%s execute --request-file=%s --credential-file=%s --result-file=%s --credential-wait-timeout=%s || status=$?
if [ -f %s ]; then
  while [ ! -e %s ]; do sleep 0.05; done
fi
exit "$status"`, dockerRequestPath, dockerRunnerPath, dockerRequestPath, dockerCredentialPath, dockerResultPath, credentialWaitTimeout.String(), dockerResultPath, dockerCompletionAckPath)
}

func dockerUploadCommand(kind DockerUploadKind) ([]string, error) {
	path := ""
	switch kind {
	case DockerUploadRequest:
		path = dockerRequestPath
	case DockerUploadCredential:
		path = dockerCredentialPath
	case DockerUploadCompletionAck:
		path = dockerCompletionAckPath
	default:
		return nil, backendValidationError("Docker upload kind is invalid")
	}
	temporary := path + ".tmp"
	command := fmt.Sprintf("umask 077; cat > %s && chmod 0400 %s && mv %s %s", temporary, temporary, temporary, path)
	if kind == DockerUploadCompletionAck {
		command = fmt.Sprintf("umask 077; : > %s && chmod 0400 %s && mv %s %s", temporary, temporary, temporary, path)
	}
	return []string{"/bin/sh", "-c", command}, nil
}

func dockerTmpfsOptions(size int) string {
	return fmt.Sprintf("rw,noexec,nosuid,nodev,mode=0700,size=%d,uid=65532,gid=65532", size)
}

func dockerWorkspaceTmpfsOptions(size int64) string {
	return fmt.Sprintf("rw,nosuid,nodev,mode=0700,size=%d,uid=65532,gid=65532", size)
}
