package check

import (
	"strings"
	"testing"
)

func TestGraderNumberBoundaries(t *testing.T) {
	cases := []struct {
		answer string
		expect string
		pass   bool
	}{
		// Standard matches for 21
		{"21", "21", true},
		{"最少需要取出 21 个糖果。", "21", true},
		{"答案是21。", "21", true},
		{"至少取21个才能保证。", "21", true},
		{"首先 7+9+8+4=28，然后……所以最少取 21 个。", "21", true},
		{"The answer is 21.", "21", true},
		{"The answer is 21. That's all.", "21", true},
		{"(21)", "21", true},
		{"[21]", "21", true},
		{"{21}", "21", true},
		{"21, 22, 23", "21", true},
		{"...21...", "21", true},
		{"121 is wrong, 21 is right", "21", true},
		{"21.5 is wrong, 21 is right", "21", true},

		// Boundary protections for 21
		{"", "21", false},
		{"答案是 35 个", "21", false},
		{"需要 121 个", "21", false},  // preceded by digit
		{"需要 219 个", "21", false},  // followed by digit
		{"2100 个", "21", false},    // followed by digits
		{"需要 21.5 个", "21", false}, // followed by decimal point and digit
		{"21.0 个也可以", "21", false}, // followed by decimal point and digit (prevent false match)
		{"121.5", "21", false},     // preceded by digit and followed by decimal
		{"5.21", "21", false},      // preceded by decimal and digit
		{".21", "21", false},       // preceded by decimal
		{"21,000", "21", false},    // thousands separator followed by digits
		{"1,210", "21", false},     // comma after digit before 21
		{"-21", "21", false},
		{"21e3", "21", false},
		{"1e21", "21", false},
		{"-21", "-21", true},
		{"21", "-21", false},

		// Decimal expect boundaries (e.g. 21.5)
		{"答案是 21.5。", "21.5", true},
		{"21.5.", "21.5", true},
		{"21.55", "21.5", false},
		{"121.5", "21.5", false},
		{"21.5.0", "21.5", false},
		{"5.21.5", "21.5", false},
	}
	for _, c := range cases {
		if got := gradeNumber(c.answer, c.expect); got != c.pass {
			t.Errorf("gradeNumber(%q, %q) = %v, want %v", c.answer, c.expect, got, c.pass)
		}
	}
}

func TestPromptGrade(t *testing.T) {
	// contains
	p := &Prompt{ID: "x", Grader: GraderContains, Expect: "Paris"}
	pass, manual, err := p.Grade("The capital is Paris, France.")
	if err != nil || manual || !pass {
		t.Errorf("contains: pass=%v manual=%v err=%v", pass, manual, err)
	}
	// exact
	p = &Prompt{ID: "x", Grader: GraderExact, Expect: "Paris"}
	pass, _, _ = p.Grade("Paris\n")
	if !pass {
		t.Error("exact should trim")
	}
	pass, _, _ = p.Grade("Paris, France")
	if pass {
		t.Error("exact must not pass superset")
	}
	// regex
	p = &Prompt{ID: "x", Grader: GraderRegex, Expect: `^46\.24$`}
	pass, _, _ = p.Grade("46.24")
	if !pass {
		t.Error("regex should match")
	}
	// number grader
	p = &Prompt{ID: "custom-num", Grader: GraderNumber, Expect: "21"}
	pass, _, _ = p.Grade("答案是 21 个")
	if !pass {
		t.Error("number grader should pass for 21")
	}
	pass, _, _ = p.Grade("答案是 21.5 个")
	if pass {
		t.Error("number grader must not pass for 21.5")
	}
	// candy prompt respects its own Grader/Expect (not hardcoded 21)
	p = &Prompt{ID: "candy", Grader: GraderNumber, Expect: "42"}
	pass, _, _ = p.Grade("答案是 42。")
	if !pass {
		t.Error("candy prompt with expect 42 should pass 42")
	}
	pass, _, _ = p.Grade("答案是 21。")
	if pass {
		t.Error("candy prompt with expect 42 must not pass 21 (candy ID no longer forces 21)")
	}
	// manual
	p = &Prompt{ID: "x", Grader: GraderNone}
	_, manual, _ = p.Grade("anything")
	if !manual {
		t.Error("none grader requires manual review")
	}
}

func TestPromptValidate(t *testing.T) {
	if err := (&Prompt{Name: "n", Text: "t", Grader: GraderContains, Expect: "21", Automatic: true}).Validate(); err != nil {
		t.Errorf("valid automatic contains rejected: %v", err)
	}
	if err := (&Prompt{Name: "n", Text: "t", Grader: GraderNumber, Expect: "21", Automatic: true}).Validate(); err != nil {
		t.Errorf("valid automatic number rejected: %v", err)
	}
	if err := (&Prompt{Name: "", Text: "t"}).Validate(); err == nil {
		t.Error("empty name accepted")
	}
	if err := (&Prompt{Name: "n", Text: ""}).Validate(); err == nil {
		t.Error("empty text accepted")
	}
	if err := (&Prompt{Name: "n", Text: "t", Grader: GraderRegex, Expect: "("}).Validate(); err == nil {
		t.Error("invalid regex accepted")
	}
	if err := (&Prompt{Name: "n", Text: "t", Grader: GraderContains, Expect: " ", Automatic: true}).Validate(); err == nil {
		t.Error("blank expectation accepted for contains")
	}
	if err := (&Prompt{Name: "n", Text: "t", Grader: "bogus"}).Validate(); err == nil {
		t.Error("bogus grader accepted")
	}
	// none + automatic must be rejected
	if err := (&Prompt{Name: "n", Text: "t", Grader: GraderNone, Automatic: true}).Validate(); err == nil {
		t.Error("none + automatic should be rejected")
	}
	if err := (&Prompt{Name: "n", Text: "t", Grader: "", Automatic: true}).Validate(); err == nil {
		t.Error("empty grader + automatic should be rejected")
	}

	// GraderNumber expectation validation edge cases
	validNumbers := []string{
		"21",
		"+21",
		"-42",
		"0",
		"-0.0",
		"3.14159",
		".5",
		"+.5",
		"-.5",
		" 21 ", // will be trimmed
		"1000",
		"+0",
		"-0",
	}
	for _, num := range validNumbers {
		p := &Prompt{Name: "n", Text: "t", Grader: GraderNumber, Expect: num, Automatic: true}
		if err := p.Validate(); err != nil {
			t.Errorf("valid number %q rejected: %v", num, err)
		}
		if p.Expect != strings.TrimSpace(num) {
			t.Errorf("p.Expect not trimmed: got %q, want %q", p.Expect, strings.TrimSpace(num))
		}
	}

	invalidNumbers := []string{
		"",
		" ",
		"\t",
		"abc",
		"21a",
		"a21",
		"21.5.5",
		"1,000",
		"+",
		"-",
		"--21",
		"++21",
		"21 21",
		"21.",
		".",
		"+.",
		"-.",
		"21-",
		"21+",
		"NaN",
		"Infinity",
	}
	for _, num := range invalidNumbers {
		p := &Prompt{Name: "n", Text: "t", Grader: GraderNumber, Expect: num, Automatic: true}
		if err := p.Validate(); err == nil {
			t.Errorf("invalid number %q accepted by GraderNumber", num)
		}
	}
}
