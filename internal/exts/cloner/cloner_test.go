package cloner

import (
	"testing"

	"bpkit/internal/bpclient"
)

// The picker must mark which sources can actually be cloned, so the only
// Windows Event source in a mixed config is the one offered.
func TestListSourcesMarksWindowsEvent(t *testing.T) {
	cfg := bpclient.Resource{"spec": map[string]any{"sources": []any{
		map[string]any{"name": "s-1", "displayName": "OTLP in", "type": "otlp"},
		map[string]any{"name": "s-2", "displayName": "Win events", "type": "windowsevents_v2:6"},
		map[string]any{"name": "s-3", "type": "windows_event"},
	}}}
	got := listSources(cfg)
	if len(got) != 3 {
		t.Fatalf("got %d sources", len(got))
	}
	if got[0].ok {
		t.Error("otlp must not be clonable")
	}
	if !got[1].ok || !got[2].ok {
		t.Error("both Windows Event sources must be clonable")
	}
	if got[0].label != "OTLP in" || got[2].label != "s-3" {
		t.Errorf("labels: %q %q", got[0].label, got[2].label)
	}
	// The cursor should land on something clonable, not on the OTLP source.
	if i := firstClonable(got); i != 1 {
		t.Errorf("firstClonable = %d", i)
	}
}

func TestListSourcesEmptyConfig(t *testing.T) {
	if got := listSources(bpclient.Resource{}); got != nil {
		t.Fatalf("want nil for a config with no spec, got %+v", got)
	}
}
