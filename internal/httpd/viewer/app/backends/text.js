// Text, as it is, on a page of its own: never rendered as markup, whatever it
// is, so that nothing in it can act on the page.

import { scroller, step } from "./common.js";

// decode reads UTF-8 unless the file says it is UTF-16, which is all the
// text that says nothing about itself is likely to be
function decode(bytes) {
  const head = new Uint8Array(bytes, 0, Math.min(2, bytes.byteLength));
  let encoding = "utf-8";
  if (head[0] === 0xff && head[1] === 0xfe) {
    encoding = "utf-16le";
  } else if (head[0] === 0xfe && head[1] === 0xff) {
    encoding = "utf-16be";
  }
  return new TextDecoder(encoding).decode(bytes);
}

export async function open(host, source, ui) {
  const box = scroller(host, "text");
  const paper = document.createElement("div");
  paper.className = "sheet-of-paper";
  const pre = document.createElement("pre");
  pre.textContent = decode(source.bytes);
  paper.append(pre);
  box.append(paper);
  if (source.truncated) {
    const cut = document.createElement("p");
    cut.className = "cut";
    cut.textContent = "Only the beginning of this file is shown. Download it to see all of it.";
    box.append(cut);
  }

  let zoom = 1;
  const apply = () => {
    box.style.setProperty("--zoom", String(zoom));
    ui.scale(zoom);
  };
  ui.fitting(false);
  apply();
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
  };
}
