"use strict";

// Form definitions and responses stay on the event; this module only edits and displays them.
window.EventForms = (() => {
  const types = {
    short_text: "Short answer", paragraph: "Paragraph", email: "Email", phone: "Phone number", url: "Website", number: "Number",
    multiple_choice: "Multiple choice", dropdown: "Dropdown", checkboxes: "Checkboxes", linear_scale: "Linear scale", rating: "Rating",
    date: "Date", time: "Time", multiple_choice_grid: "Multiple choice grid", checkbox_grid: "Checkbox grid",
  };
  const choiceTypes = ["multiple_choice", "dropdown", "checkboxes", "multiple_choice_grid", "checkbox_grid"];
  const gridTypes = ["multiple_choice_grid", "checkbox_grid"];
  const clone = (value) => JSON.parse(JSON.stringify(value));
  const node = (tag, className, text) => {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined) element.textContent = text;
    return element;
  };
  const button = (label, action, className = "editor-button") => {
    const element = node("button", className, label);
    element.type = "button";
    element.addEventListener("click", action);
    return element;
  };
  const uid = (prefix) => `${prefix}_${crypto.randomUUID().replaceAll("-", "").slice(0, 12)}`;
  const defaultForm = () => ({ sections: [{ id: "details", title: "Your details", description: "", fields: [
    { id: "name", label: "Name", type: "short_text", required: true },
    { id: "role", label: "Title / role", type: "short_text", required: true },
    { id: "email", label: "Email", type: "email", required: true },
    { id: "reason", label: "What would you like to follow up about?", type: "paragraph", required: false },
  ] }] });
  const fields = (schema) => (schema || defaultForm()).sections.flatMap((section) => section.fields);
  const editorControl = (label, control, help) => {
    const wrap = node("div", "field editor-control");
    const labelNode = node("label", "", label);
    control.id ||= uid("editor");
    labelNode.htmlFor = control.id;
    wrap.append(labelNode, control);
    if (help) wrap.append(node("p", "field-help", help));
    return wrap;
  };
  const textControl = (value, onInput, options = {}) => {
    const input = node(options.multiline ? "textarea" : "input");
    if (options.multiline) input.rows = options.rows || 2;
    else input.type = options.type || "text";
    input.value = value ?? "";
    if (options.maxLength) input.maxLength = options.maxLength;
    if (options.required) input.required = true;
    if (options.placeholder) input.placeholder = options.placeholder;
    if (options.min !== undefined) input.min = options.min;
    if (options.max !== undefined) input.max = options.max;
    if (options.step !== undefined) input.step = options.step;
    if (options.disabled) input.disabled = true;
    input.addEventListener("input", () => onInput(input.value));
    return input;
  };
  const selectControl = (items, value, onChange) => {
    const select = node("select");
    items.forEach(([id, label]) => {
      const option = node("option", "", label);
      option.value = id;
      select.append(option);
    });
    select.value = value || "";
    select.addEventListener("change", () => onChange(select.value));
    return select;
  };

  function builder(container, initial = defaultForm()) {
    const schema = clone(initial);
    let active = schema.sections[0].fields[0]?.id;
    const cleanupRoutes = () => {
      schema.sections.forEach((section, index) => {
        const valid = new Set(["submit", ...schema.sections.slice(index + 1).map((item) => item.id)]);
        if (section.nextSectionId && !valid.has(section.nextSectionId)) delete section.nextSectionId;
        section.fields.forEach((field) => {
          if (field.branches) field.branches = field.branches.filter((branch) => valid.has(branch.sectionId) && (field.options || []).includes(branch.value));
        });
      });
    };
    const move = (array, index, direction) => {
      [array[index], array[index + direction]] = [array[index + direction], array[index]];
      cleanupRoutes();
      render();
    };
    const targets = (index) => [["", index === schema.sections.length - 1 ? "Submit form (default)" : "Continue to the next section"], ...schema.sections.slice(index + 1).map((section, offset) => [section.id, `Section ${index + offset + 2}: ${section.title || "Untitled"}`]), ["submit", "Submit form"]];
    const preview = () => {
      const problem = validate();
      if (problem) { document.getElementById("create-feedback").textContent = problem; document.getElementById("create-feedback").hidden = false; return; }
      const dialog = node("dialog", "form-preview-dialog");
      const top = node("div", "preview-toolbar");
      top.append(node("span", "eyebrow", "Attendee preview"), button("Back to editor ×", () => dialog.close()));
      dialog.append(top, node("h1", "preview-event-name", document.getElementById("create-name").value || "Your event"));
      const eventDescription = document.getElementById("create-description").value;
      if (eventDescription) dialog.append(node("p", "intro", eventDescription));
      const form = node("form", "card form-card");
      const fieldset = node("fieldset");
      const message = node("p", "feedback"); message.hidden = true; message.setAttribute("role", "status");
      form.append(fieldset, message, node("p", "form-note", "Preview only. Your answers won’t be saved."));
      dialog.append(form); document.body.append(dialog);
      const navigator = mountParticipant(fieldset, clone(schema));
      form.addEventListener("submit", (event) => {
        event.preventDefault();
        if (navigator.advance() !== null) {
          form.replaceChildren(node("h2", "", "You’re all set."), node("p", "field-help", "This is how the confirmation appears after someone submits your form."), button("Back to editor", () => dialog.close(), "button"));
        }
      });
      dialog.addEventListener("close", () => dialog.remove()); dialog.showModal();
    };
    const addField = (section) => {
      if (fields(schema).length >= 100) return;
      const field = { id: uid("field"), label: "Untitled question", description: "", type: "short_text", required: false };
      section.fields.push(field);
      active = field.id;
      render();
      container.querySelector(`[data-field="${active}"]`).scrollIntoView({ block: "nearest", behavior: "smooth" });
    };
    const renderField = (section, sectionIndex, field, index) => {
      const card = node("article", `question-editor${active === field.id ? " is-active" : ""}`);
      card.dataset.field = field.id;
      const heading = button("", () => { active = active === field.id ? "" : field.id; render(); }, "question-summary");
      heading.setAttribute("aria-expanded", String(active === field.id));
      const icon = node("span", "question-index", String(index + 1).padStart(2, "0"));
      const title = node("span", "question-summary-title", field.label || "Untitled question");
      const type = node("span", "question-type", types[field.type]);
      if (field.required) type.append(node("span", "required-marker", " *"));
      heading.append(icon, title, type, node("span", "question-chevron", active === field.id ? "−" : "+"));
      card.append(heading);
      if (active !== field.id) return card;
      const editor = node("div", "question-editor-body");
      const main = node("div", "editor-main-row");
      const label = textControl(field.label, (value) => { field.label = value; title.textContent = value || "Untitled question"; }, { maxLength: 200, required: true });
      main.append(editorControl("Question", label));
      const typeInput = selectControl(Object.entries(types), field.type, (value) => {
        field.type = value;
        delete field.options; delete field.rows; delete field.min; delete field.max; delete field.minimum; delete field.maximum; delete field.minLabel; delete field.maxLabel; delete field.branches;
        if (choiceTypes.includes(value)) field.options = ["Option 1", "Option 2"];
        if (gridTypes.includes(value)) field.rows = ["Row 1", "Row 2"];
        if (["linear_scale", "rating"].includes(value)) { field.min = 1; field.max = 5; }
        render();
      });
      typeInput.disabled = field.id === "email";
      main.append(editorControl("Answer type", typeInput));
      editor.append(main, editorControl("Help text", textControl(field.description, (value) => { field.description = value; }, { maxLength: 1000, placeholder: "Add instructions for this question (optional)" })));
      if (choiceTypes.includes(field.type)) {
        const choices = textControl((field.options || []).join("\n"), (value) => { field.options = value.split("\n").map((item) => item.trim()).filter(Boolean); }, { multiline: true, rows: 3, maxLength: 10000, required: true });
        choices.addEventListener("change", () => { cleanupRoutes(); render(); });
        editor.append(editorControl("Choices", choices, "One choice per line. Up to 50 choices."));
      }
      if (gridTypes.includes(field.type)) editor.append(editorControl("Rows", textControl((field.rows || []).join("\n"), (value) => { field.rows = value.split("\n").map((item) => item.trim()).filter(Boolean); }, { multiline: true, required: true, maxLength: 5000 }), "One row per line. Up to 25 rows."));
      if (["linear_scale", "rating", "number"].includes(field.type)) {
        const range = node("div", "editor-range-row");
        const minimumKey = field.type === "number" ? "minimum" : "min";
        const maximumKey = field.type === "number" ? "maximum" : "max";
        range.append(editorControl(field.type === "number" ? "Minimum (optional)" : "From", textControl(field[minimumKey], (value) => { if (value === "") delete field[minimumKey]; else field[minimumKey] = Number(value); }, { type: "number", required: field.type !== "number", min: field.type === "number" ? undefined : field.type === "rating" ? 1 : 0, max: field.type === "number" ? undefined : 1, step: field.type === "number" ? "any" : 1, disabled: field.type === "rating" })));
        range.append(editorControl(field.type === "number" ? "Maximum (optional)" : "To", textControl(field[maximumKey], (value) => { if (value === "") delete field[maximumKey]; else field[maximumKey] = Number(value); }, { type: "number", required: field.type !== "number", min: field.type === "number" ? undefined : field.type === "rating" ? 3 : 2, max: field.type === "number" ? undefined : 10, step: field.type === "number" ? "any" : 1 })));
        editor.append(range);
        if (field.type !== "number") {
          const labels = node("div", "editor-range-row");
          labels.append(editorControl("Low label (optional)", textControl(field.minLabel, (value) => { field.minLabel = value; }, { maxLength: 200 })), editorControl("High label (optional)", textControl(field.maxLabel, (value) => { field.maxLabel = value; }, { maxLength: 200 })));
          editor.append(labels);
        }
      }
      if (["multiple_choice", "dropdown"].includes(field.type)) {
        const anotherBranch = section.fields.some((other) => other.id !== field.id && (other.branches || []).length);
        const routing = node("details", "routing-editor");
        routing.open = Boolean((field.branches || []).length);
        routing.append(node("summary", "", "Go to a section based on the answer"));
        if (anotherBranch) routing.append(node("p", "field-help", "Another question in this section already controls navigation. Each section can have one routing question."));
        else {
          routing.append(node("p", "field-help", "Choose a destination for each answer. Unchanged answers follow this section’s default route."));
          (field.options || []).forEach((option) => {
            const row = node("div", "branch-row");
            const current = (field.branches || []).find((item) => item.value === option)?.sectionId || "";
            const target = selectControl([["", "Use section default"], ...targets(sectionIndex).filter(([value]) => value)], current, (value) => {
              field.branches = (field.branches || []).filter((item) => item.value !== option);
              if (value) field.branches.push({ value: option, sectionId: value });
            });
            target.setAttribute("aria-label", `After choosing ${option}`);
            row.append(node("span", "", option), target);
            routing.append(row);
          });
        }
        editor.append(routing);
      }
      const footer = node("div", "question-editor-footer");
      const tools = node("div", "question-tools");
      const up = button("↑", () => move(section.fields, index, -1)); up.disabled = index === 0; up.setAttribute("aria-label", `Move ${field.label} up`);
      const down = button("↓", () => move(section.fields, index, 1)); down.disabled = index === section.fields.length - 1; down.setAttribute("aria-label", `Move ${field.label} down`);
      tools.append(up, down);
      if (field.id !== "email") {
        const duplicate = button("Duplicate", () => {
          if (fields(schema).length >= 100) return;
          const copy = clone(field); copy.id = uid("field"); copy.label = `${field.label} (copy)`; delete copy.branches;
          section.fields.splice(index + 1, 0, copy); active = copy.id; render();
        });
        duplicate.disabled = fields(schema).length >= 100;
        tools.append(duplicate, button("Delete", () => { section.fields.splice(index, 1); active = ""; render(); }, "editor-button delete-button"));
        if (schema.sections.length > 1) {
          const relocate = selectControl([["", "Move to section…"], ...schema.sections.filter((item) => item.id !== section.id).map((item) => [item.id, item.title || "Untitled section"])], "", (target) => {
            if (!target) return;
            section.fields.splice(index, 1); delete field.branches;
            schema.sections.find((item) => item.id === target).fields.push(field); active = field.id; render();
          });
          relocate.className = "move-field-select"; relocate.setAttribute("aria-label", `Move ${field.label} to another section`); tools.append(relocate);
        }
      }
      footer.append(tools);
      if (field.id === "email") footer.append(node("span", "pinned-field", "Required email · used to merge repeat responses"));
      else {
        const toggle = node("label", "required-toggle");
        const checkbox = node("input"); checkbox.type = "checkbox"; checkbox.checked = Boolean(field.required);
        checkbox.addEventListener("change", () => { field.required = checkbox.checked; type.textContent = types[field.type] + (field.required ? " *" : ""); });
        toggle.append(checkbox, node("span", "", "Required")); footer.append(toggle);
      }
      editor.append(footer); card.append(editor); return card;
    };
    function render() {
      container.replaceChildren();
      const heading = node("div", "builder-heading");
      const headingActions = node("div", "builder-heading-actions");
      headingActions.append(node("span", "builder-count", `${fields(schema).length} questions · ${schema.sections.length} ${schema.sections.length === 1 ? "section" : "sections"}`), button("Preview form ↗", preview, "button secondary compact"));
      heading.append(node("h2", "", "Questions & sections"), headingActions);
      container.append(heading, node("p", "field-help builder-help", "Start with the contact template. Add questions, choose what’s required, and send people to the right section."));
      schema.sections.forEach((section, sectionIndex) => {
        const card = node("section", "section-editor");
        const header = node("div", "section-editor-header");
        const marker = node("span", "section-marker", `SECTION ${String(sectionIndex + 1).padStart(2, "0")}`);
        const tools = node("div", "question-tools");
        if (sectionIndex > 0) {
          const up = button("↑", () => move(schema.sections, sectionIndex, -1)); up.disabled = sectionIndex === 1; up.setAttribute("aria-label", "Move section up");
          const down = button("↓", () => move(schema.sections, sectionIndex, 1)); down.disabled = sectionIndex === schema.sections.length - 1; down.setAttribute("aria-label", "Move section down");
          tools.append(up, down, button("Delete section", () => { schema.sections.splice(sectionIndex, 1); cleanupRoutes(); render(); }, "editor-button delete-button"));
        } else tools.append(node("span", "section-pin", "Contact section"));
        header.append(marker, tools); card.append(header);
        const info = node("div", "section-editor-info");
        info.append(editorControl("Section title", textControl(section.title, (value) => { section.title = value; }, { maxLength: 200, placeholder: "Section title" })), editorControl("Section description", textControl(section.description, (value) => { section.description = value; }, { maxLength: 1000, placeholder: "A short introduction (optional)" })));
        card.append(info);
        section.fields.forEach((field, index) => card.append(renderField(section, sectionIndex, field, index)));
        const footer = node("div", "section-editor-footer");
        const add = button("+ Add question", () => addField(section), "button secondary compact"); add.disabled = fields(schema).length >= 100;
        footer.append(add, editorControl("After this section", selectControl(targets(sectionIndex), section.nextSectionId, (value) => { if (value) section.nextSectionId = value; else delete section.nextSectionId; })));
        card.append(footer); container.append(card);
      });
      const addSection = button("+ Add section", () => {
        if (schema.sections.length >= 20) return;
        schema.sections.push({ id: uid("section"), title: `Section ${schema.sections.length + 1}`, description: "", fields: [] });
        active = ""; render();
        container.lastElementChild.previousElementSibling.scrollIntoView({ block: "nearest", behavior: "smooth" });
      }, "button secondary add-section");
      addSection.disabled = schema.sections.length >= 20;
      container.append(addSection);
    }
    const validate = () => {
        const all = fields(schema);
        const missing = all.find((field) => !field.label.trim() || (choiceTypes.includes(field.type) && (!(field.options || []).length || new Set(field.options).size !== field.options.length || field.options.length > 50)) || (gridTypes.includes(field.type) && (!(field.rows || []).length || new Set(field.rows).size !== field.rows.length || field.rows.length > 25)) || (["linear_scale", "rating"].includes(field.type) && (!Number.isInteger(field.min) || !Number.isInteger(field.max) || field.min < (field.type === "rating" ? 1 : 0) || field.min > 1 || field.max < (field.type === "rating" ? 3 : 2) || field.max > 10)) || (field.type === "number" && field.minimum !== undefined && field.maximum !== undefined && field.minimum > field.maximum));
        if (missing) { active = missing.id; render(); return `Check “${missing.label || "Untitled question"}”: add a question label and valid, unique choices or rows. Scale ranges must start at 0 or 1 and end at 2–10 (3–10 for ratings).`; }
        return "";
    };
    render();
    return { value: () => { cleanupRoutes(); return clone(schema); }, validate };
  }

  function mountParticipant(container, initial) {
    const schema = initial || defaultForm();
    const saved = new Map();
    let path = [0];
    let position = 0;
    let submitting = false;
    let controls = [];
    const questions = node("div", "participant-questions");
    const heading = node("div", "participant-section-heading");
    const progress = node("p", "form-progress");
    const title = node("h2");
    const description = node("p", "field-help");
    heading.append(progress, title, description);
    const actions = node("div", "participant-navigation");
    const back = button("← Back", () => { capture(); position--; render(); }, "button secondary");
    const next = node("button", "button", "Continue →"); next.type = "submit"; next.dataset.participantSubmit = "";
    actions.append(back, next); container.append(heading, questions, actions);
    const addField = (field, fieldIndex) => {
      const grouped = ["multiple_choice", "checkboxes", "linear_scale", "rating", ...gridTypes].includes(field.type);
      const wrap = node(grouped ? "fieldset" : "div", "field response-field");
      const label = node(grouped ? "legend" : "label", "question-label", field.label);
      label.append(node("span", field.required ? "required-marker" : "optional", field.required ? " *" : "Optional"));
      wrap.append(label);
      if (field.description) wrap.append(node("p", "field-help question-help", field.description));
      const prefix = `answer-${fieldIndex}`;
      const prior = saved.get(field.id) || [];
      let read;
      let validate = () => {};
      const wire = (input) => { input.addEventListener("input", () => input.setCustomValidity("")); input.addEventListener("change", () => input.setCustomValidity("")); return input; };
      const choice = (value, type, name, checked, text = value) => {
        const option = node("label", "choice-option");
        const input = wire(node("input")); input.type = type; input.name = name; input.value = value; input.checked = checked;
        option.append(input, node("span", "", text)); return { option, input };
      };
      if (gridTypes.includes(field.type)) {
        const scroll = node("div", "grid-scroll");
        const table = node("table", "response-grid");
        const head = node("thead"); const headRow = node("tr"); headRow.append(node("th", "", ""));
        field.options.forEach((option) => { const cell = node("th", "", option); cell.scope = "col"; headRow.append(cell); });
        head.append(headRow); table.append(head);
        const body = node("tbody"); const rows = [];
        field.rows.forEach((row, index) => {
          const cells = []; const tr = node("tr"); const rowHead = node("th", "", row); rowHead.scope = "row"; tr.append(rowHead);
          let previous = [];
          if (field.type === "checkbox_grid") { try { previous = JSON.parse(prior[index] || "[]"); } catch (_) { previous = []; } }
          field.options.forEach((option) => {
            const input = wire(node("input")); input.type = field.type === "checkbox_grid" ? "checkbox" : "radio"; input.name = `${prefix}-row-${index}`; input.value = option;
            input.setAttribute("aria-label", `${row}: ${option}`); input.checked = field.type === "checkbox_grid" ? previous.includes(option) : prior[index] === option;
            if (field.required && input.type === "radio") input.required = true;
            const cell = node("td"); cell.append(input); tr.append(cell); cells.push(input);
          });
          rows.push(cells); body.append(tr);
          cells.forEach((input) => input.addEventListener("change", () => cells.forEach((item) => item.setCustomValidity(""))));
        });
        table.append(body); scroll.append(table); wrap.append(scroll);
        read = () => rows.map((row) => field.type === "checkbox_grid" ? JSON.stringify(row.filter((item) => item.checked).map((item) => item.value)) : row.find((item) => item.checked)?.value || "");
        validate = () => rows.forEach((row) => row[0].setCustomValidity(field.required && !row.some((input) => input.checked) ? "Choose an answer for each row." : ""));
      } else if (["multiple_choice", "checkboxes", "linear_scale", "rating"].includes(field.type)) {
        const scale = ["linear_scale", "rating"].includes(field.type);
        const minimum = field.type === "rating" ? 1 : (field.min ?? 0);
        const options = scale ? Array.from({ length: (field.max ?? 5) - minimum + 1 }, (_, index) => String(index + minimum)) : field.options;
        const list = node("div", scale ? "scale-options" : "choice-options");
        const inputs = options.map((option) => {
          const item = choice(option, field.type === "checkboxes" ? "checkbox" : "radio", prefix, prior.includes(option), field.type === "rating" ? `${option} ★` : option);
          if (scale) item.option.classList.add("scale-option");
          if (field.required && item.input.type === "radio") item.input.required = true;
          list.append(item.option); return item.input;
        });
        inputs.forEach((input) => input.addEventListener("change", () => inputs.forEach((item) => item.setCustomValidity(""))));
        wrap.append(list);
        if (scale && (field.minLabel || field.maxLabel)) { const endpoints = node("div", "scale-labels"); endpoints.append(node("span", "", field.minLabel || ""), node("span", "", field.maxLabel || "")); wrap.append(endpoints); }
        read = () => inputs.filter((item) => item.checked).map((item) => item.value);
        validate = () => inputs[0]?.setCustomValidity(field.required && !inputs.some((input) => input.checked) ? "Choose at least one answer." : "");
      } else {
        let input;
        if (field.type === "dropdown") {
          input = selectControl([["", "Choose an answer"], ...field.options.map((option) => [option, option])], prior[0], () => {});
        } else if (field.type === "paragraph") { input = node("textarea"); input.rows = 3; input.maxLength = 5000; }
        else {
          input = node("input");
          input.type = ({ email: "email", phone: "tel", url: "url", number: "number", date: "date", time: "time" })[field.type] || "text";
          if (input.type === "number") { input.step = "any"; if (field.minimum !== undefined) input.min = field.minimum; if (field.maximum !== undefined) input.max = field.maximum; }
          else if (!["date", "time"].includes(input.type)) input.maxLength = field.type === "email" ? 254 : 500;
          input.autocomplete = ({ name: "name", role: "organization-title", email: "email" })[field.id] || (field.type === "phone" ? "tel" : "off");
          if (field.type === "email") { input.inputMode = "email"; input.autocapitalize = "none"; input.spellcheck = false; }
        }
        input.id = prefix; input.name = field.id; input.required = Boolean(field.required); input.value = prior[0] || ""; label.htmlFor = input.id;
        wire(input); wrap.append(input); read = () => input.value.trim() ? [input.value.trim()] : [];
        validate = () => input.setCustomValidity(field.required && !input.value.trim() ? "Please answer this question." : "");
      }
      controls.push({ field, read, validate }); questions.append(wrap);
    };
    const capture = () => controls.forEach(({ field, read }) => saved.set(field.id, read()));
    const destination = () => {
      const index = path[position]; const section = schema.sections[index];
      const routed = section.fields.flatMap((field) => (field.branches || []).filter((branch) => (saved.get(field.id) || [])[0] === branch.value))[0]?.sectionId;
      const target = routed || section.nextSectionId;
      if (target === "submit") return -1;
      if (target) return schema.sections.findIndex((item) => item.id === target);
      return index + 1 < schema.sections.length ? index + 1 : -1;
    };
    const nextLabel = () => { next.textContent = destination() === -1 ? "Request a follow-up →" : "Continue →"; };
    questions.addEventListener("change", () => { capture(); nextLabel(); });
    function render() {
      const index = path[position]; const section = schema.sections[index];
      progress.textContent = schema.sections.length > 1 ? `Section ${index + 1} of ${schema.sections.length}` : "* Required questions";
      title.textContent = section.title || ""; title.hidden = !section.title;
      description.textContent = section.description || ""; description.hidden = !section.description;
      controls = []; questions.replaceChildren(); section.fields.forEach(addField);
      back.hidden = position === 0; back.disabled = submitting;
      nextLabel();
      next.disabled = submitting;
    }
    render();
    return {
      advance: () => {
        controls.forEach(({ validate }) => validate());
        if (!container.closest("form").reportValidity()) return null;
        capture(); const target = destination();
        if (target !== -1) {
          if (path[position + 1] !== target) {
            path = path.slice(0, position + 1); path.push(target);
            const kept = new Set(path);
            schema.sections.forEach((section, index) => { if (!kept.has(index)) section.fields.forEach((field) => saved.delete(field.id)); });
          }
          position++; render();
          title.tabIndex = -1; title.focus(); container.scrollIntoView({ block: "start", behavior: "smooth" }); return null;
        }
        const visible = new Set(path.slice(0, position + 1).flatMap((index) => schema.sections[index].fields.map((field) => field.id)));
        return fields(schema).filter((field) => visible.has(field.id)).flatMap((field) => {
          const values = saved.get(field.id) || [];
          return values.some((value) => value && value !== "[]") ? [{ fieldId: field.id, values }] : [];
        });
      },
      setSubmitting: (value) => { submitting = value; next.disabled = value; back.disabled = value; if (value) next.textContent = "Saving your details…"; else nextLabel(); },
    };
  }

  const answerText = (field, lead) => {
    const answer = (lead.answers || []).find((item) => item.fieldId === field.id);
    const values = answer?.values || (lead[field.id] ? [lead[field.id]] : []);
    if (gridTypes.includes(field.type)) return (field.rows || []).map((row, index) => {
      let value = values[index] || "";
      if (field.type === "checkbox_grid") { try { value = JSON.parse(value || "[]").join(", "); } catch (_) {} }
      return value ? `${row}: ${value}` : "";
    }).filter(Boolean).join("\n") || "—";
    return values.join(", ") || "—";
  };
  return { builder, mountParticipant, defaultForm, fields, answerText };
})();
