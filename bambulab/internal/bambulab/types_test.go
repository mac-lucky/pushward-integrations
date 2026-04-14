package bambulab

import "testing"

func ptr[T any](v T) *T { return &v }

func TestMerge_FullState(t *testing.T) {
	var s MergedState
	s.Merge(&PrintStatus{
		GcodeState:         ptr("RUNNING"),
		SubtaskName:        ptr("Benchy.3mf"),
		GcodeFile:          ptr("/sdcard/Benchy.3mf"),
		Percent:            ptr(42),
		RemainingTime:      ptr(120),
		LayerNum:           ptr(50),
		TotalLayerNum:      ptr(200),
		NozzleTemper:       ptr(215.0),
		NozzleTargetTemper: ptr(220.0),
		BedTemper:          ptr(58.5),
		BedTargetTemper:    ptr(60.0),
		SpeedLevel:         ptr(2),
		SpeedMagnitude:     ptr(100),
		PrintError:         ptr(0),
		FailReason:         ptr(""),
	})

	if s.GcodeState != "RUNNING" {
		t.Errorf("GcodeState = %q, want RUNNING", s.GcodeState)
	}
	if s.SubtaskName != "Benchy.3mf" {
		t.Errorf("SubtaskName = %q, want Benchy.3mf", s.SubtaskName)
	}
	if s.GcodeFile != "/sdcard/Benchy.3mf" {
		t.Errorf("GcodeFile = %q, want /sdcard/Benchy.3mf", s.GcodeFile)
	}
	if s.Percent != 42 {
		t.Errorf("Percent = %d, want 42", s.Percent)
	}
	if s.RemainingTime != 120 {
		t.Errorf("RemainingTime = %d, want 120", s.RemainingTime)
	}
	if s.LayerNum != 50 {
		t.Errorf("LayerNum = %d, want 50", s.LayerNum)
	}
	if s.TotalLayerNum != 200 {
		t.Errorf("TotalLayerNum = %d, want 200", s.TotalLayerNum)
	}
	if s.NozzleTemper != 215.0 {
		t.Errorf("NozzleTemper = %f, want 215.0", s.NozzleTemper)
	}
	if s.NozzleTarget != 220.0 {
		t.Errorf("NozzleTarget = %f, want 220.0", s.NozzleTarget)
	}
	if s.BedTemper != 58.5 {
		t.Errorf("BedTemper = %f, want 58.5", s.BedTemper)
	}
	if s.BedTarget != 60.0 {
		t.Errorf("BedTarget = %f, want 60.0", s.BedTarget)
	}
	if s.SpeedLevel != 2 {
		t.Errorf("SpeedLevel = %d, want 2", s.SpeedLevel)
	}
	if s.SpeedMagnitude != 100 {
		t.Errorf("SpeedMagnitude = %d, want 100", s.SpeedMagnitude)
	}
	if s.PrintError != 0 {
		t.Errorf("PrintError = %d, want 0", s.PrintError)
	}
	if s.FailReason != "" {
		t.Errorf("FailReason = %q, want empty", s.FailReason)
	}
}

func TestMerge_DeltaPreservesExisting(t *testing.T) {
	s := MergedState{
		GcodeState:    "RUNNING",
		SubtaskName:   "Benchy.3mf",
		Percent:       42,
		RemainingTime: 120,
		LayerNum:      50,
		TotalLayerNum: 200,
		NozzleTemper:  215.0,
		NozzleTarget:  220.0,
		BedTemper:     58.5,
		BedTarget:     60.0,
	}

	// Delta: only percent and remaining time changed
	s.Merge(&PrintStatus{
		Percent:       ptr(45),
		RemainingTime: ptr(115),
	})

	// Updated fields
	if s.Percent != 45 {
		t.Errorf("Percent = %d, want 45", s.Percent)
	}
	if s.RemainingTime != 115 {
		t.Errorf("RemainingTime = %d, want 115", s.RemainingTime)
	}

	// Preserved fields (nil pointers don't overwrite)
	if s.GcodeState != "RUNNING" {
		t.Errorf("GcodeState = %q, want RUNNING", s.GcodeState)
	}
	if s.SubtaskName != "Benchy.3mf" {
		t.Errorf("SubtaskName = %q, want Benchy.3mf", s.SubtaskName)
	}
	if s.LayerNum != 50 {
		t.Errorf("LayerNum = %d, want 50", s.LayerNum)
	}
	if s.TotalLayerNum != 200 {
		t.Errorf("TotalLayerNum = %d, want 200", s.TotalLayerNum)
	}
	if s.NozzleTemper != 215.0 {
		t.Errorf("NozzleTemper = %f, want 215.0", s.NozzleTemper)
	}
	if s.NozzleTarget != 220.0 {
		t.Errorf("NozzleTarget = %f, want 220.0", s.NozzleTarget)
	}
	if s.BedTemper != 58.5 {
		t.Errorf("BedTemper = %f, want 58.5", s.BedTemper)
	}
	if s.BedTarget != 60.0 {
		t.Errorf("BedTarget = %f, want 60.0", s.BedTarget)
	}
}

