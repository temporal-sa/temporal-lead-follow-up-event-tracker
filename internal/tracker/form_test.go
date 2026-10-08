package tracker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func normalizeTestField(t *testing.T, field Field) Field {
	t.Helper()
	form := Form{Sections: []Section{{ID: "first", Fields: []Field{{ID: "email", Label: "Email", Type: "email", Required: true}, field}}}}
	normalized, err := form.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return normalized.Sections[0].Fields[1]
}

func TestFieldTypesValidateAndCanonicalize(t *testing.T) {
	minimum, maximum := 1.0, 10.0
	for _, tc := range []struct {
		field   Field
		valid   []string
		want    []string
		invalid []string
	}{
		{Field{Type: "short_text"}, []string{"  Pat  "}, []string{"Pat"}, []string{strings.Repeat("x", 501)}},
		{Field{Type: "paragraph"}, []string{" hello\nworld "}, []string{"hello\nworld"}, []string{strings.Repeat("x", 5001)}},
		{Field{Type: "email"}, []string{" PAT@EXAMPLE.COM "}, []string{"pat@example.com"}, []string{"Pat <pat@example.com>"}},
		{Field{Type: "phone"}, []string{" +1 (555) 555-1234 "}, []string{"+1 (555) 555-1234"}, []string{strings.Repeat("x", 501)}},
		{Field{Type: "url"}, []string{"https://example.com/path?q=1"}, []string{"https://example.com/path?q=1"}, []string{"javascript:alert(1)"}},
		{Field{Type: "number", Minimum: &minimum, Maximum: &maximum}, []string{"02.50"}, []string{"2.5"}, []string{"NaN"}},
		{Field{Type: "multiple_choice", Options: []string{"A", "B"}}, []string{" B "}, []string{"B"}, []string{"C"}},
		{Field{Type: "dropdown", Options: []string{"A", "B"}}, []string{"A"}, []string{"A"}, []string{"C"}},
		{Field{Type: "checkboxes", Options: []string{"A", "B", "C"}}, []string{"C", "A"}, []string{"A", "C"}, []string{"A", "A"}},
		{Field{Type: "linear_scale", Min: 0, Max: 10}, []string{"0"}, []string{"0"}, []string{"11"}},
		{Field{Type: "rating", Max: 5}, []string{"05"}, []string{"5"}, []string{"0"}},
		{Field{Type: "date"}, []string{"2028-02-29"}, []string{"2028-02-29"}, []string{"2026-02-29"}},
		{Field{Type: "time"}, []string{"14:05"}, []string{"14:05"}, []string{"25:00"}},
		{Field{Type: "multiple_choice_grid", Options: []string{"A", "B"}, Rows: []string{"First", "Second"}}, []string{"A", "B"}, []string{"A", "B"}, []string{"A", "C"}},
		{Field{Type: "checkbox_grid", Options: []string{"A", "B"}, Rows: []string{"First", "Second"}}, []string{`["B","A"]`, `["B"]`}, []string{`["A","B"]`, `["B"]`}, []string{`["A"]`, `"B"`}},
	} {
		t.Run(tc.field.Type, func(t *testing.T) {
			tc.field.ID, tc.field.Label, tc.field.Required = "question", "Question", true
			field := normalizeTestField(t, tc.field)
			got, err := normalizeAnswer(field, tc.valid)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
			if _, err := normalizeAnswer(field, tc.invalid); err == nil {
				t.Fatal("accepted invalid answer")
			}
			if _, err := normalizeAnswer(field, nil); err == nil {
				t.Fatal("accepted missing required answer")
			}
			field.Required = false
			if got, err := normalizeAnswer(field, nil); err != nil || len(got) != 0 {
				t.Fatal("optional answer required")
			}
		})
	}
}

