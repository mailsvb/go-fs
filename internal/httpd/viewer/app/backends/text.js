// Text, in the editor Ace: read only when the file was opened to be viewed,
// editable when it was opened to be edited. Ace lays text out as text, never
// as markup, whatever it is, so that nothing in it can act on the page; it
// colours what it recognises by the name of the file.
//
// Saving is the page's: the backend says whether there is anything unsaved,
// and hands over the text as bytes in the encoding the file came in.

import { Failure, step } from "./common.js";

const base = new URL("../ace/", import.meta.url);

// fontSize is the size text is shown at, at a zoom of 100%
const fontSize = 13;

// load runs a script of Ace, which is not a module but sets a global
function load(src) {
  return new Promise((resolve, reject) => {
    const script = document.createElement("script");
    script.src = src;
    script.onload = resolve;
    script.onerror = () => reject(new Failure("The editor could not be loaded."));
    document.head.append(script);
  });
}

async function editorLibrary() {
  if (!window.ace) {
    await load(new URL("ace.js", base).href);
  }
  const ace = window.ace;
  // the modes, the themes and the search box are loaded as they are needed,
  // from beside ace.js
  ace.config.set("basePath", base.href);
  const modelist = await new Promise((resolve) => ace.config.loadModule("ace/ext/modelist", resolve));
  return { ace, modelist };
}

// decode reads UTF-8 unless the file says it is UTF-16, which is all the text
// that says nothing about itself is likely to be, and remembers which it was
// and whether it began with a byte order mark, so that it is saved the same
// way. strict refuses what is not valid in the encoding, which an editor
// would otherwise save back with the bytes it could not read replaced.
function decode(bytes, strict) {
  const head = new Uint8Array(bytes, 0, Math.min(3, bytes.byteLength));
  let encoding = "utf-8";
  let bom = false;
  if (head[0] === 0xff && head[1] === 0xfe) {
    encoding = "utf-16le";
    bom = true;
  } else if (head[0] === 0xfe && head[1] === 0xff) {
    encoding = "utf-16be";
    bom = true;
  } else if (head[0] === 0xef && head[1] === 0xbb && head[2] === 0xbf) {
    bom = true;
  }
  try {
    // the decoder drops the byte order mark itself
    return { text: new TextDecoder(encoding, { fatal: strict }).decode(bytes), encoding, bom };
  } catch {
    return null;
  }
}

// encode is the text as bytes in the encoding it was read in.
function encode(text, encoding, bom) {
  if (encoding === "utf-8") {
    const body = new TextEncoder().encode(text);
    if (!bom) {
      return body;
    }
    const out = new Uint8Array(body.length + 3);
    out.set([0xef, 0xbb, 0xbf]);
    out.set(body, 3);
    return out;
  }
  const little = encoding === "utf-16le";
  const out = new Uint8Array(2 + text.length * 2);
  const view = new DataView(out.buffer);
  view.setUint16(0, 0xfeff, little);
  for (let i = 0; i < text.length; i++) {
    view.setUint16(2 + 2 * i, text.charCodeAt(i), little);
  }
  return out;
}

export async function open(host, source, ui) {
  const { ace, modelist } = await editorLibrary();

  // a file is only edited when all of it is there and all of it could be
  // read; otherwise it is shown, and the page says why it cannot be edited
  let editable = !!source.editable;
  let decoded = null;
  if (editable && source.truncated) {
    editable = false;
    ui.notice("This file is too large to be edited here; only its beginning is shown.");
  } else if (editable) {
    decoded = decode(source.bytes, true);
    if (!decoded) {
      editable = false;
      ui.notice("This file is not text in UTF-8 or UTF-16, so it cannot be edited here.");
    }
  } else if (source.truncated) {
    ui.notice("Only the beginning of this file is shown. Download it to see all of it.");
  }
  if (!decoded) {
    decoded = decode(source.bytes, false);
  }

  const box = document.createElement("div");
  box.className = "scroller text" + (editable ? "" : " read-only");
  host.append(box);
  const editor = ace.edit(box, {
    readOnly: !editable,
    useWorker: false,
    showPrintMargin: false,
    highlightActiveLine: editable,
    highlightGutterLine: editable,
    // what the file ends its lines with is what it is saved with
    newLineMode: "auto",
    fontSize,
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
    scrollPastEnd: 0.5,
  });
  const session = editor.session;
  session.setMode(modelist.getModeForPath(source.name).mode);
  session.setValue(decoded.text);
  session.getUndoManager().reset();
  session.getUndoManager().markClean();
  editor.moveCursorTo(0, 0);
  editor.renderer.scrollToRow(0);

  // the theme follows the page between light and dark
  const dark = window.matchMedia("(prefers-color-scheme: dark)");
  const theme = () => editor.setTheme(dark.matches ? "ace/theme/tomorrow_night" : "ace/theme/tomorrow");
  dark.addEventListener("change", theme);
  theme();

  if (editable) {
    editor.commands.addCommand({
      name: "save",
      bindKey: { win: "Ctrl-S", mac: "Command-S" },
      exec: () => ui.save(),
    });
    editor.on("input", () => ui.changed(!session.getUndoManager().isClean()));
    ui.editing(true);
  }

  let zoom = 1;
  const apply = () => {
    editor.setFontSize(Math.round(fontSize * zoom * 10) / 10);
    ui.scale(zoom);
  };
  ui.fitting(false);
  apply();
  editor.focus();

  return {
    zoomIn() {
      zoom = step(zoom, 1);
      apply();
    },
    zoomOut() {
      zoom = step(zoom, -1);
      apply();
    },
    fit() {
      zoom = 1;
      apply();
    },
    focus() {
      editor.focus();
    },
    dirty() {
      return editable && !session.getUndoManager().isClean();
    },
    // contents is what saving writes: the text as it is now, in the
    // encoding the file came in, and a mark to call once it is written
    contents() {
      const undo = session.getUndoManager();
      // Ace runs on what is typed after into the change before, which keeps
      // its revision; a group of its own gives it one that is not this
      const revision = undo.startNewGroup();
      return {
        bytes: encode(session.getValue(), decoded.encoding, decoded.bom),
        saved() {
          // the text as it was when it was sent is what is clean: what was
          // typed while the save was under way is still unsaved
          undo.markClean(revision);
          ui.changed(!undo.isClean());
        },
      };
    },
    destroy() {
      dark.removeEventListener("change", theme);
      editor.destroy();
    },
  };
}
