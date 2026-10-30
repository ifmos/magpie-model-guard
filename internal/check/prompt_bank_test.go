package check

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinPromptEdit_PersistsAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "guard.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	svc, err := newService(store, filepath.Join(t.TempDir(), "gate.middleware.js"), false)
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	defer svc.Close()

	// 1. Initial seeded state: candy is builtin, number grader, expect 21, automatic
	candy, err := store.Prompt("candy")
	if err != nil || candy == nil {
		t.Fatalf("candy prompt missing: %v", err)
	}
	if !candy.Builtin {
		t.Error("candy must be builtin")
	}
	if candy.Grader != GraderNumber {
		t.Errorf("candy grader = %v, want %v", candy.Grader, GraderNumber)
	}
	if candy.Expect != "21" {
		t.Errorf("candy expect = %v, want 21", candy.Expect)
	}
	if !candy.Automatic {
		t.Error("candy must be automatic")
	}

	// Verify original expect 21 grades
	pass, _, _ := candy.Grade("答案是 21。")
	if !pass {
		t.Error("original candy should pass for 21")
	}
	pass, _, _ = candy.Grade("答案是 42。")
	if pass {
		t.Error("original candy must not pass for 42")
	}

	// 2. Edit builtin prompt: change name, text, expect to 42.
	// Client attempts to forge Builtin=false; server preserves Builtin=true.
	candy.Name = "修改后的糖果推理"
	candy.Text = "新的糖果题目文本"
	candy.Expect = "42"
	candy.Builtin = false

	if err := svc.UpsertPrompt(candy); err != nil {
		t.Fatalf("UpsertPrompt on builtin failed: %v", err)
	}

	candyAfter, err := store.Prompt("candy")
	if err != nil || candyAfter == nil {
		t.Fatalf("get candy after upsert: %v", err)
	}
	if !candyAfter.Builtin {
		t.Error("server must preserve builtin=true for builtin prompts")
	}
	if candyAfter.Name != "修改后的糖果推理" {
		t.Errorf("candy name not updated: %q", candyAfter.Name)
	}
	if candyAfter.Text != "新的糖果题目文本" {
		t.Errorf("candy text not updated: %q", candyAfter.Text)
	}
	if candyAfter.Expect != "42" {
		t.Errorf("candy expect not updated: %q", candyAfter.Expect)
	}

	// 3. Verify new expectation takes effect
	pass, _, _ = candyAfter.Grade("答案是 42。")
	if !pass {
		t.Error("edited candy should pass for new expect 42")
	}
	pass, _, _ = candyAfter.Grade("答案是 21。")
	if pass {
		t.Error("edited candy must not pass for old expect 21")
	}

	// 4. Simulate restart: reopen store, re-run SeedBuiltins
	svc.Close()
	store.Close()

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	if err := store2.SeedBuiltins(); err != nil {
		t.Fatalf("SeedBuiltins on reopen: %v", err)
	}

	candyReopened, err := store2.Prompt("candy")
	if err != nil || candyReopened == nil {
		t.Fatalf("get candy on reopened store: %v", err)
	}
	if !candyReopened.Builtin {
		t.Error("candy must remain builtin on restart")
	}
	if candyReopened.Name != "修改后的糖果推理" {
		t.Errorf("user edited name lost after restart: %q", candyReopened.Name)
	}
	if candyReopened.Text != "新的糖果题目文本" {
		t.Errorf("user edited text lost after restart: %q", candyReopened.Text)
	}
	if candyReopened.Expect != "42" {
		t.Errorf("user edited expect lost after restart: %q", candyReopened.Expect)
	}
	if candyReopened.Grader != GraderNumber {
		t.Errorf("candy grader changed: %v", candyReopened.Grader)
	}
	if !candyReopened.Automatic {
		t.Error("candy automatic changed")
	}
}

func TestLegacyCandyMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Seed first
	if err := store.SeedBuiltins(); err != nil {
		t.Fatal(err)
	}

	// 1. Reset candy to legacy unedited contains/21
	_, err = store.db.Exec(`UPDATE prompts SET grader='contains', expect='21' WHERE id='candy'`)
	if err != nil {
		t.Fatal(err)
	}

	// SeedBuiltins should migrate unedited contains/21 to number/21
	if err := store.SeedBuiltins(); err != nil {
		t.Fatal(err)
	}
	c, err := store.Prompt("candy")
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if c.Grader != GraderNumber {
		t.Errorf("unedited contains/21 should be migrated to number, got %v", c.Grader)
	}

	// 2. User-edited candy (e.g. expect=99, grader=contains) must NOT be migrated
	_, err = store.db.Exec(`UPDATE prompts SET grader='contains', expect='99' WHERE id='candy'`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SeedBuiltins(); err != nil {
		t.Fatal(err)
	}
	c, err = store.Prompt("candy")
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if c.Grader != GraderContains || c.Expect != "99" {
		t.Errorf("user edited candy overwritten: grader=%v expect=%v", c.Grader, c.Expect)
	}

	// 3. User-edited candy (e.g. name changed) must NOT be migrated
	_, err = store.db.Exec(`UPDATE prompts SET name='自定义名称', grader='contains', expect='21' WHERE id='candy'`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SeedBuiltins(); err != nil {
		t.Fatal(err)
	}
	c, err = store.Prompt("candy")
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if c.Grader != GraderContains || c.Name != "自定义名称" {
		t.Errorf("user edited candy name/grader overwritten: name=%v grader=%v", c.Name, c.Grader)
	}
}

func TestCustomPrompt_EditAndSecurity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "custom.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	svc, err := newService(store, filepath.Join(t.TempDir(), "gate.middleware.js"), false)
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	defer svc.Close()

	// 1. Create custom prompt. Client attempts forgery with Builtin: true.
	custom := &Prompt{
		ID:        "",
		Name:      "自定义题目",
		Text:      "请回答42",
		Grader:    GraderExact,
		Expect:    "42",
		Automatic: true,
		Builtin:   true, // forgery attempt
	}
	if err := svc.UpsertPrompt(custom); err != nil {
		t.Fatalf("create custom prompt: %v", err)
	}
	if !strings.HasPrefix(custom.ID, "custom-") {
		t.Errorf("custom prompt id should start with custom-, got %q", custom.ID)
	}
	if custom.Builtin {
		t.Error("custom prompt must not be builtin (anti-forgery)")
	}

	saved, err := store.Prompt(custom.ID)
	if err != nil || saved == nil {
		t.Fatalf("prompt not found in store: %v", err)
	}
	if saved.Builtin {
		t.Error("stored custom prompt must have builtin=false")
	}

	// 2. Edit custom prompt. Client again attempts Builtin: true.
	custom.Name = "自定义题目-修改后"
	custom.Expect = "100"
	custom.Builtin = true

	if err := svc.UpsertPrompt(custom); err != nil {
		t.Fatalf("update custom prompt: %v", err)
	}
	if custom.Builtin {
		t.Error("custom prompt must not be builtin after update")
	}

	updated, err := store.Prompt(custom.ID)
	if err != nil || updated == nil {
		t.Fatalf("get updated custom prompt: %v", err)
	}
	if updated.Builtin {
		t.Error("updated prompt in store must have builtin=false")
	}
	if updated.Name != "自定义题目-修改后" || updated.Expect != "100" {
		t.Errorf("custom prompt update failed: name=%q expect=%q", updated.Name, updated.Expect)
	}

	// 3. Delete custom prompt
	if err := svc.DeletePrompt(custom.ID); err != nil {
		t.Fatalf("DeletePrompt custom failed: %v", err)
	}
	deleted, err := store.Prompt(custom.ID)
	if err != nil {
		t.Fatalf("query deleted prompt: %v", err)
	}
	if deleted != nil {
		t.Error("deleted prompt should be nil")
	}

	// 4. Builtin prompt cannot be deleted
	if err := svc.DeletePrompt("candy"); err == nil {
		t.Error("deleting builtin prompt should fail")
	}
}