func TestNumericBoundsIncludeZeroAndDecimals(t *testing.T) {
	minimum, maximum := 0.0, 1.5
	field := normalizeTestField(t, Field{ID: "amount", Label: "Amount", Type: "number", Minimum: &minimum, Maximum: &maximum})
	for _, value := range []string{"0", "1.25", "1.5"} {
		if _, err := normalizeAnswer(field, []string{value}); err != nil {
			t.Fatalf("valid %s: %v", value, err)
		}
	}
	for _, value := range []string{"-0.1", "1.51", "Infinity"} {
		if _, err := normalizeAnswer(field, []string{value}); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}

func branchingForm() Form {
	return Form{Sections: []Section{
		{ID: "intro", Fields: []Field{
			{ID: "email", Label: "Email", Type: "email", Required: true},
			{ID: "interest", Label: "Interest", Type: "multiple_choice", Required: true, Options: []string{"Technical", "General", "Done"}, Branches: []Branch{{Value: "General", SectionID: "general"}, {Value: "Done", SectionID: "submit"}}},
		}},
		{ID: "technical", NextSectionID: "submit", Fields: []Field{{ID: "language", Label: "Language", Type: "dropdown", Required: true, Options: []string{"Go", "Java"}}}},
		{ID: "general", Fields: []Field{{ID: "company", Label: "Company", Type: "short_text", Required: true}}},
	}}
}

func TestConditionalFormClearsHiddenFieldsAndValidatesVisitedFields(t *testing.T) {
	form, err := branchingForm().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	event := Event{Form: &form}
	input := Submission{RequestID: "12345678-1234-1234", Answers: []Answer{
		{FieldID: "company", Values: []string{"Hidden stale value"}},
		{FieldID: "interest", Values: []string{"Technical"}},
		{FieldID: "email", Values: []string{" PAT@EXAMPLE.COM "}},
		{FieldID: "language", Values: []string{"Go"}},
	}}
	got, err := input.NormalizeForEvent(event)
	if err != nil || got.Email != "pat@example.com" || len(got.Answers) != 3 {
		t.Fatalf("normalized=%+v error=%v", got, err)
	}
	if got.Answers[0].FieldID != "email" || got.Answers[1].FieldID != "interest" || got.Answers[2].FieldID != "language" {
		t.Fatal("answers are not in stable form order")
	}
	input.Answers[1].Values = []string{"General"}
	input.Answers[3].Values = []string{"invalid hidden choice"}
	got, err = input.NormalizeForEvent(event)
	if err != nil || len(got.Answers) != 3 || got.Answers[2].FieldID != "company" {
		t.Fatalf("hidden question was validated: %+v %v", got, err)
	}
	input.Answers[0].Values = nil
	if _, err := input.NormalizeForEvent(event); err == nil {
		t.Fatal("missing required question on visited path accepted")
	}
	input.Answers[1].Values = []string{"Done"}
	got, err = input.NormalizeForEvent(event)
	if err != nil || len(got.Answers) != 2 {
		t.Fatalf("early submit failed: %+v %v", got, err)
	}
	input.Answers[2].Values = nil
	if _, err := input.NormalizeForEvent(event); err == nil {
		t.Fatal("email was bypassed by early submit")
	}
}

func TestFormDefinitionRejectsUnsafeBranchingAndMissingDedupe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Form)
	}{
		{"optional email", func(f *Form) { f.Sections[0].Fields[0].Required = false }},
		{"email moved", func(f *Form) {
			f.Sections[0].Fields[0], f.Sections[1].Fields[0] = f.Sections[1].Fields[0], f.Sections[0].Fields[0]
		}},
		{"email removed", func(f *Form) { f.Sections[0].Fields = f.Sections[0].Fields[1:] }},
		{"backward next", func(f *Form) { f.Sections[1].NextSectionID = "intro" }},
		{"backward branch", func(f *Form) { f.Sections[0].Fields[1].Branches[0].SectionID = "intro" }},
		{"missing section", func(f *Form) { f.Sections[0].Fields[1].Branches[0].SectionID = "missing" }},
		{"unknown branch option", func(f *Form) { f.Sections[0].Fields[1].Branches[0].Value = "unknown" }},
		{"duplicate section", func(f *Form) { f.Sections[1].ID = "intro" }},
		{"duplicate field", func(f *Form) { f.Sections[1].Fields[0].ID = "email" }},
		{"duplicate option", func(f *Form) { f.Sections[0].Fields[1].Options = []string{"General", "General"} }},
		{"unknown type", func(f *Form) { f.Sections[1].Fields[0].Type = "file_upload" }},
		{"branch paragraph", func(f *Form) { f.Sections[0].Fields[1].Type = "paragraph"; f.Sections[0].Fields[1].Options = nil }},
		{"two branching fields", func(f *Form) {
			question := f.Sections[0].Fields[1]
			question.ID = "second"
			f.Sections[0].Fields = append(f.Sections[0].Fields, question)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := branchingForm()
			tc.change(&form)
			if _, err := form.Normalize(); err == nil {
				t.Fatal("invalid schema accepted")
			}
		})
	}
}

