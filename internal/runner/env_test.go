package runner

import (
	"slices"
	"testing"
)

func TestHarnessEnvKeepsXAIKeyAndJobEnvDropsIt(t *testing.T) {
	environ := []string{
		"PATH=/bin", "HOME=/home/a", "XAI_API_KEY=xai-secret",
		"SCHMEIDE_NATS_TOKEN=nats-secret", "AWS_SECRET_ACCESS_KEY=aws-secret", "GOPATH=/go",
	}
	if got, want := jobEnv(environ, []string{"GOPATH"}), []string{"PATH=/bin", "HOME=/home/a", "GOPATH=/go"}; !slices.Equal(got, want) {
		t.Errorf("jobEnv = %q, want %q", got, want)
	}
	want := []string{"PATH=/bin", "HOME=/home/a", "XAI_API_KEY=xai-secret"}
	if got := harnessEnv(environ, nil); !slices.Equal(got, want) {
		t.Errorf("harnessEnv = %q, want %q", got, want)
	}
}
