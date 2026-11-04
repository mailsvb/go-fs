// What the backends share: how a file that cannot be shown is told, the
// steps of the zoom, the tokens of the page, and the links a document may
// open.

// Failure is what a backend throws for a file it cannot show, with what to
// tell the person looking at it.
export class Failure extends Error {
  constructor(reason, detail) {
    super(reason);
    this.reason = reason;
    this.detail = detail || "";
  }
}

const steps = [0.1, 0.25, 0.33, 0.5, 0.67, 0.75, 0.9, 1, 1.1, 1.25, 1.5, 1.75, 2, 2.5, 3, 4, 5, 8];

// step is the next scale of the zoom from scale, up for a positive dir.
export function step(scale, dir) {
  if (dir > 0) {
    return steps.find((s) => s > scale * 1.001) ?? steps[steps.length - 1];
  }
  return [...steps].reverse().find((s) => s < scale * 0.999) ?? steps[0];
}

// token is the value of a custom property of the page, which the libraries
// are handed so that what they draw looks like the rest.
export function token(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export function pixels(name) {
  return parseFloat(token(name)) || 0;
}

// scroller is the element a backend draws into, filling the stage.
export function scroller(host, kind) {
  const box = document.createElement("div");
  box.className = "scroller " + kind;
  host.append(box);
  return box;
}

// openLink opens a link of a document in a tab of its own, and only one that
// leads to the web or writes a mail: a document is not to reach anything of
// this origin, nor run script through a javascript: link.
export function openLink(href) {
  let target;
  try {
    target = new URL(href);
  } catch {
    return;
  }
  if (["http:", "https:", "mailto:"].includes(target.protocol)) {
    window.open(target.href, "_blank", "noopener,noreferrer");
  }
}

// stylesheet adds the style of a library to the page, once.
export function stylesheet(href) {
  const url = String(href);
  if (document.querySelector('link[rel="stylesheet"][href="' + CSS.escape(url) + '"]')) {
    return;
  }
  const link = document.createElement("link");
  link.rel = "stylesheet";
  link.href = url;
  document.head.append(link);
}
