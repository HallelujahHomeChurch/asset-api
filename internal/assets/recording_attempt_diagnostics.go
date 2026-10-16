package assets

import "time"

// Internal attempt evidence; not part of the capture or inventory wire contracts.
type RecordingAttemptDiagnostics struct {
	ObservedAt     time.Time                       `json:"observedAt"`
	ElapsedMS      int64                           `json:"elapsedMs"`
	WorkFinishedAt *time.Time                      `json:"workFinishedAt,omitempty"`
	Phases         map[string]int64                `json:"phaseElapsedMs"`
	Stages         map[string]RecordingStageTiming `json:"stages"`
	FailureStage   string                          `json:"failureStage"`
	FailureReason  string                          `json:"failureReason"`
}

type RecordingStageTiming struct {
	ElapsedMS int64 `json:"workerElapsedMs"`
	Calls     int64 `json:"calls"`
}
