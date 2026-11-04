// The document viewer: the page the listing opens in a frame over itself to
// show a file. The bar, the backdrop, loading, a password and a failure are
// the same for every kind of file; what draws the file is a backend, one
// module per kind in backends/, which is only loaded for a file of its kind.
//
// A backend exports open(host, source, ui) and resolves to what the bar
// drives: goTo(index), zoomIn(), zoomOut(), fit("width" | "page"), focus()
// and destroy(), any of which it may leave out. It tells the bar what it has
// through ui: pages(count, label), page(index), sheets(names, index),
// scale(factor), fits(mode), fitting(offered) and notice(text).
//
// A file opened with mode=edit is opened to be edited, which only the text
// backend does. Where it can, it says so with ui.editing(true), tells of
// changes with ui.changed(dirty), and asks for a save with ui.save(); its
// controller then also has dirty() and contents(), which is the bytes to
// write and a saved() to call once they are written. Saving is a PUT of the
// file, as an upload that replaces it is, made on the condition that nobody
// changed it since it was read.

import { Failure } from "./backends/common.js";

const kinds = ["pdf", "docx", "xlsx", "pptx", "sheet", "text", "image"];

// what the text backend shows of a file at most; the rest is left to the
// download, since a browser does not lay out a log of gigabytes
const textLimit = 10 << 20;

const $ = (id) => document.getElementById(id);

const params = new URLSearchParams(location.search);
const src = params.get("src") || "";
const name = params.get("name") || src.split("/").pop() || "file";
const kind = params.get("kind") || "";
const editing = params.get("mode") === "edit" && kind === "text";

// the frame is only ever pointed at a path of the server that served it
const local = src.startsWith("/") && !src.startsWith("//") && !src.startsWith("/\\");
const url = local ? new URL(src, location.origin) : null;

document.title = name;
$("name").textContent = name;
$("name").title = name;

// the download is the file without the marker the listing adds to view it,
// and so is where a file that is edited is saved to
let plainUrl = "";
if (url) {
  const plain = new URL(url);
  if (plain.searchParams.get("go-fs") === "view") {
    plain.searchParams.delete("go-fs");
  }
  plain.searchParams.delete("view");
  plainUrl = plain.pathname + plain.search;
  for (const link of [$("download"), $("failed-download")]) {
    link.href = plainUrl;
  }
}

// --- the bar ----------------------------------------------------------------

let controller = null;
let current = 0;
let count = 0;

const ui = {
  pages(total, label) {
    count = total;
    $("pages").hidden = total < 1;
    $("count").textContent = "/ " + total;
    $("page").setAttribute("aria-label", label || "Page");
    $("prev").title = "Previous " + (label || "page").toLowerCase();
    $("next").title = "Next " + (label || "page").toLowerCase();
    ui.page(current);
  },
  page(index) {
    current = Math.max(0, Math.min(index, Math.max(count - 1, 0)));
    if (document.activeElement !== $("page")) {
      $("page").value = String(current + 1);
    }
    $("prev").disabled = current <= 0;
    $("next").disabled = current >= count - 1;
  },
  sheets(names, index) {
    const select = $("sheets");
    select.replaceChildren(...names.map((sheet, i) => {
      const option = document.createElement("option");
      option.value = String(i);
      option.textContent = sheet;
      return option;
    }));
    select.value = String(index);
    $("sheets-group").hidden = names.length < 2;
  },
  scale(factor) {
    $("zoom").hidden = false;
    $("scale").textContent = Math.round(factor * 100) + "%";
  },
  // fits says which fit the scale is, "" for one set by zooming
  fits(mode) {
    $("fit-width").classList.toggle("active", mode === "width");
    $("fit-page").classList.toggle("active", mode === "page");
  },
  // fitting says whether fitting means anything, which for text it does not
  fitting(offered) {
    $("fit-width").hidden = !offered;
    $("fit-page").hidden = !offered;
  },
  progress(done, total) {
    const known = total > 0;
    $("meter").parentElement.classList.toggle("unknown", !known);
    $("meter").style.width = known ? Math.min(100, (done / total) * 100) + "%" : "0";
    $("progress").textContent = known
      ? "Loading " + readable(done) + " of " + readable(total)
      : "Loading " + readable(done);
  },
  askPassword,
  // notice says something about the file above it, bad for what went wrong
  notice(text, bad) {
    delete $("notice").dataset.saved;
    $("notice").textContent = text || "";
    $("notice").classList.toggle("bad", !!bad);
    $("notice").hidden = !text;
  },
  // editing offers Save, for a file the backend can edit
  editing(on) {
    $("save").hidden = !on;
  },
  // changed says whether there is anything to save
  changed(dirty) {
    // that it was saved is no longer so once it changes again
    if (dirty && $("notice").dataset.saved) {
      ui.notice("");
    }
    $("dirty").hidden = !dirty;
    $("save").disabled = !dirty || saving;
  },
  save() {
    save(false);
  },
};

