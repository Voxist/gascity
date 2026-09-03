package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processenv"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

// secretsEnvFileName is the dotenv file under GC_HOME that the supervisor
// merges into its service environment. See supervisorSecretsEnvFileName in
// cmd_supervisor_lifecycle.go, which owns the read side.
const secretsEnvFileName = "secrets.env"

// newProviderCredentialsCmd builds `gc provider credentials <provider>`:
// report which environment variable actually backs each of a provider's
// credentials, and optionally write a new value to the machine-local source
// the supervisor reads.
//
// It reports rather than rotates because rotation is not something this
// process can complete. A session's environment is the supervisor's own
// os.Environ(), fixed when the supervisor exec'd; a credential change moves no
// config fingerprint (internal/runtime/fingerprint.go:274-277 says so in as
// many words), so no session restarts on its own; and nothing re-reads the
// supervisor's environment short of the re-exec that `gc restart` performs. A
// command that claimed to rotate a live fleet would be claiming three things
// it cannot do.
func newProviderCredentialsCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		setStdin    bool
		setFromFile string
		role        string
	)
	cmd := &cobra.Command{
		Use:   "credentials <provider>",
		Short: "Show which environment variable backs a provider's credentials, and optionally set it",
		Long: `Report which environment variable actually holds each of a provider's
credentials, and optionally write a new value to the machine-local source the
supervisor reads.

Which variable that is, is not obvious. A provider declares its credential
env-var names through its upstream_env binding (api_key and auth_token; never
base_url), those names are resolved through the provider's inheritance chain,
and each one's value may interpolate a different variable again. This command
performs that resolution and refuses, naming the reason, wherever no single
variable holds the credential.

With --set-stdin or --set-from-file it writes the new value into the
machine-local secrets file under GC_HOME (` + secretsEnvFileName + `) —
atomically, mode 0600, preserving every other line. The credential is never
taken from the command line, so it cannot reach the process argument vector or
the shell history.

Setting a value does NOT apply it. A credential change moves no config
fingerprint, so no agent restarts on its own, and the supervisor resolves
session environment from its own environment, which is fixed at exec. Run
"gc restart" afterwards to re-exec the supervisor and cycle the agents; until
then every session keeps the old credential.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if setStdin && setFromFile != "" {
				fmt.Fprintf(stderr, "gc: --set-stdin and --set-from-file are mutually exclusive\n") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if code := runProviderCredentials(args[0], role, setStdin, setFromFile, stdout, stderr); code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&setStdin, "set-stdin", false, "read the new credential from stdin and write it to the machine-local secrets file")
	cmd.Flags().StringVar(&setFromFile, "set-from-file", "", "read the new credential from this file and write it to the machine-local secrets file")
	cmd.Flags().StringVar(&role, "role", "", "restrict --set to one credential role (api_key or auth_token)")
	return cmd
}

// runProviderCredentials resolves the provider's credential bindings, prints
// them, and performs the optional write. It returns a process exit code.
func runProviderCredentials(providerName, role string, setStdin bool, setFromFile string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc provider credentials: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, _, err := loadCityConfigWithBuiltinPacks(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc provider credentials: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Read the chain-resolved provider, not the raw leaf. Session start
	// resolves the provider through its base chain
	// (cmd/gc/template_resolve.go:163 -> config.ResolveProviderChain), so a
	// provider inheriting its credential entry from a parent has one — and a
	// command reading only the leaf would report, wrongly, that it has none.
	resolved, ok := config.ResolvedProviderCached(cfg, providerName)
	if !ok {
		if _, isBuiltin := config.BuiltinProviders()[providerName]; isBuiltin {
			fmt.Fprintf(stderr, "gc provider credentials: %q is a built-in provider with no entry in this city's config; its credentials come straight from the supervisor environment under the harness's own names\n", providerName) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stderr, "gc provider credentials: no provider %q in this city's config\n", providerName) //nolint:errcheck // best-effort stderr
		}
		return 1
	}

	bindings := providerCredentialSources(&resolved)
	if len(bindings) == 0 {
		fmt.Fprintf(stderr, "gc provider credentials: provider %q declares no upstream_env.api_key or upstream_env.auth_token binding, so which of its env keys holds a credential is not stated anywhere; declare one before rotating\n", providerName) //nolint:errcheck // best-effort stderr
		return 1
	}

	secretsPath := filepath.Join(supervisor.DefaultHome(), secretsEnvFileName)
	renderCredentialBindings(stdout, providerName, resolved, bindings, secretsPath)

	if !setStdin && setFromFile == "" {
		renderCredentialApplyHint(stdout, providerName, secretsPath)
		return 0
	}
	return applyProviderCredential(bindings, role, setStdin, setFromFile, secretsPath, stdout, stderr)
}

// renderCredentialBindings prints one line per declared credential role.
func renderCredentialBindings(w io.Writer, providerName string, resolved config.ResolvedProvider, bindings []credentialBinding, secretsPath string) {
	fmt.Fprintf(w, "Provider: %s\n", providerName) //nolint:errcheck // best-effort stdout
	if len(resolved.Chain) > 1 {
		hops := make([]string, 0, len(resolved.Chain))
		for _, h := range resolved.Chain {
			if h.Kind == "builtin" {
				hops = append(hops, config.BasePrefixBuiltin+h.Name)
				continue
			}
			hops = append(hops, "providers."+h.Name)
		}
		fmt.Fprintf(w, "  chain: %s\n", strings.Join(hops, " → ")) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintln(w) //nolint:errcheck // best-effort stdout

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tHARNESS VAR\tHELD BY\tNOTES") //nolint:errcheck // best-effort stdout
	for _, b := range bindings {
		if b.Refusal != "" {
			fmt.Fprintf(tw, "%s\t%s\t-\tcannot rotate: %s\n", b.Role, b.EnvKey, b.Refusal) //nolint:errcheck // best-effort stdout
			continue
		}
		notes := credentialSourceNotes(b, secretsPath)
		fmt.Fprintf(tw, "%s\t%s\t$%s\t%s\n", b.Role, b.EnvKey, b.SourceVar, notes) //nolint:errcheck // best-effort stdout
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
}

// credentialSourceNotes describes where a source variable's value comes from
// today, and warns when a value exported in this shell would shadow the file.
//
// The shadow matters because it is silent: the supervisor's service file is
// rebuilt from the calling shell's environment first and the secrets file only
// fills keys that scan left unset (cmd_supervisor_lifecycle.go:1150-1185). An
// operator who edits the file from a shell that still exports the old value
// gets no error and no rotation.
func credentialSourceNotes(b credentialBinding, secretsPath string) string {
	var notes []string
	if b.Inherited {
		notes = append(notes, "not set by provider config; forwarded from the supervisor environment")
	}
	inFile := false
	if data, err := os.ReadFile(secretsPath); err == nil {
		if entries, perr := processenv.ParseEnvFile(string(data)); perr == nil {
			_, inFile = entries[b.SourceVar]
		}
	}
	inShell := os.Getenv(b.SourceVar) != ""
	switch {
	case inFile && inShell:
		notes = append(notes, "set in both "+secretsPath+" and this shell; the shell wins when the service file is rebuilt, so a file edit alone will NOT take effect")
	case inFile:
		notes = append(notes, "set in "+secretsPath)
	case inShell:
		notes = append(notes, "set in this shell, not in "+secretsPath)
	default:
		notes = append(notes, "not set here")
	}
	return strings.Join(notes, "; ")
}

// renderCredentialApplyHint states what applying a new credential requires.
// It is printed on the read-only path so the cost is visible before the
// operator commits to anything.
func renderCredentialApplyHint(w io.Writer, providerName, secretsPath string) {
	const hint = `
Setting a credential does not apply it. A credential change moves no config
fingerprint, so no agent restarts on its own, and the supervisor resolves
session environment from its own environment, fixed when it exec'd.

To rotate:
  1. gc provider credentials %s --set-stdin   # writes %s
  2. gc restart                                # re-execs the supervisor, cycles agents

Until step 2 every running session keeps the old credential.
`
	fmt.Fprintf(w, hint, providerName, secretsPath) //nolint:errcheck // best-effort stdout
}

// applyProviderCredential reads the new credential off stdin or a file and
// writes it to the machine-local secrets file.
func applyProviderCredential(bindings []credentialBinding, role string, setStdin bool, setFromFile, secretsPath string, stdout, stderr io.Writer) int {
	targets, err := credentialWriteTargets(bindings, role)
	if err != nil {
		fmt.Fprintf(stderr, "gc provider credentials: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	secret, err := readCredential(setStdin, setFromFile)
	if err != nil {
		fmt.Fprintf(stderr, "gc provider credentials: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	assignments := make(map[string]string, len(targets))
	for _, t := range targets {
		assignments[t] = secret
	}
	if err := upsertSecretsEnvFile(secretsPath, assignments); err != nil {
		fmt.Fprintf(stderr, "gc provider credentials: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	for _, t := range targets {
		fmt.Fprintf(stdout, "wrote %s to %s\n", t, secretsPath) //nolint:errcheck // best-effort stdout
		if os.Getenv(t) != "" {
			fmt.Fprintf(stdout, "WARNING: %s is also exported in this shell. The service file is rebuilt from the shell first, so this file entry will be ignored and the rotation will NOT take effect. Unset it before running gc restart.\n", t) //nolint:errcheck // best-effort stdout
		}
	}
	fmt.Fprintf(stdout, "\nNot yet applied. Run 'gc restart' to re-exec the supervisor and cycle the agents;\nuntil then every running session keeps the old credential.\n") //nolint:errcheck // best-effort stdout
	return 0
}

// credentialWriteTargets picks the source variables a --set should write.
//
// When the usable roles resolve to more than one distinct variable they hold
// separate credentials, and writing one value to both would overwrite a
// credential the operator did not name. That requires --role rather than a
// guess.
func credentialWriteTargets(bindings []credentialBinding, role string) ([]string, error) {
	seen := make(map[string]bool)
	var targets []string
	var refused []string
	for _, b := range bindings {
		if role != "" && b.Role != role {
			continue
		}
		if b.Refusal != "" {
			refused = append(refused, fmt.Sprintf("%s: %s", b.Role, b.Refusal))
			continue
		}
		if seen[b.SourceVar] {
			continue
		}
		seen[b.SourceVar] = true
		targets = append(targets, b.SourceVar)
	}
	sort.Strings(targets)

	switch {
	case len(targets) == 0 && role != "":
		if len(refused) > 0 {
			return nil, fmt.Errorf("role %q cannot be rotated — %s", role, strings.Join(refused, "; "))
		}
		return nil, fmt.Errorf("provider declares no %q credential role", role)
	case len(targets) == 0:
		return nil, fmt.Errorf("no credential role can be rotated — %s", strings.Join(refused, "; "))
	case len(targets) > 1:
		return nil, fmt.Errorf("the credential roles resolve to different variables (%s); they hold separate credentials, so name one with --role api_key or --role auth_token", strings.Join(targets, ", "))
	}
	return targets, nil
}

// readCredential reads the new credential from stdin or a file, never from
// argv. A trailing newline is stripped because both `pass show` and a
// heredoc add one; anything else is taken literally.
func readCredential(setStdin bool, path string) (string, error) {
	var (
		data []byte
		err  error
	)
	if setStdin {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading the credential from stdin: %w", err)
		}
	} else {
		data, err = os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading the credential from %s: %w", path, err)
		}
	}
	secret := strings.TrimRight(string(data), "\r\n")
	if secret == "" {
		return "", fmt.Errorf("the supplied credential is empty")
	}
	return secret, nil
}
