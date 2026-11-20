package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// writeAuthKeyReloadStatus classifies the write-auth key step of a supervisor
// reload.
type writeAuthKeyReloadStatus string

const (
	// writeAuthKeysNotManaged means supervisor.toml sets no write-auth key and
	// a reload could not change the installed state anyway: write-auth is off,
	// or GC_CITY_WRITE_PUBKEY supplies the keys. There is nothing to report.
	writeAuthKeysNotManaged writeAuthKeyReloadStatus = "not_managed"
	// writeAuthKeysReloaded means the key list in supervisor.toml is installed.
	writeAuthKeysReloaded writeAuthKeyReloadStatus = "reloaded"
	// writeAuthKeysRefused means the reload kept the installed keys; Reason
	// says why.
	writeAuthKeysRefused writeAuthKeyReloadStatus = "refused"
)

// writeAuthKeyReload is the outcome of the write-auth key step of a supervisor
// reload. The kid lists are set only when Status is writeAuthKeysReloaded.
type writeAuthKeyReload struct {
	Status   writeAuthKeyReloadStatus `json:"status"`
	Added    []string                 `json:"added,omitempty"`
	Removed  []string                 `json:"removed,omitempty"`
	Replaced []string                 `json:"replaced,omitempty"`
	Reason   string                   `json:"reason,omitempty"`
}

// summary renders the outcome as one line for the supervisor log and for
// `gc supervisor reload`, or returns "" when there is nothing to report.
func (r writeAuthKeyReload) summary() string {
	switch r.Status {
	case writeAuthKeysReloaded:
		var changes []string
		for _, c := range []struct {
			verb string
			kids []string
		}{{"added", r.Added}, {"removed", r.Removed}, {"replaced", r.Replaced}} {
			if len(c.kids) > 0 {
				changes = append(changes, fmt.Sprintf("%s %v", c.verb, c.kids))
			}
		}
		if len(changes) == 0 {
			return "write-auth keys reloaded: no change"
		}
		return "write-auth keys reloaded: " + strings.Join(changes, ", ")
	case writeAuthKeysRefused:
		return "write-auth keys not reloaded: " + r.Reason
	default:
		return ""
	}
}

// reloadSupervisorWriteAuthKeys re-reads supervisor.toml and swaps the
// write-auth verifying keys installed on apiMux for its write_auth_verify_key
// entries, so enrolling, rotating, or revoking a grant key needs no supervisor
// restart. It logs the outcome to stderr and returns it for the client that
// asked for a reload report. Any failure (unreadable config, malformed keys,
// write-auth off at startup, or a GC_CITY_WRITE_PUBKEY override) keeps the
// current keys. A supervisor.toml with no key reloads silently when write-auth
// is off or its keys come from GC_CITY_WRITE_PUBKEY. Other supervisor.toml
// changes still require a restart.
func reloadSupervisorWriteAuthKeys(apiMux *api.SupervisorMux, stderr io.Writer) writeAuthKeyReload {
	result := applySupervisorWriteAuthKeys(apiMux)
	if line := result.summary(); line != "" {
		fmt.Fprintf(stderr, "gc supervisor: reload: %s\n", line) //nolint:errcheck // best-effort stderr
	}
	return result
}

// applySupervisorWriteAuthKeys installs supervisor.toml's write_auth_verify_key
// on apiMux and classifies the outcome.
func applySupervisorWriteAuthKeys(apiMux *api.SupervisorMux) writeAuthKeyReload {
	path := supervisor.ConfigPath()
	cfg, err := supervisorLoadConfig(path)
	if err != nil {
		return writeAuthKeyReload{Status: writeAuthKeysRefused, Reason: fmt.Sprintf("reading %s: %v", path, err)}
	}
	configKey := cfg.Supervisor.WriteAuthVerifyKey
	change, err := api.ReloadWriteAuthKeys(apiMux, configKey)
	switch {
	case err == nil:
		return writeAuthKeyReload{
			Status:   writeAuthKeysReloaded,
			Added:    change.Added,
			Removed:  change.Removed,
			Replaced: change.Replaced,
		}
	case strings.TrimSpace(configKey) == "" &&
		(errors.Is(err, api.ErrWriteAuthNotEnabled) || errors.Is(err, api.ErrWriteAuthEnvOverride)):
		return writeAuthKeyReload{Status: writeAuthKeysNotManaged}
	default:
		return writeAuthKeyReload{Status: writeAuthKeysRefused, Reason: err.Error()}
	}
}