func TestMerge_SequentialAccumulation(t *testing.T) {
	var s MergedState

	// Initial full state (like X1 series pushall response)
	s.Merge(&PrintStatus{
		GcodeState:         ptr("PREPARE"),
		SubtaskName:        ptr("Model.gcode"),
		Percent:            ptr(0),
		RemainingTime:      ptr(180),
		LayerNum:           ptr(0),
		TotalLayerNum:      ptr(300),
		NozzleTemper:       ptr(25.0),
		NozzleTargetTemper: ptr(220.0),
	})

	if s.GcodeState != "PREPARE" {
		t.Fatalf("after merge 1: GcodeState = %q, want PREPARE", s.GcodeState)
	}

	// Delta: state changes to running, nozzle heating up
	s.Merge(&PrintStatus{
		GcodeState:   ptr("RUNNING"),
		NozzleTemper: ptr(180.0),
		Percent:      ptr(1),
		LayerNum:     ptr(1),
	})

	if s.GcodeState != "RUNNING" {
		t.Errorf("after merge 2: GcodeState = %q, want RUNNING", s.GcodeState)
	}
	if s.NozzleTemper != 180.0 {
		t.Errorf("after merge 2: NozzleTemper = %f, want 180.0", s.NozzleTemper)
	}
	if s.NozzleTarget != 220.0 {
		t.Errorf("after merge 2: NozzleTarget preserved = %f, want 220.0", s.NozzleTarget)
	}
	if s.SubtaskName != "Model.gcode" {
		t.Errorf("after merge 2: SubtaskName preserved = %q, want Model.gcode", s.SubtaskName)
	}
	if s.TotalLayerNum != 300 {
		t.Errorf("after merge 2: TotalLayerNum preserved = %d, want 300", s.TotalLayerNum)
	}

	// Delta: progress update only
	s.Merge(&PrintStatus{
		Percent:       ptr(50),
		RemainingTime: ptr(90),
		LayerNum:      ptr(150),
		NozzleTemper:  ptr(220.0),
	})

	if s.Percent != 50 {
		t.Errorf("after merge 3: Percent = %d, want 50", s.Percent)
	}
	if s.RemainingTime != 90 {
		t.Errorf("after merge 3: RemainingTime = %d, want 90", s.RemainingTime)
	}
	if s.LayerNum != 150 {
		t.Errorf("after merge 3: LayerNum = %d, want 150", s.LayerNum)
	}
	if s.GcodeState != "RUNNING" {
		t.Errorf("after merge 3: GcodeState preserved = %q, want RUNNING", s.GcodeState)
	}

	// Delta: finish
	s.Merge(&PrintStatus{
		GcodeState:    ptr("FINISH"),
		Percent:       ptr(100),
		RemainingTime: ptr(0),
		LayerNum:      ptr(300),
	})

	if s.GcodeState != "FINISH" {
		t.Errorf("after merge 4: GcodeState = %q, want FINISH", s.GcodeState)
	}
	if s.Percent != 100 {
		t.Errorf("after merge 4: Percent = %d, want 100", s.Percent)
	}
	if s.LayerNum != 300 {
		t.Errorf("after merge 4: LayerNum = %d, want 300", s.LayerNum)
	}
}

func TestMerge_EmptyPrintStatus(t *testing.T) {
	s := MergedState{
		GcodeState:  "RUNNING",
		Percent:     42,
		SubtaskName: "test.gcode",
	}

	// Empty delta should change nothing
	s.Merge(&PrintStatus{})

	if s.GcodeState != "RUNNING" {
		t.Errorf("GcodeState = %q, want RUNNING", s.GcodeState)
	}
	if s.Percent != 42 {
		t.Errorf("Percent = %d, want 42", s.Percent)
	}
	if s.SubtaskName != "test.gcode" {
		t.Errorf("SubtaskName = %q, want test.gcode", s.SubtaskName)
	}
}

func TestMerge_ZeroValues(t *testing.T) {
	// A pointer to zero is a valid delta value (e.g. percent = 0 at start)
	s := MergedState{
		Percent:       50,
		RemainingTime: 100,
		NozzleTemper:  220.0,
	}

	s.Merge(&PrintStatus{
		Percent:       ptr(0),
		RemainingTime: ptr(0),
		NozzleTemper:  ptr(0.0),
	})

	if s.Percent != 0 {
		t.Errorf("Percent = %d, want 0", s.Percent)
	}
	if s.RemainingTime != 0 {
		t.Errorf("RemainingTime = %d, want 0", s.RemainingTime)
	}
	if s.NozzleTemper != 0.0 {
		t.Errorf("NozzleTemper = %f, want 0.0", s.NozzleTemper)
	}
}