function readable(bytes) {
  const units = ["bytes", "KB", "MB", "GB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return (unit ? value.toFixed(value < 10 ? 1 : 0) : value) + " " + units[unit];
}

function call(method, ...args) {
  if (controller && typeof controller[method] === "function") {
    controller[method](...args);
  }
}

$("prev").addEventListener("click", () => call("goTo", current - 1));
$("next").addEventListener("click", () => call("goTo", current + 1));
$("page").addEventListener("change", () => {
  const wanted = parseInt($("page").value, 10);
  if (wanted >= 1 && wanted <= count) {
    call("goTo", wanted - 1);
  }
  $("page").value = String(current + 1);
});
$("page").addEventListener("keydown", (event) => {
  if (event.key === "Enter") {
    $("page").blur();
  }
});
$("sheets").addEventListener("change", () => call("goTo", parseInt($("sheets").value, 10)));
$("zoom-in").addEventListener("click", () => call("zoomIn"));
$("zoom-out").addEventListener("click", () => call("zoomOut"));
$("fit-width").addEventListener("click", () => call("fit", "width"));
$("fit-page").addEventListener("click", () => call("fit", "page"));
$("close").addEventListener("click", close);
$("save").addEventListener("click", () => save(false));

function dirty() {
  return !!(controller && typeof controller.dirty === "function" && controller.dirty());
}

// close asks first when there are changes that are not saved, and otherwise
// leaves
function close() {
  // Escape on a question is its way out
  if (!$("unsaved").hidden) {
    $("unsaved-keep").click();
    return;
  }
  if (!$("conflict").hidden) {
    $("conflict-cancel").click();
    return;
  }
  if (dirty()) {
    show("unsaved");
    $("unsaved-save").focus();
    return;
  }
  leave();
}

$("unsaved-save").addEventListener("click", async () => {
  show("");
  if (await save(false)) {
    leave();
  }
});
$("unsaved-discard").addEventListener("click", () => {
  show("");
  leave();
});
$("unsaved-keep").addEventListener("click", () => {
  show("");
  call("focus");
});

// leaving is set once the viewer has been told to go, so that it is not
// asked about what is unsaved a second time as its frame is emptied
let leaving = false;

// leave hands the frame back to the listing, which removes it; a viewer
// opened on its own goes back to where it was opened from
function leave() {
  leaving = true;
  if (window.parent !== window) {
    window.parent.postMessage({ type: "go-fs-viewer", action: "close" }, location.origin);
    return;
  }
  if (history.length > 1) {
    history.back();
  } else {
    window.close();
  }
}

// a page that is about to go away with changes unsaved, reloaded or closed
// with its tab, has the browser ask first
window.addEventListener("beforeunload", (event) => {
  if (!leaving && dirty()) {
    event.preventDefault();
  }
});

// the listing passes on Escape when it has the focus rather than the frame;
// nothing else that posts a message is listened to
window.addEventListener("message", (event) => {
  if (event.origin !== location.origin || event.source !== window.parent || window.parent === window) {
    return;
  }
  if (event.data && event.data.type === "go-fs-viewer" && event.data.action === "close-request") {
    close();
  }
});

document.addEventListener("keydown", (event) => {
  if (editing && (event.ctrlKey || event.metaKey) && !event.altKey && event.key.toLowerCase() === "s") {
    event.preventDefault();
    save(false);
    return;
  }
  if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey) {
    return;
  }
  // the editor types into a text area, which only keeps the keys when it is
  // there to be typed in, not when the file is only shown
  const typing = event.target instanceof HTMLInputElement || event.target instanceof HTMLSelectElement ||
    (event.target instanceof HTMLTextAreaElement && !event.target.closest(".read-only"));
  if (event.key === "Escape") {
    event.preventDefault();
    close();
    return;
  }
  if (typing) {
    return;
  }
  const stage = $("stage");
  const scroller = stage.querySelector(".scroller");
  // left and right page only where there is nothing to scroll sideways
  const sideways = scroller && scroller.scrollWidth > scroller.clientWidth + 1;
  switch (event.key) {
    case "ArrowLeft":
      if (!sideways && count > 1) {
        event.preventDefault();
        call("goTo", current - 1);
      }
      break;
    case "ArrowRight":
      if (!sideways && count > 1) {
        event.preventDefault();
        call("goTo", current + 1);
      }
      break;
    case "+":
    case "=":
      event.preventDefault();
      call("zoomIn");
      break;
    case "-":
    case "_":
      event.preventDefault();
      call("zoomOut");
      break;
    case "0":
      event.preventDefault();
      call("fit", "width");
      break;
  }
});

