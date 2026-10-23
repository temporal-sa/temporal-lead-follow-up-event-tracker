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
  // A form export holds everything needed to set up the same event again, except its dates.
  const formExportKind = "temporal-event-form";
  const download = (blob, filename) => {
    const blobURL = URL.createObjectURL(blob);
    const anchor = node("a");
    anchor.href = blobURL;
    anchor.download = filename;
    document.body.append(anchor);
    anchor.click();
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(blobURL), 1000);
  };
  // Buttons show that they're working: a spinner, a present-tense label, and no second click.
  const busy = (button, label) => {
    const original = button.innerHTML;
    button.disabled = true;
    button.setAttribute("aria-busy", "true");
    button.textContent = label;
    return () => { button.innerHTML = original; button.disabled = false; button.removeAttribute("aria-busy"); };
  };
  // Successes are announced in a toast; errors stay inline next to the action.
  let toastStack = null;
  const toast = (message, error = false) => {
    if (!toastStack) {
      toastStack = node("div", "toast-stack");
      toastStack.setAttribute("aria-live", "polite");
      document.body.append(toastStack);
    }
    const item = node("div", `toast${error ? " error" : ""}`, message);
    toastStack.append(item);
    setTimeout(() => item.remove(), error ? 6000 : 3500);
  };
  // A toast that survives one navigation, for actions that leave the page.
  const flash = (message) => sessionStorage.setItem("flash", message);
  const showFlash = () => {
    const message = sessionStorage.getItem("flash");
    sessionStorage.removeItem("flash");
    if (message) toast(message);
  };
  // A native <dialog> confirmation that stays open, busy, until the action finishes.
  const confirmation = ({ dialog, open, cancel, confirm, busyLabel, errorId, run }) => {
    open.addEventListener("click", () => { feedback(errorId, ""); dialog.showModal(); });
    cancel.addEventListener("click", () => dialog.close());
    dialog.addEventListener("cancel", (event) => { if (confirm.disabled) event.preventDefault(); });
    confirm.addEventListener("click", async () => {
      const restore = busy(confirm, busyLabel);
      cancel.disabled = true;
      feedback(errorId, "");
      try {
        await run();
        dialog.close();
      } catch (error) {
        feedback(errorId, failure(error), "error");
      } finally {
        restore();
        cancel.disabled = false;
      }
    });
  };
  const eventPath = (id) => `/api/admin/events/${encodeURIComponent(id)}`;
  const formPath = (id) => `/${encodeURIComponent(id)}`;

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
        await post(`/api/events/${encodeURIComponent(id)}/participants`, body);
        get("participant-form").hidden = true;
        get("participant-intro").hidden = true;
        get("confirmation-message").textContent = event.form?.confirmation || EventForms.defaultConfirmation;
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
    showFlash();
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
    let formBuilder = EventForms.builder(get("form-builder"));
    get("import-form").addEventListener("click", () => get("import-form-file").click());
    get("import-form-file").addEventListener("change", async () => {
      const file = get("import-form-file").files[0];
      get("import-form-file").value = "";
      if (!file) return;
      let imported;
      try {
        if (file.size > 512 * 1024) throw new Error("too large");
        imported = JSON.parse(await file.text());
        // Accept a full export or a bare form definition.
        if (Array.isArray(imported?.sections)) imported = { form: imported };
        if (!Array.isArray(imported?.form?.sections) || !imported.form.sections.length) throw new Error("no form");
      } catch (_) {
        feedback("create-feedback", "That file isn’t a form export. Choose a .json file from Export form on an event page.", "error", true);
        return;
      }
      formBuilder = EventForms.builder(get("form-builder"), imported.form);
      if (typeof imported.name === "string") get("create-name").value = imported.name;
      if (typeof imported.description === "string") get("create-description").value = imported.description;
      if (typeof imported.qrBanner === "string") {
        get("create-qr-banner").value = imported.qrBanner;
        get("create-qr-banner").dispatchEvent(new Event("input"));
      }
      get("create-template-badge").textContent = "Imported form";
      const problem = formBuilder.validate();
      if (problem) feedback("create-feedback", `Imported ${file.name}, but it needs a fix before you can create the event. ${problem}`, "error", true);
      else feedback("create-feedback", `Imported ${file.name}. Choose the last day for responses, review the questions, and create the event.`, "success");
      get("create-end-date").focus();
    });
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
      const restore = busy(get("create-submit"), "Creating…");
      get("create-fields").disabled = true;
      feedback("create-feedback", "");
      try {
        const event = await post("/api/admin/events", body);
        flash(`Created “${event.name}”. Share its QR code or form link to start collecting responses.`);
        location.assign(`/admin/events/${encodeURIComponent(event.id)}`);
      } catch (error) {
        feedback("create-feedback", failure(error), "error", true);
        creating = false;
        restore();
        get("create-fields").disabled = false;
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
    showFlash();
    const refreshSummary = () => {
      get("detail-name").textContent = event.name;
      setDescription("detail-description", event.description);
      badge(get("detail-status"), event.status);
      get("detail-count").textContent = number.format(event.count || 0);
      get("detail-closes").textContent = dateText(event.endedAt || event.closesAt);
      get("detail-end").hidden = event.status !== "open";
      get("form-edit-toggle").hidden = event.status === "archived";
      let lifecycle = "";
      if (event.completesAt) {
        lifecycle = "Responses remain available to view and export for as long as Temporal retains them.";
      }
      get("detail-lifecycle").textContent = lifecycle;
      get("detail-lifecycle").hidden = !lifecycle;
    };
    refreshSummary();
    get("detail-qr").href = `/admin/events/${encodeURIComponent(id)}/qr`;
    get("detail-form-preview").href = formPath(id);
    get("detail-banner").value = event.qrBanner || "";
    get("banner-form").addEventListener("submit", async (submission) => {
      submission.preventDefault();
      if (get("banner-save").disabled) return;
      const restore = busy(get("banner-save"), "Saving…");
      feedback("banner-feedback", "");
      try {
        event = await post(`${url}/banner`, { banner: get("detail-banner").value.trim() });
        refreshSummary();
        toast("QR display message saved.");
      } catch (error) { feedback("banner-feedback", failure(error), "error", true); }
      finally { restore(); }
    });
    get("detail-copy").addEventListener("click", async () => {
      const link = `${location.origin}${formPath(id)}`;
      try {
        await navigator.clipboard.writeText(link);
        const button = get("detail-copy");
        button.textContent = "Copied ✓";
        setTimeout(() => { button.textContent = "Copy form link"; }, 2000);
        toast(`Form link copied: ${link}`);
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
      if (button.disabled) return;
      const restore = busy(button, "Preparing CSV…");
      feedback("detail-feedback", "");
      try {
        const response = await fetch(`${url}/export.csv`, { credentials: "same-origin" });
        if (!response.ok) {
          const data = await response.json().catch(() => ({}));
          throw new Error(data.error || "Unable to export the responses. Please try again.");
        }
        download(await response.blob(), `${event.name.replace(/[^a-zA-Z0-9_-]+/g, "-").slice(0, 80) || "event"}-leads.csv`);
        toast("CSV downloaded.");
      } catch (error) { feedback("detail-feedback", failure(error), "error", true); }
      finally { restore(); }
    });
    confirmation({
      dialog: get("end-dialog"), open: get("detail-end"), cancel: get("cancel-end"), confirm: get("confirm-end"),
      busyLabel: "Ending…", errorId: "end-dialog-feedback",
      run: async () => {
        event = await post(`${url}/end`, {});
        refreshSummary();
        toast("Event ended. New responses are closed.");
      },
    });
    confirmation({
      dialog: get("delete-dialog"), open: get("detail-delete"), cancel: get("cancel-delete"), confirm: get("confirm-delete"),
      busyLabel: "Deleting…", errorId: "delete-dialog-feedback",
      run: async () => {
        await post(`${url}/delete`, {});
        flash(`Deleted “${event.name}”.`);
        location.assign("/admin");
        // Stay busy while the browser navigates away.
        await new Promise(() => {});
      },
    });
    let cursor = "";
    let loading = false;
    let shown = false;
    const body = get("leads-body");
    let responseFields = [];
    const renderHead = () => {
      responseFields = EventForms.fields(event.form);
      get("leads-head").replaceChildren();
      responseFields.forEach((field) => { const header = node("th"); header.append(node("span", "cell-clamp", field.label)); header.title = field.label; header.scope = "col"; get("leads-head").append(header); });
      const dateHeader = node("th", "", "Last submitted · UTC"); dateHeader.scope = "col"; get("leads-head").append(dateHeader);
    };
    renderHead();
    const renderLead = (lead) => {
      const row = node("tr");
      const cells = [
        ...responseFields.map((field) => [field.label, EventForms.answerText(field, lead), "reason-cell"]),
        ["Last submitted · UTC", dateText(lead.lastSubmittedAt), "date-cell"],
      ];
      cells.forEach(([label, value, className]) => {
        // Long answers are clamped to a few lines; the tooltip and the CSV export carry the full text.
        const cell = node("td", className);
        cell.append(node("span", "cell-clamp", value));
        cell.dataset.label = label;
        cell.title = label === "Last submitted · UTC" ? `First submitted: ${dateText(lead.firstSubmittedAt)}` : value;
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
    get("refresh-leads").addEventListener("click", async () => {
      const button = get("refresh-leads");
      button.textContent = "Refreshing…";
      await load(true);
      button.textContent = "Refresh";
    });
    let formBuilder = null;
    const closeEditor = () => {
      get("form-editor").hidden = true;
      get("form-edit-toggle").setAttribute("aria-expanded", "false");
      get("detail-form-builder").replaceChildren();
      formBuilder = null;
    };
    get("form-edit-toggle").addEventListener("click", () => {
      if (!get("form-editor").hidden) { closeEditor(); return; }
      feedback("form-feedback", "");
      formBuilder = EventForms.builder(get("detail-form-builder"), event.form, {
        feedback: "form-feedback", eventName: () => event.name, eventDescription: () => event.description,
        help: "Add, remove, or reorder questions. The required email question stays in the first section so repeat responses merge.",
      });
      get("form-editor").hidden = false;
      get("form-edit-toggle").setAttribute("aria-expanded", "true");
    });
    get("form-cancel").addEventListener("click", () => { closeEditor(); get("form-edit-toggle").focus(); });
    get("form-export").addEventListener("click", () => {
      const exported = { kind: formExportKind, version: 1, exportedAt: new Date().toISOString(), name: event.name, description: event.description || "", qrBanner: event.qrBanner || "", form: event.form || EventForms.defaultForm() };
      download(new Blob([`${JSON.stringify(exported, null, 2)}\n`], { type: "application/json" }), `${event.id}-form.json`);
      toast("Form exported. Use Import form when you create an event to set it up the same way.");
    });
    let savingForm = false;
    get("form-editor").addEventListener("submit", async (submission) => {
      submission.preventDefault();
      if (savingForm || !formBuilder) return;
      const problem = formBuilder.validate();
      if (problem) { feedback("form-feedback", problem, "error", true); return; }
      if (!submission.currentTarget.reportValidity()) return;
      savingForm = true;
      const restore = busy(get("form-save"), "Saving…");
      get("form-editor-fields").disabled = true;
      feedback("form-feedback", "");
      try {
        event = await post(`${url}/form`, { form: formBuilder.value() });
        refreshSummary();
        closeEditor();
        renderHead();
        feedback("form-feedback", "");
        toast("Questions saved. New responses use the updated form.");
        await load(true);
      } catch (error) { feedback("form-feedback", failure(error), "error", true); }
      finally { savingForm = false; restore(); get("form-editor-fields").disabled = false; }
    });
    await load();
  }

  async function start() {
    const path = location.pathname.replace(/\/+$/, "") || "/";
    const participantRoute = path.match(/^\/events\/([^/]+)$/) || (path !== "/admin" && path.match(/^\/([A-Za-z0-9-]+)$/));
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
