package web

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	"go.temporal.io/sdk/temporal"
)

func customAPIForm() *tracker.Form {
	return &tracker.Form{Sections: []tracker.Section{{ID: "contact", Title: "Stay in touch", Fields: []tracker.Field{
		{ID: "email", Label: "Work email", Type: "email", Required: true},
		{ID: "role", Label: "Your role", Type: "short_text"},
		{ID: "topic", Label: "What interests you?", Type: "dropdown", Required: true, Options: []string{"Workflows", "Platform"}},
	}}}}
}

func TestCustomFormPublicSchemaAndSubmissionValidation(t *testing.T) {
	g := &fakeGateway{event: tracker.Event{ID: "demo", Name: "Conference", Status: "open", Form: customAPIForm(), QRBanner: "Private display banner", CreatedBy: "private@temporal.io"}}
	h := testHandler(t, g)
	w := request(t, h, "GET", "/api/events/demo", "", "", "")
	var public struct {
		Form tracker.Form `json:"form"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &public) != nil || len(public.Form.Sections) != 1 || public.Form.Sections[0].Fields[2].ID != "topic" || strings.Contains(w.Body.String(), "Private display banner") || strings.Contains(w.Body.String(), "private@temporal.io") {
		t.Fatalf("public schema incorrect: %d %s", w.Code, w.Body.String())
	}
	valid := `{"answers":[{"fieldId":"email","values":[" ADA@EXAMPLE.COM "]},{"fieldId":"topic","values":["Workflows"]}],"requestId":"custom-form-request-0001"}`
	w = request(t, h, "POST", "/api/events/demo/participants", valid, "", "")
	if w.Code != http.StatusOK || g.submission == nil || g.submission.Email != "ada@example.com" || g.submission.Role != "" || len(g.submission.Answers) != 2 {
		t.Fatalf("custom submit: %d %s %#v", w.Code, w.Body.String(), g.submission)
	}
	for _, invalid := range []string{
		strings.Replace(valid, `"Workflows"`, `"Unknown choice"`, 1),
		`{"answers":[{"fieldId":"email","values":["ada@example.com"]}],"requestId":"custom-form-request-0002"}`,
		strings.Replace(valid, `"fieldId":"topic"`, `"fieldId":"unknown"`, 1),
		strings.Replace(valid, `ADA@EXAMPLE.COM`, `not an email`, 1),
	} {
		g.submission = nil
		w = request(t, h, "POST", "/api/events/demo/participants", invalid, "", "")
		if w.Code != http.StatusBadRequest || g.submission != nil {
			t.Fatalf("invalid answer reached workflow: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestCreateEventCarriesCustomFormAndBanner(t *testing.T) {
	g := &fakeGateway{}
	input := tracker.CreateEvent{Name: "Custom conference", EndDate: "2099-01-01", Form: customAPIForm(), QRBanner: "  Meet your next workflow.  "}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, testHandler(t, g), "POST", "/api/admin/events", string(body), validSession, "https://leads.tmprl-demo.cloud")
	if w.Code != http.StatusCreated || g.created == nil || g.created.Form == nil || g.created.Form.Sections[0].Fields[2].ID != "topic" || g.created.QRBanner != "Meet your next workflow." {
		t.Fatalf("custom event creation: %d %s %#v", w.Code, w.Body.String(), g.created)
	}
}

func TestCustomSchemaLargerThanLegacyBodyLimitAndRequestCap(t *testing.T) {
	form := customAPIForm()
	for i := 0; i < 20; i++ {
		form.Sections[0].Fields = append(form.Sections[0].Fields, tracker.Field{
			ID: fmt.Sprintf("notes_%d", i), Label: fmt.Sprintf("Notes %d", i), Type: "paragraph", Description: strings.Repeat("a", 1000),
		})
	}
	body, err := json.Marshal(tracker.CreateEvent{Name: "Large custom form", EndDate: "2099-01-01", Form: form})
	if err != nil || len(body) <= 16*1024 {
		t.Fatalf("large schema fixture: %d bytes error=%v", len(body), err)
	}
	g := &fakeGateway{}
	h := testHandler(t, g)
	w := request(t, h, "POST", "/api/admin/events", string(body), validSession, "https://leads.tmprl-demo.cloud")
	if w.Code != http.StatusCreated || g.created == nil {
		t.Fatalf("valid schema larger than legacy cap rejected: %d %s", w.Code, w.Body.String())
	}
	g.created = nil
	w = request(t, h, "POST", "/api/admin/events", `{"name":"Conference","endDate":"2099-01-01"}`+strings.Repeat(" ", 256*1024), validSession, "https://leads.tmprl-demo.cloud")
	if w.Code != http.StatusBadRequest || g.created != nil {
		t.Fatalf("request beyond cap accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestBannerMutationRequiresEmployeeOriginAndActiveWorkflow(t *testing.T) {
	cases := []struct {
		name, token, origin, status, body string
		want                              int
	}{
		{"anonymous", "", "https://leads.tmprl-demo.cloud", "open", `{"banner":"Hello"}`, 401},
		{"missing origin", validSession, "", "open", `{"banner":"Hello"}`, 403},
		{"foreign origin", validSession, "https://evil.example", "open", `{"banner":"Hello"}`, 403},
		{"open", validSession, "https://leads.tmprl-demo.cloud", "open", `{"banner":"  Hello  "}`, 200},
		{"ended", validSession, "https://leads.tmprl-demo.cloud", "ended", `{"banner":"Hello"}`, 200},
		{"archived", validSession, "https://leads.tmprl-demo.cloud", "archived", `{"banner":"Hello"}`, 410},
		{"over limit", validSession, "https://leads.tmprl-demo.cloud", "open", `{"banner":"` + strings.Repeat("🌟", 501) + `"}`, 400},
		{"invalid property", validSession, "https://leads.tmprl-demo.cloud", "open", `{"banner":"Hello","unknown":true}`, 400},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGateway{event: tracker.Event{ID: "demo", Status: tt.status}}
			w := request(t, testHandler(t, g), "POST", "/api/admin/events/demo/banner", tt.body, tt.token, tt.origin)
			if w.Code != tt.want || (tt.want == 200 && (len(g.banners) != 1 || g.banners[0] != "Hello")) || (tt.want != 200 && len(g.banners) != 0) {
				t.Fatalf("banner mutation: %d %s calls=%v", w.Code, w.Body.String(), g.banners)
			}
		})
	}
	g := &fakeGateway{event: tracker.Event{ID: "demo", Status: "open"}, bannerError: temporal.NewApplicationError("Event has completed.", "Closed")}
	w := request(t, testHandler(t, g), "POST", "/api/admin/events/demo/banner", `{"banner":"Hello"}`, validSession, "https://leads.tmprl-demo.cloud")
	if w.Code != http.StatusGone {
		t.Fatalf("completion race returned %d", w.Code)
	}
}

func TestQRDisplayThemeBannerEscapingAndDefault(t *testing.T) {
	g := &fakeGateway{event: tracker.Event{ID: "demo", Name: "<script>alert('name')</script>", QRBanner: "<img src=x onerror=alert(1)>\nSecond line"}}
	h := testHandler(t, g)
	w := request(t, h, "GET", "/admin/events/demo/qr", "", validSession, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "<img src=x") || !strings.Contains(w.Body.String(), "&lt;img src=x") || !strings.Contains(w.Body.String(), "Second line") || !strings.Contains(w.Body.String(), `/static/temporal-logo.svg`) || !strings.Contains(w.Body.String(), `class="qr-display"`) {
		t.Fatalf("QR display incorrect: %d %s", w.Code, w.Body.String())
	}
	g.event.QRBanner = ""
	w = request(t, h, "GET", "/admin/events/demo/qr", "", validSession, "")
	if !strings.Contains(w.Body.String(), "Scan to stay in touch.") {
		t.Fatal("QR default banner missing")
	}
}

func TestCustomCSVSchemaOrderGridRowsAndSafeCells(t *testing.T) {
	form := &tracker.Form{Sections: []tracker.Section{{ID: "contact", Fields: []tracker.Field{
		{ID: "email", Label: "Email", Type: "email", Required: true},
		{ID: "topics", Label: "Topics", Type: "checkboxes", Options: []string{"One; two", "Three"}},
		{ID: "one_topic", Label: "One topic", Type: "checkboxes", Options: []string{"One; two", "Three"}},
		{ID: "satisfaction", Label: "Topics", Type: "multiple_choice_grid", Rows: []string{"Platform", "SDK"}, Options: []string{"High", "Low"}},
		{ID: "needs", Label: "Needs", Type: "checkbox_grid", Rows: []string{"Team", "Personal"}, Options: []string{"Docs", "Training"}},
		{ID: "formula", Label: "@Label", Type: "short_text"},
	}}}}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	lead := tracker.Lead{Email: "ada@example.com", FirstSubmittedAt: now, LastSubmittedAt: now, Answers: []tracker.Answer{
		{FieldID: "email", Values: []string{"ada@example.com"}},
		{FieldID: "topics", Values: []string{"One; two", "Three"}},
		{FieldID: "one_topic", Values: []string{"Three"}},
		{FieldID: "satisfaction", Values: []string{"High", ""}},
		{FieldID: "needs", Values: []string{`["Docs","Training"]`, `[]`}},
		{FieldID: "formula", Values: []string{"\t=HYPERLINK(\"evil\")"}},
	}}
	g := &fakeGateway{event: tracker.Event{ID: "demo", Name: "@Event", Count: 1, Shards: []string{"s0"}, Form: form}, shards: map[string][]tracker.Lead{"s0": {lead}}}
	w := request(t, testHandler(t, g), "GET", "/api/admin/events/demo/export.csv", "", validSession, "")
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if w.Code != http.StatusOK || err != nil || len(rows) != 2 || len(rows[0]) != 13 {
		t.Fatalf("custom CSV: %d %s error=%v", w.Code, w.Body.String(), err)
	}
	wantHeader := []string{"event_id", "event_name", "event_end_date_utc", "Email [email]", "Topics [topics]", "One topic [one_topic]", "Topics: Platform [satisfaction.row1]", "Topics: SDK [satisfaction.row2]", "Needs: Team [needs.row1]", "Needs: Personal [needs.row2]", "'@Label [formula]", "first_submitted_at_utc", "last_submitted_at_utc"}
	for i, value := range wantHeader {
		if rows[0][i] != value {
			t.Errorf("header %d = %q, want %q", i, rows[0][i], value)
		}
	}
	wantRow := []string{"demo", "'@Event", "", "ada@example.com", `["One; two","Three"]`, `["Three"]`, "High", "", `["Docs","Training"]`, "", "'\t=HYPERLINK(\"evil\")", "2026-10-08T12:00:00Z", "2026-10-08T12:00:00Z"}
	for i, value := range wantRow {
		if rows[1][i] != value {
			t.Errorf("cell %d = %q, want %q", i, rows[1][i], value)
		}
	}
}