// --- loading, a password, a failure -------------------------------------------

function show(panel) {
  for (const id of ["busy", "password", "failed", "unsaved", "conflict"]) {
    $(id).hidden = id !== panel;
  }
  $("status").hidden = panel === "";
}

// Cancelled is thrown when a password was asked for and not given.
class Cancelled extends Error {}

function fail(err) {
  if (err instanceof Cancelled) {
    close();
    return;
  }
  console.error(err);
  $("reason").textContent = err instanceof Failure ? err.reason : "The viewer could not show this file.";
  $("detail").textContent = err instanceof Failure ? err.detail : String(err && err.message ? err.message : err);
  show("failed");
}

// askPassword asks for the password of a protected file, and resolves to it,
// or rejects when the person cancels; wrong says the last one did not open it.
function askPassword(wrong) {
  return new Promise((resolve, reject) => {
    const form = $("password");
    const input = $("password-input");
    const text = $("password-text");
    text.textContent = wrong
      ? "That password did not open the file. Try again."
      : "This file is protected with a password.";
    text.classList.toggle("bad", !!wrong);
    input.value = "";
    show("password");
    input.focus();
    const done = () => {
      form.removeEventListener("submit", submitted);
      $("password-cancel").removeEventListener("click", cancelled);
      show("busy");
    };
    const submitted = (event) => {
      event.preventDefault();
      done();
      resolve(input.value);
    };
    const cancelled = () => {
      done();
      reject(new Cancelled());
    };
    form.addEventListener("submit", submitted);
    $("password-cancel").addEventListener("click", cancelled);
  });
}

// fetchBytes reads the file the way a download does, which is what the
// session allows, saying how far it has got, and stops at limit when there
// is one.
async function fetchBytes(target, limit) {
  const res = await fetch(target, { credentials: "same-origin", cache: "no-store" });
  if (!res.ok) {
    switch (res.status) {
      case 401:
      case 403:
        throw new Failure("You may not read this file.");
      case 404:
        throw new Failure("The file is not there any more.");
      default:
        throw new Failure("The server could not send the file.", res.status + " " + res.statusText);
    }
  }
  const total = Number(res.headers.get("Content-Length")) || 0;
  const reader = res.body.getReader();
  const chunks = [];
  let received = 0;
  let truncated = false;
  ui.progress(0, total);
  for (;;) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }
    chunks.push(value);
    received += value.length;
    ui.progress(received, total);
    if (limit && received >= limit) {
      truncated = true;
      await reader.cancel();
      break;
    }
  }
  const bytes = new Uint8Array(limit ? Math.min(received, limit) : received);
  let at = 0;
  for (const chunk of chunks) {
    const part = chunk.subarray(0, Math.min(chunk.length, bytes.length - at));
    bytes.set(part, at);
    at += part.length;
  }
  return { bytes: bytes.buffer, truncated, lastModified: res.headers.get("Last-Modified") || "" };
}

