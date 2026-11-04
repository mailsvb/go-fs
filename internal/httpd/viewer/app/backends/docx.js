// Word, drawn by the viewer of Office Open XML.

import { openScrolling } from "./office.js";

export function open(host, source, ui) {
  return openScrolling(host, source, ui, {
    load: () => import("../ooxml/docx.mjs"),
    make: (lib, box, options) => new lib.DocxScrollViewer(box, options),
    visible: "onVisiblePageChange",
    label: "Page",
    count: (viewer) => viewer.pageCount,
    goTo: (viewer, index) => viewer.scrollToPage(index),
  });
}
