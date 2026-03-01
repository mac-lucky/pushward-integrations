package bambulab

// Report is the top-level MQTT message from the printer.
type Report struct {
	Print *PrintStatus `json:"print,omitempty"`
}

// PrintStatus contains all print status fields from the push_status command.
// P1/A1 series send delta updates (only changed fields), so all fields are pointers
// to distinguish "not sent" from zero values.
type PrintStatus struct {
	Command string `json:"command"`

	// Print state
	GcodeState  *string `json:"gcode_state"`
	SubtaskName *string `json:"subtask_name"`
	GcodeFile   *string `json:"gcode_file"`

	// Progress
	Percent       *int `json:"mc_percent"`
	RemainingTime *int `json:"mc_remaining_time"` // minutes

	// Layers
	LayerNum      *int `json:"layer_num"`
	TotalLayerNum *int `json:"total_layer_num"`

	// Temperatures
	NozzleTemper       *float64 `json:"nozzle_temper"`
	NozzleTargetTemper *float64 `json:"nozzle_target_temper"`
	BedTemper          *float64 `json:"bed_temper"`
	BedTargetTemper    *float64 `json:"bed_target_temper"`

	// Speed
	SpeedLevel     *int `json:"spd_lvl"`
	SpeedMagnitude *int `json:"spd_mag"`

	// Error
	PrintError *int    `json:"print_error"`
	FailReason *string `json:"fail_reason"`
}

// GcodeState constants matching the printer's gcode_state field.
const (
	StateIdle    = "IDLE"
	StatePrepare = "PREPARE"
	StateRunning = "RUNNING"
	StatePause   = "PAUSE"
	StateFailed  = "FAILED"
	StateFinish  = "FINISH"
)

// MergedState holds the fully merged printer state built from delta updates.
type MergedState struct {
	GcodeState     string
	SubtaskName    string
	GcodeFile      string
	Percent        int
	RemainingTime  int // minutes
	LayerNum       int
	TotalLayerNum  int
	NozzleTemper   float64
	NozzleTarget   float64
	BedTemper      float64
	BedTarget      float64
	SpeedLevel     int
	SpeedMagnitude int
	PrintError     int
	FailReason     string
}

// Merge applies a delta PrintStatus update onto the merged state.
func (s *MergedState) Merge(p *PrintStatus) {
	if p.GcodeState != nil {
		s.GcodeState = *p.GcodeState
	}
	if p.SubtaskName != nil {
		s.SubtaskName = *p.SubtaskName
	}
	if p.GcodeFile != nil {
		s.GcodeFile = *p.GcodeFile
	}
	if p.Percent != nil {
		s.Percent = *p.Percent
	}
	if p.RemainingTime != nil {
		s.RemainingTime = *p.RemainingTime
	}
	if p.LayerNum != nil {
		s.LayerNum = *p.LayerNum
	}
	if p.TotalLayerNum != nil {
		s.TotalLayerNum = *p.TotalLayerNum
	}
	if p.NozzleTemper != nil {
		s.NozzleTemper = *p.NozzleTemper
	}
	if p.NozzleTargetTemper != nil {
		s.NozzleTarget = *p.NozzleTargetTemper
	}
	if p.BedTemper != nil {
		s.BedTemper = *p.BedTemper
	}
	if p.BedTargetTemper != nil {
		s.BedTarget = *p.BedTargetTemper
	}
	if p.SpeedLevel != nil {
		s.SpeedLevel = *p.SpeedLevel
	}
	if p.SpeedMagnitude != nil {
		s.SpeedMagnitude = *p.SpeedMagnitude
	}
	if p.PrintError != nil {
		s.PrintError = *p.PrintError
	}
	if p.FailReason != nil {
		s.FailReason = *p.FailReason
	}
}
