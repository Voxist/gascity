package tmux

import "testing"

// TestWithEnvUnsetPrefix covers the string shapes withEnvUnsetPrefix produces
// for every combination of withheld keys and PATH re-assertion. These are
// pure string-construction cases — the real-tmux proof that resetPath
// actually survives a hostile ~/.zshenv lives in
// TestNewSessionWithCommandAndEnv_PATHSurvivesZshenv (integration-tagged).
func TestWithEnvUnsetPrefix(t *testing.T) {
	tests := []struct {
		name      string
		command   string
		unsetKeys []string
		resetPath string
		want      string
		wantErr   bool
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
			name:      "resetPath only, simple command: sh -c wrap forces exec",
			command:   "claude --model opus",
			resetPath: "/usr/bin:/bin",
			want:      "env PATH='/usr/bin:/bin' sh -c 'exec claude --model opus'",
		},
		{
			name:      "resetPath + unset keys, simple command: -u flags precede the PATH reassert, exec forced",
			command:   "claude",
			unsetKeys: []string{"GC_CONTROLLER_TOKEN"},
			resetPath: "/usr/bin:/bin",
			want:      "env -u GC_CONTROLLER_TOKEN PATH='/usr/bin:/bin' sh -c 'exec claude'",
		},
		{
			name:      "compound caller command survives as one opaque sh -c unit, no exec forced",
			command:   "cd /work && exec claude",
			resetPath: "/usr/bin:/bin",
			want:      "env PATH='/usr/bin:/bin' sh -c 'cd /work && exec claude'",
		},
		{
			name:      "command containing single quotes is escaped, not broken, no exec forced (quoting disqualifies exec-safety)",
			command:   `echo 'hi there'`,
			resetPath: "/usr/bin:/bin",
			want:      `env PATH='/usr/bin:/bin' sh -c 'echo '\''hi there'\'''`,
		},
		{
			name:      "PATH value itself containing a single quote is escaped; command still simple, exec forced",
			command:   "claude",
			resetPath: "/weird'path/bin:/bin",
			want:      `env PATH='/weird'\''path/bin:/bin' sh -c 'exec claude'`,
		},
		{
			name:      "PATH value containing $ is quoted literally, not expanded; command still simple, exec forced",
			command:   "claude",
			resetPath: "/bin:$HOME/bin",
			want:      "env PATH='/bin:$HOME/bin' sh -c 'exec claude'",
		},
		{
			name:      "leading NAME=VALUE assignment disqualifies exec (exec cannot run it the way env can)",
			command:   "FOO=bar claude",
			resetPath: "/usr/bin:/bin",
			want:      "env PATH='/usr/bin:/bin' sh -c 'FOO=bar claude'",
		},
		{
			name:      "semicolon list disqualifies exec",
			command:   "claude; echo done",
			resetPath: "/usr/bin:/bin",
			want:      "env PATH='/usr/bin:/bin' sh -c 'claude; echo done'",
		},
		{
			name:      "pipeline disqualifies exec",
			command:   "claude | tee /tmp/log",
			resetPath: "/usr/bin:/bin",
			want:      "env PATH='/usr/bin:/bin' sh -c 'claude | tee /tmp/log'",
		},
		{
			name:      "command substitution disqualifies exec",
			command:   "claude --dir $(pwd)",
			resetPath: "/usr/bin:/bin",
			want:      "env PATH='/usr/bin:/bin' sh -c 'claude --dir $(pwd)'",
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
			got, err := withEnvUnsetPrefix(tt.command, tt.unsetKeys, tt.resetPath)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("withEnvUnsetPrefix(%q, %v, %q) = nil error, want an error", tt.command, tt.unsetKeys, tt.resetPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("withEnvUnsetPrefix(%q, %v, %q) unexpected error: %v", tt.command, tt.unsetKeys, tt.resetPath, err)
			}
			if got != tt.want {
				t.Errorf("withEnvUnsetPrefix(%q, %v, %q) = %q, want %q", tt.command, tt.unsetKeys, tt.resetPath, got, tt.want)
			}
		})
	}
}

// TestCommandIsExecSafe pins the classifier withEnvUnsetPrefix uses to decide
// whether a command is safe to force with a literal `exec` inside the sh -c
// wrap. It must stay conservative: every case that would break under exec
// (compound lists, an assignment prefix exec cannot run) has to return false,
// even at the cost of missing an exec-safe case exec could have handled.
func TestCommandIsExecSafe(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{name: "empty command", command: "", want: false},
		{name: "bare program name", command: "claude", want: true},
		{name: "program with flags", command: "claude --model opus --resume", want: true},
		{name: "leading VAR=val assignment", command: "FOO=bar claude", want: false},
		{name: "semicolon list", command: "claude; echo done", want: false},
		{name: "and-list", command: "cd /work && claude", want: false},
		{name: "or-list", command: "claude || true", want: false},
		{name: "pipeline", command: "claude | tee /tmp/log", want: false},
		{name: "command substitution dollar-paren", command: "claude --dir $(pwd)", want: false},
		{name: "backtick substitution", command: "claude --dir `pwd`", want: false},
		{name: "variable expansion", command: "claude --home $HOME", want: false},
		{name: "redirection", command: "claude > /tmp/out", want: false},
		{name: "subshell", command: "(claude)", want: false},
		{name: "single-quoted argument", command: "echo 'hi there'", want: false},
		{name: "double-quoted argument", command: "echo \"hi there\"", want: false},
		{name: "embedded newline", command: "claude\necho done", want: false},
		{name: "background operator", command: "claude &", want: false},
		{name: "cd builtin — exec cd fails outright", command: "cd /x", want: false},
		{name: "source builtin", command: "source f", want: false},
		{name: "dot builtin", command: ". f", want: false},
		{name: "export builtin", command: "export FOO=bar", want: false},
		{name: "ulimit builtin", command: "ulimit -n 1", want: false},
		{name: "umask builtin", command: "umask 022", want: false},
		{name: "exit builtin", command: "exit 0", want: false},
		{name: "set builtin", command: "set -e", want: false},
		{name: "unset builtin", command: "unset FOO", want: false},
		{name: "eval builtin", command: "eval claude", want: false},
		{name: "trap builtin", command: "trap '' INT", want: false},
		{name: "wait builtin", command: "wait 123", want: false},
		{name: "type builtin", command: "type claude", want: false},
		{name: "read builtin", command: "read x", want: false},
		{name: "true/false/test/bracket builtins", command: "true", want: false},
		{name: "echo builtin", command: "echo hi", want: false},
		{name: "pwd builtin", command: "pwd", want: false},
		// Not itself a bare builtin word: an agent binary that merely starts
		// with one (no word boundary match) must still be exec-safe.
		{name: "program name with a builtin as a substring, not the whole word", command: "cdc-agent --flag", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandIsExecSafe(tt.command); got != tt.want {
				t.Errorf("commandIsExecSafe(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}
