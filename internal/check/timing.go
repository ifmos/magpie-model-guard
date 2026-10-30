package check

import (
	"errors"
	"fmt"
	"time"
)

const (
	// ScheduleModeFixed uses immediate secondary recheck: 2 errors disable, 1 pass recovers.
	ScheduleModeFixed = "fixed"
	// ScheduleModeStaged uses multi-stage interval checks.
	ScheduleModeStaged = "staged"

	ScheduleStageNormal      = "normal"
	ScheduleStageRetry       = "retry"
	ScheduleStageIsolated    = "isolated"
	ScheduleStageRecovery    = "recovery"
	ScheduleStageUnavailable = "unavailable"

	MinScheduleMin = 1
	MaxScheduleMin = 43200
)

// TimingPolicy defines intervals in minutes for staged scheduling.
type TimingPolicy struct {
	NormalMin          int `json:"normal_min"`
	ErrorRetryMin      int `json:"error_retry_min"`
	IsolationFirstMin  int `json:"isolation_first_min"`
	IsolationLaterMin  int `json:"isolation_later_min"`
	RecoveryConfirmMin int `json:"recovery_confirm_min"`
	UnavailableMin     int `json:"unavailable_min"`
}

// DefaultTimingPolicy returns default intervals (minutes).
func DefaultTimingPolicy() TimingPolicy {
	return TimingPolicy{
		NormalMin:          30,
		ErrorRetryMin:      2,
		IsolationFirstMin:  360,
		IsolationLaterMin:  1440,
		RecoveryConfirmMin: 2,
		UnavailableMin:     30,
	}
}

// ValidateTimingPolicyMinutes checks that a minute value is within 1..43200.
func ValidateTimingPolicyMinutes(min int) error {
	if min < MinScheduleMin || min > MaxScheduleMin {
		return fmt.Errorf("时间间隔须为 %d–%d 分钟", MinScheduleMin, MaxScheduleMin)
	}
	return nil
}

// ValidateGlobalTimingPolicy validates all fields for GuardConfig.
func ValidateGlobalTimingPolicy(p TimingPolicy) error {
	fields := []struct {
		name string
		val  int
	}{
		{"正常检测间隔", p.NormalMin},
		{"错误复测间隔", p.ErrorRetryMin},
		{"首次隔离间隔", p.IsolationFirstMin},
		{"后续隔离间隔", p.IsolationLaterMin},
		{"恢复确认间隔", p.RecoveryConfirmMin},
		{"不可用隔离间隔", p.UnavailableMin},
	}
	for _, f := range fields {
		if err := ValidateTimingPolicyMinutes(f.val); err != nil {
			return fmt.Errorf("%s须为 %d–%d 分钟", f.name, MinScheduleMin, MaxScheduleMin)
		}
	}
	return nil
}

// ValidateTargetTimingPolicy validates non-zero fields for Target override.
func ValidateTargetTimingPolicy(p *TimingPolicy) error {
	if p == nil {
		return nil
	}
	fields := []struct {
		name string
		val  int
	}{
		{"正常检测间隔", p.NormalMin},
		{"错误复测间隔", p.ErrorRetryMin},
		{"首次隔离间隔", p.IsolationFirstMin},
		{"后续隔离间隔", p.IsolationLaterMin},
		{"恢复确认间隔", p.RecoveryConfirmMin},
		{"不可用隔离间隔", p.UnavailableMin},
	}
	for _, f := range fields {
		if f.val != 0 {
			if err := ValidateTimingPolicyMinutes(f.val); err != nil {
				return fmt.Errorf("%s须为 %d–%d 分钟", f.name, MinScheduleMin, MaxScheduleMin)
			}
		}
	}
	return nil
}

// ValidateScheduleMode validates a schedule mode string.
func ValidateScheduleMode(mode string, allowEmpty bool) error {
	if mode == "" {
		if allowEmpty {
			return nil
		}
		return errors.New("调度模式不能为空")
	}
	if mode != ScheduleModeFixed && mode != ScheduleModeStaged {
		return fmt.Errorf("无效调度模式：%s，仅支持 fixed 或 staged", mode)
	}
	return nil
}

