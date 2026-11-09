// The bar that plays audio: the listing keeps it in a frame at the bottom of
// the window, and keeps it there as one browses, so that what plays goes on
// playing. It plays a queue, the files of audio of the folder a track was
// started in, in the order the listing showed them, one after the other.
//
// The listing tells it what to play with a message, {type: "go-fs-player",
// action: "play", queue: [{src, name, type}], index, sonos}, or to pause, and
// is told when the bar is ready and when it was closed. Nothing else that
// posts a message is listened to, and only a file of this server is played.
//
// Where the listing offers Play on Sonos (sonos is true), the bar has a
// button that asks the listing for its dialog, {action: "sonos", queue,
// index}: the server then plays the queue on the speakers chosen, by itself,
// and the listing hands the bar what it plays, {action: "remote", session}.
// Until that ends the bar is its remote control: it stops playing itself,
// asks the server what the speakers do every second, and tells the server to
// pause, to play another file, to change the volume, or to stop.

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
// sonosOffered says the listing offers Play on Sonos, which the bar then
// has a button for
let sonosOffered = false;
// failures counts the tracks that failed one after the other, so that a
// queue of nothing playable stops rather than going round
let failures = 0;
let player = null;
// session is what speakers play, while the bar is their remote control
let session = null;
let asking = null;

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
        // playing here takes the bar back from the speakers, which play on
        leaveRemote();
        sonosOffered = data.sonos === true;
        $("sonos").hidden = !sonosOffered;
        queue = wanted;
        failures = 0;
        load(at);
        break;
      }
      case "pause":
        if (!session) {
          player.pause();
        }
        break;
      case "remote": {
        const view = sessionOf(data);
        if (view) {
          enterRemote(view);
        }
        break;
      }
    }
  });
  tell("ready");
}

$("close").addEventListener("click", () => {
  if (player) {
    player.pause();
  }
  // the speakers play on; the listing shows them again when it is opened
  leaveRemote();
  tell("closed");
});

$("sonos").addEventListener("click", () => {
  if (parent && queue.length > 0) {
    parent.postMessage({ type: message, action: "sonos", queue, index }, location.origin);
  }
});

// --- the remote control of speakers -------------------------------------

// sessionLink is where the bar asks the server about its session: the
// folder of the session's files, which the account may read
function sessionLink(op, extra) {
  const folder = session.folder.split("/").map(encodeURIComponent).join("/");
  return folder + "?go-fs=sonos&sonos=" + op + (extra || "");
}

