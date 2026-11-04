// Excel, drawn by the viewer of Office Open XML on a canvas that fills the
// stage and scrolls its cells itself; the sheets are chosen in the bar.

import { failure, loadOptions, protectedFile } from "./office.js";
import { scroller } from "./common.js";

export function open(host, source, ui) {
  return openSheet(host, source, ui, null);
}

// openSheet opens a workbook, or delimited text as a sheet of one when
// format says which, csv or tsv.
export async function openSheet(host, source, ui, format) {
  const lib = await import("../ooxml/xlsx.mjs");
  let password;
  let wrong = false;
  for (;;) {
    const box = scroller(host, "grid");
    const canvas = document.createElement("canvas");
    box.append(canvas);
    const viewer = new lib.XlsxSheetViewer(canvas, {
      ...loadOptions(password),
      resizable: true,
      showScrollbars: true,
      onScaleChange: (scale) => ui.scale(scale),
      onSheetChange: (index) => ui.sheets(viewer.sheetNames, index),
    });
    try {
      await viewer.load(source.bytes.slice(0), format ? { format } : undefined);
    } catch (err) {
      viewer.destroy();
      box.remove();
      if (!format && protectedFile(err)) {
        password = await ui.askPassword(wrong || err.code === "invalid-password");
        wrong = true;
        continue;
      }
      throw failure(err);
    }
    ui.sheets(viewer.sheetNames, viewer.sheetIndex);
    ui.scale(viewer.getScale());
    ui.fits("");
    return {
      goTo(index) {
        viewer.goToSheet(index);
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
