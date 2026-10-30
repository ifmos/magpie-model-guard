package check

import (
	"fmt"
	"regexp"
	"strings"
)

// Builtin prompt texts. The candy puzzle text is kept byte-identical to the
// community canonical version; do not normalize whitespace.

const candyPrompt = "不使用任何外部工具回答以下问题：\n\n在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）\n\n        苹果味  桃子味  西瓜味\n圆形       7      9      8\n五角星形   7      6      4\n"

// BuiltinPrompt is a prompt shipped with the plugin.
type BuiltinPrompt struct {
	ID   string
	Name string
	Text string
}

// Builtins returns the builtin prompt bank. The candy puzzle is the only
// one with an automatic grader; everything else is a manual-review sample.
func Builtins() []BuiltinPrompt {
	return []BuiltinPrompt{
		{ID: "candy", Name: "糖果推理", Text: candyPrompt},
		{ID: "repeat", Name: "逐字复述", Text: "請復述以下每一個單詞並解釋他的涵義：\n\n1. .DataGridViewColumnHeadersHeightSizeMode\n2. 日以上更新していないブログに表示しています\n3. 锅内倒入植物油烧热\n4. 开通天眼生意通银牌及以上会员\n5. 百度百科内容由网友共同编辑 \n6. \"EDMFunc\"\n7. StarSrvGroupBody\n8. 给主人留下些什么吧\n9. 方も多いのではないでしょうか\n10. 转载请附上原文出处链接及本声明\n11. 不代表新浪看点观点或立场\n12. 豫冠薰衣草疤痕精华素\n13. 请问 everydaycalculation是什么意思\n14. 百度百科企业词条\n15. intFragmentation\n\n"},
		{ID: "japan", Name: "知识覆盖 / 日本首相", Text: "不联网搜索，直接告诉我当今的日本首相是谁\n\n"},
		{ID: "person", Name: "知识覆盖 / 人物", Text: "don’t search the internet, who is Thibault Sottiaux on X\n\n"},
		{ID: "svg", Name: "SVG 创作", Text: "Generate an SVG of a pelican riding a bicycle\n\n"},
	}
}

// GraderKind is the automatic grading strategy of a custom prompt.
type GraderKind string

const (
	// GraderContains passes when the answer contains the expected substring.
	GraderContains GraderKind = "contains"
	// GraderExact passes when the trimmed answer equals the expectation.
	GraderExact GraderKind = "exact"
	// GraderRegex passes when the answer matches the expected regex.
	GraderRegex GraderKind = "regex"
	// GraderNumber passes when the answer contains the expected standalone number.
	GraderNumber GraderKind = "number"
	// GraderNone means no automatic grading: manual review only.
	GraderNone GraderKind = "none"
)

// Prompt is a probe prompt, builtin or user-defined.
type Prompt struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Text      string     `json:"text"`
	Builtin   bool       `json:"builtin"`
	Grader    GraderKind `json:"grader"`
	Expect    string     `json:"expect,omitempty"`
	Automatic bool       `json:"automatic"`
}

var validNumberPattern = regexp.MustCompile(`^[+-]?(\d+(\.\d+)?|\.\d+)$`)

// Validate checks prompt fields; name and text are required, and a grader
// needs an expectation. Regex graders must compile.
func (p *Prompt) Validate() error {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return fmt.Errorf("题目名称不能为空")
	}
	if p.Text == "" || strings.TrimSpace(p.Text) == "" {
		return fmt.Errorf("题目文本不能为空")
	}
	switch p.Grader {
	case GraderContains, GraderExact, GraderRegex, GraderNumber:
		if strings.TrimSpace(p.Expect) == "" {
			return fmt.Errorf("自动判分题目需要判分规则")
		}
		if p.Grader == GraderRegex {
			if _, err := regexp.Compile(p.Expect); err != nil {
				return fmt.Errorf("正则表达式无效: %v", err)
			}
		}
		if p.Grader == GraderNumber {
			p.Expect = strings.TrimSpace(p.Expect)
			if !validNumberPattern.MatchString(p.Expect) {
				return fmt.Errorf("数字判分期望值必须是有效数字")
			}
		}
	case GraderNone:
		p.Expect = ""
		if p.Automatic {
			return fmt.Errorf("手动题不参与自动门禁")
		}
	case "":
		p.Grader = GraderNone
	default:
		return fmt.Errorf("未知判分类型 %q", p.Grader)
	}
	if p.Automatic && p.Grader == GraderNone {
		return fmt.Errorf("自动门禁题目需要判分规则")
	}
	return nil
}

// Grade evaluates an answer for this prompt.
// Returns (passed, humanReviewRequired, error).
func (p *Prompt) Grade(answer string) (bool, bool, error) {
	switch p.Grader {
	case GraderContains:
		return strings.Contains(answer, p.Expect), false, nil
	case GraderExact:
		return strings.TrimSpace(answer) == strings.TrimSpace(p.Expect), false, nil
	case GraderRegex:
		re, err := regexp.Compile(p.Expect)
		if err != nil {
			return false, true, err
		}
		return re.MatchString(answer), false, nil
	case GraderNumber:
		return gradeNumber(answer, p.Expect), false, nil
	default:
		return false, true, nil // manual review
	}
}

func gradeNumber(answer, expect string) bool {
	target := strings.TrimSpace(expect)
	if target == "" {
		return false
	}
	tLen := len(target)
	for i := 0; i+tLen <= len(answer); i++ {
		if answer[i:i+tLen] != target {
			continue
		}
		if i > 0 {
			prev := answer[i-1]
			if isDigitByte(prev) || prev == '+' || prev == '-' || prev == 'e' || prev == 'E' {
				continue
			}
			if prev == '.' {
				if i >= 2 && answer[i-2] == '.' {
					// ellipsis, okay
				} else {
					continue
				}
			}
			if prev == ',' && i >= 2 && isDigitByte(answer[i-2]) {
				continue
			}
		}
		if i+tLen < len(answer) {
			next := answer[i+tLen]
			if isDigitByte(next) || next == 'e' || next == 'E' {
				continue
			}
			if next == '.' {
				if i+tLen+1 < len(answer) && isDigitByte(answer[i+tLen+1]) {
					continue
				}
			}
			if next == ',' {
				if i+tLen+1 < len(answer) && isDigitByte(answer[i+tLen+1]) {
					continue
				}
			}
		}
		return true
	}
	return false
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }
