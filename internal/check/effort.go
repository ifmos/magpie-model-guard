package check

import (
	"fmt"
	"strings"
)

// ProbeEffortModelDefault is the per-target sentinel that opts out of the
// global thinking level: the probe sends no reasoning_effort and the model
// keeps its own default.
const ProbeEffortModelDefault = "default"

// probeEffortLevels are the reasoning_effort values Magpie accepts for its
// Chat Completions surface ("off" is normalized to none upstream).
var probeEffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// ValidateProbeEffort accepts an empty value (no reasoning_effort sent), a
// known level, or the target-only "default" sentinel.
func ValidateProbeEffort(value string, allowDefault bool) error {
	value = strings.TrimSpace(value)
	if value == "" || (allowDefault && value == ProbeEffortModelDefault) {
		return nil
	}
	for _, level := range probeEffortLevels {
		if value == level {
			return nil
		}
	}
	return fmt.Errorf("思考强度须为 %s 之一", strings.Join(probeEffortLevels, " / "))
}

// EffectiveProbeEffort resolves the effort one probe sends: an unset target
// inherits the global level, and the model-default sentinel sends none.
func EffectiveProbeEffort(target, global string) string {
	target, global = strings.TrimSpace(target), strings.TrimSpace(global)
	switch target {
	case "":
		return global
	case ProbeEffortModelDefault:
		return ""
	default:
		return target
	}
}
