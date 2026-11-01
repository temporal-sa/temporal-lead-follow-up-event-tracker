package web

import (
	"strings"
	"testing"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	"go.temporal.io/api/serviceerror"
)

func TestEventLinksUnfurlWithTheEventPreview(t *testing.T) {
	event := tracker.Event{ID: "demo", Slug: "renamed", Name: "Conference", Description: "Welcome", Status: "open",
		Preview: &tracker.LinkPreview{Title: `<b>Booth</b> & demo`, Description: `Say "hi" at booth 7`}}
	for _, path := range []string{"/renamed", "/old-link", "/events/demo"} {
		w := request(t, testHandler(t, &fakeGateway{event: event}), "GET", path, "", "", "")
		body := w.Body.String()
		for _, want := range []string{
			`<meta property="og:title" content="&lt;b&gt;Booth&lt;/b&gt; &amp; demo">`,
			`<meta property="og:description" content="Say &#34;hi&#34; at booth 7">`,
			`<meta name="description" content="Say &#34;hi&#34; at booth 7">`,
			`<meta property="og:url" content="https://leads.tmprl-demo.cloud/renamed">`,
			`<meta property="og:image" content="https://leads.tmprl-demo.cloud/static/link-preview.png?v=`,
			`<meta name="twitter:image" content="https://leads.tmprl-demo.cloud/static/link-preview.png?v=`,
			`<meta name="twitter:card" content="summary_large_image">`,
			`<template id="participant-template">`,
		} {
			if w.Code != 200 || !strings.Contains(body, want) {
				t.Fatalf("%s: %d, missing %s in\n%s", path, w.Code, want, body)
			}
		}
		if strings.Contains(body, "<b>Booth") || strings.Contains(body, "Stay in touch with Temporal after your event.") {
			t.Fatalf("%s: unescaped preview or generic description", path)
		}
	}
}

func TestLinkPreviewImageIsServed(t *testing.T) {
	w := request(t, testHandler(t, &fakeGateway{}), "GET", linkPreviewImage, "", "", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestEventPreviewDefaultsToNameAndWelcomeMessage(t *testing.T) {
	tests := []struct {
		name        string
		event       tracker.Event
		title, desc string
	}{
		{"no preview", tracker.Event{ID: "demo", Name: "Conference", Description: "Welcome"}, "Conference", "Welcome"},
		{"no welcome message", tracker.Event{ID: "demo", Name: "Conference"}, "Conference", "Stay in touch with Temporal after your event."},
		{"title only", tracker.Event{ID: "demo", Name: "Conference", Description: "Welcome", Preview: &tracker.LinkPreview{Title: "Booth 7"}}, "Booth 7", "Welcome"},
		{"description only", tracker.Event{ID: "demo", Name: "Conference", Preview: &tracker.LinkPreview{Description: "Come by"}}, "Conference", "Come by"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := request(t, testHandler(t, &fakeGateway{event: tt.event}), "GET", "/demo", "", "", "").Body.String()
			if !strings.Contains(body, `<meta property="og:title" content="`+tt.title+`">`) || !strings.Contains(body, `<meta property="og:description" content="`+tt.desc+`">`) {
				t.Fatalf("want %q / %q in\n%s", tt.title, tt.desc, body)
			}
		})
	}
}

func TestPagesWithoutAnEventKeepTheGenericHead(t *testing.T) {
	deleted := time.Now()
	tests := []struct {
		name, path string
		g          *fakeGateway
	}{
		{"home", "/", &fakeGateway{event: tracker.Event{ID: "demo", Name: "Conference"}}},
		{"admin", "/admin/events/demo", &fakeGateway{event: tracker.Event{ID: "demo", Name: "Conference"}}},
		{"missing event", "/demo", &fakeGateway{eventError: tracker.ErrNotStored}},
		{"deleted event", "/demo", &fakeGateway{event: tracker.Event{ID: "demo", Name: "Conference", DeletedAt: &deleted}}},
		{"Temporal unavailable", "/events/demo", &fakeGateway{eventError: serviceerror.NewUnavailable("down")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := request(t, testHandler(t, tt.g), "GET", tt.path, "", validSession, "")
			body := w.Body.String()
			if w.Code != 200 || strings.Contains(body, "og:") || strings.Contains(body, "Conference") || !strings.Contains(body, `<meta name="description" content="Stay in touch with Temporal after your event.">`) {
				t.Fatalf("%d\n%s", w.Code, body)
			}
		})
	}
	if w := request(t, testHandler(t, &fakeGateway{}), "GET", "/not_a_slug", "", "", ""); w.Code != 404 {
		t.Fatalf("invalid slug: %d", w.Code)
	}
}

func TestPreviewMutationRequiresEmployeeOriginAndActiveWorkflow(t *testing.T) {
	tests := []struct {
		name, token, origin, status, body string
		want                              int
	}{
		{"anonymous", "", "https://leads.tmprl-demo.cloud", "open", `{"title":"Hi","description":""}`, 401},
		{"missing origin", validSession, "", "open", `{"title":"Hi","description":""}`, 403},
		{"foreign origin", validSession, "https://evil.example", "open", `{"title":"Hi","description":""}`, 403},
		{"open", validSession, "https://leads.tmprl-demo.cloud", "open", `{"title":"  Hi  ","description":"  There  "}`, 200},
		{"ended", validSession, "https://leads.tmprl-demo.cloud", "ended", `{"title":"Hi","description":"There"}`, 200},
		{"archived", validSession, "https://leads.tmprl-demo.cloud", "archived", `{"title":"Hi","description":"There"}`, 410},
		{"title over limit", validSession, "https://leads.tmprl-demo.cloud", "open", `{"title":"` + strings.Repeat("🌟", 121) + `","description":""}`, 400},
		{"description over limit", validSession, "https://leads.tmprl-demo.cloud", "open", `{"title":"","description":"` + strings.Repeat("🌟", 301) + `"}`, 400},
		{"invalid property", validSession, "https://leads.tmprl-demo.cloud", "open", `{"title":"Hi","unknown":true}`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGateway{event: tracker.Event{ID: "demo", Status: tt.status}}
			w := request(t, testHandler(t, g), "POST", "/api/admin/events/demo/preview", tt.body, tt.token, tt.origin)
			if w.Code != tt.want || (tt.want != 200 && len(g.previews) != 0) {
				t.Fatalf("preview mutation: %d %s calls=%v", w.Code, w.Body.String(), g.previews)
			}
			if tt.want == 200 && (len(g.previews) != 1 || g.previews[0].Title != "Hi" || g.previews[0].Description != "There") {
				t.Fatalf("preview not normalized: %v", g.previews)
			}
		})
	}
}
