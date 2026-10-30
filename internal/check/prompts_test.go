package check

import "testing"

func TestGradeCandy(t *testing.T) {
	cases := []struct {
		answer string
		pass   bool
	}{
		{"21", true},
		{"最少需要取出 21 个糖果。", true},
		{"答案是21。", true},
		{"需要 121 个", false}, // 21 preceded by digit
		{"需要 219 个", false}, // 21 followed by digit
		{"2100 个", false},   // 21 followed by digit
		{"答案是 35 个", false}, // wrong answer, no 21
		{"21.0 个也可以", true}, // "21." — dot is not a digit
		{"至少取21个才能保证。", true},
		{"", false},
		{"首先 7+9+8+4=28，然后……所以最少取 21 个。", true},
	}
	for _, c := range cases {
		if got := gradeCandy(c.answer); got != c.pass {
			t.Errorf("gradeCandy(%q) = %v, want %v", c.answer, got, c.pass)
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
	// builtin candy dispatch
	p = &Prompt{ID: "candy", Grader: GraderContains, Expect: "21"}
	pass, _, _ = p.Grade("最少取 21 个")
	if !pass {
		t.Error("candy builtin should use the standalone-number grader")
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
		t.Errorf("valid automatic prompt rejected: %v", err)
	}
	if err := (&Prompt{Name: "", Text: "t"}).Validate(); err == nil {
		t.Error("empty name accepted")
	}
	if err := (&Prompt{Name: "n", Text: "t", Grader: GraderRegex, Expect: "("}).Validate(); err == nil {
		t.Error("invalid regex accepted")
	}
	if err := (&Prompt{Name: "n", Text: "t", Grader: GraderContains, Expect: " ", Automatic: true}).Validate(); err == nil {
		t.Error("blank expectation accepted")
	}
	if err := (&Prompt{Name: "n", Text: "t", Grader: "bogus"}).Validate(); err == nil {
		t.Error("bogus grader accepted")
	}
}

func TestBuiltinBank(t *testing.T) {
	list := Builtins()
	found := false
	for _, b := range list {
		if b.ID == "candy" {
			found = true
			if b.Text == "" {
				t.Error("candy prompt text empty")
			}
		}
	}
	if !found {
		t.Fatal("candy prompt missing from builtins")
	}
}
