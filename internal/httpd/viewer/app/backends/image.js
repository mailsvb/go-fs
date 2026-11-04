// An image, on the backdrop, fitted to the stage unless it is smaller. Only
// the types a browser shows as a picture and nothing else: an SVG is a page
// that can run script, so it is never one of them.

import { Failure, pixels, scroller, step } from "./common.js";

const types = {
  png: "image/png",
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  gif: "image/gif",
  webp: "image/webp",
  avif: "image/avif",
  bmp: "image/bmp",
};

export async function open(host, source, ui) {
  const extension = source.name.split(".").pop().toLowerCase();
  const href = URL.createObjectURL(new Blob([source.bytes], { type: types[extension] || "application/octet-stream" }));
  const img = new Image();
  img.alt = source.name;
  img.src = href;
  try {
    await img.decode();
  } catch {
    URL.revokeObjectURL(href);
    throw new Failure("The browser cannot show this image.");
  }
  const box = scroller(host, "image");
  box.append(img);

  const gap = pixels("--page-gap");
  const fitted = (mode) => {
    const across = (box.clientWidth - 2 * gap) / img.naturalWidth;
    const down = (box.clientHeight - 2 * gap) / img.naturalHeight;
    return Math.max(0.01, mode === "width" ? across : Math.min(across, down));
  };
  // the fit it opens with never makes a picture larger than it is; one asked
  // for does
  let mode = "page";
  let capped = true;
  let scale = Math.min(1, fitted("page"));
  const apply = () => {
    img.style.width = Math.round(img.naturalWidth * scale) + "px";
    img.style.height = Math.round(img.naturalHeight * scale) + "px";
    ui.scale(scale);
    ui.fits(mode);
  };
  apply();

  // a fit stays a fit as the frame changes size
  const resized = new ResizeObserver(() => {
    if (mode) {
      scale = capped ? Math.min(1, fitted(mode)) : fitted(mode);
      apply();
    }
  });
  resized.observe(box);

  return {
    zoomIn() {
      scale = step(scale, 1);
      mode = "";
      apply();
    },
    zoomOut() {
      scale = step(scale, -1);
      mode = "";
      apply();
    },
    fit(wanted) {
      mode = wanted;
      capped = false;
      scale = fitted(wanted);
      apply();
    },
    destroy() {
      resized.disconnect();
      URL.revokeObjectURL(href);
    },
  };
}
