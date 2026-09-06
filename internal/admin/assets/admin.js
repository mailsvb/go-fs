// The page is generated from the schema the server sends, which it in turn
// generates from the configuration struct. Nothing here knows the name of a
// single setting, so a key added to the configuration file shows up with no
// change to this file.

let schema = null;
let values = null;
// what each stored certificate or key actually is, keyed "ftps.cert", since
// base64 on its own tells the reader nothing
let summaries = {};
// the tab that is open, kept across a reload so that Apply does not send the
// reader back to the first one
let selected = 0;

const banner = document.getElementById("banner");
const tabs = document.getElementById("tabs");
const panels = document.getElementById("panels");
const footer = document.getElementById("footer");
const status = document.getElementById("status");
const applyButton = document.getElementById("apply");

function say(text, good) {
  banner.textContent = text;
  banner.classList.toggle("good", good === true);
  banner.hidden = text === "";
}

async function load() {
  const answer = await fetch("/api/config", { headers: { Accept: "application/json" } });
  if (!answer.ok) {
    say("The configuration could not be read: " + (await answer.text()));
    return;
  }
  const state = await answer.json();
  schema = state.schema;
  values = state.values;
  summaries = state.summaries || {};

  document.getElementById("path").textContent = state.path;
  if (!state.writable) {
    say("This file cannot be written, so Apply will fail: " + state.writeError);
  } else if (!state.reload) {
    say("general.reloadConfig is off, so a change is written to the file but only "
      + "takes effect when go-fs is restarted.", true);
  } else {
    say("");
  }
  applyButton.disabled = !state.writable;

  render();
  footer.hidden = false;
}

function render() {
  tabs.replaceChildren();
  panels.replaceChildren();
  schema.sections.forEach((section, index) => {
    tabs.append(tab(section, index));
    panels.append(panel(section, index));
  });
  select(Math.min(selected, schema.sections.length - 1));
}

function tab(section, index) {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = section.label;
  button.setAttribute("role", "tab");
  button.addEventListener("click", () => select(index));
  return button;
}

function select(index) {
  selected = index;
  Array.from(tabs.children).forEach((button, i) =>
    button.setAttribute("aria-selected", String(i === index)));
  Array.from(panels.children).forEach((section, i) => (section.hidden = i !== index));
}

function panel(section, index) {
  const element = document.createElement("section");
  element.hidden = index !== selected;
  if (section.help) {
    element.append(paragraph(section.help, "section-help"));
  }
  element.append(fieldGrid(section.fields, values[section.key], section.key));

  (section.tables || []).forEach((table) => {
    element.append(tableBlock(section, table));
  });
  return element;
}

// fieldGrid lays out the plain keys of a section or of one record.
function fieldGrid(fields, holder, section) {
  const grid = document.createElement("div");
  grid.className = "fields";
  fields.forEach((field) => {
    const label = document.createElement("label");
    label.textContent = field.label;
    const input = field.upload
      ? materialEditor(field, holder, section)
      : editor(field, holder);
    label.htmlFor = input.id;
    grid.append(label, input.element);
    if (field.help) {
      grid.append(paragraph(field.help, "help"));
    }
  });
  return grid;
}

