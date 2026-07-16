package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type dockerCall struct {
	environment map[string]string
	args        []string
}

type fakeDockerRunner struct {
	calls []dockerCall
	run   func(map[string]string, []string) ([]byte, error)
}

func (f *fakeDockerRunner) Run(_ context.Context, environment map[string]string, args ...string) ([]byte, error) {
	copiedEnvironment := make(map[string]string, len(environment))
	for key, value := range environment {
		copiedEnvironment[key] = value
	}
	f.calls = append(f.calls, dockerCall{environment: copiedEnvironment, args: append([]string(nil), args...)})
	return f.run(environment, args)
}

func TestCloudflaredReconcileCreatesHardenedContainerWithoutTokenInArguments(t *testing.T) {
	const token = "eyJhIjoic2VjcmV0In0"
	runner := &fakeDockerRunner{}
	runner.run = func(environment map[string]string, args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "info "):
			return []byte("27.0"), nil
		case strings.HasPrefix(joined, "container inspect "):
			return nil, errors.New("not found")
		case strings.HasPrefix(joined, "image inspect "):
			return []byte("image"), nil
		case strings.HasPrefix(joined, "container create "):
			if environment["TUNNEL_TOKEN"] != token {
				t.Fatalf("create did not receive token through environment")
			}
			return []byte("container-id"), nil
		case strings.HasPrefix(joined, "container start "):
			return []byte(cloudflaredContainerName), nil
		default:
			t.Fatalf("unexpected docker call: %s", joined)
			return nil, nil
		}
	}
	connector := NewDockerCloudflaredConnector("cloudflare/cloudflared:test", "minidock-edge")
	connector.runner = runner
	if err := connector.Reconcile(context.Background(), token); err != nil {
		t.Fatal(err)
	}

	var createArgs string
	for _, call := range runner.calls {
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, token) {
			t.Fatalf("tunnel token leaked into Docker argv: %s", joined)
		}
		if strings.HasPrefix(joined, "container create ") {
			createArgs = joined
		}
	}
	for _, required := range []string{
		"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges:true",
		"--network minidock-edge", "--restart unless-stopped", "--env TUNNEL_TOKEN",
		"--memory 256m", "--cpus 0.50", "--pids-limit 128", "--log-opt max-size=10m",
		cloudflaredManagedLabel + "=true",
	} {
		if !strings.Contains(createArgs, required) {
			t.Errorf("create args missing %q: %s", required, createArgs)
		}
	}
	if strings.Contains(createArgs, "--publish") || strings.Contains(createArgs, " -p ") {
		t.Fatalf("connector unexpectedly publishes a host port: %s", createArgs)
	}
}

func TestCloudflaredReconcileIsIdempotent(t *testing.T) {
	token := "high-entropy-token"
	runner := &fakeDockerRunner{}
	runner.run = func(_ map[string]string, args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "info ") {
			return []byte("27.0"), nil
		}
		if strings.Contains(joined, "com.docker.compose.project") {
			return []byte(""), nil
		}
		if strings.HasPrefix(joined, "container inspect ") {
			return []byte("running|true|cloudflared|" + cloudflaredTokenHash(token) + "|cloudflare/cloudflared:test"), nil
		}
		t.Fatalf("idempotent reconcile invoked %s", joined)
		return nil, nil
	}
	connector := NewDockerCloudflaredConnector("cloudflare/cloudflared:test", "minidock-edge")
	connector.runner = runner
	if err := connector.Reconcile(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("idempotent reconcile made %d calls, want 3", len(runner.calls))
	}
}

func TestCloudflaredReconcileMigratesOnlySameComposeProject(t *testing.T) {
	const token = "new-token"
	runner := &fakeDockerRunner{}
	runner.run = func(_ map[string]string, args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "info "):
			return []byte("27.0"), nil
		case strings.HasPrefix(joined, "container ls "):
			if !strings.Contains(joined, "label=com.docker.compose.project=minidock") || !strings.Contains(joined, "label=com.docker.compose.service=cloudflared") {
				t.Fatalf("legacy lookup is not project-scoped: %s", joined)
			}
			return []byte("legacy-container-id\n"), nil
		case strings.Contains(joined, `index .Config.Labels "com.docker.compose.project"`):
			return []byte("minidock"), nil
		case joined == "container rm --force legacy-container-id":
			return nil, nil
		case strings.HasPrefix(joined, "container inspect "):
			return nil, errors.New("not found")
		case strings.HasPrefix(joined, "image inspect "):
			return []byte("image"), nil
		case strings.HasPrefix(joined, "container create "), strings.HasPrefix(joined, "container start "):
			return []byte("ok"), nil
		default:
			t.Fatalf("unexpected docker call: %s", joined)
			return nil, nil
		}
	}
	connector := NewDockerCloudflaredConnector("cloudflare/cloudflared:test", "minidock-edge")
	connector.runner = runner
	if err := connector.Reconcile(context.Background(), token); err != nil {
		t.Fatal(err)
	}
}

func TestCloudflaredRemoveRefusesForeignContainer(t *testing.T) {
	runner := &fakeDockerRunner{}
	runner.run = func(_ map[string]string, args []string) ([]byte, error) {
		if strings.HasPrefix(strings.Join(args, " "), "container inspect ") {
			return []byte("running|false|cloudflared||other:image"), nil
		}
		return nil, nil
	}
	connector := NewDockerCloudflaredConnector("cloudflare/cloudflared:test", "minidock-edge")
	connector.runner = runner
	if err := connector.Remove(context.Background()); err == nil || !strings.Contains(err.Error(), "no eliminará") {
		t.Fatalf("foreign container removal error = %v", err)
	}
}