func TestDefaultTemplateAndLegacyCompatibility(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	event, err := NewEvent("id", "employee@temporal.io", CreateEvent{Name: "Event", EndDate: "2026-10-09", QRBanner: "  Meet our team  "}, now)
	if err != nil || event.Form == nil || !reflect.DeepEqual(event.EffectiveForm(), DefaultForm()) || event.QRBanner != "Meet our team" {
		t.Fatalf("default event=%+v error=%v", event, err)
	}
	input := Submission{Name: "Pat", Role: "Engineer", Email: "P@EXAMPLE.COM", Reason: "Curious", RequestID: "12345678-1234-1234"}
	legacy, _ := input.Normalize()
	modern, err := input.NormalizeForEvent(event)
	if err != nil || len(modern.Answers) != 4 || modern.Email != legacy.Email {
		t.Fatalf("fixed payload compatibility=%+v %v", modern, err)
	}
	old, err := modern.NormalizeForEvent(Event{})
	if err != nil || old.Digest() != legacy.Digest() || len(old.Answers) != 0 {
		t.Fatalf("legacy digest changed: %+v %v", old, err)
	}
	modern.Answers = append(modern.Answers, modern.Answers[0])
	if _, err := modern.NormalizeForEvent(Event{}); err == nil {
		t.Fatal("duplicate legacy answer accepted")
	}
	if _, err := modern.NormalizeForEvent(event); err == nil {
		t.Fatal("duplicate modern answer accepted")
	}
	if _, err := NormalizeBanner(strings.Repeat("x", 501)); err == nil {
		t.Fatal("long banner accepted")
	}
}

func TestOptionalDefaultFieldsAndDedupeDigest(t *testing.T) {
	form := DefaultForm()
	form.Sections[0].Fields[0].Required = false
	form.Sections[0].Fields[1].Required = false
	event := Event{Form: &form}
	input := Submission{RequestID: "12345678-1234-1234", Answers: []Answer{{FieldID: "email", Values: []string{"p@example.com"}}}}
	got, err := input.NormalizeForEvent(event)
	if err != nil || got.Name != "" || got.Role != "" || len(got.Answers) != 1 {
		t.Fatalf("optional defaults remain required: %+v %v", got, err)
	}
	form.Sections[0].Fields = append(form.Sections[0].Fields, Field{ID: "topics", Label: "Topics", Type: "checkboxes", Options: []string{"SDK", "Cloud"}})
	input.Answers = append(input.Answers, Answer{FieldID: "topics", Values: []string{"Cloud", "SDK"}})
	a, _ := input.NormalizeForEvent(event)
	input.Answers[1].Values = []string{"SDK", "Cloud"}
	b, _ := input.NormalizeForEvent(event)
	if a.Digest() != b.Digest() {
		t.Fatal("checkbox order changes retry digest")
	}
	input.Answers[1].Values = []string{"SDK"}
	c, _ := input.NormalizeForEvent(event)
	if a.Digest() == c.Digest() {
		t.Fatal("custom fields are not included in retry digest")
	}
}

func TestCustomAnswersPersistThroughShardJSONAndReplacement(t *testing.T) {
	state := ShardState{EventID: "event", Leads: []Lead{}}
	lead := Lead{Email: "p@example.com", Sequence: 1, Answers: []Answer{{FieldID: "topics", Values: []string{"Go", "Cloud"}}, {FieldID: "grid", Values: []string{`["A","B"]`, `[]`}}}}
	result, err := state.Apply(WriteCommand{Lead: lead})
	if err != nil || result.Full {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored ShardState
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Find(lead.Email).Answers, lead.Answers) || !reflect.DeepEqual(AnswerValues(*restored.Find(lead.Email), "topics"), []string{"Go", "Cloud"}) {
		t.Fatal("custom answers lost during durable-state round trip")
	}
	lead.Sequence = 2
	lead.Answers = []Answer{{FieldID: "topics", Values: []string{"Go"}}}
	_, _ = restored.Apply(WriteCommand{Lead: lead})
	if len(restored.Leads) != 1 || len(restored.Find(lead.Email).Answers[0].Values) != 1 {
		t.Fatal("custom update was duplicated rather than replaced")
	}
}