// gcTuningReloadStatus classifies the GC-tuning check of a supervisor reload.
type gcTuningReloadStatus string

const (
	// gcTuningUnchanged means supervisor.toml's gogc, memory_limit_mib and
	// pprof_addr match what the supervisor applied at startup, or the file
	// could not be read (the write-auth key step reports that). There is
	// nothing to report.
	gcTuningUnchanged gcTuningReloadStatus = ""
	// gcTuningChanged means supervisor.toml's values differ from the ones
	// applied at startup; they take effect on the next start.
	gcTuningChanged gcTuningReloadStatus = "changed"
	// gcTuningInvalid means supervisor.toml holds a value the next start will
	// refuse.
	gcTuningInvalid gcTuningReloadStatus = "invalid"
)

// gcTuningReload is the outcome of the GC-tuning check of a supervisor reload.
// A reload never applies gogc, memory_limit_mib or pprof_addr; it only
// reports when supervisor.toml no longer matches what the supervisor applied
// at startup.
type gcTuningReload struct {
	Status gcTuningReloadStatus `json:"status"`
	Detail string               `json:"detail,omitempty"`
}

// summary renders the outcome as one line for the supervisor log and for
// `gc supervisor reload`, or returns "" when there is nothing to report.
func (r gcTuningReload) summary() string {
	switch r.Status {
	case gcTuningChanged:
		return "GC tuning changed in supervisor.toml (" + r.Detail + "); not applied by reload, takes effect on restart"
	case gcTuningInvalid:
		return "GC tuning invalid in supervisor.toml: " + r.Detail + "; nothing applied, and the next start will refuse"
	default:
		return ""
	}
}

// checkSupervisorGCTuningReload re-reads supervisor.toml and compares its
// gogc, memory_limit_mib and pprof_addr with startup, the values the
// supervisor applied when it started. It applies nothing. It logs the outcome
// to stderr and returns it for the client that asked for a reload report.
func checkSupervisorGCTuningReload(startup supervisor.Section, stderr io.Writer) gcTuningReload {
	result := classifySupervisorGCTuningReload(startup)
	if line := result.summary(); line != "" {
		fmt.Fprintf(stderr, "gc supervisor: reload: %s\n", line) //nolint:errcheck // best-effort stderr
	}
	return result
}

// classifySupervisorGCTuningReload is checkSupervisorGCTuningReload without
// the log line.
func classifySupervisorGCTuningReload(startup supervisor.Section) gcTuningReload {
	cfg, err := supervisorLoadConfig(supervisor.ConfigPath())
	if err != nil {
		return gcTuningReload{Status: gcTuningUnchanged}
	}
	current := cfg.Supervisor
	if err := current.ValidateGCTuning(); err != nil {
		return gcTuningReload{Status: gcTuningInvalid, Detail: err.Error()}
	}
	if err := current.ValidatePprofAddr(); err != nil {
		return gcTuningReload{Status: gcTuningInvalid, Detail: err.Error()}
	}
	var changes []string
	if a, b := formatOptionalInt(startup.GOGC), formatOptionalInt(current.GOGC); a != b {
		changes = append(changes, fmt.Sprintf("gogc %s -> %s", a, b))
	}
	if a, b := formatOptionalInt64(startup.MemoryLimitMiB), formatOptionalInt64(current.MemoryLimitMiB); a != b {
		changes = append(changes, fmt.Sprintf("memory_limit_mib %s -> %s", a, b))
	}
	if a, b := formatOptionalString(startup.PprofAddr), formatOptionalString(current.PprofAddr); a != b {
		changes = append(changes, fmt.Sprintf("pprof_addr %s -> %s", a, b))
	}
	if len(changes) == 0 {
		return gcTuningReload{Status: gcTuningUnchanged}
	}
	return gcTuningReload{Status: gcTuningChanged, Detail: strings.Join(changes, ", ")}
}