// A table that repeats in the file, [[ftp.users]] and the like, is shown as a
// list with one record per entry, each of which can be removed, and an Add
// button that appends an empty one.
function tableBlock(section, table) {
  const block = document.createElement("div");
  block.className = "table";

  const heading = document.createElement("h3");
  heading.textContent = section.key + "." + table.key;
  block.append(heading);
  if (table.help) {
    block.append(paragraph(table.help, "help"));
  }

  const list = document.createElement("div");
  block.append(list);

  const draw = () => {
    list.replaceChildren();
    const records = values[section.key][table.key] || [];
    if (records.length === 0) {
      list.append(paragraph("No entries.", "empty"));
    }
    records.forEach((record, i) => {
      const card = document.createElement("div");
      card.className = "record";

      const title = document.createElement("h4");
      title.textContent = table.key + " " + (i + 1);
      card.append(title);

      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "plain remove";
      remove.textContent = "Remove";
      remove.addEventListener("click", () => {
        records.splice(i, 1);
        draw();
      });
      card.append(remove);

      card.append(fieldGrid(table.fields, record, ""));
      list.append(card);
    });
  };

  const add = document.createElement("button");
  add.type = "button";
  add.className = "plain";
  add.textContent = "Add " + table.key;
  add.addEventListener("click", () => {
    if (!values[section.key][table.key]) {
      values[section.key][table.key] = [];
    }
    values[section.key][table.key].push(blank(table.fields));
    draw();
  });

  draw();
  block.append(add);
  return block;
}

// blank is a new record with every key at its zero value, which is what an
// unset key means in the file.
function blank(fields) {
  const record = {};
  fields.forEach((field) => {
    if (field.kind === "bool") record[field.key] = false;
    else if (field.kind === "int") record[field.key] = 0;
    else if (field.kind === "lines") record[field.key] = [];
    else record[field.key] = "";
  });
  return record;
}

let sequence = 0;

// editor builds the input for one key and wires it straight into the value it
// edits, so that Apply posts what is on the screen with nothing to collect.
function editor(field, holder) {
  const id = "field-" + ++sequence;
  const set = (value) => (holder[field.key] = value);

  if (field.kind === "bool") {
    const input = document.createElement("input");
    input.type = "checkbox";
    input.id = id;
    input.checked = holder[field.key] === true;
    input.addEventListener("change", () => set(input.checked));
    return { element: input, id };
  }

  if (field.kind === "lines") {
    const input = document.createElement("textarea");
    input.id = id;
    input.spellcheck = false;
    input.placeholder = "one entry per line";
    input.value = (holder[field.key] || []).join("\n");
    input.addEventListener("input", () =>
      set(input.value.split("\n").map((line) => line.trim()).filter((line) => line !== "")));
    return { element: input, id };
  }

  const input = document.createElement("input");
  input.id = id;
  input.spellcheck = false;

  if (field.kind === "int") {
    input.type = "number";
    input.step = "1";
    input.value = String(holder[field.key] ?? 0);
    // kept as text: an empty box is not a number, and the server reads "" as 0
    input.addEventListener("input", () => set(input.value));
    return { element: input, id };
  }

  input.type = field.kind === "secret" ? "password" : "text";
  input.value = holder[field.key] ?? "";
  input.addEventListener("input", () => set(input.value));
  if (field.kind !== "secret") {
    return { element: input, id };
  }

  const wrapper = document.createElement("div");
  wrapper.className = "secret";
  const reveal = document.createElement("button");
  reveal.type = "button";
  reveal.textContent = "show";
  reveal.addEventListener("click", () => {
    const hidden = input.type === "password";
    input.type = hidden ? "text" : "password";
    reveal.textContent = hidden ? "hide" : "show";
  });
  wrapper.append(input, reveal);
  return { element: wrapper, id };
}

