package main

import (
	"context"
	"strings"
	"testing"
)

func TestParseBuiltinActionSpec(t *testing.T) {
	spec, err := parseBuiltinActionSpec(`{"collector":"top_processes","sort":"cpu","limit":15}`)
	if err != nil {
		t.Fatalf("parseBuiltinActionSpec returned error: %v", err)
	}
	if spec.Collector != "top_processes" {
		t.Fatalf("unexpected collector: %s", spec.Collector)
	}
	if spec.Sort != "cpu" {
		t.Fatalf("unexpected sort: %s", spec.Sort)
	}
	if spec.Limit != 15 {
		t.Fatalf("unexpected limit: %d", spec.Limit)
	}
}

func TestParseBuiltinActionSpecRejectsUnknownFields(t *testing.T) {
	_, err := parseBuiltinActionSpec(`{"collector":"host_identity","unknown":true}`)
	if err == nil {
		t.Fatalf("expected unknown field rejection")
	}
}

func TestDecodeLegacyProbeText(t *testing.T) {
	if got := decodeLegacyProbeText(`printf "host anomaly categraf container io"`); got != "host anomaly categraf container io" {
		t.Fatalf("unexpected legacy probe decode: %q", got)
	}
	if got := decodeLegacyProbeText(`plain literal signal`); got != "plain literal signal" {
		t.Fatalf("unexpected plain probe decode: %q", got)
	}
}

func TestRunBuiltinActionHostIdentity(t *testing.T) {
	output, err := runBuiltinAction(context.Background(), builtinActionSpec{Collector: "host_identity"})
	if err != nil {
		t.Fatalf("runBuiltinAction returned error: %v", err)
	}
	if !strings.Contains(output, "current_time=") {
		t.Fatalf("expected host_identity output to contain current_time, got: %s", output)
	}
}

func TestNormalizeActionRunLegacyFields(t *testing.T) {
	run := normalizeActionRun(ActionRun{
		LegacyShell:   "go_builtin",
		LegacyCommand: `{"collector":"host_identity"}`,
		ExitCode:      0,
	})
	if run.Engine != "go_builtin" {
		t.Fatalf("unexpected engine: %s", run.Engine)
	}
	if run.Spec != `{"collector":"host_identity"}` {
		t.Fatalf("unexpected spec: %s", run.Spec)
	}
	if run.LegacyShell != "" || run.LegacyCommand != "" {
		t.Fatalf("legacy fields should be cleared: %+v", run)
	}
}

func TestNormalizeWalkRecordMigratesLegacyCommands(t *testing.T) {
	record := normalizeWalkRecord(WalkRecord{
		Steps: []WalkStep{{
			LegacyCommands: []ActionRun{{
				LegacyShell:   "go_builtin",
				LegacyCommand: `{"collector":"host_identity"}`,
			}},
		}},
	})
	if len(record.Steps) != 1 || len(record.Steps[0].Actions) != 1 {
		t.Fatalf("expected migrated actions, got: %+v", record.Steps)
	}
	if record.Steps[0].Actions[0].Spec != `{"collector":"host_identity"}` {
		t.Fatalf("unexpected migrated spec: %+v", record.Steps[0].Actions[0])
	}
}

func TestNormalizeProbeInputLegacyCommand(t *testing.T) {
	probeAction, probeText := normalizeProbeInput("", "", `printf "host anomaly categraf"`)
	if probeAction != "" {
		t.Fatalf("expected no action for legacy literal, got %q", probeAction)
	}
	if probeText != "host anomaly categraf" {
		t.Fatalf("unexpected probe text: %q", probeText)
	}

	probeAction, probeText = normalizeProbeInput("", "", `{"collector":"host_identity"}`)
	if probeAction != `{"collector":"host_identity"}` {
		t.Fatalf("unexpected probe action: %q", probeAction)
	}
	if probeText != "" {
		t.Fatalf("expected empty probe text, got %q", probeText)
	}
}
