// What the Word and PowerPoint backends share: the viewer of Office Open XML
// scrolls the pages or the slides one under the other, on the backdrop and
// with the shadow of the page every other backend draws.

import { Failure, openLink, pixels, scroller, token } from "./common.js";

// failure says why a file of Office did not open, in the words of the person
// who tried to open it.
export function failure(err) {
  switch (err && err.code) {
    case "legacy-binary-format":
      return new Failure("This is an older Office file, .doc, .xls or .ppt, which the viewer cannot show.");
    case "not-ooxml":
      return new Failure("This file is not a document the viewer can read.", err.message);
    case "unsupported-encryption":
      return new Failure("This file is protected in a way the viewer cannot open.");
  }
  if (err && err.name === "OoxmlResourceLimitError") {
    return new Failure("This file is too large to show in the browser.", err.message);
  }
  return new Failure("The viewer could not open this file.", err && err.message);
}

// protectedFile reports an error that asks for a password, and whether the
// one given was wrong.
export function protectedFile(err) {
  const code = err && err.code;
  return code === "encrypted" || code === "invalid-password";
}

// loadOptions are what every viewer of the library is given: nothing from
// the web, Google's fonts included, and links that only open the web.
export function loadOptions(password) {
  return {
    useGoogleFonts: false,
    password,
    enableHyperlinks: true,
    onHyperlinkClick(target) {
      if (target.kind === "external") {
        openLink(target.url);
      }
    },
  };
}

// openScrolling opens a document whose pages scroll: kind says which viewer
// of the library, what it calls a page, and how to go to one.
export async function openScrolling(host, source, ui, kind) {
  const lib = await kind.load();
  const gap = pixels("--page-gap");
  let password;
  let wrong = false;
  for (;;) {
    const box = scroller(host, "office");
    const viewer = kind.make(lib, box, {
      ...loadOptions(password),
      background: token("--backdrop"),
      pageShadow: token("--page-shadow"),
      gap,
      paddingTop: gap,
      paddingBottom: gap,
      paddingLeft: gap,
      paddingRight: gap,
      refitOnResize: true,
      onScaleChange: (scale) => ui.scale(scale),
      [kind.visible]: (top, total) => {
        ui.pages(total, kind.label);
        ui.page(top);
      },
    });
    try {
      // the library may hand the buffer to its worker, which would leave
      // nothing to try a password with
      await viewer.load(source.bytes.slice(0));
    } catch (err) {
      viewer.destroy();
      box.remove();
      if (protectedFile(err)) {
        password = await ui.askPassword(wrong || err.code === "invalid-password");
        wrong = true;
        continue;
      }
      throw failure(err);
    }
    viewer.fitWidth();
    ui.pages(kind.count(viewer), kind.label);
    ui.scale(viewer.getScale());
    ui.fits("width");
    return {
      goTo(index) {
        kind.goTo(viewer, Math.max(0, Math.min(kind.count(viewer) - 1, index)));
      },
      zoomIn() {
        viewer.zoomIn();
        ui.fits("");
      },
      zoomOut() {
        viewer.zoomOut();
        ui.fits("");
      },
      fit(mode) {
        if (mode === "page") {
          viewer.fitPage();
        } else {
          viewer.fitWidth();
        }
        ui.fits(mode);
      },
      destroy() {
        viewer.destroy();
      },
    };
  }
}
