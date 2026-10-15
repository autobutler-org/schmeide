package repoconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content *string
		want    string
		errHas  []string
	}{
		{name: "valid", content: ptr("gate = \"make check test\"\n"), want: "make check test"},
		{name: "missing file", content: nil, errHas: []string{Path, "not found", "create"}},
		{name: "malformed", content: ptr("gate = \n"), errHas: []string{Path, "malformed", "fix the syntax"}},
		{name: "unknown key", content: ptr("gate = \"x\"\ngaet = \"y\"\n"), errHas: []string{Path, "unknown key", "gaet"}},
		{name: "empty gate", content: ptr("gate = \"  \"\n"), errHas: []string{Path, "gate", "set gate"}},
		{name: "no gate", content: ptr("# nothing\n"), errHas: []string{Path, "gate", "set gate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.content != nil {
				p := filepath.Join(dir, filepath.FromSlash(Path))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(*tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(dir)
			if tc.errHas == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.Gate != tc.want {
					t.Fatalf("gate = %q, want %q", cfg.Gate, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			for _, s := range tc.errHas {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q missing %q", err, s)
				}
			}
		})
	}
}

func TestMergePolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		want    string
		errHas  []string
	}{
		{name: "absent", content: "gate = \"make check\"\n", want: ""},
		{name: "empty", content: "gate = \"make check\"\nmerge = \"\"\n", want: ""},
		{name: "blank", content: "gate = \"make check\"\nmerge = \"  \"\n", want: ""},
		{name: "human", content: "gate = \"make check\"\nmerge = \"human\"\n", want: MergeHuman},
		{name: "factory", content: "gate = \"make check\"\nmerge = \"factory\"\n", want: MergeFactory},
		{name: "nope", content: "gate = \"make check\"\nmerge = \"nope\"\n", errHas: []string{"nope", MergeFactory, MergeHuman}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse(Path, []byte(tc.content))
			if tc.errHas == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.Gate != "make check" || cfg.Merge != tc.want {
					t.Fatalf("config = %+v, want gate %q and merge %q", cfg, "make check", tc.want)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			for _, s := range tc.errHas {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q missing %q", err, s)
				}
			}
		})
	}
}

func TestUnknownKeyDoesNotClaimOnlyGateIsSupported(t *testing.T) {
	t.Parallel()
	_, err := Parse(Path, []byte("gate = \"x\"\ngaet = \"y\"\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown key") || !strings.Contains(err.Error(), "gaet") {
		t.Fatalf("error = %q, want the unknown key", err)
	}
	if strings.Contains(err.Error(), `only "gate"`) {
		t.Fatalf("error = %q, want no claim that only gate is supported", err)
	}
}

func ptr(s string) *string { return &s }
