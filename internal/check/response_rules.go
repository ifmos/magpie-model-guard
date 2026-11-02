package check

import (
	"fmt"
	"regexp"
	"strings"
)

// Response-model judgment constants.
const (
	ResponseModelOff = "off"
	ResponseModelOn  = "on"

	ResponseMatchExact    = "exact"
	ResponseMatchContains = "contains"
	ResponseMatchGlob     = "glob"
	ResponseMatchRegex    = "regex"

	ResponseActionDisable = "disable"
	ResponseActionReroute = "reroute"
	ResponseActionRecord  = "record"

	ResponseRuleInherit = "inherit"
	ResponseRuleCustom  = "custom"
)

// ResponseModelRule matches the real model returned in a business reply.
// Pattern matching is case-insensitive; regex may opt out with (?-i).
type ResponseModelRule struct {
	ID       string `json:"id"`
	Match    string `json:"match"`
	Pattern  string `json:"pattern"`
	Action   string `json:"action"`
	Disabled bool   `json:"disabled,omitempty"`
}

// matches reports whether the rule's pattern matches a returned model id.
func (r ResponseModelRule) matches(model string) bool {
	model = strings.TrimSpace(model)
	pattern := strings.TrimSpace(r.Pattern)
	if model == "" || pattern == "" {
		return false
	}
	switch r.Match {
	case ResponseMatchExact:
		return strings.EqualFold(model, pattern)
	case ResponseMatchContains:
		return strings.Contains(strings.ToLower(model), strings.ToLower(pattern))
	case ResponseMatchGlob:
		return globMatch(pattern, model)
	case ResponseMatchRegex:
		return patternMatches("re:"+pattern, model)
	}
	return false
}

func validResponseMatch(v string) bool {
	switch v {
	case ResponseMatchExact, ResponseMatchContains, ResponseMatchGlob, ResponseMatchRegex:
		return true
	}
	return false
}

func validResponseAction(v string) bool {
	switch v {
	case ResponseActionDisable, ResponseActionReroute, ResponseActionRecord:
		return true
	}
	return false
}

func validResponseRuleMode(v string) bool {
	switch v {
	case "", ResponseRuleInherit, ResponseRuleCustom:
		return true
	}
	return false
}

// NormalizeResponseRules validates rules, fills defaults and stable ids.
func NormalizeResponseRules(rules []ResponseModelRule) ([]ResponseModelRule, error) {
	out := make([]ResponseModelRule, 0, len(rules))
	ids := make(map[string]bool, len(rules))
	for i, r := range rules {
		r.ID = strings.TrimSpace(r.ID)
		r.Pattern = strings.TrimSpace(r.Pattern)
		if r.Match == "" {
			r.Match = ResponseMatchContains
		}
		if r.Action == "" {
			r.Action = ResponseActionDisable
		}
		if !validResponseMatch(r.Match) {
			return nil, fmt.Errorf("规则 %d 的匹配方式无效", i+1)
		}
		if !validResponseAction(r.Action) {
			return nil, fmt.Errorf("规则 %d 的动作无效", i+1)
		}
		if r.Pattern == "" {
			return nil, fmt.Errorf("规则 %d 的匹配字符串不能为空", i+1)
		}
		if len(r.Pattern) > 512 || strings.ContainsAny(r.Pattern, "\r\n") {
			return nil, fmt.Errorf("规则 %d 的匹配字符串过长或含换行", i+1)
		}
		if r.Match == ResponseMatchRegex {
			if _, err := regexp.Compile("(?i)^(?:" + r.Pattern + ")$"); err != nil {
				if _, err := regexp.Compile(r.Pattern); err != nil {
					return nil, fmt.Errorf("规则 %d 的正则无效: %v", i+1, err)
				}
			}
		}
		if r.ID == "" {
			for {
				r.ID = "rule-" + randomHex(4)
				if !ids[r.ID] {
					break
				}
			}
		} else if ids[r.ID] {
			return nil, fmt.Errorf("规则 ID %s 重复", r.ID)
		}
		ids[r.ID] = true
		out = append(out, r)
	}
	return out, nil
}

// MatchResponseRule returns the first active rule that matches model.
func MatchResponseRule(rules []ResponseModelRule, model string) *ResponseModelRule {
	for i := range rules {
		r := rules[i]
		if r.Disabled {
			continue
		}
		if r.matches(model) {
			cp := r
			return &cp
		}
	}
	return nil
}

// effectiveResponseRules picks a target's custom rules, else the global set
// when the global mode is on.
func effectiveResponseRules(cfg GuardConfig, t *Target) []ResponseModelRule {
	if t != nil && t.ResponseRuleMode == ResponseRuleCustom {
		return t.ResponseModelRules
	}
	if cfg.ResponseModelMode == ResponseModelOn {
		return cfg.ResponseRules
	}
	return nil
}

// hasResponseRules reports whether any active business-reply rule exists.
func hasResponseRules(cfg GuardConfig, targets []*Target) bool {
	check := func(rules []ResponseModelRule) bool {
		for _, r := range rules {
			if !r.Disabled {
				return true
			}
		}
		return false
	}
	if cfg.ResponseModelMode == ResponseModelOn && check(cfg.ResponseRules) {
		return true
	}
	for _, t := range targets {
		if t != nil && t.ResponseRuleMode == ResponseRuleCustom && check(t.ResponseModelRules) {
			return true
		}
	}
	return false
}

// ResponseModelEvent is one completed business reply reported by Magpie's
// async response hook.
type ResponseModelEvent struct {
	RequestedModel string `json:"requested_model"`
	ActualModel    string `json:"actual_model"`
	Protocol       string `json:"protocol,omitempty"`
	Agent          string `json:"agent,omitempty"`
	Success        bool   `json:"success"`
}

// ResponseModelHook is the JSON handshake between the Go backend and the
// Magpie async response hook.
type ResponseModelHook struct {
	Origin  string `json:"origin"`
	Control string `json:"control"`
	Enabled bool   `json:"enabled"`
}
