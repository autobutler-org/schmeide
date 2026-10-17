package runner

import (
	"slices"
	"testing"
)

func TestHarnessEnvKeepsXAIKeyAndDropsSecrets(t *testing.T) {
	environ := []string{
		"PATH=/bin", "HOME=/home/a", "XAI_API_KEY=xai-secret",
		"SCHMEIDE_NATS_TOKEN=nats-secret", "AWS_SECRET_ACCESS_KEY=aws-secret", "GOPATH=/go",
	}
	want := []string{"PATH=/bin", "HOME=/home/a", "XAI_API_KEY=xai-secret"}
	if got := harnessEnv(environ, nil); !slices.Equal(got, want) {
		t.Errorf("harnessEnv = %q, want %q", got, want)
	}
}

func TestGateEnvKeepsBaseAndGatePassEnvAndDropsSecrets(t *testing.T) {
	environ := []string{
		"PATH=/bin", "HOME=/home/a", "SSL_CERT_FILE=/ca.pem", "LC_ALL=C", "XAI_API_KEY=xai-secret",
		"ANTHROPIC_API_KEY=ant-secret", "SCHMEIDE_NATS_TOKEN=nats-secret", "SCHMEIDE_STEWARD=a",
		"AWS_SECRET_ACCESS_KEY=aws-secret", "GOPATH=/go",
	}
	want := []string{"PATH=/bin", "HOME=/home/a", "SSL_CERT_FILE=/ca.pem", "LC_ALL=C", "GOPATH=/go"}
	if got := gateEnv(environ, []string{"GOPATH", "SCHMEIDE_NATS_TOKEN"}); !slices.Equal(got, want) {
		t.Errorf("gateEnv = %q, want %q", got, want)
	}
}

func TestScratchHomeReplacesHomeTmpdirAndXDGDirs(t *testing.T) {
	env := []string{
		"PATH=/bin", "HOME=/home/a", "TMPDIR=/tmp", "XDG_CONFIG_HOME=/home/a/.config",
		"XDG_CACHE_HOME=/home/a/.cache", "XDG_RUNTIME_DIR=/run/user/1",
	}
	want := []string{"PATH=/bin", "XDG_RUNTIME_DIR=/run/user/1", "HOME=/scratch", "TMPDIR=/scratch"}
	if got := scratchHome(env, "/scratch"); !slices.Equal(got, want) {
		t.Errorf("scratchHome = %q, want %q", got, want)
	}
	if env[1] != "HOME=/home/a" {
		t.Errorf("scratchHome changed its input: %q", env)
	}
}