// ResolveTimingPolicy merges global defaults, global config, and target override.
func ResolveTimingPolicy(target *TimingPolicy, global TimingPolicy) TimingPolicy {
	res := DefaultTimingPolicy()
	if global.NormalMin > 0 {
		res.NormalMin = global.NormalMin
	}
	if global.ErrorRetryMin > 0 {
		res.ErrorRetryMin = global.ErrorRetryMin
	}
	if global.IsolationFirstMin > 0 {
		res.IsolationFirstMin = global.IsolationFirstMin
	}
	if global.IsolationLaterMin > 0 {
		res.IsolationLaterMin = global.IsolationLaterMin
	}
	if global.RecoveryConfirmMin > 0 {
		res.RecoveryConfirmMin = global.RecoveryConfirmMin
	}
	if global.UnavailableMin > 0 {
		res.UnavailableMin = global.UnavailableMin
	}

	if target != nil {
		if target.NormalMin > 0 {
			res.NormalMin = target.NormalMin
		}
		if target.ErrorRetryMin > 0 {
			res.ErrorRetryMin = target.ErrorRetryMin
		}
		if target.IsolationFirstMin > 0 {
			res.IsolationFirstMin = target.IsolationFirstMin
		}
		if target.IsolationLaterMin > 0 {
			res.IsolationLaterMin = target.IsolationLaterMin
		}
		if target.RecoveryConfirmMin > 0 {
			res.RecoveryConfirmMin = target.RecoveryConfirmMin
		}
		if target.UnavailableMin > 0 {
			res.UnavailableMin = target.UnavailableMin
		}
	}
	return res
}

// EffectiveScheduleMode returns the resolved schedule mode ("fixed" or "staged").
func EffectiveScheduleMode(targetMode, globalMode string) string {
	if targetMode == ScheduleModeFixed || targetMode == ScheduleModeStaged {
		return targetMode
	}
	if globalMode == ScheduleModeFixed || globalMode == ScheduleModeStaged {
		return globalMode
	}
	return ScheduleModeFixed
}

// DefaultStageForState returns the schedule stage corresponding to a Target.State.
func DefaultStageForState(state string) string {
	switch state {
	case StateFailed, StateReview:
		return ScheduleStageIsolated
	case StateUnavailable:
		return ScheduleStageUnavailable
	default:
		return ScheduleStageNormal
	}
}

// StagedTransitionInput holds inputs for state transition in staged mode.
type StagedTransitionInput struct {
	CurrentStage      string
	CurrentState      string
	FailStreak        int
	RecoveryPasses    int
	IsolationRechecks int
	RetryPromptID     string
	FailedPromptID    string
	Verdict           string
	Detail            string
	Policy            TimingPolicy
	Now               time.Time
}

// StagedTransitionOutput holds outputs of state transition in staged mode.
type StagedTransitionOutput struct {
	NextStage         string
	NextState         string
	FailStreak        int
	RecoveryPasses    int
	IsolationRechecks int
	RetryPromptID     string
	NextCheckAt       time.Time
	Reason            string
}