// A certificate or a key is not typed in: it is the content of a file, held in
// the configuration as base64. So the box comes with an Upload button that
// posts the file for the server to validate, and a Generate button that makes
// a real one instead of the throwaway the server makes at every start when the
// key is empty. Both only put a value on the page; Apply writes it.
function materialEditor(field, holder, section) {
  const id = "field-" + ++sequence;
  const path = section ? section + "." + field.key : field.key;

  const wrapper = document.createElement("div");
  wrapper.className = "material";

  const input = document.createElement("textarea");
  input.id = id;
  input.className = "blob";
  input.rows = 3;
  input.spellcheck = false;
  input.placeholder = "empty: generated at every start";
  input.value = holder[field.key] ?? "";

  const summary = paragraph(summaries[path] || "", "summary");
  const problem = paragraph("", "problem");
  const report = (text) => {
    problem.textContent = text;
    problem.hidden = text === "";
  };
  report("");

  // a value typed or pasted in is not the one the server described
  input.addEventListener("input", () => {
    holder[field.key] = input.value.trim();
    summary.textContent = "";
    report("");
  });

  const buttons = document.createElement("div");
  buttons.className = "buttons";

  const picker = document.createElement("input");
  picker.type = "file";
  picker.hidden = true;
  picker.accept = accepts(field.upload);
  picker.addEventListener("change", async () => {
    const file = picker.files[0];
    picker.value = "";
    if (!file) return;
    report("");
    try {
      const result = await post("/api/upload", {
        kind: field.upload,
        filename: file.name,
        content: await base64Of(file),
      });
      holder[field.key] = result.value;
      summaries[path] = result.summary;
      render();
    } catch (error) {
      report(String(error.message || error));
    }
  });

  buttons.append(picker, plainButton("Upload\u2026", () => picker.click()));

  // only a certificate and a host key can be generated: a private key on its
  // own would not match any certificate
  if (field.upload !== "tlskey") {
    buttons.append(plainButton("Generate", async () => {
      report("");
      try {
        const result = await post("/api/generate", { kind: field.upload });
        holder[field.key] = result.value;
        summaries[path] = result.summary;
        if (field.pair) {
          holder[field.pair] = result.pairValue;
          summaries[section + "." + field.pair] = result.pairSummary;
        }
        render();
      } catch (error) {
        report(String(error.message || error));
      }
    }));
  }

  buttons.append(plainButton("Clear", () => {
    holder[field.key] = "";
    summaries[path] = "";
    render();
  }));

  // a private key is not shown until it is asked for, the way the one line
  // secrets are masked
  if (field.kind === "secret" && input.value !== "") {
    input.hidden = true;
    buttons.append(plainButton("show", (event) => {
      input.hidden = !input.hidden;
      event.target.textContent = input.hidden ? "show" : "hide";
    }));
  }

  wrapper.append(input, buttons, summary, problem);
  return { element: wrapper, id };
}

function plainButton(text, onClick) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "plain";
  button.textContent = text;
  button.addEventListener("click", onClick);
  return button;
}

// accepts is a hint for the file dialog only. What a file may be is decided by
// the server, which parses it.
function accepts(kind) {
  if (kind === "certificate") return ".pem,.crt,.cer,.cert";
  return ".pem,.key,.p8";
}

// base64Of reads a file the way it is posted. A data URL is the one reader
// result that is already base64, so the prefix is all there is to strip.
function base64Of(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error("the file could not be read"));
    reader.onload = () => resolve(String(reader.result).split(",")[1] || "");
    reader.readAsDataURL(file);
  });
}

async function post(url, body) {
  const answer = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await answer.text();
  if (!answer.ok) {
    throw new Error(text.trim());
  }
  return JSON.parse(text);
}

function paragraph(text, className) {
  const element = document.createElement("p");
  element.className = className;
  element.textContent = text;
  return element;
}

applyButton.addEventListener("click", async () => {
  applyButton.disabled = true;
  status.textContent = "writing...";
  try {
    const answer = await fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(values),
    });
    const text = await answer.text();
    if (!answer.ok) {
      say(text.trim());
      status.textContent = "not written";
      return;
    }
    const result = JSON.parse(text);
    const note = "Written to " + result.path + ". The previous file is " + result.backup
      + (result.reload
        ? ". go-fs applies it within the next few seconds."
        : ". general.reloadConfig is off, so it applies at the next restart.");
    status.textContent = "written";
    // reading the file back is what the page then shows, so the message about
    // the write is set after it rather than being cleared by it
    await load();
    say(note, true);
  } catch (error) {
    say(String(error));
    status.textContent = "not written";
  } finally {
    applyButton.disabled = false;
  }
});

load();
