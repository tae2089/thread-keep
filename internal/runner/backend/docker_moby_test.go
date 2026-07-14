package backend

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

type mobyClientFake struct {
	createOptions client.ContainerCreateOptions
}

func (f *mobyClientFake) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{}, nil
}

func (f *mobyClientFake) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.createOptions = options
	return client.ContainerCreateResult{ID: "container-1"}, nil
}

func (f *mobyClientFake) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, nil
}

func (f *mobyClientFake) ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error) {
	return client.ExecCreateResult{}, nil
}

func (f *mobyClientFake) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	return client.ExecAttachResult{}, nil
}

func (f *mobyClientFake) ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error) {
	return client.ExecInspectResult{}, nil
}

func (f *mobyClientFake) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return client.ContainerStopResult{}, nil
}

func (f *mobyClientFake) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, nil
}

func TestMobyDockerEngineCreatesRestrictedRunnerContainer(t *testing.T) {
	engineClient := &mobyClientFake{}
	engine := newMobyDockerEngine(engineClient)
	labels := map[string]string{dockerLabelManaged: "true", dockerLabelSpecDigest: strings.Repeat("a", 64)}
	_, err := engine.Create(context.Background(), DockerCreateSpec{
		Name:                  "thread-keep-runner-test",
		Image:                 "registry.invalid/thread-keep-runner@sha256:" + strings.Repeat("b", 64),
		Network:               "thread-keep-runner",
		User:                  dockerRunnerUser,
		CPULimitMillis:        250,
		MemoryLimitBytes:      64 << 20,
		WorkspaceLimitBytes:   256 << 20,
		MaxRequestBytes:       1 << 20,
		MaxResultBytes:        16 << 20,
		CredentialWaitTimeout: 30 * time.Second,
		ExecutionTimeout:      time.Minute,
		ReadonlyRootFS:        true,
		Labels:                labels,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	created := engineClient.createOptions
	if created.Config == nil || created.HostConfig == nil {
		t.Fatalf("ContainerCreate() options = %+v", created)
	}
	if created.Config.Image == "" || created.Config.User != dockerRunnerUser || len(created.Config.Entrypoint) != 2 || created.Config.Entrypoint[0] != "/bin/sh" {
		t.Fatalf("container config = %+v", created.Config)
	}
	wrapper := strings.Join(created.Config.Cmd, " ")
	for _, required := range []string{dockerRunnerPath, dockerRequestPath, dockerCredentialPath, dockerResultPath, dockerCompletionAckPath} {
		if !strings.Contains(wrapper, required) {
			t.Fatalf("runner wrapper %q is missing %q", wrapper, required)
		}
	}
	if created.HostConfig.NetworkMode != "thread-keep-runner" || !created.HostConfig.ReadonlyRootfs || created.HostConfig.RestartPolicy.Name != "no" || created.HostConfig.Privileged || created.HostConfig.AutoRemove {
		t.Fatalf("host security config = %+v", created.HostConfig)
	}
	if len(created.HostConfig.CapDrop) != 1 || created.HostConfig.CapDrop[0] != "ALL" || len(created.HostConfig.SecurityOpt) != 1 || created.HostConfig.SecurityOpt[0] != "no-new-privileges" {
		t.Fatalf("host privilege config = %+v", created.HostConfig)
	}
	for _, path := range []string{"/run/thread-keep-request", "/run/thread-keep-secret", "/run/thread-keep-result"} {
		if !strings.Contains(created.HostConfig.Tmpfs[path], "noexec") || !strings.Contains(created.HostConfig.Tmpfs[path], "uid=65532") {
			t.Fatalf("tmpfs %s = %q", path, created.HostConfig.Tmpfs[path])
		}
	}
	if strings.Contains(created.HostConfig.Tmpfs["/tmp"], "noexec") || !strings.Contains(created.HostConfig.Tmpfs["/tmp"], "nosuid") {
		t.Fatalf("workspace tmpfs = %q", created.HostConfig.Tmpfs["/tmp"])
	}
	if created.HostConfig.Memory != 64<<20 || created.HostConfig.NanoCPUs != 250_000_000 || created.HostConfig.PidsLimit == nil || *created.HostConfig.PidsLimit != dockerRunnerPIDsLimit {
		t.Fatalf("container resources = %+v", created.HostConfig.Resources)
	}
}
