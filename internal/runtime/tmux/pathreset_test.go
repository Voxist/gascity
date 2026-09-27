package tmux

import "testing"

// TestWithEnvUnsetPrefix covers the string shapes withEnvUnsetPrefix produces
// for every combination of withheld keys and PATH re-assertion. These are
// pure string-construction cases — the real-tmux proof that resetPath
// actually survives a hostile ~/.zshenv (and a hostile .bashrc/.cshrc/etc.)
// lives in TestNewSessionWithCommandAndEnv_PATHSurvivesHostileStartupFile and
// TestRespawnAgent_PATHSurvivesHostileStartupFile (integration-tagged).
func TestWithEnvUnsetPrefix(t *testing.T) {
	tests := []struct {
		name          string
		command       string
		unsetKeys     []string
		resetPath     string
		shellBasename string
		want          string
		wantErr       bool
	}{
		{
			name:    "no keys, no reset: passthrough",
			command: "claude --model opus",
			want:    "claude --model opus",
		},
		{
			name:      "empty command is never wrapped, even with a reset",
			command:   "",
			resetPath: "/usr/bin:/bin",
			want:      "",
		},
		{
			name:      "unset keys only: unchanged env -u prefix (pre-existing shape)",
			command:   "claude --model opus",
			unsetKeys: []string{"GC_CONTROLLER_TOKEN"},
			want:      "env -u GC_CONTROLLER_TOKEN claude --model opus",
		},
		{
			name:      "multiple unset keys only, no reset",
			command:   "claude",
			unsetKeys: []string{"GC_CONTROLLER_TOKEN", "BEADS_TOKEN"},
			want:      "env -u GC_CONTROLLER_TOKEN -u BEADS_TOKEN claude",
		},
		{
			name:      "resetPath only: export prefix, no wrap, no shell change",
			command:   "claude --model opus",
			resetPath: "/usr/bin:/bin",
			want:      "export PATH='/usr/bin:/bin'; claude --model opus",
		},
		{
			name:      "resetPath + unset keys: export precedes the env -u prefix",
			command:   "claude",
			unsetKeys: []string{"GC_CONTROLLER_TOKEN"},
			resetPath: "/usr/bin:/bin",
			want:      "export PATH='/usr/bin:/bin'; env -u GC_CONTROLLER_TOKEN claude",
		},
		{
			name:      "caller-authored compound command is untouched, just prefixed",
			command:   "cd /work && exec claude",
			resetPath: "/usr/bin:/bin",
			want:      "export PATH='/usr/bin:/bin'; cd /work && exec claude",
		},
		{
			name:      "command containing single quotes is not disqualified — no exec-safety analysis exists anymore",
			command:   `echo 'hi there'`,
			resetPath: "/usr/bin:/bin",
			want:      `export PATH='/usr/bin:/bin'; echo 'hi there'`,
		},
		{
			name:      "leading VAR=val assignment is untouched",
			command:   "FOO=bar claude",
			resetPath: "/usr/bin:/bin",
			want:      "export PATH='/usr/bin:/bin'; FOO=bar claude",
		},
		{
			name:      "command already starting with exec is untouched",
			command:   "exec claude --model opus",
			resetPath: "/usr/bin:/bin",
			want:      "export PATH='/usr/bin:/bin'; exec claude --model opus",
		},
		{
			name:      "multi-line command is untouched",
			command:   "cd /work\nexec claude",
			resetPath: "/usr/bin:/bin",
			want:      "export PATH='/usr/bin:/bin'; cd /work\nexec claude",
		},
		{
			name:      "PATH value itself containing a single quote is escaped",
			command:   "claude",
			resetPath: "/weird'path/bin:/bin",
			want:      `export PATH='/weird'\''path/bin:/bin'; claude`,
		},
		{
			name:      "PATH value containing $ is quoted literally, not expanded",
			command:   "claude",
			resetPath: "/bin:$HOME/bin",
			want:      "export PATH='/bin:$HOME/bin'; claude",
		},
		{
			name:          "csh shell basename uses setenv, not export",
			command:       "claude",
			resetPath:     "/usr/bin:/bin",
			shellBasename: "csh",
			want:          "setenv PATH '/usr/bin:/bin'; claude",
		},
		{
			name:          "tcsh shell basename uses setenv, not export",
			command:       "claude",
			resetPath:     "/usr/bin:/bin",
			shellBasename: "tcsh",
			want:          "setenv PATH '/usr/bin:/bin'; claude",
		},
		{
			name:          "fish shell basename still uses export (fish 3.x's POSIX-compatible builtin)",
			command:       "claude",
			resetPath:     "/usr/bin:/bin",
			shellBasename: "fish",
			want:          "export PATH='/usr/bin:/bin'; claude",
		},
		{
			name:          "bash shell basename uses export",
			command:       "claude",
			resetPath:     "/usr/bin:/bin",
			shellBasename: "bash",
			want:          "export PATH='/usr/bin:/bin'; claude",
		},
		{
			name:          "unrecognized/empty shell basename defaults to export",
			command:       "claude",
			resetPath:     "/usr/bin:/bin",
			shellBasename: "",
			want:          "export PATH='/usr/bin:/bin'; claude",
		},
		{
			name:      "invalid unset key name is rejected regardless of resetPath",
			command:   "claude",
			unsetKeys: []string{"not-a-valid-name"},
			resetPath: "/usr/bin:/bin",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := withEnvUnsetPrefix(tt.command, tt.unsetKeys, tt.resetPath, tt.shellBasename)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("withEnvUnsetPrefix(%q, %v, %q, %q) = nil error, want an error", tt.command, tt.unsetKeys, tt.resetPath, tt.shellBasename)
				}
				return
			}
			if err != nil {
				t.Fatalf("withEnvUnsetPrefix(%q, %v, %q, %q) unexpected error: %v", tt.command, tt.unsetKeys, tt.resetPath, tt.shellBasename, err)
			}
			if got != tt.want {
				t.Errorf("withEnvUnsetPrefix(%q, %v, %q, %q) = %q, want %q", tt.command, tt.unsetKeys, tt.resetPath, tt.shellBasename, got, tt.want)
			}
		})
	}
}

