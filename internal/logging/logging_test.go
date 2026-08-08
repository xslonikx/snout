package logging

import (
	"log/syslog"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	level, err := parseLevel("Info")
	if err != nil || level != Info {
		t.Fatalf("unexpected level: %v, %v", level, err)
	}
	facility, err := parseFacility("LOCAL0")
	if err != nil || facility != syslog.LOG_LOCAL0 {
		t.Fatalf("unexpected facility: %v, %v", facility, err)
	}
}
