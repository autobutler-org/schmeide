package repoconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestAutomate(t *testing.T) {
	t.Parallel()
	const gate = "gate = \"make check\"\n"
	cases := []struct {
		name    string
		content string
		want    Automate
		errHas  []string
	}{
		{name: "absent table", content: gate},
		{name: "empty table", content: gate + "[automate]\n"},
		{name: "all on", content: gate + "[automate]\nmerge = \"on\"\nreview_fix = \"on\"\ncreate_issues = \"on\"\n", want: Automate{Merge: true, ReviewFix: true, CreateIssues: true}},
		{name: "all off", content: gate + "[automate]\nmerge = \"off\"\nreview_fix = \"off\"\ncreate_issues = \"off\"\n"},
		{name: "merge only", content: gate + "[automate]\nmerge = \"on\"\n", want: Automate{Merge: true}},
		{name: "review_fix only", content: gate + "[automate]\nreview_fix = \"on\"\n", want: Automate{ReviewFix: true}},
		{name: "create_issues only", content: gate + "[automate]\ncreate_issues = \"on\"\n", want: Automate{CreateIssues: true}},
		{name: "empty", content: gate + "[automate]\nmerge = \"\"\n"},
		{name: "blank", content: gate + "[automate]\nreview_fix = \"  \"\n"},
		{name: "dotted key", content: gate + "automate.merge = \"on\"\n", want: Automate{Merge: true}},
		{name: "yes", content: gate + "[automate]\nmerge = \"yes\"\n", errHas: []string{Path, `automate.merge = "yes" is not a legal value; set automate.merge = "on" or automate.merge = "off"`}},
		{name: "case sensitive", content: gate + "[automate]\nreview_fix = \"On\"\n", errHas: []string{Path, `automate.review_fix = "On" is not a legal value`}},
		{name: "create_issues checked", content: gate + "[automate]\ncreate_issues = \"true\"\n", errHas: []string{`automate.create_issues = "true" is not a legal value`}},
		{name: "unknown key", content: gate + "[automate]\nmerg = \"on\"\n", errHas: []string{Path, "unknown key", "automate.merg"}},
		{name: "top-level merge", content: gate + "merge = \"factory\"\n", errHas: []string{Path + `: merge moved into [automate]; set it as [automate] merge = "on"`}},
		{name: "top-level merge human", content: gate + "merge = \"human\"\n", errHas: []string{"merge moved into [automate]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse(Path, []byte(tc.content))
			if tc.errHas == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.Gate != "make check" || cfg.Automate != tc.want {
					t.Fatalf("config = %+v, want gate %q and automate %+v", cfg, "make check", tc.want)
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

func TestCheckWait(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		want    time.Duration
		errHas  []string
	}{
		{name: "absent", content: "", want: 2 * time.Minute},
		{name: "empty", content: "check_wait = \"\"\n", want: 2 * time.Minute},
		{name: "ten minutes", content: "check_wait = \"10m\"\n", want: 10 * time.Minute},
		{name: "unparseable", content: "check_wait = \"soon\"\n", errHas: []string{"check_wait", "soon", "10m"}},
		{name: "zero", content: "check_wait = \"0s\"\n", errHas: []string{"check_wait", "10m"}},
		{name: "negative", content: "check_wait = \"-1m\"\n", errHas: []string{"check_wait", "10m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse(Path, []byte("gate = \"make check\"\n"+tc.content))
			if tc.errHas == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.CheckWait != tc.want {
					t.Fatalf("check wait = %v, want %v", cfg.CheckWait, tc.want)
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

func TestTimeoutSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		gate    time.Duration
		session time.Duration
		errHas  []string
	}{
		{name: "30m selects 30 minutes", content: "gate_timeout = \"30m\"\nsession_timeout = \"30m\"\n", gate: 30 * time.Minute, session: 30 * time.Minute},
		{name: "missing key selects the defaults", content: "", gate: DefaultGateTimeout, session: DefaultSessionTimeout},
		{name: "empty selects the defaults", content: "gate_timeout = \"\"\nsession_timeout = \"\"\n", gate: DefaultGateTimeout, session: DefaultSessionTimeout},
		{name: "blank selects the defaults", content: "gate_timeout = \"  \"\nsession_timeout = \"  \"\n", gate: DefaultGateTimeout, session: DefaultSessionTimeout},
		{name: "unparseable gate", content: "gate_timeout = \"soon\"\n", errHas: []string{"gate_timeout", "soon", "30m"}},
		{name: "unparseable session", content: "session_timeout = \"later\"\n", errHas: []string{"session_timeout", "later", "30m"}},
		{name: "zero gate", content: "gate_timeout = \"0s\"\n", errHas: []string{"gate_timeout", "0s", "30m"}},
		{name: "negative session", content: "session_timeout = \"-1m\"\n", errHas: []string{"session_timeout", "-1m", "30m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse(Path, []byte("gate = \"make check\"\n"+tc.content))
			if tc.errHas == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.GateTimeout != tc.gate || cfg.SessionTimeout != tc.session {
					t.Fatalf("gate %v session %v, want gate %v session %v", cfg.GateTimeout, cfg.SessionTimeout, tc.gate, tc.session)
				}
				if DefaultGateTimeout != 30*time.Minute || DefaultSessionTimeout != 2*time.Hour {
					t.Fatalf("defaults are gate %v session %v, want 30m and 2h", DefaultGateTimeout, DefaultSessionTimeout)
				}
				// A set key overrides a runner flag. A missing key keeps the flag.
				flag := time.Hour
				wantGate, wantSession := tc.gate, tc.session
				if !cfg.gateTimeoutSet {
					wantGate = flag
				}
				if !cfg.sessionTimeoutSet {
					wantSession = flag
				}
				if cfg.GateTimeoutOr(flag) != wantGate || cfg.SessionTimeoutOr(flag) != wantSession {
					t.Fatalf("with a %v flag, gate %v session %v, want gate %v session %v", flag, cfg.GateTimeoutOr(flag), cfg.SessionTimeoutOr(flag), wantGate, wantSession)
				}
				if cfg.GateTimeoutOr(0) != tc.gate || cfg.SessionTimeoutOr(0) != tc.session {
					t.Fatalf("with no flag, gate %v session %v, want gate %v session %v", cfg.GateTimeoutOr(0), cfg.SessionTimeoutOr(0), tc.gate, tc.session)
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

func TestNonStringValueNamesKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		content string
		errHas  []string
	}{
		{content: "gate = 5\n", errHas: []string{"gate", `gate = "make check test"`}},
		{content: "gate = \"x\"\n[automate]\nmerge = true\n", errHas: []string{"automate.merge", `automate.merge = "on"`}},
		{content: "gate = \"x\"\n[automate]\nreview_fix = 1\n", errHas: []string{"automate.review_fix", `automate.review_fix = "on"`}},
		{content: "gate = \"x\"\n[automate]\ncreate_issues = false\n", errHas: []string{"automate.create_issues", `automate.create_issues = "on"`}},
		{content: "gate = \"x\"\nautomate = \"on\"\n", errHas: []string{"automate must be a table", `[automate] merge = "on"`}},
		{content: "gate = \"x\"\ncheck_wait = 10\n", errHas: []string{"check_wait", `check_wait = "10m"`}},
		{content: "gate = \"x\"\ngate_timeout = 30\n", errHas: []string{"gate_timeout", `gate_timeout = "30m"`}},
		{content: "gate = \"x\"\nsession_timeout = 1800\n", errHas: []string{"session_timeout", `session_timeout = "30m"`}},
	}
	for _, tc := range cases {
		_, err := Parse(Path, []byte(tc.content))
		if err == nil {
			t.Fatalf("%q: expected error", tc.content)
		}
		for _, s := range tc.errHas {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%q: error %q missing %q", tc.content, err, s)
			}
		}
		if strings.Contains(err.Error(), "struct field") {
			t.Errorf("%q: error %q names a Go field, want the key", tc.content, err)
		}
	}
}

func ptr(s string) *string { return &s }