// ApplyStagedTransition calculates the next state, stage, counters, and check time.
func ApplyStagedTransition(in StagedTransitionInput) StagedTransitionOutput {
	stage := in.CurrentStage
	if stage == "" {
		stage = DefaultStageForState(in.CurrentState)
	}
	out := StagedTransitionOutput{
		NextStage:         stage,
		NextState:         in.CurrentState,
		FailStreak:        in.FailStreak,
		RecoveryPasses:    in.RecoveryPasses,
		IsolationRechecks: in.IsolationRechecks,
		RetryPromptID:     in.RetryPromptID,
		Reason:            in.Detail,
	}

	switch in.Verdict {
	case VerdictUnavailable:
		// Unavailable isolates immediately for unavailable_min and interrupts recovery passes.
		out.NextStage = ScheduleStageUnavailable
		out.NextState = StateUnavailable
		out.RecoveryPasses = 0
		// Keep retesting the same prompt that first failed; never silently switch prompts.
		if out.RetryPromptID == "" && in.FailedPromptID != "" {
			out.RetryPromptID = in.FailedPromptID
		}
		out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.UnavailableMin) * time.Minute)
		return out

	case VerdictPassed:
		switch stage {
		case ScheduleStageNormal:
			out.NextStage = ScheduleStageNormal
			out.NextState = StatePassed
			out.FailStreak = 0
			out.RecoveryPasses = 0
			out.IsolationRechecks = 0
			out.RetryPromptID = ""
			out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.NormalMin) * time.Minute)

		case ScheduleStageRetry:
			// Passed while in retry stage (before 3 failures).
			// Never reached isolation, so recover directly to normal!
			out.NextStage = ScheduleStageNormal
			out.NextState = StatePassed
			out.FailStreak = 0
			out.RecoveryPasses = 0
			out.IsolationRechecks = 0
			out.RetryPromptID = ""
			out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.NormalMin) * time.Minute)

		case ScheduleStageIsolated, ScheduleStageUnavailable:
			// First pass while isolated stays blocked; confirm after recovery_confirm_min.
			out.NextStage = ScheduleStageRecovery
			out.NextState = StateFailed // keep blocked!
			out.RecoveryPasses = 1
			out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.RecoveryConfirmMin) * time.Minute)

		case ScheduleStageRecovery:
			if in.RecoveryPasses >= 1 {
				// Second pass restores normal interval and normal prompt selection.
				out.NextStage = ScheduleStageNormal
				out.NextState = StatePassed
				out.FailStreak = 0
				out.RecoveryPasses = 0
				out.IsolationRechecks = 0
				out.RetryPromptID = ""
				out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.NormalMin) * time.Minute)
			} else {
				// 1st pass in recovery stage
				out.NextStage = ScheduleStageRecovery
				out.NextState = StateFailed
				out.RecoveryPasses = 1
				out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.RecoveryConfirmMin) * time.Minute)
			}
		}

	case VerdictFailed:
		switch stage {
		case ScheduleStageNormal:
			// 1st error
			out.FailStreak = 1
			out.NextStage = ScheduleStageRetry
			// Target is NOT isolated yet! Keep unblocked
			if out.NextState == "" || out.NextState == StateUnknown {
				out.NextState = StatePassed
			}
			out.RecoveryPasses = 0
			out.IsolationRechecks = 0
			if in.FailedPromptID != "" {
				out.RetryPromptID = in.FailedPromptID
			}
			out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.ErrorRetryMin) * time.Minute)

		case ScheduleStageRetry:
			out.FailStreak++
			if in.FailedPromptID != "" {
				out.RetryPromptID = in.FailedPromptID
			}
			if out.FailStreak < 3 {
				// 2nd error: still in retry stage! Not isolated yet!
				out.NextStage = ScheduleStageRetry
				out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.ErrorRetryMin) * time.Minute)
			} else {
				// Third consecutive error isolates the target.
				out.NextStage = ScheduleStageIsolated
				out.NextState = StateFailed // Now blocked!
				out.IsolationRechecks = 0
				out.RecoveryPasses = 0
				// First isolation uses isolation_first_min.
				out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.IsolationFirstMin) * time.Minute)
			}

		case ScheduleStageIsolated:
			// Failed again while isolated
			out.FailStreak++
			out.IsolationRechecks++
			out.RecoveryPasses = 0
			out.NextStage = ScheduleStageIsolated
			out.NextState = StateFailed
			// Later failures use isolation_later_min.
			out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.IsolationLaterMin) * time.Minute)

		case ScheduleStageRecovery:
			// Failed during recovery confirmation!
			out.FailStreak++
			out.IsolationRechecks++
			out.RecoveryPasses = 0
			out.NextStage = ScheduleStageIsolated
			out.NextState = StateFailed
			// Later failures use isolation_later_min.
			out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.IsolationLaterMin) * time.Minute)

		case ScheduleStageUnavailable:
			// Failed after unavailable
			out.FailStreak = 1
			out.IsolationRechecks = 0
			out.RecoveryPasses = 0
			out.NextStage = ScheduleStageIsolated
			out.NextState = StateFailed
			out.NextCheckAt = in.Now.Add(time.Duration(in.Policy.IsolationFirstMin) * time.Minute)
		}
	}

	return out
}