// TestResolvePaneShellBasename pins the precedence: env["SHELL"] (the
// caller's explicit intent) before the ambient os.Getenv("SHELL") (what tmux
// itself resolves its default-shell from), before an empty default.
func TestResolvePaneShellBasename(t *testing.T) {
	t.Run("env SHELL wins over ambient", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/zsh")
		got := resolvePaneShellBasename(map[string]string{"SHELL": "/usr/local/bin/tcsh"})
		if got != "tcsh" {
			t.Errorf("resolvePaneShellBasename = %q, want %q", got, "tcsh")
		}
	})

	t.Run("falls back to ambient os.Getenv(SHELL) when env has none", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/bash")
		got := resolvePaneShellBasename(map[string]string{})
		if got != "bash" {
			t.Errorf("resolvePaneShellBasename = %q, want %q", got, "bash")
		}
	})

	t.Run("empty when neither is set", func(t *testing.T) {
		t.Setenv("SHELL", "")
		got := resolvePaneShellBasename(map[string]string{})
		if got != "" {
			t.Errorf("resolvePaneShellBasename = %q, want empty", got)
		}
	})
}

// TestPathAssignmentStatement pins the csh-family branch, verified against
// real zsh/bash/dash/fish/csh/tcsh in
// TestNewSessionWithCommandAndEnv_PATHSurvivesHostileStartupFile.
func TestPathAssignmentStatement(t *testing.T) {
	tests := []struct {
		shellBasename string
		want          string
	}{
		{shellBasename: "csh", want: "setenv PATH '/a:/b'; "},
		{shellBasename: "tcsh", want: "setenv PATH '/a:/b'; "},
		{shellBasename: "sh", want: "export PATH='/a:/b'; "},
		{shellBasename: "bash", want: "export PATH='/a:/b'; "},
		{shellBasename: "zsh", want: "export PATH='/a:/b'; "},
		{shellBasename: "ksh", want: "export PATH='/a:/b'; "},
		{shellBasename: "dash", want: "export PATH='/a:/b'; "},
		{shellBasename: "fish", want: "export PATH='/a:/b'; "},
		{shellBasename: "", want: "export PATH='/a:/b'; "},
	}
	for _, tt := range tests {
		t.Run(tt.shellBasename, func(t *testing.T) {
			if got := pathAssignmentStatement(tt.shellBasename, "/a:/b"); got != tt.want {
				t.Errorf("pathAssignmentStatement(%q, ...) = %q, want %q", tt.shellBasename, got, tt.want)
			}
		})
	}
}
