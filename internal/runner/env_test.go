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
