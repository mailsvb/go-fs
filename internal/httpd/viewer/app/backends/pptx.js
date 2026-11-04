// PowerPoint, drawn by the viewer of Office Open XML: the slides scroll as
// the pages of a document do.

import { openScrolling } from "./office.js";

export function open(host, source, ui) {
  return openScrolling(host, source, ui, {
    load: () => import("../ooxml/pptx.mjs"),
    make: (lib, box, options) => new lib.PptxScrollViewer(box, options),
    visible: "onVisibleSlideChange",
    label: "Slide",
    count: (viewer) => viewer.slideCount,
    goTo: (viewer, index) => viewer.scrollToSlide(index),
  });
}
