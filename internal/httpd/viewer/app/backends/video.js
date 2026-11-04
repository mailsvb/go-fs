// A video, in Video.js, filling the stage. It is never read into the page:
// the browser streams it from the file, asking the server for the ranges it
// needs as it plays and seeks, and plays only what it can play itself, since
// nothing is converted. A video it cannot play fails as any file does, with
// the download offered instead.

import { Failure, mediaKeys, scroller, videojs } from "./common.js";

export async function open(host, source, ui) {
  const library = await videojs();
  const box = scroller(host, "video");
  const element = document.createElement("video");
  element.className = "video-js vjs-fill";
  element.setAttribute("playsinline", "");
  element.setAttribute("aria-label", source.name);
  box.append(element);

  const player = library(element, {
    controls: true,
    autoplay: true,
    preload: "auto",
    fill: true,
    playbackRates: [0.5, 0.75, 1, 1.25, 1.5, 2],
    userActions: { hotkeys: mediaKeys },
    controlBar: { pictureInPictureToggle: true, remainingTimeDisplay: false },
  });
  ui.fitting(false);

  // the stage is shown once the browser knows what it plays; until then the
  // viewer says it is opening, and a video it cannot play is a failure
  await new Promise((resolve, reject) => {
    const failed = () => {
      player.off("loadedmetadata", loaded);
      const error = player.error();
      player.dispose();
      box.remove();
      reject(new Failure(error && error.code === 4
        ? "The browser cannot play this video."
        : "The video could not be loaded.", error ? error.message : ""));
    };
    const loaded = () => {
      player.off("error", failed);
      resolve();
    };
    player.one("loadedmetadata", loaded);
    player.one("error", failed);
    player.src(source.type ? { src: source.src, type: source.type } : source.src);
  });

  // a play refused before the person touched the frame is left to them
  player.play()?.catch(() => {});

  return {
    focus() {
      player.el().focus({ preventScroll: true });
    },
    destroy() {
      player.dispose();
    },
  };
}