// formatOptionalInt renders an optional supervisor.toml integer, "unset" when
// the key is absent.
func formatOptionalInt(v *int) string {
	if v == nil {
		return "unset"
	}
	return strconv.Itoa(*v)
}

// formatOptionalString renders an optional supervisor.toml string, "unset"
// when the key is absent or empty.
func formatOptionalString(v string) string {
	if v == "" {
		return "unset"
	}
	return strconv.Quote(v)
}

// formatOptionalInt64 is formatOptionalInt for an int64 key.
func formatOptionalInt64(v *int64) string {
	if v == nil {
		return "unset"
	}
	return strconv.FormatInt(*v, 10)
}

// supervisorReloadReportCommand is the control-socket verb `gc supervisor
// reload` sends. The supervisor reloads exactly as for the bare "reload" verb
// but answers with one supervisorReloadReply JSON line that also carries the
// write-auth key outcome, so the command can report a refused key list instead
// of plain success. The bare verb, which city lifecycle commands and
// controller pokes send, keeps its "ok"/"busy"/"timeout" reply. A supervisor
// that predates this verb closes the connection without replying.
const supervisorReloadReportCommand = "reload-report"

// supervisorReloadOutcome is how far a reload got before the supervisor
// replied. The values are the bare "reload" verb's replies.
type supervisorReloadOutcome string

const (
	// supervisorReloadOK means the reload's reconcile finished.
	supervisorReloadOK supervisorReloadOutcome = "ok"
	// supervisorReloadBusy means the reload could not be queued.
	supervisorReloadBusy supervisorReloadOutcome = "busy"
	// supervisorReloadTimeout means the reload was queued but its reconcile
	// did not finish in time.
	supervisorReloadTimeout supervisorReloadOutcome = "timeout"
)

// supervisorReloadReply answers supervisorReloadReportCommand.
type supervisorReloadReply struct {
	Outcome supervisorReloadOutcome `json:"outcome"`
	// WriteAuthKeys is the outcome of the reload's write-auth key step. It is
	// absent when the supervisor replied before that step ran: always for
	// busy, and for a timeout while an earlier reconcile still held the
	// supervisor's main loop.
	WriteAuthKeys *writeAuthKeyReload `json:"write_auth_keys,omitempty"`
	// GCTuning is the outcome of the reload's GC-tuning check. It is absent
	// when there is nothing to report or the supervisor replied before the
	// check ran.
	GCTuning *gcTuningReload `json:"gc_tuning,omitempty"`
}

// reportWriteAuthKeys hands the write-auth key outcome to the client waiting
// on r, if any. The channel is buffered, so the main loop never blocks here.
func (r reconcileRequest) reportWriteAuthKeys(result writeAuthKeyReload) {
	if r.writeAuthKeys != nil {
		r.writeAuthKeys <- result
	}
}

// reportGCTuning hands the GC-tuning check to the client waiting on r, if
// any. The channel is buffered, so the main loop never blocks here.
func (r reconcileRequest) reportGCTuning(result gcTuningReload) {
	if r.gcTuning != nil {
		r.gcTuning <- result
	}
}

