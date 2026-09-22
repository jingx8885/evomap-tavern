package agent

import "testing"

func TestParseStageCommand(t *testing.T) {
	if _, _, ok := parseStageCommand("/desk x"); ok {
		t.Fatal("/desk is not /stage")
	}
	op, arg, ok := parseStageCommand("/stage")
	if !ok || op != "open" || arg != "" {
		t.Fatalf("open got %q %q %v", op, arg, ok)
	}
	op, arg, ok = parseStageCommand("/stage decorate 暖纸金线")
	if !ok || op != "decorate" || arg != "暖纸金线" {
		t.Fatalf("decorate got %q %q", op, arg)
	}
	op, arg, ok = parseStageCommand("/stage image a red rose")
	if !ok || op != "image" || arg != "a red rose" {
		t.Fatalf("image got %q %q", op, arg)
	}
}
