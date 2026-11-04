// PDF, drawn by pdf.js: its viewer component scrolls the pages, with their
// text selectable and their links working. A script in the PDF never runs;
// its forms are shown as they are filled in, not to be filled.

import { Failure, pixels, scroller, stylesheet } from "./common.js";

const base = new URL("../pdfjs/", import.meta.url);

// the viewer translates its own few strings from files it would fetch; there
// is nothing of it to translate here
const l10n = {
  getLanguage: () => "en",
  getDirection: () => "ltr",
  get: async () => "",
  translate: async () => {},
  translateOnce: async () => {},
  destroy: async () => {},
  pause() {},
  resume() {},
};

export async function open(host, source, ui) {
  const lib = await import("../pdfjs/build/pdf.min.mjs");
  lib.GlobalWorkerOptions.workerSrc = new URL("build/pdf.worker.min.mjs", base).href;
  // the component reads the library from the global the library sets
  const parts = await import("../pdfjs/web/pdf_viewer.mjs");
  stylesheet(new URL("web/pdf_viewer.css", base));

  let cancelled = null;
  const task = lib.getDocument({
    data: source.bytes,
    cMapUrl: new URL("cmaps/", base).href,
    cMapPacked: true,
    standardFontDataUrl: new URL("standard_fonts/", base).href,
    wasmUrl: new URL("wasm/", base).href,
    iccUrl: new URL("iccs/", base).href,
    enableXfa: false,
  });
  task.onPassword = (update, reason) => {
    ui.askPassword(reason === lib.PasswordResponses.INCORRECT_PASSWORD).then(update, (err) => {
      cancelled = err;
      task.destroy();
    });
  };
  let doc;
  try {
    doc = await task.promise;
  } catch (err) {
    if (cancelled) {
      throw cancelled;
    }
    if (err && err.name === "InvalidPDFException") {
      throw new Failure("This is not a PDF the viewer can read.", err.message);
    }
    throw new Failure("The viewer could not open this PDF.", err && err.message);
  }

  const box = scroller(host, "pdf");
  const pages = document.createElement("div");
  pages.className = "pdfViewer";
  box.append(pages);
  const eventBus = new parts.EventBus();
  const linkService = new parts.PDFLinkService({
    eventBus,
    externalLinkTarget: parts.LinkTarget.BLANK,
    externalLinkRel: "noopener noreferrer nofollow",
  });
  const viewer = new parts.PDFViewer({
    container: box,
    viewer: pages,
    eventBus,
    linkService,
    l10n,
    removePageBorders: true,
    textLayerMode: 1,
    annotationMode: lib.AnnotationMode.ENABLE,
    annotationEditorMode: lib.AnnotationEditorType.DISABLE,
    imageResourcesPath: new URL("web/images/", base).href,
  });
  linkService.setViewer(viewer);

  // The fits are worked out here rather than left to pdf.js, which fills the
  // frame to its edges once the borders of its pages are gone: a page keeps
  // the gap around it every other backend leaves. They go by the first page,
  // as pdf.js does, at the size it is shown at, in CSS pixels.
  const first = (await doc.getPage(1)).getViewport({ scale: 96 / 72 });
  const gap = pixels("--page-gap");
  let mode = "width";
  const fitted = () => {
    const across = (box.clientWidth - 2 * gap) / first.width;
    const down = (box.clientHeight - 2 * gap) / first.height;
    return mode === "page" ? Math.min(across, down) : across;
  };
  const refit = () => {
    // a frame that is being closed has no size to fit
    if (mode && box.clientWidth > 0) {
      viewer.currentScale = Math.max(0.1, fitted());
      ui.fits(mode);
    }
  };
  eventBus.on("pagesinit", refit);
  eventBus.on("pagechanging", (event) => ui.page(event.pageNumber - 1));
  eventBus.on("scalechanging", (event) => ui.scale(event.scale));
  viewer.setDocument(doc);
  linkService.setDocument(doc, null);
  ui.pages(doc.numPages, "Page");

  // a fit stays a fit as the frame changes size
  const resized = new ResizeObserver(refit);
  resized.observe(box);

  return {
    goTo(index) {
      viewer.currentPageNumber = Math.max(1, Math.min(doc.numPages, index + 1));
    },
    zoomIn() {
      mode = "";
      ui.fits("");
      viewer.increaseScale();
    },
    zoomOut() {
      mode = "";
      ui.fits("");
      viewer.decreaseScale();
    },
    fit(wanted) {
      mode = wanted;
      refit();
    },
    destroy() {
      resized.disconnect();
      // ending the task ends the document and its worker
      task.destroy();
    },
  };
}