// handleSupervisorReloadReport serves supervisorReloadReportCommand on conn. It
// queues a reload as the bare "reload" verb does and replies once the
// reconcile finishes, the queue stays full, or the wait times out.
func handleSupervisorReloadReport(conn net.Conn, reconcileCh chan reconcileRequest) {
	req := reconcileRequest{done: make(chan struct{}), writeAuthKeys: make(chan writeAuthKeyReload, 1), gcTuning: make(chan gcTuningReload, 1)}
	select {
	case reconcileCh <- req:
	case <-time.After(supervisorReloadQueueTimeout):
		writeJSONLine(conn, supervisorReloadReply{Outcome: supervisorReloadBusy})
		return
	}
	reply := supervisorReloadReply{Outcome: supervisorReloadTimeout}
	select {
	case <-req.done:
		reply.Outcome = supervisorReloadOK
	case <-time.After(supervisorReloadWaitTimeout):
	}
	// The main loop reports the key outcome before it reconciles, so it is
	// already buffered once done closes, and a reply that times out during the
	// reconcile still carries it.
	select {
	case keys := <-req.writeAuthKeys:
		reply.WriteAuthKeys = &keys
	default:
	}
	select {
	case gc := <-req.gcTuning:
		if gc.Status != gcTuningUnchanged {
			reply.GCTuning = &gc
		}
	default:
	}
	writeJSONLine(conn, reply)
}

// errNoSupervisorReloadReport reports that the supervisor closed the
// connection without answering supervisorReloadReportCommand, as a supervisor
// that predates the verb does.
var errNoSupervisorReloadReport = errors.New("supervisor sent no reload report")