async function ask(op, body) {
  const options = { method: body ? "POST" : "GET", credentials: "same-origin", headers: { Accept: "application/json" } };
  if (body) {
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  const res = await fetch(sessionLink(op, body ? "" : "&id=" + encodeURIComponent(session.id)), options);
  if (!res.ok) {
    const text = (await res.text()).trim();
    const err = new Error(text || res.statusText);
    err.status = res.status;
    throw err;
  }
  return res.status === 204 ? null : res.json();
}

function clock(seconds) {
  seconds = Math.max(0, Math.floor(seconds || 0));
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = String(seconds % 60).padStart(2, "0");
  return h > 0 ? h + ":" + String(m).padStart(2, "0") + ":" + s : m + ":" + s;
}

let sliding = false;

function showSession(view) {
  session = view;
  const name = view.title || view.name;
  $("name").textContent = name;
  $("name").title = view.name;
  document.title = name;
  const at = view.files > 1 ? " · " + (view.index + 1) + " / " + view.files : "";
  if (view.state === "playing" || view.state === "paused") {
    where("On " + view.room + at, false);
  } else {
    where(view.message || "Ended.", view.state === "failed");
  }
  const paused = view.state === "paused" || view.transport === "PAUSED_PLAYBACK";
  $("r-toggle").title = paused ? "Play" : "Pause";
  $("r-toggle").querySelector("use").setAttribute("href", paused ? "#i-play" : "#i-pause");
  $("r-previous").disabled = false;
  $("r-next").disabled = view.index >= view.files - 1;
  $("r-clock").textContent = view.duration > 0 ? clock(view.position) + " / " + clock(view.duration) : clock(view.position);
  $("r-done").style.width = view.duration > 0 ? Math.min(100, (100 * view.position) / view.duration) + "%" : "0";
  if (!sliding) {
    $("r-volume").value = view.volume;
  }
}

function enterRemote(view) {
  if (player) {
    player.pause();
  }
  $("controls").hidden = true;
  $("sonos").hidden = true;
  $("remote").hidden = false;
  $("note").setAttribute("href", "#i-speaker");
  $("remote").querySelectorAll("button, input").forEach((el) => { el.disabled = false; });
  showSession(view);
  follow();
}

// leaveRemote has the bar play by itself again, what it played before
function leaveRemote() {
  clearTimeout(asking);
  asking = null;
  if (!session) {
    return;
  }
  session = null;
  $("remote").hidden = true;
  $("controls").hidden = false;
  $("sonos").hidden = !sonosOffered;
  $("note").setAttribute("href", "#i-music");
  if (queue[index]) {
    $("name").textContent = queue[index].name;
    document.title = queue[index].name;
  }
}

// ended shows how the session ended for a moment, then has the bar play by
// itself again, or closes it where it played nothing itself
function ended(view, text) {
  clearTimeout(asking);
  asking = null;
  if (view) {
    showSession(view);
  }
  if (text) {
    where(text, false);
  }
  $("remote").querySelectorAll("button, input").forEach((el) => { el.disabled = true; });
  setTimeout(() => {
    if (session && (!view || session.id === view.id) && $("r-stop").disabled) {
      leaveRemote();
      if (queue.length === 0) {
        tell("closed");
        return;
      }
      where(text || (view && view.message) || "", false);
    }
  }, 4000);
}

// follow asks what the speakers do, every second while the bar is seen
function follow() {
  clearTimeout(asking);
  if (!session) {
    return;
  }
  const id = session.id;
  if (document.visibilityState !== "visible") {
    asking = setTimeout(follow, 1000);
    return;
  }
  ask("session").then((view) => {
    if (!session || session.id !== id) {
      return;
    }
    if (view.state === "playing" || view.state === "paused") {
      showSession(view);
      asking = setTimeout(follow, 1000);
    } else {
      ended(view);
    }
  }).catch((err) => {
    if (!session || session.id !== id) {
      return;
    }
    if (err.status === 404) {
      ended(null, "The speakers stopped playing.");
      return;
    }
    where(err.message, true);
    asking = setTimeout(follow, 3000);
  });
}

function command(op, body) {
  if (!session) {
    return Promise.resolve();
  }
  return ask(op, body).then((view) => {
    if (view && view.id) {
      if (view.state === "playing" || view.state === "paused") {
        showSession(view);
      } else {
        ended(view);
        return;
      }
    }
    follow();
  }).catch((err) => where(err.message, true));
}

$("r-toggle").addEventListener("click", () => {
  const paused = session && (session.state === "paused" || session.transport === "PAUSED_PLAYBACK");
  command(paused ? "resume" : "pause", { group: session.group });
});
$("r-previous").addEventListener("click", () => {
  // as in the bar, back to the start of the file unless it only just began
  const to = session.position > 3 || session.index === 0 ? session.index : session.index - 1;
  command("skip", { session: session.id, index: to });
});
$("r-next").addEventListener("click", () => {
  command("skip", { session: session.id, index: session.index + 1 });
});
$("r-stop").addEventListener("click", () => {
  command("end", { session: session.id });
});
$("r-volume").addEventListener("input", () => {
  sliding = true;
});
$("r-volume").addEventListener("change", () => {
  sliding = false;
  command("volume", { group: session.group, volume: parseInt($("r-volume").value, 10) || 0 });
});
document.addEventListener("visibilitychange", () => {
  if (session && document.visibilityState === "visible") {
    follow();
  }
});

// a session as the listing hands it on, if it is one
function sessionOf(data) {
  const view = data && data.session;
  if (!view || typeof view.id !== "string" || typeof view.folder !== "string" || !local(view.folder)) {
    return null;
  }
  return view;
}

start().catch((err) => {
  console.error(err);
  where("The player could not be loaded.", true);
});