// --- saving -------------------------------------------------------------------

// lastModified is the time of the file as it was read, or last saved, which a
// save is made on the condition of
let lastModified = "";
let saving = false;

// save writes what the backend has back to the file, and resolves to whether
// it did. force saves over a file someone else changed meanwhile, which is
// only done once the person has said so.
async function save(force) {
  if (!editing || saving || !controller || typeof controller.contents !== "function" || !plainUrl) {
    return false;
  }
  if (!force && !dirty()) {
    return true;
  }
  const { bytes, saved } = controller.contents();
  const headers = { "Content-Type": "application/octet-stream" };
  if (lastModified && !force) {
    headers["If-Unmodified-Since"] = lastModified;
  }
  saving = true;
  $("save").disabled = true;
  let res;
  try {
    res = await fetch(plainUrl, { method: "PUT", headers, body: bytes, credentials: "same-origin", cache: "no-store" });
  } catch (err) {
    ui.notice("The file could not be saved: the server could not be reached.", true);
    return false;
  } finally {
    saving = false;
    ui.changed(dirty());
  }
  if (res.status === 412) {
    return (await askOverwrite()) ? save(true) : false;
  }
  if (!res.ok) {
    const text = (await res.text().catch(() => "")).trim();
    ui.notice(saveFailure(res.status, text), true);
    return false;
  }
  lastModified = res.headers.get("Last-Modified") || lastModified;
  saved();
  ui.notice("Saved.", false);
  $("notice").dataset.saved = "1";
  if (window.parent !== window) {
    window.parent.postMessage({ type: "go-fs-viewer", action: "saved" }, location.origin);
  }
  return true;
}

function saveFailure(status, text) {
  switch (status) {
    case 401:
    case 403:
      return "The file could not be saved: you may not change it.";
    case 413:
      return "The file could not be saved: it is larger than this server accepts.";
    case 423:
      return "The file could not be saved: another program has locked it.";
  }
  return "The file could not be saved: the server answered " + status + (text && text.length < 200 ? " (" + text + ")." : ".");
}

// askOverwrite asks whether to save over a file that someone else changed
// since it was read, and resolves to the answer
function askOverwrite() {
  return new Promise((resolve) => {
    show("conflict");
    $("conflict-cancel").focus();
    const answer = (overwrite) => () => {
      $("conflict-overwrite").removeEventListener("click", yes);
      $("conflict-cancel").removeEventListener("click", no);
      show("");
      if (!overwrite) {
        call("focus");
      }
      resolve(overwrite);
    };
    const yes = answer(true);
    const no = answer(false);
    $("conflict-overwrite").addEventListener("click", yes);
    $("conflict-cancel").addEventListener("click", no);
  });
}

async function open() {
  if (!url || !kinds.includes(kind)) {
    throw new Failure("There is no file to show.");
  }
  show("busy");
  const backend = await import("./backends/" + kind + ".js");
  const read = await fetchBytes(url.pathname + url.search, kind === "text" ? textLimit : 0);
  lastModified = read.lastModified;
  $("progress").textContent = "Opening…";
  $("meter").parentElement.classList.add("unknown");
  const stage = $("stage");
  controller = await backend.open(stage, { name, bytes: read.bytes, truncated: read.truncated, editable: editing }, ui);
  show("");
  if (typeof controller.focus === "function") {
    controller.focus();
  } else {
    stage.focus({ preventScroll: true });
  }
}

// a backend that fails after it has opened, a page that does not render,
// reports through the console; what fails here is the file not opening
open().catch(fail);

window.addEventListener("pagehide", () => call("destroy"));
