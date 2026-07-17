package events

// HostLoadSample is the periodic host-load event type (vp-qvqk defect 3:
// no host-load stream). Emitted by the controller's host-load sampler at
// patrol cadence on its own goroutine, so load excursions stay
// attributable while the reconcile loop itself is wedged — previously
// every excursion cost a manual ps/uptime forensic and left no
// retrospective series.
//
// HostLoadSample is intentionally omitted from KnownEventTypes AND from
// the payload registry (the same deferral as ProviderHealthGateAlert —
// see the KnownEventTypes comment in events.go): RegisterPayload would
// sweep the payload into the generated EventPayload union on the API
// wire, and the typed SSE projection is a follow-up. Until then,
// subscribers receive it via the custom-event envelope; emitters marshal
// HostLoadSamplePayload so the wire shape stays typed at the source.
const HostLoadSample = "host.load_sample"

// HostLoadSamplePayload is the typed payload for host.load_sample events.
// RunnableProcs and TotalCPUPercent ride alongside the load averages
// because load alone cannot discriminate CPU oversubscription from I/O
// wait: on Darwin the load average also counts uninterruptible/blocked
// threads, so a high load with few runnable processes means
// blocked-on-I/O, not CPU-starved.
type HostLoadSamplePayload struct {
	Load1           float64 `json:"load1" doc:"1-minute host load average."`
	Load5           float64 `json:"load5" doc:"5-minute host load average."`
	Load15          float64 `json:"load15" doc:"15-minute host load average."`
	Cores           int     `json:"cores" doc:"Logical CPU count — the denominator for reading the load averages."`
	RunnableProcs   int     `json:"runnable_procs" doc:"Processes in runnable state (R) at sample time. Discriminates CPU oversubscription (high) from blocked-on-I/O (low) under identical load averages."`
	TotalCPUPercent float64 `json:"total_cpu_percent" doc:"Sum of per-process %CPU across the whole process table (100 = one saturated core)."`
}

// IsEventPayload marks HostLoadSamplePayload as an events.Payload variant
// so the SSE-projection follow-up can register it without reshaping it.
func (HostLoadSamplePayload) IsEventPayload() {}