// reloadSupervisorReport is `gc supervisor reload`: it reloads the running
// supervisor and reports both the reconcile and the write-auth key outcome,
// exiting 1 when the supervisor refused the configured key list. A supervisor
// that predates the report verb is reloaded with the bare verb instead, with a
// warning that it does not reload write-auth keys.
func reloadSupervisorReport(stdout, stderr io.Writer, jsonOut bool) int {
	sockPath, _ := runningSupervisorSocket()
	if sockPath == "" {
		fmt.Fprintln(stderr, "gc supervisor reload: supervisor is not running; start it with 'gc supervisor start'") //nolint:errcheck // best-effort stderr
		return 1
	}
	reply, err := requestSupervisorReloadReport(sockPath)
	if errors.Is(err, errNoSupervisorReloadReport) {
		fmt.Fprintln(stderr, "gc supervisor reload: warning: the running supervisor does not reload write-auth keys; restart it to apply write_auth_verify_key changes") //nolint:errcheck // best-effort stderr
		return reloadSupervisorJSON(stdout, stderr, jsonOut)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor reload: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return writeSupervisorReloadReply(stdout, stderr, jsonOut, reply)
}

// requestSupervisorReloadReport sends supervisorReloadReportCommand to the
// supervisor socket at sockPath and returns its reply.
func requestSupervisorReloadReport(sockPath string) (supervisorReloadReply, error) {
	conn, err := dialGCSocket(sockPath, 2*time.Second)
	if err != nil {
		return supervisorReloadReply{}, fmt.Errorf("connecting to supervisor: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	return exchangeSupervisorReloadReport(conn)
}

// exchangeSupervisorReloadReport sends supervisorReloadReportCommand on conn
// and decodes the one-line reply. A connection closed with no reply yields
// errNoSupervisorReloadReport.
func exchangeSupervisorReloadReport(conn net.Conn) (supervisorReloadReply, error) {
	if err := conn.SetDeadline(time.Now().Add(supervisorReloadWaitTimeout + 5*time.Second)); err != nil {
		return supervisorReloadReply{}, fmt.Errorf("setting supervisor reload deadline: %w", err)
	}
	if _, err := io.WriteString(conn, supervisorReloadReportCommand+"\n"); err != nil {
		return supervisorReloadReply{}, fmt.Errorf("sending reload to supervisor: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	line = bytes.TrimSpace(line)
	switch {
	case len(line) == 0 && (err == nil || errors.Is(err, io.EOF)):
		return supervisorReloadReply{}, errNoSupervisorReloadReport
	case len(line) == 0:
		return supervisorReloadReply{}, fmt.Errorf("supervisor not responding (may be shutting down): %w", err)
	}
	var reply supervisorReloadReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return supervisorReloadReply{}, fmt.Errorf("decoding supervisor reload reply %q: %w", line, err)
	}
	return reply, nil
}

// writeSupervisorReloadReply prints reply for `gc supervisor reload` and
// returns the command's exit code: 0 only when the reconcile finished and the
// supervisor did not refuse the configured write-auth keys. In JSON mode stdout
// carries only the JSON result, so every other line goes to stderr.
func writeSupervisorReloadReply(stdout, stderr io.Writer, jsonOut bool, reply supervisorReloadReply) int {
	switch reply.Outcome {
	case supervisorReloadOK:
	case supervisorReloadBusy:
		fmt.Fprintln(stderr, "gc supervisor reload: reconcile queue is busy; try again shortly") //nolint:errcheck // best-effort stderr
		return 1
	case supervisorReloadTimeout:
		printWriteAuthKeyReload(stdout, stderr, jsonOut, reply.WriteAuthKeys)
		printGCTuningReload(stdout, stderr, jsonOut, reply.GCTuning)
		fmt.Fprintln(stderr, "gc supervisor reload: reconcile did not finish before timeout") //nolint:errcheck // best-effort stderr
		return 1
	default:
		fmt.Fprintf(stderr, "gc supervisor reload: unexpected supervisor reply outcome %q\n", reply.Outcome) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !jsonOut {
		fmt.Fprintln(stdout, "Reconciliation triggered.") //nolint:errcheck // best-effort stdout
	}
	refused := printWriteAuthKeyReload(stdout, stderr, jsonOut, reply.WriteAuthKeys)
	invalid := printGCTuningReload(stdout, stderr, jsonOut, reply.GCTuning)
	if refused || invalid {
		return 1
	}
	if jsonOut {
		return writeLifecycleActionJSONOrExit(stdout, stderr, "gc supervisor reload", lifecycleActionJSON{
			Command: "supervisor reload",
			Action:  "reload",
			Message: "Reconciliation triggered.",
		})
	}
	return 0
}

// printWriteAuthKeyReload prints the write-auth key outcome when there is one
// to report and reports whether the supervisor refused the configured keys. A
// refusal goes to stderr; a reload goes to stdout, or to stderr in JSON mode.
func printWriteAuthKeyReload(stdout, stderr io.Writer, jsonOut bool, keys *writeAuthKeyReload) (refused bool) {
	if keys == nil {
		return false
	}
	refused = keys.Status == writeAuthKeysRefused
	line := keys.summary()
	switch {
	case line == "":
	case refused || jsonOut:
		fmt.Fprintf(stderr, "gc supervisor reload: %s\n", line) //nolint:errcheck // best-effort stderr
	default:
		fmt.Fprintln(stdout, line) //nolint:errcheck // best-effort stdout
	}
	return refused
}

// printGCTuningReload prints the GC-tuning check when there is one to report
// and reports whether supervisor.toml holds a value the next start will
// refuse. An invalid value goes to stderr; a change goes to stdout, or to
// stderr in JSON mode.
func printGCTuningReload(stdout, stderr io.Writer, jsonOut bool, gc *gcTuningReload) (invalid bool) {
	if gc == nil {
		return false
	}
	invalid = gc.Status == gcTuningInvalid
	line := gc.summary()
	switch {
	case line == "":
	case invalid || jsonOut:
		fmt.Fprintf(stderr, "gc supervisor reload: %s\n", line) //nolint:errcheck // best-effort stderr
	default:
		fmt.Fprintln(stdout, line) //nolint:errcheck // best-effort stdout
	}
	return invalid
}
