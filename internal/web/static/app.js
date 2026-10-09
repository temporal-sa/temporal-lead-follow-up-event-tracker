"use strict";

(() => {
  const app = document.getElementById("app");
  const number = new Intl.NumberFormat();
  const utcDate = new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "short", timeZone: "UTC" });
  const shortDate = new Intl.DateTimeFormat("en", { dateStyle: "medium", timeZone: "UTC" });
  const get = (id) => document.getElementById(id);
  const node = (tag, className, content) => {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (content !== undefined) element.textContent = content;
    return element;
  };
  const dateText = (value) => {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? "Unavailable" : `${utcDate.format(date)} UTC`;
  };
  const feedback = (id, message, kind = "", focus = false) => {
    const element = get(id);
    element.textContent = message;
    element.className = `feedback${kind ? ` ${kind}` : ""}`;
    element.hidden = !message;
    element.setAttribute("role", kind === "error" ? "alert" : "status");
    if (focus && message) element.focus();
  };
  const render = (template) => {
    app.replaceChildren(get(template).content.cloneNode(true));
    app.setAttribute("aria-busy", "false");
  };
  const api = async (url, options = {}) => {
    const response = await fetch(url, { credentials: "same-origin", ...options });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(data.error || (response.status === 401 || response.status === 403 ? "Your employee session has expired. Sign in again to continue." : "Something went wrong. Please try again."));
      error.status = response.status;
      throw error;
    }
    return data;
  };
  const post = (url, body) => api(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  const failure = (error) => error instanceof TypeError ? "We couldn't connect. Check your connection and try again." : error.message;
  const errorPage = (title, message, retry = true) => {
    app.replaceChildren();
    app.setAttribute("aria-busy", "false");
    const section = node("section", "error-page");
    section.append(node("p", "eyebrow", "Event follow-up"), node("h1", "", title), node("p", "", message));
    if (retry) {
      const button = node("button", "button", "Try again");
      button.type = "button";
      button.addEventListener("click", () => location.reload());
      section.append(button);
    }
    app.append(section);
  };
  const badge = (element, status) => {
    const known = ["open", "ended", "archived"].includes(status) ? status : "ended";
    element.className = `status-badge ${known}`;
    element.textContent = known;
  };
  const setDescription = (id, text) => {
    get(id).textContent = text || "";
    get(id).hidden = !text;
  };
  const eventPath = (id) => `/api/admin/events/${encodeURIComponent(id)}`;

  async function participant(id) {
    let event;
    try { event = await api(`/api/events/${encodeURIComponent(id)}`); }
    catch (error) {
      errorPage(error.status === 404 ? "Event unavailable" : "Unable to load this event", error.status === 404 ? "Check the event link or ask the Temporal team for a new QR code." : failure(error), error.status !== 404);
      return;
    }
    document.title = `${event.name} · Temporal follow-up`;
    render("participant-template");
    get("event-name").textContent = event.name;
    setDescription("event-description", event.description);
    get("participant-intro").hidden = Boolean(event.description);
    if (event.status !== "open") {
      get("participant-form").hidden = true;
      get("participant-intro").hidden = true;
      get("event-closed").hidden = false;
      return;
    }
    let attempt = null;
    let submitting = false;
    const navigator = EventForms.mountParticipant(get("participant-fields"), event.form);
    get("participant-form").addEventListener("submit", async (submission) => {
      submission.preventDefault();
      if (submitting) return;
      const answers = navigator.advance();
      feedback("participant-feedback", "");
      if (answers === null) return;
      const body = { answers };
      const key = JSON.stringify(body);
      if (!attempt || attempt.key !== key) attempt = { key, id: crypto.randomUUID() };
      body.requestId = attempt.id;
      submitting = true;
      get("participant-fields").disabled = true;
      navigator.setSubmitting(true);
      feedback("participant-feedback", "Saving your details…");
      try {
        const saved = await post(`/api/events/${encodeURIComponent(id)}/participants`, body);
        get("participant-form").hidden = true;
        get("participant-intro").hidden = true;
        get("flight-code").textContent = saved.flightCode || "";
        get("participant-success").hidden = false;
        get("participant-success").focus();
        attempt = null;
      } catch (error) {
        feedback("participant-feedback", failure(error), "error", true);
      } finally {
        submitting = false;
        get("participant-fields").disabled = false;
        navigator.setSubmitting(false);
      }
    });
  }

  async function dashboard() {
    document.title = "Events · Temporal follow-up";
    render("admin-template");
    let cursor = "";
    let loading = false;
    let loaded = false;
    const list = get("event-list");
    const renderEvent = (event) => {
      if (event.deletedAt) return;
      const card = node("a", "card event-card");
      card.href = `/admin/events/${encodeURIComponent(event.id)}`;
      const top = node("div", "event-card-top");
      const status = node("span");
      badge(status, event.status);
      top.append(status, node("span", "event-count", `${number.format(event.count || 0)} responses`));
      card.append(top, node("h2", "", event.name));
      if (event.description) card.append(node("p", "event-card-description", event.description));
      const footer = node("div", "event-card-footer");
      const endDate = new Date(`${event.endDate}T00:00:00Z`);
      footer.append(node("span", "", Number.isNaN(endDate.getTime()) ? "View event" : `Through ${shortDate.format(endDate)} · UTC`));
      const arrow = node("span", "arrow", "→");
      arrow.setAttribute("aria-hidden", "true");
      footer.append(arrow);
      card.append(footer);
      list.append(card);
    };
    const load = async () => {
      if (loading) return;
      loading = true;
      get("events-more").disabled = true;
      feedback("events-feedback", "Loading events…");
      try {
        const data = await api(`/api/admin/events${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""}`);
        (data.events || []).forEach(renderEvent);
        cursor = data.nextCursor || "";
        loaded = true;
        get("events-more").hidden = !cursor;
        get("events-empty").hidden = Boolean(list.children.length);
        feedback("events-feedback", "");
      } catch (error) {
        feedback("events-feedback", failure(error), "error");
        get("events-more").textContent = loaded ? "Try loading more events" : "Try again";
        get("events-more").hidden = false;
      } finally {
        loading = false;
        get("events-more").disabled = false;
      }
    };
    get("events-more").addEventListener("click", load);
    const hideCreate = () => {
      get("create-section").hidden = true;
      get("show-create").setAttribute("aria-expanded", "false");
      get("show-create").focus();
    };
    get("show-create").addEventListener("click", () => {
      const hidden = !get("create-section").hidden;
      get("create-section").hidden = hidden;
      get("show-create").setAttribute("aria-expanded", String(!hidden));
      if (!hidden) get("create-name").focus();
    });
    get("cancel-create").addEventListener("click", hideCreate);
    const formBuilder = EventForms.builder(get("form-builder"));
    get("create-qr-banner").addEventListener("input", () => {
      get("create-banner-preview").textContent = get("create-qr-banner").value.trim() || "Scan to keep the conversation going.";
    });
    get("create-end-date").min = new Date().toISOString().slice(0, 10);
    get("create-end-date").addEventListener("change", () => {
      const date = new Date(`${get("create-end-date").value}T00:00:00Z`);
      if (Number.isNaN(date.getTime())) return;
      date.setUTCDate(date.getUTCDate() + 1);
      get("create-date-help").textContent = `Submissions close ${dateText(date)}. Responses remain available to view and export for as long as Temporal retains them.`;
    });
    let creating = false;
    get("create-form").addEventListener("submit", async (submission) => {
      submission.preventDefault();
      if (creating) return;
      const formProblem = formBuilder.validate();
      if (formProblem) { feedback("create-feedback", formProblem, "error", true); return; }
      if (!submission.currentTarget.reportValidity()) return;
      const values = new FormData(submission.currentTarget);
      const body = { name: String(values.get("name") || "").trim(), description: String(values.get("description") || "").trim(), endDate: values.get("endDate"), form: formBuilder.value(), qrBanner: get("create-qr-banner").value.trim() };
      if (!body.name) {
        feedback("create-feedback", "Please enter an event name.", "error", true);
        return;
      }
      creating = true;
      get("create-fields").disabled = true;
      get("create-submit").textContent = "Creating…";
      feedback("create-feedback", "Creating your event…");
      try {
        const event = await post("/api/admin/events", body);
        location.assign(`/admin/events/${encodeURIComponent(event.id)}`);
      } catch (error) {
        feedback("create-feedback", failure(error), "error", true);
        creating = false;
        get("create-fields").disabled = false;
        get("create-submit").textContent = "Create event";
      }
    });
    await load();
  }

  async function detail(id) {
    const url = eventPath(id);
    let event;
    try { event = await api(url); }
    catch (error) {
      errorPage("Event unavailable", failure(error));
      return;
    }
    document.title = `${event.name} · Temporal responses`;
    render("detail-template");
    const refreshSummary = () => {
      get("detail-name").textContent = event.name;
      setDescription("detail-description", event.description);
      badge(get("detail-status"), event.status);
      get("detail-count").textContent = number.format(event.count || 0);
      get("detail-closes").textContent = dateText(event.endedAt || event.closesAt);
      get("detail-end").hidden = event.status !== "open";
      let lifecycle = "";
      if (event.completesAt) {
        lifecycle = "Responses remain available to view and export for as long as Temporal retains them.";
      }
      get("detail-lifecycle").textContent = lifecycle;
      get("detail-lifecycle").hidden = !lifecycle;
    };
    refreshSummary();
    get("detail-qr").href = `/admin/events/${encodeURIComponent(id)}/qr`;
    get("detail-form-preview").href = `/events/${encodeURIComponent(id)}`;
    get("detail-banner").value = event.qrBanner || "";
    get("banner-form").addEventListener("submit", async (submission) => {
      submission.preventDefault();
      get("banner-save").disabled = true;
      feedback("banner-feedback", "Saving display message…");
      try {
        event = await post(`${url}/banner`, { banner: get("detail-banner").value.trim() });
        refreshSummary();
        feedback("banner-feedback", "QR display message saved.", "success");
      } catch (error) { feedback("banner-feedback", failure(error), "error", true); }
      finally { get("banner-save").disabled = false; }
    });
    get("detail-copy").addEventListener("click", async () => {
      const link = `${location.origin}/events/${encodeURIComponent(id)}`;
      try {
        await navigator.clipboard.writeText(link);
        feedback("detail-feedback", "Form link copied.", "success");
      } catch (_) {
        const element = get("detail-feedback");
        element.replaceChildren(node("span", "", "Copy this form link: "));
        const anchor = node("a", "", link);
        anchor.href = link;
        element.append(anchor);
        element.className = "feedback";
        element.hidden = false;
      }
    });
    get("detail-export").addEventListener("click", async () => {
      const button = get("detail-export");
      button.disabled = true;
      feedback("detail-feedback", "Preparing your export…");
      try {
        const response = await fetch(`${url}/export.csv`, { credentials: "same-origin" });
        if (!response.ok) {
          const data = await response.json().catch(() => ({}));
          throw new Error(data.error || "Unable to export the responses. Please try again.");
        }
        const blob = await response.blob();
        const blobURL = URL.createObjectURL(blob);
        const anchor = node("a");
        anchor.href = blobURL;
        anchor.download = `${event.name.replace(/[^a-zA-Z0-9_-]+/g, "-").slice(0, 80) || "event"}-leads.csv`;
        document.body.append(anchor);
        anchor.click();
        anchor.remove();
        setTimeout(() => URL.revokeObjectURL(blobURL), 1000);
        feedback("detail-feedback", "Your CSV export is ready.", "success");
      } catch (error) { feedback("detail-feedback", failure(error), "error", true); }
      finally { button.disabled = false; }
    });
    const dialog = get("end-dialog");
    get("detail-end").addEventListener("click", () => dialog.showModal());
    get("cancel-end").addEventListener("click", () => dialog.close());
    const deleteDialog = get("delete-dialog");
    get("detail-delete").addEventListener("click", () => deleteDialog.showModal());
    get("cancel-delete").addEventListener("click", () => deleteDialog.close());
    get("confirm-delete").addEventListener("click", async () => {
      get("confirm-delete").disabled = true;
      deleteDialog.close();
      get("detail-delete").disabled = true;
      feedback("detail-feedback", "Deleting event…");
      try {
        await post(`${url}/delete`, {});
        location.assign("/admin");
      } catch (error) {
        feedback("detail-feedback", failure(error), "error", true);
        get("detail-delete").disabled = false;
        get("confirm-delete").disabled = false;
      }
    });
    get("confirm-end").addEventListener("click", async () => {
      get("confirm-end").disabled = true;
      dialog.close();
      get("detail-end").disabled = true;
      feedback("detail-feedback", "Ending event…");
      try {
        event = await post(`${url}/end`, {});
        refreshSummary();
        feedback("detail-feedback", "Event ended. New responses are now closed.", "success", true);
      } catch (error) { feedback("detail-feedback", failure(error), "error", true); }
      finally {
        get("detail-end").disabled = false;
        get("confirm-end").disabled = false;
      }
    });
    let cursor = "";
    let loading = false;
    let shown = false;
    const body = get("leads-body");
    const responseFields = EventForms.fields(event.form);
    responseFields.forEach((field) => { const header = node("th", "", field.label); header.scope = "col"; get("leads-head").append(header); });
    const dateHeader = node("th", "", "Last submitted · UTC"); dateHeader.scope = "col"; get("leads-head").append(dateHeader);
    const renderLead = (lead) => {
      const row = node("tr");
      const cells = [
        ...responseFields.map((field) => [field.label, EventForms.answerText(field, lead), "reason-cell"]),
        ["Last submitted · UTC", dateText(lead.lastSubmittedAt), "date-cell"],
      ];
      cells.forEach(([label, value, className]) => {
        const cell = node("td", className, value);
        cell.dataset.label = label;
        if (label === "Last submitted · UTC") cell.title = `First submitted: ${dateText(lead.firstSubmittedAt)}`;
        row.append(cell);
      });
      body.append(row);
    };
    const load = async (reset = false) => {
      if (loading) return;
      loading = true;
      get("leads-more").disabled = true;
      get("refresh-leads").disabled = true;
      feedback("leads-feedback", "Loading participants…");
      try {
        const readCursor = reset ? "" : cursor;
        const data = await api(`${url}/participants${readCursor ? `?cursor=${encodeURIComponent(readCursor)}` : ""}`);
        if (reset) body.replaceChildren();
        (data.leads || []).forEach(renderLead);
        cursor = data.nextCursor || "";
        shown = true;
        get("leads-more").textContent = "Load more participants";
        get("leads-more").hidden = !cursor;
        get("leads-empty").hidden = Boolean(body.children.length);
        get("leads-table-wrap").hidden = !body.children.length;
        feedback("leads-feedback", "");
        if (reset) {
          try { event = await api(url); refreshSummary(); }
          catch (error) { feedback("detail-feedback", failure(error), "error"); }
        }
      } catch (error) {
        feedback("leads-feedback", failure(error), "error");
        get("leads-more").textContent = shown ? "Try loading more participants" : "Try again";
        get("leads-more").hidden = false;
      } finally {
        loading = false;
        get("leads-more").disabled = false;
        get("refresh-leads").disabled = false;
      }
    };
    get("leads-more").addEventListener("click", () => load());
    get("refresh-leads").addEventListener("click", () => load(true));
    await load();
  }

  async function start() {
    const path = location.pathname.replace(/\/+$/, "") || "/";
    const participantRoute = path.match(/^\/events\/([^/]+)$/);
    if (participantRoute) {
      await participant(decodeURIComponent(participantRoute[1]));
      return;
    }
    if (path !== "/" && path !== "/admin" && !/^\/admin\/events\/[^/]+$/.test(path)) {
      errorPage("Page unavailable", "Check the event link or open your event's QR code.", false);
      return;
    }
    const session = await api("/api/session");
    if (!session.authenticated) {
      render("home-template");
      const login = get("home-login");
      login.href = session.loginUrl || "/admin";
      if (path !== "/") {
        app.querySelector("h1").textContent = "Sign in to manage events.";
        app.querySelector(".intro").textContent = "The event control panel is available to authenticated Temporal employees.";
      }
      return;
    }
    get("employee-identity").textContent = session.email || "Temporal employee";
    get("employee-identity").hidden = false;
    if (path === "/") {
      location.replace("/admin");
      return;
    }
    const detailRoute = path.match(/^\/admin\/events\/([^/]+)$/);
    if (detailRoute) await detail(decodeURIComponent(detailRoute[1]));
    else await dashboard();
  }
  start().catch((error) => errorPage("Unable to load this page", failure(error)));
})();
