// The bar that plays audio: the listing keeps it in a frame at the bottom of
// the window, and keeps it there as one browses, so that what plays goes on
// playing. It plays a queue, the files of audio of the folder a track was
// started in, in the order the listing showed them, one after the other.
//
// The listing tells it what to play with a message, {type: "go-fs-player",
// action: "play", queue: [{src, name, type}], index}, or to pause, and is told
// when the bar is ready and when it was closed. Nothing else that posts a
// message is listened to, and only a file of this server is played.

import { mediaKeys, videojs } from "./backends/common.js";

const $ = (id) => document.getElementById(id);

const message = "go-fs-player";

// the bar only ever runs in the frame of a page of this server
const parent = window.parent !== window ? window.parent : null;

function tell(action) {
  if (parent) {
    parent.postMessage({ type: message, action }, location.origin);
  }
}

// local is a path of the server the bar was served by, and nothing else
function local(src) {
  return typeof src === "string" && src.startsWith("/") && !src.startsWith("//") && !src.startsWith("/\\");
}

let queue = [];
let index = -1;
// failures counts the tracks that failed one after the other, so that a
// queue of nothing playable stops rather than going round
let failures = 0;
let player = null;

function where(text, bad) {
  $("where").textContent = text;
  $("where").classList.toggle("bad", !!bad);
}

function position() {
  return queue.length > 1 ? index + 1 + " / " + queue.length : "";
}

function buttons() {
  if (!player) {
    return;
  }
  const previous = player.getChild("ControlBar").getChild("PreviousTrack");
  const next = player.getChild("ControlBar").getChild("NextTrack");
  previous.enable();
  if (index >= queue.length - 1) {
    next.disable();
  } else {
    next.enable();
  }
}

function load(at) {
  if (at < 0 || at >= queue.length) {
    return;
  }
  index = at;
  const track = queue[index];
  $("name").textContent = track.name;
  $("name").title = track.name;
  document.title = track.name;
  where(position(), false);
  buttons();
  player.error(null);
  player.src(track.type ? { src: track.src, type: track.type } : track.src);
  player.play()?.catch(() => {});
  if ("mediaSession" in navigator) {
    try {
      navigator.mediaSession.metadata = new MediaMetadata({ title: track.name });
    } catch {
      // a browser without the metadata of a session plays all the same
    }
  }
}

// previous starts the track over unless it has only just started, and only
// then goes back one, as every player does
function previous() {
  if (player.currentTime() > 3 || index <= 0) {
    player.currentTime(0);
    player.play()?.catch(() => {});
    return;
  }
  failures = 0;
  load(index - 1);
}

function next() {
  if (index < queue.length - 1) {
    load(index + 1);
  }
}

function controls(library) {
  const Button = library.getComponent("Button");
  class TrackButton extends Button {
    constructor(owner, options) {
      super(owner, options);
      this.controlText(options.label);
      this.el().querySelector(".vjs-icon-placeholder").classList.add(options.icon);
    }

    buildCSSClass() {
      return "vjs-track-control " + super.buildCSSClass();
    }

    handleClick() {
      this.options_.act();
    }
  }
  class PreviousTrack extends TrackButton {
    constructor(owner, options) {
      super(owner, { ...options, label: "Previous", icon: "vjs-icon-previous-item", act: previous });
    }
  }
  class NextTrack extends TrackButton {
    constructor(owner, options) {
      super(owner, { ...options, label: "Next", icon: "vjs-icon-next-item", act: () => { failures = 0; next(); } });
    }
  }
  // the speed is a button that steps to the next rate, and from the last back
  // to the first: the menu of Video.js opens upwards, out of the frame the bar
  // is kept in, where it cannot be seen
  class RateButton extends Button {
    constructor(owner, options) {
      super(owner, options);
      this.label = document.createElement("span");
      this.label.className = "vjs-rate-value";
      this.label.setAttribute("aria-hidden", "true");
      this.el().appendChild(this.label);
      this.update();
      this.on(owner, "ratechange", this.update);
    }

    buildCSSClass() {
      return "vjs-rate-control " + super.buildCSSClass();
    }

    update() {
      const rate = this.player().playbackRate();
      this.label.textContent = rate + "×";
      this.controlText("Speed " + rate + "×, click for the next");
    }

    // the default rate too, which the next track of the queue starts at
    handleClick() {
      const rates = this.player().playbackRates();
      const rate = rates[(rates.indexOf(this.player().playbackRate()) + 1) % rates.length];
      this.player().defaultPlaybackRate(rate);
      this.player().playbackRate(rate);
    }
  }
  library.registerComponent("PreviousTrack", PreviousTrack);
  library.registerComponent("NextTrack", NextTrack);
  library.registerComponent("RateButton", RateButton);
}

async function start() {
  const library = await videojs();
  controls(library);
  player = library($("audio"), {
    controls: true,
    audioOnlyMode: true,
    preload: "auto",
    playbackRates: [0.75, 1, 1.25, 1.5, 2],
    userActions: { hotkeys: mediaKeys },
    controlBar: {
      children: [
        "PreviousTrack",
        "playToggle",
        "NextTrack",
        "currentTimeDisplay",
        "timeDivider",
        "durationDisplay",
        "progressControl",
        "RateButton",
        { name: "volumePanel", inline: true },
      ],
    },
  });
  player.on("ended", () => {
    failures = 0;
    next();
  });
  player.on("playing", () => {
    failures = 0;
    where(position(), false);
  });
  // a track the browser cannot play is passed over for the next
  player.on("error", () => {
    failures++;
    const error = player.error();
    const reason = error && error.code === 4 ? "The browser cannot play this file." : "This file could not be played.";
    if (failures >= queue.length || index >= queue.length - 1) {
      where(reason, true);
      return;
    }
    where(reason + " Playing the next one…", true);
    setTimeout(next, 1500);
  });
  if ("mediaSession" in navigator) {
    try {
      navigator.mediaSession.setActionHandler("previoustrack", previous);
      navigator.mediaSession.setActionHandler("nexttrack", next);
    } catch {
      // the keys of the keyboard still do what the browser makes of them
    }
  }

  window.addEventListener("message", (event) => {
    if (event.origin !== location.origin || !parent || event.source !== parent) {
      return;
    }
    const data = event.data;
    if (!data || data.type !== message) {
      return;
    }
    switch (data.action) {
      case "play": {
        const wanted = Array.isArray(data.queue)
          ? data.queue.filter((track) => track && local(track.src)).map((track) => ({
            src: track.src,
            name: String(track.name || track.src.split("/").pop()),
            type: typeof track.type === "string" ? track.type : "",
          }))
          : [];
        if (wanted.length === 0) {
          return;
        }
        const at = Math.max(0, Math.min(Number(data.index) || 0, wanted.length - 1));
        queue = wanted;
        failures = 0;
        load(at);
        break;
      }
      case "pause":
        player.pause();
        break;
    }
  });
  tell("ready");
}

$("close").addEventListener("click", () => {
  if (player) {
    player.pause();
  }
  tell("closed");
});

start().catch((err) => {
  console.error(err);
  where("The player could not be loaded.", true);
});
