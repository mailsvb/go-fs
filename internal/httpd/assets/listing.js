// The page is complete without this script: the listing is rendered by the
// server and the sort headers are ordinary links. What is added here is the
// part a link cannot do — sorting without a round trip, filtering, the options
// menu of a row, and the requests that create, rename, remove and upload.
(function () {
  "use strict";

  // #/admin is the address the admin interface is remembered by, but a
  // fragment never reaches the server, so the page turns it into the marker
  // the server answers under. Typing it into the address bar of a listing
  // that is already open changes only the fragment, which is what the
  // second hook is for.
  function toAdmin() {
    if (window.location.hash === "#/admin") {
      window.location.replace(window.location.pathname + "?go-fs=admin");
      return true;
    }
    return false;
  }
  window.addEventListener("hashchange", toAdmin);
  if (toAdmin()) {
    return;
  }

  // The menu in the corner of the header is a details element and opens and
  // closes on its own; what is added is closing it on a click beside it and
  // on Escape, as a menu is expected to.
  (function () {
    var nav = document.querySelector("details.nav");
    if (!nav) {
      return;
    }
    document.addEventListener("click", function (event) {
      if (nav.open && !nav.contains(event.target)) {
        nav.open = false;
      }
    }, true);
    nav.addEventListener("keydown", function (event) {
      if (event.key === "Escape" && nav.open) {
        event.preventDefault();
        nav.open = false;
        nav.querySelector("summary").focus();
      }
    });
  })();

  // The headings of the table stick just below the header, so they need its
  // height, which changes as the header wraps. On a phone the path is one line
  // that scrolls, and it is scrolled to its end, to the folder the page is in.
  (function () {
    var header = document.querySelector("body > header");
    if (!header) {
      return;
    }
    var crumbs = header.querySelector(".crumbs");
    function toEnd() {
      if (crumbs) {
        crumbs.scrollLeft = crumbs.scrollWidth;
      }
    }
    toEnd();
    if (!window.ResizeObserver) {
      return;
    }
    new ResizeObserver(function () {
      document.documentElement.style.setProperty("--header-height", header.getBoundingClientRect().height + "px");
      toEnd();
    }).observe(header);
  })();

  var table = document.getElementById("listing");
  if (!table) {
    return;
  }
  var body = table.tBodies[0];
  // folder is the path this page lists, always with a trailing slash and
  // already escaped: every request below appends one escaped segment to it.
  var folder = table.dataset.folder;
  // remote is set on the listing of a stored server: the link of this folder
  // of it, ending in the escaped path an escaped name is appended to, while
  // folder stays the root of the served tree the transfers are asked after
  // under. baseQuery is the part of the query its links keep before the order.
  var remote = table.dataset.remote || "";
  var baseQuery = table.dataset.query || "";
  // shown is this folder as the dialogs name it
  var shown = table.dataset.path || decodeURI(folder);
  var banner = document.getElementById("banner");
  // maxChunkSize is the largest piece an upload is split into; 0 means
  // chunked upload is off and a large file is sent as one request, as before.
  var maxChunkSize = parseInt(table.dataset.maxChunkSize, 10) || 0;

  function rows() {
    return Array.prototype.slice.call(body.rows).filter(function (row) {
      return row.dataset.name !== undefined;
    });
  }

  function segment(name) {
    return (remote || folder) + encodeURIComponent(name);
  }

  var bannerText = banner.querySelector(".text");
  // A report of something done goes away by itself after a while; a problem
  // stays until it is dismissed, so that it is not missed.
  var bannerTimer = null;

  function say(message, good) {
    window.clearTimeout(bannerTimer);
    bannerText.textContent = message;
    banner.classList.toggle("good", good === true);
    banner.hidden = false;
    if (good === true) {
      bannerTimer = window.setTimeout(clear, 15000);
    }
  }

  function clear() {
    window.clearTimeout(bannerTimer);
    banner.hidden = true;
  }

  banner.querySelector(".close").addEventListener("click", clear);

  // A message that has to survive the reload after a fetch is kept for the
  // next page of this tab. Storage may be off; the reload then says nothing.
  var carried = "go-fs-listing-banner";
  try {
    var kept = window.sessionStorage.getItem(carried);
    if (kept) {
      window.sessionStorage.removeItem(carried);
      say(kept, true);
    }
  } catch (ignored) {
    // nothing carried
  }

  function sayAfterReload(message) {
    try {
      window.sessionStorage.setItem(carried, message);
    } catch (ignored) {
      // the reload shows the new file, which says it as well
    }
    reload();
  }

  // reload shows the folder again as it is now. While the bar below plays,
  // the page is not left: the folder is shown in its shell instead, so that
  // the music goes on.
  function reload() {
    if (audio && audio.keepsPlaying()) {
      audio.enterShell(window.location.href, true);
      return;
    }
    window.location.reload();
  }

  // A refused write is answered with a bare status, so what it meant depends on
  // what was asked for: the same 404 is a folder with something still in it,
  // a name already taken, or a file somebody else removed first.
  var refusals = {
    create: {
      404: "That name is taken, or the folder above it is gone.",
      405: "There is already something with that name."
    },
    rename: {
      404: "The file is gone, or the name you typed is not allowed here.",
      412: "There is already something with that name."
    },
    file: { 404: "That file is already gone." },
    folder: { 404: "The folder still has something in it, or it is already gone." },
    upload: {
      403: "You may not upload here, or there is already a file with that name and you may not replace it.",
      404: "There is already a file with that name.",
      416: "This upload lost sync with the server — try uploading it again."
    },
    fetch: {
      403: "Only an account that is logged in and may create files here can fetch into this folder."
    },
    share: {
      403: "Only a signed-in account that may read this file can share it.",
      404: "That file is gone."
    },
    send: {
      403: "Only an account that is logged in and may read this file can send it.",
      404: "That file is gone."
    },
    browse: {
      403: "You may not list that folder.",
      404: "That folder is gone."
    },
    "remote-fetch": {
      403: "Only an account that is logged in and may create files in that folder can fetch into it.",
      404: "That server is no longer stored."
    },
    mp3tags: {
      403: "You may not change this file here.",
      404: "That file is gone, or it is not an MP3 file.",
      409: "The volume change is larger than this file allows.",
      412: "The file was changed since the editor read it \u2014 open it again.",
      415: "The cover must be a JPEG or PNG image of at most 10 MiB.",
      422: "This file holds no MP3 audio the editor can measure or change."
    },
    "remote-measure": {
      403: "Only an account that is logged in may fetch from this server, and only where it allows downloads.",
      404: "That file or folder is gone, or the server is no longer stored."
    }
  };

  function reason(what, status, text) {
    // a session can now run out while the page is open, which is a different
    // thing from never having been allowed
    if (status === 401) {
      return "Your session has ended \u2014 reload the page to log in again.";
    }
    var known = refusals[what] || {};
    if (known[status]) {
      return known[status];
    }
    if (status === 403) {
      return "You are not allowed to do that here.";
    }
    if (status === 409) {
      return "The folder above it does not exist.";
    }
    if (status === 413) {
      return text || "The file is larger than this server accepts.";
    }
    return text || ("The server answered " + status + ".");
  }

  function send(what, method, url, headers) {
    return fetch(url, {
      method: method,
      headers: headers || {},
      credentials: "same-origin"
    }).then(function (res) {
      if (res.ok) {
        return null;
      }
      return res.text().then(function (text) {
        throw new Error(reason(what, res.status, text.trim()));
      });
    });
  }

  function done() {
    reload();
  }

  function failed(err) {
    say(err.message);
  }

  // --- sorting ---------------------------------------------------------
  //
  // The default order — folders first, then files, both by name — is the one
  // the server rendered, so the header cycles back to it after descending
  // rather than leaving no way to return to it.

  var order = { key: table.dataset.sort || "", dir: table.dataset.dir || "asc" };
  // the columns the server rendered, so that a remembered order naming one it
  // no longer has is thrown away rather than applied to nothing
  var columns = Array.prototype.map.call(table.querySelectorAll("th[data-key]"), function (cell) {
    return cell.dataset.key;
  });
  var remembered = "go-fs.listing.order";

  function value(row, key) {
    if (key === "size") {
      return Number(row.dataset.size);
    }
    if (key === "date") {
      return Number(row.dataset.time);
    }
    if (key === "type") {
      return row.dataset.kind.toLowerCase();
    }
    return row.dataset.name.toLowerCase();
  }

  function compare(a, b, key, sign) {
    var x = value(a, key);
    var y = value(b, key);
    if (x < y) {
      return -sign;
    }
    if (x > y) {
      return sign;
    }
    // a stable tie break, so two files of the same size never swap places
    return a.dataset.name.toLowerCase() < b.dataset.name.toLowerCase() ? -1 : 1;
  }

  function arrange() {
    var list = rows();
    var sign = order.dir === "desc" ? -1 : 1;
    if (order.key === "") {
      list.sort(function (a, b) {
        var group = Number(b.dataset.dir) - Number(a.dataset.dir);
        return group !== 0 ? group : compare(a, b, "name", 1);
      });
    } else {
      var key = order.key;
      list.sort(function (a, b) {
        return compare(a, b, key, sign);
      });
    }
    list.forEach(function (row) {
      body.appendChild(row);
    });

    Array.prototype.forEach.call(table.tHead.rows[0].cells, function (cell) {
      var key = cell.dataset.key;
      if (key === undefined) {
        return;
      }
      if (key === order.key && order.key !== "") {
        cell.setAttribute("aria-sort", order.dir === "desc" ? "descending" : "ascending");
        cell.querySelector(".caret").textContent = order.dir === "desc" ? "▼" : "▲";
      } else {
        cell.removeAttribute("aria-sort");
        cell.querySelector(".caret").textContent = "";
      }
    });
  }

  // next is the state a click on a column moves to: ascending, descending, then
  // no order at all, which is the grouped default the page was rendered in.
  function next(key) {
    if (order.key !== key) {
      return { key: key, dir: "asc" };
    }
    if (order.dir === "asc") {
      return { key: key, dir: "desc" };
    }
    return { key: "", dir: "asc" };
  }

  function retarget() {
    Array.prototype.forEach.call(table.querySelectorAll("th[data-key] a"), function (link) {
      var state = next(link.parentNode.dataset.key);
      link.setAttribute("href", query(state));
    });
  }

  function query(state) {
    if (state.key === "") {
      return "?" + baseQuery;
    }
    return "?" + (baseQuery ? baseQuery + "&" : "") + "sort=" + state.key + "&dir=" + state.dir;
  }

  // show writes the order into the address bar, so that a reload, a copied link
  // and what is on screen all say the same thing.
  function show() {
    var url = window.location.pathname;
    if (order.key !== "" || baseQuery) {
      url += query(order);
    }
    window.history.replaceState(null, "", url);
  }

  // The order is remembered for the server rather than for one folder: somebody
  // who sorted by size wants the folder they open next sorted by size too.
  // Storage can be switched off or full, which costs the preference and nothing
  // else, so every use of it is allowed to fail quietly.
  function keep(state) {
    try {
      if (state.key === "") {
        window.localStorage.removeItem(remembered);
      } else {
        window.localStorage.setItem(remembered, state.key + ":" + state.dir);
      }
    } catch (ignored) {
      return;
    }
  }

  function recall() {
    var stored = null;
    try {
      stored = window.localStorage.getItem(remembered);
    } catch (ignored) {
      return null;
    }
    if (!stored) {
      return null;
    }
    var parts = stored.split(":");
    if (columns.indexOf(parts[0]) === -1 || (parts[1] !== "asc" && parts[1] !== "desc")) {
      // written by an older version, or by hand
      keep({ key: "" });
      return null;
    }
    return { key: parts[0], dir: parts[1] };
  }

  Array.prototype.forEach.call(table.querySelectorAll("th[data-key] a"), function (link) {
    link.addEventListener("click", function (event) {
      event.preventDefault();
      order = next(link.parentNode.dataset.key);
      arrange();
      retarget();
      keep(order);
      show();
    });
  });

  // A query string is somebody asking for an order outright — a link they were
  // sent, or a reload — so it wins and becomes the preference. With none, the
  // preference is what decides, which is how an order survives walking into
  // another folder: the links between folders are relative and carry nothing.
  if (new URLSearchParams(window.location.search).has("sort")) {
    keep(order);
  } else {
    var restored = recall();
    if (restored) {
      order = restored;
      arrange();
      retarget();
      show();
    }
  }

  // --- filtering -------------------------------------------------------

  var filter = document.getElementById("filter");
  if (filter) {
    filter.addEventListener("input", function () {
      var needle = filter.value.trim().toLowerCase();
      rows().forEach(function (row) {
        row.classList.toggle("gone",
          needle !== "" && row.dataset.name.toLowerCase().indexOf(needle) === -1);
      });
    });
  }

  // --- dialogs ---------------------------------------------------------

  var asking = document.getElementById("prompt");
  var checking = document.getElementById("confirm");
  var onAccept = null;
  var onAgree = null;

  function ask(title, detail, current, label, then) {
    asking.querySelector("h2").textContent = title;
    asking.querySelector("p").textContent = detail;
    asking.querySelector(".go").textContent = label;
    var field = asking.querySelector("input");
    field.value = current;
    onAccept = function () {
      var typed = field.value.trim();
      if (typed === "" || typed === current) {
        return;
      }
      then(typed);
    };
    asking.showModal();
    field.focus();
    var stem = current.lastIndexOf(".");
    field.setSelectionRange(0, stem > 0 ? stem : current.length);
  }

  // The submit event rather than close: a dialog closed by its own form does
  // not reliably report a close, and submit says which button was pressed
  // while the answer is still in front of it. An implicit submit — Enter in the
  // field — names no button, and the markup puts the acting one first so that
  // it is the one the browser would have picked anyway.
  function answered(dialog, taken) {
    dialog.querySelector("form").addEventListener("submit", function (event) {
      var pending = taken();
      if (event.submitter && event.submitter.value === "cancel") {
        return;
      }
      if (pending) {
        pending();
      }
    });
  }

  answered(asking, function () {
    var pending = onAccept;
    onAccept = null;
    return pending;
  });

  answered(checking, function () {
    var pending = onAgree;
    onAgree = null;
    return pending;
  });

  // --- viewing a file --------------------------------------------------

  // A name the document viewer shows opens it over the page rather than in a
  // tab: a click with a modifier, or with the middle button, still does what
  // the link does. Edit in the menu of a file of text opens it there too, in
  // the editor. The frame is emptied when the dialog closes, which ends
  // whatever the libraries in it were doing; a file saved in it meanwhile
  // has changed its size and time, which a reload of the listing shows.
  var viewer = document.getElementById("viewer");
  var openViewer = null;
  var audio = null;
  if (viewer) {
    var viewerFrame = viewer.querySelector("iframe");
    var viewerOpener = null;
    var viewerSaved = false;
    openViewer = function (opener, name, source, kind, mode, type) {
      // the frame is at a path of its own, so the file is named from the root
      var src = new URL(source, location.href);
      var query = new URLSearchParams({ src: src.pathname + src.search, name: name, kind: kind });
      if (mode) {
        query.set("mode", mode);
      }
      if (type) {
        query.set("type", type);
      }
      viewerOpener = opener;
      viewerSaved = false;
      viewerFrame.title = name;
      viewerFrame.src = viewer.dataset.page + "?" + query.toString();
      viewer.showModal();
      viewerFrame.focus();
    };
    body.addEventListener("click", function (event) {
      var link = event.target.closest("a[data-viewer]");
      if (!link || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) {
        return;
      }
      event.preventDefault();
      if (link.dataset.viewer === "audio") {
        if (audio) {
          audio.play(link);
        }
        return;
      }
      if (link.dataset.viewer === "video" && audio) {
        audio.pause();
      }
      openViewer(link, link.closest("tr").dataset.name, link.dataset.viewerSrc, link.dataset.viewer, "",
        link.dataset.viewerType || "");
    });
    // the viewer closes itself on Escape or its Close button, which it says
    // from its frame, and says when it has saved a file; nothing else that
    // posts a message is listened to
    window.addEventListener("message", function (event) {
      if (event.origin !== location.origin || event.source !== viewerFrame.contentWindow) {
        return;
      }
      if (!event.data || event.data.type !== "go-fs-viewer") {
        return;
      }
      if (event.data.action === "close") {
        viewer.close();
      } else if (event.data.action === "saved") {
        viewerSaved = true;
      }
    });
    // Escape while the page has the focus rather than the frame is left to
    // the viewer as well, which asks first when there is something unsaved
    viewer.addEventListener("cancel", function (event) {
      event.preventDefault();
      viewerFrame.contentWindow.postMessage({ type: "go-fs-viewer", action: "close-request" }, location.origin);
    });
    viewer.addEventListener("close", function () {
      // the event comes a moment after the dialog closed, by which time it
      // may have been opened again on another file
      if (viewer.open) {
        return;
      }
      viewerFrame.src = "about:blank";
      if (viewerSaved) {
        done();
        return;
      }
      if (viewerOpener) {
        viewerOpener.focus();
        viewerOpener = null;
      }
    });
  }

  // --- playing audio ---------------------------------------------------
  //
  // Video opens in the viewer as a document does; audio plays in a bar at the
  // bottom of the window instead, which goes on playing as one browses. The
  // bar is a page of the viewer in a frame of its own, so that the player in
  // it runs under the viewer's policy rather than this page's. A file is only
  // offered to be played when the browser says it may play it, since nothing
  // is converted: anything else stays a download.
  //
  // A page that is left takes its frames with it, so while the bar plays,
  // the page is not left: the first time it would be, by a link to another
  // page or by a reload, it becomes a shell instead. It hides what it shows
  // and opens the page it was going to in a frame that fills the window above
  // the bar, where everything after happens; the address bar and the title
  // follow that frame, and the history of the browser walks it, back to the
  // page as it was before the shell. The listing in the shell hands what it
  // plays up to the shell.
  //
  // The bar may hand its queue to Sonos speakers: it asks for the dialog of
  // Play on Sonos, which the page at the top shows, over the shell too. The
  // server then plays the queue by itself, and the bar is the remote control
  // of that, also after the page was closed and opened again: the listing
  // says what the account has speakers play, and the bar is opened on it.
  if (viewer && viewer.dataset.player) {
    audio = (function (page) {
      var message = "go-fs-player";
      var root = document.documentElement;

      // alike are types a browser plays as another it knows better: a .mov
      // is the container an .mp4 grew out of, and the browsers that play the
      // usual ones, H.264 in it, still say they know nothing of QuickTime
      var alike = { "video/quicktime": "video/mp4" };

      // playableAs is the type the browser says it can play a file as, ""
      // for none; "maybe" is as good as it ever says
      function playableAs(kind, type) {
        var probe = document.createElement(kind);
        if (!type || typeof probe.canPlayType !== "function") {
          return "";
        }
        if (probe.canPlayType(type) !== "") {
          return type;
        }
        if (alike[type] && probe.canPlayType(alike[type]) !== "") {
          return alike[type];
        }
        return "";
      }
      Array.prototype.forEach.call(body.querySelectorAll("a[data-viewer='audio'], a[data-viewer='video']"), function (link) {
        var type = playableAs(link.dataset.viewer, link.dataset.viewerType);
        if (type) {
          link.dataset.viewerType = type;
        } else {
          delete link.dataset.viewer;
        }
      });

      // queueOf is the audio of this folder as the listing shows it now,
      // and where in it the link is
      function queueOf(link) {
        var queue = [];
        var index = 0;
        rows().forEach(function (row) {
          var item = row.querySelector("a[data-viewer='audio']");
          if (!item || (row.classList.contains("gone") && item !== link)) {
            return;
          }
          if (item === link) {
            index = queue.length;
          }
          var src = new URL(item.dataset.viewerSrc, location.href);
          queue.push({ src: src.pathname + src.search, name: row.dataset.name, type: item.dataset.viewerType || "" });
        });
        return { queue: queue, index: index };
      }

      // shellOf is the page whose shell this listing is shown in, null when
      // it is not in one
      function shellOf() {
        if (window.parent === window) {
          return null;
        }
        try {
          if (window.parent.location.origin === location.origin &&
            window.parent.document.documentElement.hasAttribute("data-go-fs-shell")) {
            return window.parent;
          }
        } catch (ignored) {
          // a page of another origin frames this one, which plays nothing
        }
        return null;
      }

      var host = shellOf();
      if (host) {
        return {
          play: function (link) {
            var wanted = queueOf(link);
            host.postMessage({ type: message, action: "play", queue: wanted.queue, index: wanted.index }, location.origin);
          },
          pause: function () {
            host.postMessage({ type: message, action: "pause" }, location.origin);
          },
          remote: function (session) {
            host.postMessage({ type: message, action: "remote", session: session }, location.origin);
          },
          keepsPlaying: function () {
            return false;
          },
          enterShell: function () {},
        };
      }

      // --- the bar, in the page at the top -------------------------------

      var bar = null;
      var ready = false;
      // pending is what the bar is told once it is ready
      var pending = [];

      function toBar(data) {
        data.type = message;
        if (ready) {
          bar.contentWindow.postMessage(data, location.origin);
        } else {
          pending.push(data);
        }
      }

      function openBar() {
        if (bar) {
          return;
        }
        bar = document.createElement("iframe");
        bar.className = "player";
        bar.title = "Player";
        bar.src = page;
        ready = false;
        pending = [];
        document.body.append(bar);
        document.body.classList.add("playing");
      }

      function play(queue, index) {
        openBar();
        toBar({ action: "play", queue: queue, index: index, sonos: !!document.getElementById("sonos-dialog") });
      }

      // remote has the bar show what speakers play, and control them
      function remote(session) {
        if (!session || !session.id) {
          return;
        }
        openBar();
        toBar({ action: "remote", session: session });
      }

      function pause() {
        if (bar && ready) {
          toBar({ action: "pause" });
        }
      }

      function closeBar() {
        if (!bar) {
          return;
        }
        bar.remove();
        bar = null;
        ready = false;
        pending = [];
        document.body.classList.remove("playing");
      }

      // --- the shell -----------------------------------------------------

      var shell = null;
      var title = document.title;

      function shelled() {
        return root.classList.contains("shelled");
      }

      // synced makes the address bar and the title say what the shell shows
      function synced() {
        if (!shell || !shelled()) {
          return;
        }
        try {
          var inside = shell.contentWindow.location;
          if (inside.origin !== location.origin) {
            return;
          }
          window.history.replaceState(window.history.state, "", inside.href);
          document.title = shell.contentDocument.title || title;
        } catch (ignored) {
          // nothing of this server, which is never framed here
        }
      }

      // enterShell opens a page in the shell; replace says it takes the
      // place of this one in the history, as a reload does, rather than
      // coming after it
      function enterShell(href, replace) {
        if (!shell) {
          shell = document.createElement("iframe");
          shell.className = "shell";
          shell.title = "Files";
          shell.addEventListener("load", synced);
          root.setAttribute("data-go-fs-shell", "");
          document.body.append(shell);
          shell.src = href;
        } else {
          shell.contentWindow.location.replace(href);
        }
        if (replace) {
          window.history.replaceState({ goFsShell: true }, "", href);
        } else {
          window.history.pushState({ goFsShell: true }, "", href);
        }
        root.classList.add("shelled");
      }

      // back past the first page of the shell is this page again, and
      // forward is the shell again
      window.addEventListener("popstate", function (event) {
        if (!shell) {
          return;
        }
        var inShell = !!(event.state && event.state.goFsShell);
        root.classList.toggle("shelled", inShell);
        if (inShell) {
          synced();
        } else {
          document.title = title;
        }
      });

      // a link to another page of this server is followed in the shell while
      // the bar plays; a file is not a page, and a download does not leave
      // the page anyway
      document.addEventListener("click", function (event) {
        if (!bar || shelled() || event.defaultPrevented || event.button !== 0 ||
          event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) {
          return;
        }
        var link = event.target.closest("a[href]");
        if (!link || link.hasAttribute("download") || (link.target && link.target !== "_self")) {
          return;
        }
        var row = link.closest("tr[data-name]");
        if (row && row.dataset.dir !== "1") {
          return;
        }
        var target = new URL(link.href, location.href);
        if (target.origin !== location.origin ||
          (target.pathname === location.pathname && target.search === location.search && target.hash)) {
          return;
        }
        event.preventDefault();
        enterShell(target.href, false);
      });

      // the bar says when it is ready and when it was closed; the listing in
      // the shell asks to play and to pause; nothing else is listened to
      window.addEventListener("message", function (event) {
        if (event.origin !== location.origin || !event.data || event.data.type !== message) {
          return;
        }
        if (bar && event.source === bar.contentWindow) {
          if (event.data.action === "ready") {
            ready = true;
            var waiting = pending;
            pending = [];
            waiting.forEach(toBar);
          } else if (event.data.action === "closed") {
            closeBar();
          } else if (event.data.action === "sonos" && sonosPlaying && Array.isArray(event.data.queue)) {
            sonosPlaying.openQueue(event.data.queue, event.data.index);
          }
          return;
        }
        if (shell && event.source === shell.contentWindow) {
          if (event.data.action === "play" && Array.isArray(event.data.queue)) {
            play(event.data.queue, event.data.index);
          } else if (event.data.action === "pause") {
            pause();
          } else if (event.data.action === "remote") {
            remote(event.data.session);
          }
        }
      });

      // what the account has speakers play comes back with the page
      var playingOn = document.getElementById("sonos-dialog");
      if (playingOn && playingOn.dataset.session) {
        try {
          remote(JSON.parse(playingOn.dataset.session));
        } catch (ignored) {
          // a page that cannot read it shows no bar for it
        }
      }

      return {
        play: function (link) {
          var wanted = queueOf(link);
          play(wanted.queue, wanted.index);
        },
        pause: pause,
        remote: remote,
        keepsPlaying: function () {
          return !!bar;
        },
        enterShell: enterShell,
      };
    })(viewer.dataset.player);
  }

  // --- actions ---------------------------------------------------------

  var makeFolder = document.getElementById("new-folder");
  if (makeFolder) {
    makeFolder.addEventListener("click", function () {
      clear();
      ask("New folder", "It is created in " + shown + ".", "", "Create", function (name) {
        send("create", "MKCOL", segment(name) + "/").then(done).catch(failed);
      });
    });
  }

  // --- the options menu ------------------------------------------------
  //
  // One menu serves every row, as on the registry page. It is placed below
  // the button that opened it and remembers the row, which is what the items
  // act on. What the row is decides what the menu holds: a folder downloads
  // as an archive, and Delete is there only where the account may delete
  // that kind of thing.

  var menu = document.getElementById("menu");
  var download = menu.querySelector("[data-do='download']");
  var removal = menu.querySelector("[data-do='delete']");
  // Share is there for a file only: a link names one file as it is now
  var sharing = menu.querySelector("[data-do='share']");
  // and so is Send File, which uploads one file
  var sendItem = menu.querySelector("[data-do='send']");
  // and so is Fetch, on the page of a stored server, which also takes a
  // folder where the account may create folders
  var remoteFetchItem = menu.querySelector("[data-do='remote-fetch']");
  // Import into registry is there for a file whose name an image archive has;
  // it opens the registry page's Import dialog with that file
  var importing = menu.querySelector("[data-do='import']");
  // Edit is there for a file of text the account may replace, which the
  // server marks; it opens the file in the viewer's editor
  var editing = menu.querySelector("[data-do='edit']");
  // Edit MP3 Tag is there for an MP3 file where the account may replace it
  var mp3Item = menu.querySelector("[data-do='mp3tag']");
  // Play on Sonos is there for a file a Sonos speaker plays, by the
  // extensions sonosTypes in sonos.go names
  var sonosItem = menu.querySelector("[data-do='sonos']");
  var sonosPlayable = /\.(mp3|m4a|aac|flac|ogg|oga|wav)$/i;
  var archiveName = /\.(tar|tgz|tzst|txz|tbz2?|tar\.(gz|zst|xz|bz2))$/i;
  var opener = null;

  function items() {
    return Array.prototype.filter.call(menu.querySelectorAll("button, a"), function (item) {
      return !item.hidden;
    });
  }

  function openMenu(button) {
    var row = button.closest("tr");
    var isFolder = row.dataset.dir === "1";
    // a stored server may not allow downloads, and then there is none
    if (download) {
      download.href = !isFolder ? segment(row.dataset.name)
        : remote ? segment(row.dataset.name) + "&archive=1" : segment(row.dataset.name) + "/?go-fs=archive";
      download.querySelector("span").textContent = isFolder ? "Download as .tar.xz" : "Download";
    }
    if (removal) {
      removal.hidden = row.dataset.delete !== "1";
    }
    if (sharing) {
      sharing.hidden = isFolder;
    }
    if (sendItem) {
      sendItem.hidden = isFolder;
    }
    if (remoteFetchItem) {
      remoteFetchItem.hidden = isFolder && !remoteFetchItem.dataset.folders;
    }
    if (editing) {
      editing.hidden = isFolder || !row.dataset.edit || !openViewer;
    }
    if (mp3Item) {
      mp3Item.hidden = isFolder || !/\.mp3$/i.test(row.dataset.name);
    }
    if (sonosItem) {
      sonosItem.hidden = isFolder || !sonosPlayable.test(row.dataset.name);
    }
    if (importing) {
      importing.hidden = isFolder || !archiveName.test(row.dataset.name);
      importing.href = importing.dataset.registry + "&import=" + encodeURIComponent(row.dataset.name);
    }
    // where a stored server allows nothing on the row, there is no menu
    if (!items().length) {
      return;
    }
    opener = button;
    button.setAttribute("aria-expanded", "true");
    menu.hidden = false;
    var box = button.getBoundingClientRect();
    var left = box.right + window.scrollX - menu.offsetWidth;
    menu.style.left = Math.max(window.scrollX + 8, left) + "px";
    var top = box.bottom + window.scrollY + 4;
    // a menu that would run off the bottom opens above its button instead
    if (box.bottom + menu.offsetHeight + 8 > window.innerHeight) {
      top = box.top + window.scrollY - menu.offsetHeight - 4;
    }
    menu.style.top = top + "px";
    items()[0].focus();
  }

  function closeMenu(refocus) {
    if (!opener) {
      return;
    }
    menu.hidden = true;
    opener.setAttribute("aria-expanded", "false");
    if (refocus) {
      opener.focus();
    }
    opener = null;
  }

  body.addEventListener("click", function (event) {
    var button = event.target.closest("button[data-do='menu']");
    if (!button) {
      return;
    }
    event.stopPropagation();
    clear();
    if (opener === button) {
      closeMenu(true);
      return;
    }
    closeMenu(false);
    openMenu(button);
  });

  document.addEventListener("click", function (event) {
    if (opener && !menu.contains(event.target)) {
      closeMenu(false);
    }
  });

  window.addEventListener("resize", function () {
    closeMenu(false);
  });

  menu.addEventListener("keydown", function (event) {
    var list = items();
    var at = list.indexOf(document.activeElement);
    switch (event.key) {
      case "Escape":
        event.preventDefault();
        closeMenu(true);
        break;
      case "Tab":
        closeMenu(false);
        break;
      case "ArrowDown":
        event.preventDefault();
        list[(at + 1) % list.length].focus();
        break;
      case "ArrowUp":
        event.preventDefault();
        list[(at - 1 + list.length) % list.length].focus();
        break;
      case "Home":
        event.preventDefault();
        list[0].focus();
        break;
      case "End":
        event.preventDefault();
        list[list.length - 1].focus();
        break;
    }
  });

  // Download is a link and is left to the browser: the answer is an
  // attachment, so following it saves the file and leaves the page as it is.
  menu.addEventListener("click", function (event) {
    var item = event.target.closest("[data-do]");
    if (!item || !opener) {
      return;
    }
    var row = opener.closest("tr");
    closeMenu(item.dataset.do === "download" || item.dataset.do === "import");
    if (item.dataset.do === "rename") {
      renameEntry(row);
    } else if (item.dataset.do === "delete") {
      deleteEntry(row);
    } else if (item.dataset.do === "share") {
      shareEntry(row);
    } else if (item.dataset.do === "send") {
      sending.open(row);
    } else if (item.dataset.do === "remote-fetch") {
      remoteFetching.open(row);
    } else if (item.dataset.do === "mp3tag" && mp3Editing) {
      mp3Editing.open(row);
    } else if (item.dataset.do === "sonos" && sonosPlaying) {
      sonosPlaying.open(row);
    } else if (item.dataset.do === "edit" && openViewer) {
      openViewer(row.querySelector("a") || row, row.dataset.name, row.dataset.edit, "text", "edit");
    }
  });

  function renameEntry(row) {
    var name = row.dataset.name;
    ask("Rename", shown + name, name, "Rename", function (typed) {
      send("rename", "MOVE", segment(name), { Destination: segment(typed) }).then(done).catch(failed);
    });
  }

  function deleteEntry(row) {
    var name = row.dataset.name;
    var isFolder = row.dataset.dir === "1";
    checking.querySelector("h2").textContent = isFolder ? "Delete folder" : "Delete file";
    var trashed = table.dataset.trash === "1";
    checking.querySelector("p").textContent = isFolder
      ? "Delete " + shown + name + "? Only an empty folder can be removed." +
        (trashed ? " It is moved to the trash." : "")
      : "Delete " + shown + name + (trashed ? "? It is moved to the trash." : "? This cannot be undone.");
    onAgree = function () {
      send(isFolder ? "folder" : "file", "DELETE", segment(name)).then(done).catch(failed);
    };
    checking.showModal();
  }

  // --- sharing a file --------------------------------------------------
  //
  // The server computes the link, since only it holds the secret the key is
  // signed with; it answers the path and the query. The address in front of
  // them is general.internalAddress or general.externalAddress when they are
  // configured, and otherwise the origin this page was loaded from, which is
  // right behind a proxy as well. With both configured the dialog has a
  // dropdown between them, which starts on the one this page was loaded from,
  // or on the external one when it was loaded from neither.

  var shareDialog = document.getElementById("share-dialog");
  var shareFields = shareDialog ? shareDialog.querySelectorAll("input.link") : [];
  var copyButtons = shareDialog ? shareDialog.querySelectorAll("button[data-copy]") : [];
  var shareBases = shareDialog ? {
    internal: shareDialog.dataset.internal || "",
    external: shareDialog.dataset.external || ""
  } : {};
  var shareChoice = shareDialog ? shareDialog.querySelector("#share-base") : null;
  // shared is the file the dialog is open for, so that another choice of
  // address can build its lines again
  var shared = null;

  function shareField(kind) {
    return shareDialog.querySelector("input.link[data-kind='" + kind + "']");
  }

  // shellQuote makes a word safe to paste into a POSIX shell: the name may
  // hold spaces or quotes, and the link's query an & or a ?.
  function shellQuote(text) {
    return "'" + text.replace(/'/g, "'\\''") + "'";
  }

  // sameAddress reports whether a configured address names the origin, or
  // failing that the host, this page was loaded from.
  function sameAddress(address, part) {
    try {
      return new URL(address)[part] === window.location[part];
    } catch (err) {
      return false;
    }
  }

  // defaultBase is the choice the dropdown starts on.
  function defaultBase() {
    var parts = ["origin", "hostname"];
    for (var i = 0; i < parts.length; i++) {
      var internal = sameAddress(shareBases.internal, parts[i]);
      var external = sameAddress(shareBases.external, parts[i]);
      if (internal !== external) {
        return internal ? "internal" : "external";
      }
      if (external) {
        return "external";
      }
    }
    return "external";
  }

  // shareBase is the address the lines are built on.
  function shareBase() {
    if (shareChoice) {
      return shareBases[shareChoice.value];
    }
    return shareBases.external || shareBases.internal || window.location.origin;
  }

  // fillShare writes the link and the commands for the file the dialog is
  // open for.
  function fillShare() {
    var url = shareBase() + shared.path;
    // the commands name the output file, or the key would end up in it
    shareField("link").value = url;
    shareField("curl").value = "curl -fL -o " + shellQuote(shared.name) + " " + shellQuote(url);
    shareField("wget").value = "wget -O " + shellQuote(shared.name) + " " + shellQuote(url);
    copyButtons.forEach(function (button) {
      window.clearTimeout(button.copiedTimer);
      button.querySelector("span").textContent = "Copy";
    });
  }

  function shareEntry(row) {
    var name = row.dataset.name;
    fetch(segment(name) + "?go-fs=share", {
      headers: { Accept: "application/json" },
      credentials: "same-origin"
    }).then(function (res) {
      if (res.ok) {
        return res.json();
      }
      return res.text().then(function (text) {
        throw new Error(reason("share", res.status, text.trim()));
      });
    }).then(function (view) {
      shareDialog.querySelector("p").textContent = "Anyone with this link can download " +
        decodeURI(folder) + name + " without logging in. It stops working as soon as " +
        "the file is changed, renamed or replaced.";
      shared = { name: name, path: view.path };
      if (shareChoice) {
        shareChoice.value = defaultBase();
      }
      fillShare();
      shareDialog.showModal();
      shareField("link").focus();
      shareField("link").select();
    }).catch(failed);
  }

  // The clipboard API is only there in a secure context, which a plain http
  // page on another host is not; the selection is copied the old way there.
  function copyField(field) {
    field.focus();
    field.select();
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(field.value);
    }
    return new Promise(function (resolve, reject) {
      if (document.execCommand("copy")) {
        resolve();
        return;
      }
      reject(new Error("copy refused"));
    });
  }

  if (shareDialog) {
    if (shareChoice) {
      shareChoice.addEventListener("change", function () {
        if (shared) {
          fillShare();
        }
      });
    }
    shareFields.forEach(function (field) {
      field.addEventListener("focus", function () {
        field.select();
      });
    });
    // each line copies its own field; the buttons do not submit, so the
    // dialog stays open and the line can still be read after copying
    copyButtons.forEach(function (button) {
      var label = button.querySelector("span");
      button.addEventListener("click", function () {
        copyField(button.parentNode.querySelector("input.link")).then(function () {
          label.textContent = "Copied";
        }, function () {
          label.textContent = "Press Ctrl+C";
        });
        window.clearTimeout(button.copiedTimer);
        button.copiedTimer = window.setTimeout(function () {
          label.textContent = "Copy";
        }, 2000);
      });
    });
  }

  // --- fetching from a URL ---------------------------------------------
  //
  // The server does the download, as a job of its own, the way the registry
  // page pulls an image, and it runs on to its end whether or not a page is
  // still open. The dialog only starts it, and waits until the remote has
  // answered and the file is on its way; from then on it is one of the
  // downloads the button in the header follows, together with every other
  // fetch of the account.

  // the same units the server writes into the Size column
  function readableSize(size) {
    function tenth(value) {
      return Math.round(value * 10) / 10;
    }
    if (size > 1000000000) {
      return tenth(size / 1024 / 1024 / 1024) + " GB";
    }
    if (size > 1000000) {
      return tenth(size / 1024 / 1024) + " MB";
    }
    return tenth(size / 1024) + " KB";
  }

  function readableRate(perSecond) {
    return readableSize(perSecond || 0) + "/s";
  }

  function fetchJobURL(id) {
    return folder + "?go-fs=fetch-job&id=" + encodeURIComponent(id);
  }

  function askFetch(method, url, body) {
    return askJSON("fetch", method, url, body);
  }

  // askJSON sends a request of a transfer and reads why it was refused, in
  // the words of what was asked for
  function askJSON(what, method, url, body, signal) {
    var options = {
      method: method,
      headers: { Accept: "application/json" },
      credentials: "same-origin",
      signal: signal
    };
    if (body !== undefined) {
      options.headers["Content-Type"] = "application/json";
      options.body = JSON.stringify(body);
    }
    return fetch(url, options).then(function (res) {
      if (res.ok) {
        return res;
      }
      return res.text().then(function (text) {
        throw new Error(reason(what, res.status, text.trim()));
      });
    });
  }

  // here is this folder as the server names it, unescaped, which is how a
  // fetch says where it stores
  var here = (function () {
    if (remote) {
      // no fetch ends up on a stored server
      return null;
    }
    try {
      return folder.split("/").map(decodeURIComponent).join("/");
    } catch (ignored) {
      return folder;
    }
  })();

  // The transfers: the button in the header says how fast the fetches and
  // the sends of the account go together and fills as far as they have got,
  // and opens the window that lists them one by one. The page asks after them every second
  // while one runs or the window is open, and once when it loads, which
  // finds those started before a reload or in another tab.
  var downloads = (function () {
    var button = document.getElementById("downloads");
    var dialog = document.getElementById("downloads-dialog");
    if (!button || !dialog) {
      return { refresh: function () {} };
    }
    var label = button.querySelector(".rate");
    var list = dialog.querySelector(".downloads-list");
    var none = dialog.querySelector("p.none");
    var menu = document.getElementById("menu");
    var queue = document.getElementById("queue");
    var timer = null;
    var asking = false;
    var again = false;
    // shown are the fetches the button counts: those that run, and those
    // that failed until they are cleared
    var shown = [];
    // told are the fetches whose end this page has already told about
    var told = {};
    // rows are the lines of the window, by job
    var rows = {};

    function running(job) {
      return job.state === "running";
    }

    // quiet is a page a reload would take nothing from: no dialog or menu
    // open, and no upload under way or with reasons left on the screen
    function quiet() {
      return !document.querySelector("dialog[open]") &&
        (!menu || menu.hidden) && (!queue || queue.hidden);
    }

    // pending is what the fetches that stored into this folder said, while
    // the page waits to be quiet enough to reload
    var pending = null;

    function reloadWhenQuiet() {
      if (quiet()) {
        sayAfterReload(pending);
        return;
      }
      window.setTimeout(reloadWhenQuiet, 500);
    }

    // A transfer that ended is told about once and taken off the list. A
    // fetch that stored into this folder reloads the page to show the file,
    // once that takes nothing from the screen; a send changes nothing here.
    function finish(ended) {
      if (ended.length === 0) {
        return;
      }
      var messages = ended.map(function (job) {
        return job.message;
      }).join(" ");
      var mine = ended.some(function (job) {
        return job.kind !== "send" && job.folder === here;
      });
      Promise.all(ended.map(function (job) {
        return askFetch("DELETE", fetchJobURL(job.id)).catch(function () {
          // it is forgotten within the hour anyway
        });
      })).then(function () {
        if (!mine) {
          say(messages, true);
          return;
        }
        var waiting = pending !== null;
        pending = waiting ? pending + " " + messages : messages;
        if (waiting) {
          say(pending + " The page reloads when you are done here.", true);
          return;
        }
        if (quiet()) {
          sayAfterReload(pending);
          return;
        }
        say(pending + " The page reloads when you are done here.", true);
        reloadWhenQuiet();
      });
    }

    function line(job) {
      var row = rows[job.id];
      if (!row) {
        row = { item: document.createElement("li") };
        var cell = document.createElement("span");
        cell.className = "name";
        row.file = document.createElement("span");
        row.file.className = "file";
        row.where = document.createElement("span");
        row.where.className = "folder";
        cell.appendChild(row.file);
        cell.appendChild(row.where);
        row.share = document.createElement("span");
        row.share.className = "share";
        row.speed = document.createElement("span");
        row.speed.className = "speed";
        var track = document.createElement("span");
        track.className = "track";
        row.fill = document.createElement("span");
        row.fill.className = "fill";
        track.appendChild(row.fill);
        row.message = document.createElement("span");
        row.message.className = "message";
        row.stop = document.createElement("button");
        row.stop.type = "button";
        row.stop.className = "plain";
        row.stop.addEventListener("click", function () {
          row.stop.disabled = true;
          askFetch("DELETE", fetchJobURL(job.id)).then(refresh, function (err) {
            row.stop.disabled = false;
            failed(err);
          });
        });
        [cell, row.share, row.speed, track, row.message, row.stop].forEach(function (part) {
          row.item.appendChild(part);
        });
        rows[job.id] = row;
      }
      var name = job.name || job.what;
      var bad = job.state === "failed";
      var sent = job.kind === "send";
      row.file.textContent = name;
      row.file.title = name;
      row.where.textContent = job.folder ? (sent ? "to " : "into ") + job.folder : "";
      row.where.title = row.where.textContent;
      row.item.classList.toggle("failed", bad);
      row.message.hidden = !bad;
      row.message.textContent = bad ? (job.message || (sent ? "The send failed." : "The fetch failed.")) : "";
      row.stop.textContent = bad ? "Clear" : "Stop";
      row.stop.setAttribute("aria-label", (bad ? "Clear " : "Stop ") + name);
      if (bad) {
        return row.item;
      }
      // a fetch of a folder counts its files as well as its bytes
      var files = job.blobsTotal > 0 ? job.blobsDone + "/" + job.blobsTotal + " files · " : "";
      if (job.phase !== "downloading") {
        row.share.textContent = "";
        row.speed.textContent = job.phase === "listing" ? "Analysing…" : "Connecting…";
        row.fill.style.width = "0";
      } else if (job.bytesTotal > 0) {
        var percent = Math.floor(Math.min(job.bytesDone, job.bytesTotal) * 100 / job.bytesTotal);
        row.share.textContent = files + percent + "%";
        row.speed.textContent = readableRate(job.bytesPerSecond);
        row.fill.style.width = percent + "%";
      } else {
        // the remote did not say how large it is, or a folder holds only
        // empty files
        row.share.textContent = files + readableSize(job.bytesDone);
        row.speed.textContent = readableRate(job.bytesPerSecond);
        row.fill.style.width = "0";
      }
      return row.item;
    }

    function render(jobs) {
      var ended = [];
      shown = jobs.filter(function (job) {
        if (job.state === "done" && !told[job.id]) {
          told[job.id] = true;
          ended.push(job);
        }
        return running(job) || job.state === "failed";
      });
      finish(ended);

      var active = shown.filter(running);
      var speed = 0;
      var done = 0;
      var total = 0;
      active.forEach(function (job) {
        speed += job.bytesPerSecond || 0;
        if (job.bytesTotal > 0) {
          done += Math.min(job.bytesDone, job.bytesTotal);
          total += job.bytesTotal;
        }
      });
      var percent = total > 0 ? Math.floor(done * 100 / total) : 0;
      var lost = shown.length - active.length;
      button.hidden = shown.length === 0;
      button.style.setProperty("--progress", active.length > 0 ? percent + "%" : "0%");
      button.classList.toggle("bad", active.length === 0 && lost > 0);
      var words;
      if (active.length > 0) {
        label.textContent = readableRate(speed) + (active.length > 1 ? " · " + active.length : "");
        words = (active.length === 1 ? "1 transfer" : active.length + " transfers") + ", " +
          (total > 0 ? percent + "% done" : "size unknown") + ", " + readableRate(speed);
      } else {
        label.textContent = lost + " failed";
        words = lost === 1 ? "1 transfer failed" : lost + " transfers failed";
      }
      button.title = words;
      button.setAttribute("aria-label", "Transfers: " + words);

      var keep = {};
      shown.forEach(function (job) {
        keep[job.id] = true;
        list.appendChild(line(job));
      });
      Object.keys(rows).forEach(function (id) {
        if (!keep[id]) {
          list.removeChild(rows[id].item);
          delete rows[id];
        }
      });
      none.hidden = shown.length > 0;
    }

    function refresh() {
      window.clearTimeout(timer);
      timer = null;
      if (asking) {
        again = true;
        return;
      }
      asking = true;
      var wait = 1000;
      askFetch("GET", folder + "?go-fs=fetch-jobs").then(function (res) {
        return res.json();
      }).then(function (view) {
        render(view.jobs || []);
      }).catch(function () {
        // the server may be restarting; what was shown stays, and is asked
        // after less often
        wait = 5000;
      }).then(function () {
        asking = false;
        if (again) {
          again = false;
          refresh();
          return;
        }
        if (dialog.open || shown.some(running)) {
          timer = window.setTimeout(refresh, wait);
        }
      });
    }

    // heard is what the server told of a transfer of the account as it
    // started or ended, or null when the stream opened, which may have been
    // after something ended. An end is taken from what was told rather than
    // from the list, since another tab may have taken it off that already.
    function heard(job) {
      if (job && job.state === "done" && !told[job.id]) {
        told[job.id] = true;
        finish([job]);
      }
      refresh();
    }

    // listen opens the stream the server tells of the transfers on, and
    // hands each thing it hears to tell. A stream the server refused, as it
    // does once the session ended, is tried again after a while.
    function listen(tell) {
      var source = new EventSource(folder + "?go-fs=events");
      source.addEventListener("open", function () {
        tell(null);
      });
      source.addEventListener("transfer", function (event) {
        try {
          tell(JSON.parse(event.data));
        } catch (ignored) {
          // not an event of this page
        }
      });
      source.addEventListener("error", function () {
        if (source.readyState === EventSource.CLOSED) {
          window.setTimeout(function () {
            listen(tell);
          }, 30000);
        }
      });
    }

    // Each stream holds a connection, and a browser opens only a few to one
    // server, so one tab holds the stream and hands what it hears to the
    // others; when it closes, another takes it over.
    if (window.EventSource) {
      if (navigator.locks && window.BroadcastChannel) {
        var channel = new BroadcastChannel("go-fs-transfers");
        channel.addEventListener("message", function (event) {
          heard(event.data);
        });
        navigator.locks.request("go-fs-transfers", function () {
          listen(function (job) {
            channel.postMessage(job);
            heard(job);
          });
          // held for as long as the tab is open
          return new Promise(function () {});
        });
      } else {
        listen(heard);
      }
    }

    button.addEventListener("click", function () {
      clear();
      dialog.showModal();
      refresh();
    });
    refresh();
    return { refresh: refresh };
  })();

  (function () {
    var dialog = document.getElementById("fetch-dialog");
    var button = document.getElementById("fetch");
    if (!dialog || !button) {
      return;
    }
    var form = dialog.querySelector("form");
    var headers = dialog.querySelector("details.headers");
    var progress = dialog.querySelector(".progress");
    var bar = progress.querySelector("progress");
    var status = progress.querySelector(".status");
    var go = form.querySelector("button[value='start']");
    var stop = form.querySelector("button[value='stop']");
    var close = form.querySelector("button[value='cancel']");
    // media is Download as, there while the server has yt-dlp: a video or
    // its audio takes no login and no headers, which are hidden for it
    var media = form.elements.media || null;
    var plainOnly = dialog.querySelectorAll(".plain-only");
    // job is the fetch the dialog waits on until its download begins, null
    // while it waits on none
    var job = null;

    function chosenMedia() {
      return media ? media.value : "";
    }

    function showMedia() {
      var hide = chosenMedia() !== "";
      Array.prototype.forEach.call(plainOnly, function (element) {
        element.hidden = hide;
      });
    }

    // youTube says a URL is a YouTube video, which is fetched as one rather
    // than as the page it is
    function youTube(text) {
      var host;
      try {
        host = new URL(text.trim()).hostname.toLowerCase();
      } catch (e) {
        return false;
      }
      return host === "youtu.be" || host === "youtube.com" || /\.youtube\.com$/.test(host);
    }

    function field(name) {
      return form.elements[name];
    }

    // stored is the name of the server chosen, "" for the login typed in
    function stored() {
      return field("server").value;
    }

    // the protocol chosen for the login typed in
    function protocol() {
      var select = field("protocol");
      return select.options[select.selectedIndex] || null;
    }

    function defaultPort() {
      var chosen = protocol();
      return chosen ? parseInt(chosen.dataset.port, 10) || 0 : 22;
    }

    // whether the host of the login typed in shows a key to accept first
    function showsKey() {
      var chosen = protocol();
      return !chosen || chosen.dataset.hostkey === "true";
    }

    // The fields of the login typed in are left out, not only hidden, for a
    // stored server, so that the form does not ask for them.
    function choose() {
      var server = stored() !== "";
      manual.hidden = server;
      manual.disabled = server;
    }

    function busy(on) {
      Array.prototype.forEach.call(form.querySelectorAll("input, textarea, select"), function (input) {
        input.disabled = on;
      });
      go.hidden = on;
      stop.hidden = !on;
      close.textContent = on ? "Close" : "Cancel";
    }

    function tell(message, bad) {
      progress.hidden = false;
      status.textContent = message;
      status.classList.toggle("bad", bad === true);
    }

    function quiet() {
      progress.hidden = true;
      status.textContent = "";
      status.classList.remove("bad");
    }

    function fresh() {
      form.reset();
      headers.open = false;
      showMedia();
      quiet();
    }

    // wait asks after the fetch until the remote has answered. A download
    // that has begun leaves the dialog for the button in the header; one that
    // could not begin is told here, where the URL can still be corrected.
    function wait() {
      var id = job;
      if (!id) {
        return;
      }
      askFetch("GET", fetchJobURL(id)).then(function (res) {
        return res.json();
      }).then(function (view) {
        if (job !== id) {
          return;
        }
        if (view.state === "running" && view.phase !== "downloading") {
          window.setTimeout(wait, 400);
          return;
        }
        job = null;
        busy(false);
        if (view.state === "running" || view.state === "done") {
          dialog.close();
          fresh();
          downloads.refresh();
          return;
        }
        bar.value = 0;
        if (view.state === "cancelled") {
          tell("Stopped.");
          return;
        }
        tell(view.message || "The fetch failed.", true);
        // told here, so the downloads need not tell it again
        askFetch("DELETE", fetchJobURL(id)).catch(function () {});
      }).catch(function (err) {
        if (job !== id) {
          return;
        }
        job = null;
        busy(false);
        bar.value = 0;
        tell(err.message, true);
      });
    }

    // The submit event rather than close, for the reason the other dialogs
    // give: which button was pressed is only known there.
    form.addEventListener("submit", function (event) {
      var pressed = event.submitter ? event.submitter.value : "start";
      if (pressed === "cancel") {
        return;
      }
      event.preventDefault();
      if (pressed === "stop") {
        if (job) {
          askFetch("DELETE", fetchJobURL(job)).catch(failed);
        }
        return;
      }
      if (job) {
        return;
      }
      var kind = chosenMedia();
      var body = {
        url: field("url").value.trim(),
        username: kind ? "" : field("username").value.trim(),
        password: kind ? "" : field("password").value,
        headers: kind ? "" : field("headers").value,
        skipVerify: !field("verify").checked,
        media: kind
      };
      busy(true);
      bar.removeAttribute("value");
      tell(kind ? "Looking up the " + kind + "…" : "Connecting…");
      askFetch("POST", folder + "?go-fs=fetch", body).then(function (res) {
        return res.json();
      }).then(function (view) {
        job = view.id;
        wait();
      }).catch(function (err) {
        busy(false);
        bar.value = 0;
        tell(err.message, true);
      });
    });

    // Closed while the remote has not answered yet: the fetch goes on, and
    // the downloads follow it from here.
    dialog.addEventListener("close", function () {
      if (job) {
        job = null;
        busy(false);
        downloads.refresh();
      }
    });

    if (media) {
      media.addEventListener("change", showMedia);
      // a YouTube link is a video, unless Download as was chosen otherwise
      field("url").addEventListener("input", function () {
        if (media.value === "" && youTube(field("url").value)) {
          media.value = "video";
          showMedia();
        }
      });
    }

    button.addEventListener("click", function () {
      clear();
      fresh();
      dialog.showModal();
      field("url").focus();
    });
  })();

  // --- choosing a folder ------------------------------------------------
  //
  // Send File and Fetch File both list a folder for the user to choose where
  // a file goes: a path of crumbs above a list of what is in it, whose
  // folders are buttons that carry the path they lead to, and whose files
  // are shown only, the one named like the file that goes there marked, as
  // it is about to be replaced.

  function joinedPath(path, entry) {
    return (path === "/" ? "" : path) + "/" + entry;
  }

  function folderButton(label, path, icon) {
    var button = document.createElement("button");
    button.type = "button";
    button.dataset.path = path;
    var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "ic");
    var use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", icon);
    svg.appendChild(use);
    var text = document.createElement("span");
    text.className = "label";
    text.textContent = label;
    button.appendChild(svg);
    button.appendChild(text);
    return button;
  }

  // renderFolders lists a folder into crumbs and list, and reports whether a
  // file named name is in it
  function renderFolders(crumbs, list, view, name) {
    crumbs.textContent = "";
    var parts = view.path.split("/").filter(Boolean);
    var root = document.createElement("button");
    root.type = "button";
    root.dataset.path = "/";
    root.textContent = "/";
    if (parts.length === 0) {
      root.setAttribute("aria-current", "location");
    }
    crumbs.appendChild(root);
    parts.forEach(function (part, i) {
      if (i > 0) {
        var sep = document.createElement("span");
        sep.className = "sep";
        sep.textContent = "/";
        crumbs.appendChild(sep);
      }
      var button = document.createElement("button");
      button.type = "button";
      button.dataset.path = "/" + parts.slice(0, i + 1).join("/");
      button.textContent = part;
      if (i === parts.length - 1) {
        button.setAttribute("aria-current", "location");
      }
      crumbs.appendChild(button);
    });

    list.textContent = "";
    var clash = false;
    if (view.parent) {
      var up = document.createElement("li");
      up.appendChild(folderButton("..", view.parent, "#i-up"));
      list.appendChild(up);
    }
    (view.entries || []).forEach(function (entry) {
      var item = document.createElement("li");
      if (entry.dir) {
        item.appendChild(folderButton(entry.name, joinedPath(view.path, entry.name), "#i-folder"));
      } else {
        var line = document.createElement("span");
        line.className = "entry";
        if (entry.name === name) {
          clash = true;
          line.classList.add("clash");
        }
        var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
        svg.setAttribute("class", "ic");
        var use = document.createElementNS("http://www.w3.org/2000/svg", "use");
        use.setAttribute("href", "#i-file");
        svg.appendChild(use);
        var label = document.createElement("span");
        label.className = "label";
        label.textContent = entry.name;
        var size = document.createElement("span");
        size.className = "size";
        size.textContent = readableSize(entry.size);
        line.appendChild(svg);
        line.appendChild(label);
        line.appendChild(size);
        item.appendChild(line);
      }
      list.appendChild(item);
    });
    if (view.truncated) {
      var note = document.createElement("li");
      note.className = "note";
      note.textContent = "Not everything in this folder is shown.";
      list.appendChild(note);
    } else if (!view.entries || view.entries.length === 0) {
      var empty = document.createElement("li");
      empty.className = "note";
      empty.textContent = "This folder is empty.";
      list.appendChild(empty);
    }
    return clash;
  }

  // --- sending to another host -----------------------------------------
  //
  // The server sends the file, as a job of its own like a fetch, and the
  // dialog leads up to it in three steps: the host and the login; the key
  // the host shows, which is accepted before any login is offered to it; and
  // the folder of the host the file goes into. A key once accepted is
  // remembered by this browser for its host and port, so that the next send
  // there goes straight to the folders, and a key that changed since is
  // asked about again, with a warning. Once the send is under way it is one
  // of the transfers the button in the header follows.
  //
  // A server the administrator stored is chosen by its name instead, and
  // goes straight to its folders: the server holds its login and its key,
  // which this page never sees. Manual, the first choice, is the login typed
  // in, which is not stored anywhere but in the fields.
  var sending = (function () {
    var dialog = document.getElementById("send-dialog");
    if (!dialog) {
      return { open: function () {} };
    }
    var form = dialog.querySelector("form");
    var what = dialog.querySelector("p.what");
    var fields = dialog.querySelector(".fields");
    var manual = fields.querySelector("fieldset.manual");
    var connected = dialog.querySelector(".connected");
    var loginText = connected.querySelector(".login");
    var change = connected.querySelector(".change");
    var keyBox = dialog.querySelector(".hostkey");
    var warning = keyBox.querySelector(".warning");
    var keyHost = keyBox.querySelector(".host");
    var keyType = keyBox.querySelector(".fingerprint .type");
    var keyPrint = keyBox.querySelector(".fingerprint code");
    var remote = dialog.querySelector(".remote");
    var crumbs = remote.querySelector(".remote-path");
    var list = remote.querySelector(".remote-list");
    var where = remote.querySelector(".where");
    var spinner = remote.querySelector(".spinner");
    var progress = dialog.querySelector(".progress");
    var bar = progress.querySelector("progress");
    var status = progress.querySelector(".status");
    var connect = form.querySelector("button[value='connect']");
    var accept = form.querySelector("button[value='accept']");
    var go = form.querySelector("button[value='start']");
    var stop = form.querySelector("button[value='stop']");
    var close = form.querySelector("button[value='cancel']");
    var knownKeys = "go-fs-host-keys";
    // name is the file the dialog sends, step the step it is at, shown the
    // key the host showed, key the one accepted, and at the folder of the
    // host listed now
    var name = null;
    var step = "connect";
    var shown = null;
    var key = null;
    var at = null;
    // asked counts the questions put to the server, so that the answer to
    // one that was overtaken is dropped
    var asked = 0;
    // job is the send the dialog waits on until its upload begins
    var job = null;

    function field(name) {
      return form.elements[name];
    }

    // stored is the name of the server chosen, "" for the login typed in
    function stored() {
      return field("server").value;
    }

    // the protocol chosen for the login typed in
    function protocol() {
      var select = field("protocol");
      return select.options[select.selectedIndex] || null;
    }

    function defaultPort() {
      var chosen = protocol();
      return chosen ? parseInt(chosen.dataset.port, 10) || 0 : 22;
    }

    // whether the host of the login typed in shows a key to accept first
    function showsKey() {
      var chosen = protocol();
      return !chosen || chosen.dataset.hostkey === "true";
    }

    // whether the login typed in is an address and a token, an Artifactory
    function byToken() {
      var chosen = protocol();
      return chosen !== null && chosen.dataset.token === "true";
    }

    // The fields of the login typed in are left out, not only hidden, for a
    // stored server, so that the form does not ask for them; and so are those
    // of the login the protocol chosen does not ask for.
    function choose() {
      var server = stored() !== "";
      manual.hidden = server;
      manual.disabled = server;
      Array.prototype.forEach.call(manual.querySelectorAll("fieldset.login"), function (set) {
        set.hidden = (set.dataset.login === "token") !== byToken();
        set.disabled = set.hidden;
      });
    }

    function hostId() {
      var id = field("host").value.trim().toLowerCase() + ":" + (parseInt(field("port").value, 10) || defaultPort());
      // the keys of SFTP hosts were remembered before there was another
      return protocol().value === "sftp" ? id : protocol().value + "://" + id;
    }

    function known() {
      try {
        return JSON.parse(window.localStorage.getItem(knownKeys) || "{}") || {};
      } catch (ignored) {
        return {};
      }
    }

    // A key that cannot be remembered is only asked about again next time.
    function remember(id, fingerprint) {
      try {
        var keys = known();
        keys[id] = fingerprint;
        window.localStorage.setItem(knownKeys, JSON.stringify(keys));
      } catch (ignored) {
        // asked again
      }
    }

    function login(path) {
      if (stored()) {
        return { server: stored(), path: path };
      }
      return {
        protocol: protocol().value,
        host: field("host").value.trim(),
        port: parseInt(field("port").value, 10) || 0,
        username: field("username").value.trim(),
        password: field("password").value,
        url: field("url").value.trim(),
        token: field("token").value.trim(),
        hostKey: key,
        path: path
      };
    }

    function ask(action, body) {
      return askJSON("send", "POST", segment(name) + "?go-fs=" + action, body).then(function (res) {
        return res.json();
      });
    }

    // Once logged in, the fields give way to a line naming the login, and
    // their room to the folders of the host.
    function show(next) {
      step = next;
      if (next === "folder") {
        if (stored()) {
          loginText.textContent = stored();
        } else if (byToken()) {
          loginText.textContent = login().url;
        } else {
          var host = login().host;
          loginText.textContent = login().username + "@" +
            (host.indexOf(":") >= 0 ? "[" + host + "]" : host) + ":" + login().port;
        }
      }
      fields.hidden = next === "folder";
      connected.hidden = next !== "folder";
      keyBox.hidden = next !== "hostkey";
      remote.hidden = next !== "folder";
      connect.hidden = next !== "connect";
      accept.hidden = next !== "hostkey";
      go.hidden = next !== "folder";
    }

    function tell(message, bad) {
      progress.hidden = false;
      status.textContent = message;
      status.classList.toggle("bad", bad === true);
    }

    function quiet() {
      progress.hidden = true;
      status.textContent = "";
      status.classList.remove("bad");
    }

    // working is the dialog while it waits on an answer of the server: the
    // fields and the buttons that act are locked, Cancel is not
    function working(on) {
      form.classList.toggle("working", on);
      Array.prototype.forEach.call(form.querySelectorAll("input, select, .remote button"), function (input) {
        input.disabled = on;
      });
      [connect, accept, go, change].forEach(function (button) {
        button.disabled = on;
      });
      if (on) {
        bar.removeAttribute("value");
      } else {
        bar.value = 0;
        spinner.hidden = true;
      }
    }

    // busy is the dialog while a send it started has not begun its upload
    function busy(on) {
      working(on);
      go.hidden = on;
      stop.hidden = !on;
      close.textContent = on ? "Close" : "Cancel";
    }

    // what the dialog knows of the host is forgotten once the host or the
    // login is changed, and it starts over at the first step; a question
    // still open is dropped, so the form no longer waits on it
    function forget() {
      asked++;
      shown = null;
      key = null;
      at = null;
      working(false);
      quiet();
      show("connect");
    }

    function failure(ticket, err) {
      if (ticket !== asked) {
        return;
      }
      working(false);
      tell(err.message, true);
      // a login that was refused is corrected in the fields; a folder that
      // cannot be read leaves the one shown before
      if (!at) {
        show("connect");
      }
    }

    function connectHost() {
      // a stored server, and a host that shows no key, are logged in to at once
      if (stored() || !showsKey()) {
        browse("");
        return;
      }
      var ticket = ++asked;
      working(true);
      tell("Connecting…");
      ask("send-hostkey", { protocol: login().protocol, host: login().host, port: login().port }).then(function (view) {
        if (ticket !== asked) {
          return;
        }
        working(false);
        shown = view;
        var before = known()[hostId()];
        if (before === view.fingerprint) {
          key = view.fingerprint;
          browse("");
          return;
        }
        quiet();
        warning.hidden = !before;
        warning.textContent = before
          ? "The key of this host has changed since you last accepted it. That happens when the host was set up anew, " +
            "and when someone stands between it and this server. Do not accept it unless you know why it changed."
          : "";
        keyHost.textContent = view.host;
        keyType.textContent = view.keyType;
        keyPrint.textContent = view.fingerprint;
        show("hostkey");
        accept.focus();
      }).catch(function (err) {
        failure(ticket, err);
      });
    }

    function acceptKey() {
      remember(hostId(), shown.fingerprint);
      key = shown.fingerprint;
      browse("");
    }

    function browse(path) {
      var ticket = ++asked;
      working(true);
      // another folder of a host logged in to is waited on beside its path
      if (at) {
        quiet();
        spinner.hidden = false;
      } else {
        tell("Logging in…");
      }
      ask("send-browse", login(path)).then(function (view) {
        if (ticket !== asked) {
          return;
        }
        working(false);
        quiet();
        at = view;
        render(view);
        show("folder");
        go.focus();
      }).catch(function (err) {
        failure(ticket, err);
      });
    }

    function render(view) {
      var clash = renderFolders(crumbs, list, view, name);
      where.textContent = "Sends " + name + " to " + view.base + joinedPath(view.path, name) +
        (clash ? " · replaces the file there" : "");
    }

    remote.addEventListener("click", function (event) {
      var button = event.target.closest("button[data-path]");
      if (button && !button.disabled && !button.hasAttribute("aria-current") && !job) {
        browse(button.dataset.path);
      }
    });

    // wait asks after the send until its upload has begun, as the fetch
    // dialog does; one that could not begin is told here
    function wait() {
      var id = job;
      if (!id) {
        return;
      }
      askFetch("GET", fetchJobURL(id)).then(function (res) {
        return res.json();
      }).then(function (view) {
        if (job !== id) {
          return;
        }
        if (view.state === "running" && view.phase !== "downloading") {
          window.setTimeout(wait, 400);
          return;
        }
        job = null;
        busy(false);
        if (view.state === "running" || view.state === "done") {
          dialog.close();
          quiet();
          downloads.refresh();
          return;
        }
        if (view.state === "cancelled") {
          tell("Stopped.");
          return;
        }
        tell(view.message || "The send failed.", true);
        // told here, so the transfers need not tell it again
        askFetch("DELETE", fetchJobURL(id)).catch(function () {});
      }).catch(function (err) {
        if (job !== id) {
          return;
        }
        job = null;
        busy(false);
        tell(err.message, true);
      });
    }

    function start() {
      busy(true);
      tell("Connecting…");
      ask("send", login(at.path)).then(function (view) {
        job = view.id;
        wait();
      }).catch(function (err) {
        busy(false);
        tell(err.message, true);
      });
    }

    change.addEventListener("click", function () {
      forget();
      (stored() ? field("server") : field(byToken() ? "token" : "password")).focus();
    });

    field("server").addEventListener("change", function () {
      choose();
      forget();
    });

    // a port left at the default of one protocol follows to the next
    var lastDefault = defaultPort();
    field("protocol").addEventListener("change", function () {
      var port = field("port");
      if (!port.value || parseInt(port.value, 10) === lastDefault) {
        port.value = defaultPort();
      }
      lastDefault = defaultPort();
      choose();
      forget();
    });

    fields.addEventListener("input", function () {
      if (step !== "connect" || key) {
        forget();
      }
    });

    // Enter presses whichever button acts first in the form, hidden or not,
    // so what is done is decided by the step rather than by the button.
    form.addEventListener("submit", function (event) {
      var pressed = event.submitter ? event.submitter.value : "";
      if (pressed === "cancel") {
        return;
      }
      event.preventDefault();
      if (pressed === "stop") {
        if (job) {
          askFetch("DELETE", fetchJobURL(job)).catch(failed);
        }
        return;
      }
      if (job || form.classList.contains("working")) {
        return;
      }
      if (step === "connect") {
        connectHost();
      } else if (step === "hostkey") {
        acceptKey();
      } else if (at) {
        start();
      }
    });

    // Closed while the send has not begun its upload: it goes on, and the
    // transfers follow it from here. A question still open is dropped.
    dialog.addEventListener("close", function () {
      asked++;
      working(false);
      if (job) {
        job = null;
        busy(false);
        downloads.refresh();
      }
    });

    // The host and the login stay for the next file, which most likely goes
    // to the same place: that one opens on the folder this one went to.
    function open(row) {
      clear();
      name = row.dataset.name;
      what.textContent = "Uploads " + decodeURI(folder) + name + " to another host.";
      quiet();
      choose();
      dialog.showModal();
      if ((stored() || key || byToken()) && at) {
        browse(at.path);
        return;
      }
      forget();
      if (stored()) {
        field("server").focus();
      } else if (byToken()) {
        (field("url").value ? field("token") : field("url")).focus();
      } else {
        (field("host").value ? field("password") : field("host")).focus();
      }
    }

    return { open: open };
  })();

  // --- fetching from a stored server -----------------------------------
  //
  // The page of a stored server fetches one of its files, or one of its
  // folders with all that is below it, into the served tree: the dialog lists
  // the folders of that tree, from its root, and the server reads from the
  // stored server as a job like a fetch from a URL, which the transfers in
  // the header follow once it is under way. A folder the account may not
  // create in is listed, to pass through, but not offered to fetch into.
  //
  // The dialog says how large what it fetches is: a file by the size its row
  // shows, a folder once the server has walked it, which may take a while and
  // goes on while a folder to fetch into is chosen. Fetch is offered once
  // that is known and fits into the disk of the folder chosen; the server
  // checks it again before it reads a byte.
  var remoteFetching = (function () {
    var dialog = document.getElementById("remote-fetch-dialog");
    if (!dialog) {
      return { open: function () {} };
    }
    var form = dialog.querySelector("form");
    var title = dialog.querySelector("h2");
    var what = dialog.querySelector("p.what");
    var analysis = dialog.querySelector("p.analysis");
    var analysisText = analysis.querySelector(".text");
    var analysisSpinner = analysis.querySelector(".spinner");
    var local = dialog.querySelector(".remote");
    var crumbs = local.querySelector(".remote-path");
    var list = local.querySelector(".remote-list");
    var where = local.querySelector(".where");
    var space = local.querySelector(".space");
    var spinner = local.querySelector(".spinner");
    var progress = dialog.querySelector(".progress");
    var bar = progress.querySelector("progress");
    var status = progress.querySelector(".status");
    var go = form.querySelector("button[value='start']");
    var stop = form.querySelector("button[value='stop']");
    var close = form.querySelector("button[value='cancel']");
    // name is the file or folder the dialog fetches, isFolder whether it is
    // a folder, source the link that starts the fetch of it, and at the
    // folder listed now, which the next fetch opens on
    var name = null;
    var isFolder = false;
    var source = null;
    var at = null;
    // asked counts the folders asked for, so that the answer to one that was
    // overtaken is dropped
    var asked = 0;
    // measured is what the dialog fetches, as the server measured it, once
    // it has; measuring is the walk of a folder still under way, which a
    // dialog that closes stops
    var measured = null;
    var measuring = null;
    // job is the fetch the dialog waits on until its download begins
    var job = null;

    function tell(message, bad) {
      progress.hidden = false;
      status.textContent = message;
      status.classList.toggle("bad", bad === true);
    }

    function quiet() {
      progress.hidden = true;
      status.textContent = "";
      status.classList.remove("bad");
    }

    function plural(count, one) {
      return count + " " + one + (count === 1 ? "" : "s");
    }

    // fits says the folder listed has room for what is fetched, as far as
    // that can be told
    function fits() {
      return !measured || !at || at.free < 0 || measured.bytes <= at.free;
    }

    // Fetch is offered once a folder that may be fetched into is listed and
    // what is fetched is measured and fits into it
    function offer() {
      go.disabled = form.classList.contains("working") || !at || !at.writable ||
        (isFolder && !at.mkdir) || !measured || !fits();
    }

    function working(on) {
      form.classList.toggle("working", on);
      Array.prototype.forEach.call(local.querySelectorAll("button"), function (button) {
        button.disabled = on;
      });
      if (on) {
        bar.removeAttribute("value");
      } else {
        bar.value = 0;
        spinner.hidden = true;
      }
      offer();
    }

    // busy is the dialog while a fetch it started has not begun its download
    function busy(on) {
      working(on);
      go.hidden = on;
      stop.hidden = !on;
      close.textContent = on ? "Close" : "Cancel";
    }

    function analysed(text, bad) {
      analysisSpinner.hidden = true;
      analysisText.textContent = text;
      analysis.classList.toggle("bad", bad === true);
    }

    // describe says what the dialog fetches, once it is measured
    function describe(view) {
      if (!view.dir) {
        analysed(name + " · " + readableSize(view.bytes));
        return;
      }
      var text = plural(view.files, "file") +
        (view.folders > 0 ? " in " + plural(view.folders, "folder") : "") +
        " · " + readableSize(view.bytes);
      if (view.skipped > 0) {
        text += " · " + plural(view.skipped, "folder") + " cannot be read and " +
          (view.skipped === 1 ? "is" : "are") + " left out";
      }
      analysed(text);
    }

    function measure() {
      measuring = new AbortController();
      var signal = measuring.signal;
      var link = new URL(source, window.location.href);
      link.searchParams.set("go-fs", "remote-measure");
      analysisSpinner.hidden = false;
      analysisText.textContent = "Analysing " + name + "… this takes a while for a large folder.";
      analysis.classList.remove("bad");
      askJSON("remote-measure", "GET", link.pathname + link.search, undefined, signal).then(function (res) {
        return res.json();
      }).then(function (view) {
        if (signal.aborted) {
          return;
        }
        measuring = null;
        measured = view;
        describe(view);
        update();
      }).catch(function (err) {
        if (signal.aborted) {
          return;
        }
        measuring = null;
        analysed(err.message, true);
      });
    }

    function stopMeasuring() {
      if (measuring) {
        measuring.abort();
        measuring = null;
      }
    }

    // update says where what is fetched goes, and whether it fits there
    function update() {
      offer();
      if (!at) {
        return;
      }
      space.classList.remove("bad");
      space.textContent = "";
      if (!at.writable || !measured) {
        return;
      }
      var needed = readableSize(measured.bytes);
      if (at.free < 0) {
        space.textContent = "Needs " + needed + ".";
      } else if (!fits()) {
        space.textContent = "Not enough room: needs " + needed + ", and " + readableSize(at.free) +
          " is free in " + at.path + ".";
        space.classList.add("bad");
      } else {
        space.textContent = "Needs " + needed + " · " + readableSize(at.free) + " free.";
      }
    }

    function render(view) {
      var clash = renderFolders(crumbs, list, view, name);
      if (!view.writable || (isFolder && !view.mkdir)) {
        where.textContent = "You may not store " + (isFolder ? "folders" : "files") + " in " + view.path + ".";
      } else if (isFolder) {
        var there = (view.entries || []).some(function (entry) {
          return entry.dir && entry.name === name;
        });
        where.textContent = "Stores " + name + " as " + joinedPath(view.path, name) + "/" +
          (there ? " · fills in the folder there" : clash ? " · a file of that name is in the way" : "");
      } else {
        where.textContent = "Stores " + name + " as " + joinedPath(view.path, name) +
          (clash ? " · replaces the file there" : "");
      }
      update();
    }

    function browse(path) {
      var ticket = ++asked;
      working(true);
      quiet();
      spinner.hidden = false;
      askJSON("browse", "GET", "/?go-fs=local-browse&path=" + encodeURIComponent(path)).then(function (res) {
        return res.json();
      }).then(function (view) {
        if (ticket !== asked) {
          return;
        }
        at = view;
        working(false);
        render(view);
        (go.disabled ? close : go).focus();
      }).catch(function (err) {
        if (ticket !== asked) {
          return;
        }
        working(false);
        tell(err.message, true);
        // a folder that is gone is left for the root, once
        if (path !== "/" && !at) {
          browse("/");
        }
      });
    }

    local.addEventListener("click", function (event) {
      var button = event.target.closest("button[data-path]");
      if (button && !button.disabled && !button.hasAttribute("aria-current") && !job) {
        browse(button.dataset.path);
      }
    });

    // wait asks after the fetch until its download has begun, as Send File
    // does; one that could not begin is told here, such as one that does not
    // fit after all
    function wait() {
      var id = job;
      if (!id) {
        return;
      }
      askFetch("GET", fetchJobURL(id)).then(function (res) {
        return res.json();
      }).then(function (view) {
        if (job !== id) {
          return;
        }
        if (view.state === "running" && view.phase !== "downloading") {
          if (view.phase === "listing") {
            tell("Analysing " + name + " again before the fetch…");
          }
          window.setTimeout(wait, 400);
          return;
        }
        job = null;
        busy(false);
        if (view.state === "running" || view.state === "done") {
          dialog.close();
          quiet();
          downloads.refresh();
          return;
        }
        if (view.state === "cancelled") {
          tell("Stopped.");
          return;
        }
        tell(view.message || "The fetch failed.", true);
        // told here, so the transfers need not tell it again
        askFetch("DELETE", fetchJobURL(id)).catch(function () {});
      }).catch(function (err) {
        if (job !== id) {
          return;
        }
        job = null;
        busy(false);
        tell(err.message, true);
      });
    }

    function start() {
      busy(true);
      tell("Connecting…");
      askJSON("remote-fetch", "POST", source, { folder: at.path }).then(function (res) {
        return res.json();
      }).then(function (view) {
        job = view.id;
        wait();
      }).catch(function (err) {
        busy(false);
        tell(err.message, true);
      });
    }

    form.addEventListener("submit", function (event) {
      var pressed = event.submitter ? event.submitter.value : "";
      if (pressed === "cancel") {
        return;
      }
      event.preventDefault();
      if (pressed === "stop") {
        if (job) {
          askFetch("DELETE", fetchJobURL(job)).catch(failed);
        }
        return;
      }
      if (job || go.disabled) {
        return;
      }
      start();
    });

    // Closed while the fetch has not begun its download: it goes on, and the
    // transfers follow it from here. A folder still asked for, or still
    // measured, is dropped.
    dialog.addEventListener("close", function () {
      asked++;
      stopMeasuring();
      working(false);
      if (job) {
        job = null;
        busy(false);
        downloads.refresh();
      }
    });

    function open(row) {
      clear();
      stopMeasuring();
      name = row.dataset.name;
      isFolder = row.dataset.dir === "1";
      var link = new URL(segment(name), window.location.href);
      link.searchParams.set("go-fs", "remote-fetch");
      source = link.pathname + link.search;
      title.textContent = isFolder ? "Fetch Folder" : "Fetch File";
      what.textContent = isFolder
        ? "Downloads " + shown + name + " with all its subfolders and files into a folder of this server."
        : "Downloads " + shown + name + " into a folder of this server.";
      measured = null;
      space.textContent = "";
      if (isFolder) {
        measure();
      } else {
        measured = { dir: false, files: 1, bytes: Number(row.dataset.size) || 0 };
        describe(measured);
      }
      quiet();
      busy(false);
      dialog.showModal();
      browse(at ? at.path : "/");
    }

    return { open: open };
  })();

  // --- playing on Sonos ------------------------------------------------
  //
  // Play on Sonos in the menu of an audio file lists the groups of speakers
  // the server finds on its network. The one chosen plays the file: the
  // server tells it a link to fetch the file by, which needs no login and
  // runs out by itself. Several groups ticked play together: the others join
  // the one that leads, which the server groups them with as the Sonos app
  // does, until Ungroup splits that group into its rooms again. Under the
  // list is what the group that leads is doing, with buttons that pause it,
  // play on and stop it, and its volume.
  //
  // The bar opens the dialog on its queue, openQueue, and the server plays
  // that queue on from the file the bar plays. Either way the server plays
  // by itself once told, and the bar becomes the remote control of that.

  var sonosPlaying = (function () {
    var dialog = document.getElementById("sonos-dialog");
    if (!dialog) {
      return null;
    }
    var form = dialog.querySelector("form");
    var where = dialog.querySelector(".where");
    var list = dialog.querySelector(".sonos-groups");
    var none = dialog.querySelector(".sonos-none");
    var together = dialog.querySelector(".sonos-together");
    var ungroup = dialog.querySelector("[data-sonos='ungroup']");
    var spinner = dialog.querySelector(".sonos-head .spinner");
    var refresh = dialog.querySelector("[data-sonos='refresh']");
    var now = dialog.querySelector(".sonos-now");
    var playing = dialog.querySelector(".playing");
    var volume = document.getElementById("sonos-volume");
    var volumeShown = dialog.querySelector(".sonos-volume output");
    var status = dialog.querySelector(".status");
    var play = form.querySelector("button[value='play']");
    var heading = document.getElementById("sonos-title");
    var remembered = "go-fs-sonos-group";
    var states = { PLAYING: "Playing", PAUSED_PLAYBACK: "Paused", STOPPED: "Stopped", TRANSITIONING: "Starting" };
    // name is the file the dialog is open on, and base its path as a URL
    // path; files is the queue it plays on from there, names of files of its
    // folder, or null for the file alone. turn counts the times the dialog
    // was opened and closed, so that an answer to an earlier one is dropped
    var name = "";
    var base = "";
    var files = null;
    var turn = 0;
    var sliding = false;
    var later = null;

    function link(op, extra) {
      return base + "?go-fs=sonos&sonos=" + op + (extra || "");
    }

    function json(res) {
      return res.json();
    }

    function tell(text, bad) {
      status.textContent = text || "";
      status.classList.toggle("bad", !!bad);
    }

    function ticked() {
      return Array.prototype.slice.call(list.querySelectorAll("input:checked"));
    }

    // lead is the group ticked that the others join: the one chosen last,
    // which likely plays already, or else the first ticked
    function lead() {
      var inputs = ticked();
      var want = recalled();
      for (var i = 0; i < inputs.length; i++) {
        if (inputs[i].value === want) {
          return inputs[i];
        }
      }
      return inputs[0] || null;
    }

    function chosen() {
      var input = lead();
      return input ? input.value : "";
    }

    // others is the groups ticked that join the one that leads
    function others() {
      var first = chosen();
      return ticked().map(function (input) {
        return input.value;
      }).filter(function (id) {
        return id !== first;
      });
    }

    // marked shows which group leads, where several are ticked, and offers
    // Ungroup on a group of several rooms
    function marked() {
      var first = lead();
      var several = ticked().length > 1;
      list.querySelectorAll(".lead").forEach(function (tag) {
        tag.hidden = !several || !first || tag.parentNode !== first.parentNode;
      });
      ungroup.disabled = !first || first.dataset.rooms === "1";
      play.disabled = !first;
      if (several) {
        play.textContent = "Play together";
      } else if (files && files.length > 1) {
        play.textContent = "Play " + files.length + " files";
      } else {
        play.textContent = "Play this file";
      }
    }

    // the group chosen last is chosen again, in this browser
    function recalled() {
      try {
        return window.localStorage.getItem(remembered) || "";
      } catch (err) {
        return "";
      }
    }

    function remember(id) {
      try {
        window.localStorage.setItem(remembered, id);
      } catch (err) {
        // a browser that keeps nothing chooses the first group next time
      }
    }

    function showGroups(groups, again) {
      list.textContent = "";
      var want = recalled();
      var pick = null;
      groups.forEach(function (group) {
        var label = document.createElement("label");
        var input = document.createElement("input");
        input.type = "checkbox";
        input.name = "sonos-group";
        input.value = group.id;
        input.dataset.rooms = String((group.rooms || []).length || 1);
        var room = document.createElement("span");
        room.className = "room";
        room.textContent = group.name;
        label.appendChild(input);
        label.appendChild(room);
        if (group.rooms && group.rooms.length > 1) {
          var rooms = document.createElement("span");
          rooms.className = "meta";
          rooms.textContent = group.rooms.join(", ");
          label.appendChild(rooms);
        }
        var tag = document.createElement("span");
        tag.className = "lead";
        tag.textContent = "leads";
        tag.hidden = true;
        label.appendChild(tag);
        list.appendChild(label);
        if (!pick || group.id === want) {
          pick = input;
        }
      });
      none.hidden = groups.length > 0;
      together.hidden = groups.length < 2;
      if (pick) {
        pick.checked = true;
      }
      marked();
      return showState(again);
    }

    function loadGroups(fresh, again) {
      var mine = turn;
      spinner.hidden = false;
      refresh.disabled = true;
      return askJSON("sonos", "GET", link("groups", fresh ? "&refresh=1" : "")).then(json).then(function (view) {
        if (mine === turn) {
          return showGroups(view.groups || [], again);
        }
      }).catch(function (err) {
        if (mine === turn) {
          tell(err.message, true);
        }
      }).then(function () {
        if (mine === turn) {
          spinner.hidden = true;
          refresh.disabled = false;
        }
      });
    }

    function stateText(state) {
      var text = states[state.transport] || state.transport || "";
      if (state.title) {
        text += ": " + state.title;
      }
      return state.file ? text + " (this file)" : text;
    }

    // showState asks what the group chosen is doing, and asks again a moment
    // later, by when a speaker that was just told something has started
    function showState(again) {
      var id = chosen();
      var mine = turn;
      clearTimeout(later);
      if (!id) {
        now.disabled = true;
        playing.textContent = "";
        return Promise.resolve();
      }
      return askJSON("sonos", "GET", link("state", "&group=" + encodeURIComponent(id))).then(json).then(function (state) {
        if (mine !== turn || id !== chosen()) {
          return;
        }
        playing.textContent = stateText(state);
        if (!sliding) {
          volume.value = state.volume;
          volumeShown.value = state.volume;
        }
        now.disabled = false;
        if (again) {
          later = setTimeout(showState, 1500);
        }
      }).catch(function (err) {
        if (mine === turn && id === chosen()) {
          now.disabled = true;
          tell(err.message, true);
        }
      });
    }

    function command(op, body) {
      var id = chosen();
      if (!id) {
        return;
      }
      var mine = turn;
      body = body || {};
      body.group = id;
      tell("");
      askJSON("sonos", "POST", link(op), body).then(function () {
        if (mine === turn) {
          return showState(true);
        }
      }).catch(function (err) {
        if (mine === turn) {
          tell(err.message, true);
        }
      });
    }

    function open(row) {
      show(row.dataset.name, folder + encodeURIComponent(row.dataset.name), null, shown + row.dataset.name);
    }

    // openQueue opens the dialog on the queue of the bar: the files of a
    // folder, of which the one at index plays
    function openQueue(queue, index) {
      var track = queue[index];
      var path = track && typeof track.src === "string" ? track.src.split("?")[0] : "";
      if (path.charAt(0) !== "/" || path.charAt(1) === "/" || path.charAt(1) === "\\") {
        return;
      }
      var names = queue.map(function (item) {
        return String((item && item.name) || "");
      }).filter(function (item) {
        return sonosPlayable.test(item);
      });
      var after = names.length - 1 - names.indexOf(String(track.name));
      var text;
      try {
        text = decodeURIComponent(path);
      } catch (err) {
        text = path;
      }
      if (after > 0) {
        text += after === 1 ? ", then the next file" : ", then the " + after + " files after it";
      }
      show(String(track.name), path, names, text);
    }

    function show(file, at, queue, text) {
      turn++;
      name = file;
      base = at;
      files = queue;
      heading.textContent = files && files.length > 1 ? "Play folder on Sonos" : "Play on Sonos";
      where.textContent = text;
      tell("");
      list.textContent = "";
      none.hidden = true;
      together.hidden = true;
      now.disabled = true;
      playing.textContent = "";
      play.disabled = true;
      dialog.showModal();
      loadGroups(false);
    }

    refresh.addEventListener("click", function () {
      tell("");
      loadGroups(true);
    });

    list.addEventListener("change", function () {
      tell("");
      marked();
      showState();
    });

    ungroup.addEventListener("click", function () {
      var id = chosen();
      if (!id) {
        return;
      }
      var mine = turn;
      ungroup.disabled = true;
      tell("");
      askJSON("sonos", "POST", link("ungroup"), { group: id }).catch(function (err) {
        if (mine === turn) {
          tell(err.message, true);
        }
      }).then(function () {
        // the list shows what is grouped now, also where some room did not
        // leave
        if (mine === turn) {
          return loadGroups(false);
        }
      });
    });

    dialog.querySelectorAll("[data-sonos='resume'], [data-sonos='pause'], [data-sonos='stop']").forEach(function (button) {
      button.addEventListener("click", function () {
        command(button.dataset.sonos);
      });
    });

    volume.addEventListener("input", function () {
      sliding = true;
      volumeShown.value = volume.value;
    });

    volume.addEventListener("change", function () {
      sliding = false;
      command("volume", { volume: parseInt(volume.value, 10) || 0 });
    });

    // The submit event rather than close, for the reason the other dialogs
    // give: which button was pressed is only known there.
    form.addEventListener("submit", function (event) {
      var pressed = event.submitter ? event.submitter.value : "play";
      if (pressed === "cancel") {
        return;
      }
      event.preventDefault();
      var id = chosen();
      if (!id || play.disabled) {
        return;
      }
      var mine = turn;
      var join = others();
      play.disabled = true;
      tell((join.length ? "Grouping the speakers to play " : "Telling the speaker to play ") + name + "…");
      var asked = { group: id, join: join };
      if (files) {
        asked.files = files;
      }
      askJSON("sonos", "POST", link("play"), asked).then(json).then(function (answer) {
        if (mine !== turn) {
          return;
        }
        remember(answer.group);
        var failed = answer.failed || [];
        // the bar is the remote control of what the speakers play now; the
        // dialog stays open only to say which rooms did not join
        if (audio && audio.remote && answer.session) {
          audio.remote(answer.session);
          if (!failed.length) {
            dialog.close();
            return;
          }
        }
        if (failed.length) {
          tell("Playing on " + answer.name + ", without: " + failed.join(" "), true);
        } else {
          tell("Playing on " + answer.name + ".");
        }
        // a group that others joined is listed as one now
        if (join.length) {
          return loadGroups(false, true);
        }
        return showState(true);
      }).catch(function (err) {
        if (mine === turn) {
          tell(err.message, true);
        }
      }).then(function () {
        if (mine === turn) {
          marked();
        }
      });
    });

    dialog.addEventListener("close", function () {
      turn++;
      clearTimeout(later);
      name = "";
      base = "";
      files = null;
    });

    return { open: open, openQueue: openQueue };
  })();

  // --- editing MP3 tags ------------------------------------------------
  //
  // Edit MP3 Tag in the menu of an MP3 file, and Edit MP3 Tags in the header
  // for every MP3 file of the folder, open the editor over the page: the
  // fields of the tag, the cover and the volume. The server keeps every other
  // tag of a file as it is, and changes the volume without re-encoding, in
  // steps of 1.5 dB that the same change the other way undoes exactly.

  var mp3Editing = (function () {
    var single = document.getElementById("mp3-dialog");
    if (!single) {
      return null;
    }
    var several = document.getElementById("mp3-folder-dialog");
    var stepDB = 5 * Math.log(2) / Math.LN10;
    var fieldNames = ["title", "artist", "albumArtist", "album", "year", "genre", "track"];
    var fieldLabels = {
      title: "Title", artist: "Artist", albumArtist: "Album Artist", album: "Album",
      year: "Year", genre: "Genre", track: "Track"
    };
    var maxCover = 10 * 1024 * 1024;
    var mp3Name = /\.mp3$/i;

    function isMP3(row) {
      return row.dataset.dir !== "1" && mp3Name.test(row.dataset.name);
    }

    // link is where the editor asks about a file of this folder, or about the
    // folder itself for the name ""
    function link(name, part) {
      return remote
        ? remote + encodeURIComponent(name) + "&mp3tags=" + part
        : folder + encodeURIComponent(name) + "?go-fs=mp3tags&mp3tags=" + part;
    }

    function signed(n) {
      return (n > 0 ? "+" : n < 0 ? "−" : "") + Math.abs(n);
    }

    function decibels(steps) {
      var db = steps * stepDB;
      var size = Math.abs(db).toFixed(1);
      return (size === "0.0" ? "±" : db > 0 ? "+" : "−") + size + " dB";
    }

    function stepsOf(input) {
      return parseInt(input.value, 10) || 0;
    }

    function loudnessOf(change) {
      return change.loudness === null ? "silence" : change.loudness.toFixed(1).replace("-", "−") + " LUFS";
    }

    // measuredText says what a change to the target loudness was planned on
    function measuredText(change) {
      return "Measured " + loudnessOf(change) + ", peak " + change.peak.toFixed(2) +
        ". Reaching −18 LUFS takes " + decibels(change.steps) + " (" + signed(change.steps) + " steps)" +
        (change.limited ? ", limited to keep the audio from clipping" : "") + ".";
    }

    function appliedText(gain) {
      var text = gain.steps
        ? "Changed by " + decibels(gain.steps) + " (" + signed(gain.steps) + " steps) so far."
        : "Not changed so far.";
      if (gain.trackGain) {
        text += " ReplayGain of the track " + gain.trackGain + (gain.albumGain ? ", of the album " + gain.albumGain : "") + ".";
      }
      return text;
    }

    function json(res) {
      return res.json();
    }

    function load(name) {
      return askJSON("mp3tags", "GET", link(name, "tags")).then(json);
    }

    function analyze(name) {
      return askJSON("mp3tags", "POST", link(name, "analyze"), {}).then(json);
    }

    function planAlbum(names) {
      return askJSON("mp3tags", "POST", link("", "album"), { names: names }).then(json);
    }

    // save sends an edit on the condition that the file is still the one
    // the editor read, so that a change made meanwhile is not overwritten
    function save(name, modified, edit) {
      return fetch(link(name, "tags"), {
        method: "POST",
        headers: { "Content-Type": "application/json", "If-Unmodified-Since": modified },
        credentials: "same-origin",
        body: JSON.stringify(edit)
      }).then(function (res) {
        if (res.ok) {
          return null;
        }
        return res.text().then(function (text) {
          throw new Error(reason("mp3tags", res.status, text.trim()));
        });
      });
    }

    // readCover reads an image picked to be the cover: as the save sends it,
    // and as the editor shows it until then
    function readCover(file) {
      return new Promise(function (resolve, reject) {
        if (!/^image\/(jpeg|png)$/.test(file.type)) {
          reject(new Error("The cover must be a JPEG or PNG image."));
          return;
        }
        if (file.size > maxCover) {
          reject(new Error("The cover must be at most 10 MiB."));
          return;
        }
        var reader = new FileReader();
        reader.onload = function () {
          var url = String(reader.result);
          resolve({ url: url, data: url.slice(url.indexOf(",") + 1) });
        };
        reader.onerror = function () {
          reject(new Error("The image could not be read."));
        };
        reader.readAsDataURL(file);
      });
    }

    // picker opens the file chooser of a dialog, and hands the image picked
    // to the function the latest call named
    function picker(input, failure) {
      var pending = null;
      input.addEventListener("change", function () {
        var file = input.files && input.files[0];
        input.value = "";
        if (file && pending) {
          readCover(file).then(pending).catch(failure);
        }
      });
      return function (then) {
        pending = then;
        input.click();
      };
    }

    // inOrder runs work on each item, one after the other, until the dialog
    // it was started for is closed
    function inOrder(list, work, alive) {
      return list.reduce(function (chain, item, i) {
        return chain.then(function () {
          return alive() ? work(item, i) : null;
        });
      }, Promise.resolve());
    }

    // --- one file ---

    var one = (function () {
      var form = single.querySelector("form");
      var where = single.querySelector(".where");
      var status = single.querySelector(".status");
      var image = single.querySelector(".art img");
      var none = single.querySelector(".art .none");
      var removeCover = single.querySelector("[data-cover='remove']");
      var replaceCover = single.querySelector("[data-cover='replace']");
      var applied = single.querySelector(".applied");
      var measured = single.querySelector(".measured");
      var steps = form.elements.namedItem("steps");
      var db = single.querySelector(".db");
      var analyzing = single.querySelector("[data-do='analyze']");
      var suggest = single.querySelector("[data-do='suggest']");
      var saving = form.querySelector("button[value='save']");
      // state is the file the dialog is open for, null once it closed
      var state = null;

      function tell(message, bad) {
        status.textContent = message || "";
        status.classList.toggle("bad", bad === true);
      }

      var pick = picker(single.querySelector("input[type='file']"), function (err) {
        tell(err.message, true);
      });

      function field(key) {
        return form.elements.namedItem(key);
      }

      function showCover(url) {
        image.hidden = !url;
        none.hidden = !!url;
        if (url) {
          image.src = url;
        } else {
          image.removeAttribute("src");
        }
        removeCover.disabled = !url;
      }

      function showSteps() {
        db.textContent = decibels(stepsOf(steps));
      }

      function busy(on) {
        saving.disabled = on;
        analyzing.disabled = on;
        replaceCover.disabled = on;
      }

      function open(row) {
        var name = row.dataset.name;
        var asked = { name: name, info: null, cover: null, analysis: null };
        state = asked;
        form.reset();
        where.textContent = shown + name;
        showCover(null);
        applied.textContent = "";
        measured.textContent = "";
        suggest.hidden = true;
        steps.removeAttribute("min");
        steps.removeAttribute("max");
        showSteps();
        busy(true);
        tell("Reading the tag…");
        single.showModal();
        load(name).then(function (info) {
          if (state !== asked) {
            return;
          }
          asked.info = info;
          fieldNames.forEach(function (key) {
            field(key).value = info.fields[key] || "";
          });
          showCover(info.cover ? link(name, "cover") : null);
          applied.textContent = appliedText(info.gain);
          if (info.measured) {
            measured.textContent = "Measured before: Analyze shows it at once.";
          }
          busy(false);
          tell("");
          field("title").focus();
        }).catch(function (err) {
          if (state === asked) {
            tell(err.message, true);
          }
        });
      }

      replaceCover.addEventListener("click", function () {
        var asked = state;
        pick(function (picked) {
          if (state === asked && asked) {
            asked.cover = { data: picked.data };
            showCover(picked.url);
            tell("");
          }
        });
      });

      removeCover.addEventListener("click", function () {
        if (state) {
          state.cover = { remove: true };
          showCover(null);
        }
      });

      analyzing.addEventListener("click", function () {
        var asked = state;
        if (!asked || !asked.info) {
          return;
        }
        busy(true);
        tell("");
        measured.textContent = "Measuring the loudness…";
        analyze(asked.name).then(function (analysis) {
          if (state !== asked) {
            return;
          }
          asked.analysis = analysis;
          steps.min = -analysis.maxDown;
          steps.max = analysis.maxUp;
          measured.textContent = measuredText(analysis.track);
          suggest.hidden = false;
          busy(false);
        }).catch(function (err) {
          if (state === asked) {
            measured.textContent = "";
            busy(false);
            tell(err.message, true);
          }
        });
      });

      suggest.addEventListener("click", function () {
        if (state && state.analysis) {
          steps.value = state.analysis.track.steps;
          showSteps();
        }
      });

      steps.addEventListener("input", showSteps);

      // The submit event rather than close, for the reason the other dialogs
      // give: which button was pressed is only known there.
      form.addEventListener("submit", function (event) {
        var pressed = event.submitter ? event.submitter.value : "save";
        if (pressed === "cancel") {
          return;
        }
        event.preventDefault();
        var asked = state;
        if (!asked || !asked.info || saving.disabled) {
          return;
        }
        var edit = { fields: {}, gain: { steps: stepsOf(steps), replayGain: !!asked.analysis } };
        fieldNames.forEach(function (key) {
          edit.fields[key] = field(key).value.trim();
        });
        if (asked.cover) {
          edit.cover = asked.cover;
        }
        busy(true);
        tell(edit.gain.steps ? "Saving and changing the volume…" : "Saving…");
        save(asked.name, asked.info.modified, edit).then(function () {
          if (state === asked) {
            single.close();
          }
          done();
        }).catch(function (err) {
          if (state === asked) {
            busy(false);
            tell(err.message, true);
          }
        });
      });

      single.addEventListener("close", function () {
        state = null;
      });

      return { open: open };
    })();

    // --- every file of the folder ---

    var all = several && (function () {
      var form = several.querySelector("form");
      var filesBody = several.querySelector("tbody.files");
      var allRow = several.querySelector("tbody.all");
      var status = several.querySelector(".status");
      var progress = several.querySelector(".progress");
      var bar = progress.querySelector("progress");
      var measured = several.querySelector(".measured");
      var offset = form.elements.namedItem("offset");
      var db = several.querySelector(".db");
      var analyzing = several.querySelector("[data-do='analyze']");
      var saving = form.querySelector("button[value='save']");
      // files are the rows of the table, each with what the server said of
      // its file; album is the change planned for all of them as an album
      var files = [];
      var album = null;
      // session counts the times the dialog was opened, so that what an
      // earlier one started stops once it closed
      var session = 0;
      var savedAny = false;

      function tell(message, bad) {
        status.textContent = message || "";
        status.classList.toggle("bad", bad === true);
      }

      var pick = picker(several.querySelector("input[type='file']"), function (err) {
        tell(err.message, true);
      });

      function mode() {
        var checked = form.querySelector("input[name='mode']:checked");
        return checked ? checked.value : "track";
      }

      function busy(on) {
        saving.disabled = on;
        analyzing.disabled = on;
      }

      // planned is the change a save makes to the volume of a file
      function planned(file) {
        var base = 0;
        if (file.analysis) {
          base = mode() === "album" && album ? album.steps : file.analysis.track.steps;
        }
        return base + stepsOf(offset);
      }

      function showThumb(file, url) {
        file.thumb.textContent = "";
        if (url) {
          var img = document.createElement("img");
          img.alt = "";
          img.src = url;
          file.thumb.appendChild(img);
        } else {
          var none = document.createElement("span");
          none.className = "none";
          none.textContent = "—";
          file.thumb.appendChild(none);
        }
      }

      function showVolume(file) {
        var cell = file.volume;
        cell.textContent = "";
        cell.classList.remove("bad");
        if (file.problem) {
          cell.textContent = file.problem;
          cell.classList.add("bad");
          return;
        }
        if (!file.info) {
          return;
        }
        var now = document.createElement("span");
        now.title = "Changed so far; after the arrow, the change a save makes";
        now.textContent = file.info.gain.steps ? decibels(file.info.gain.steps) : "—";
        cell.appendChild(now);
        var change = planned(file);
        if (change !== 0 || file.analysis) {
          var next = document.createElement("span");
          next.className = "planned";
          next.textContent = " → " + decibels(change);
          if (file.analysis) {
            next.title = measuredText(file.analysis.track);
          }
          cell.appendChild(next);
        }
      }

      function mark(file, key) {
        var input = file.inputs[key];
        input.classList.toggle("changed", !!file.info && input.value.trim() !== (file.info.fields[key] || ""));
      }

      function smallButton(label, icon, then) {
        var button = document.createElement("button");
        button.type = "button";
        button.className = "plain small";
        button.title = label;
        button.setAttribute("aria-label", label);
        var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
        svg.setAttribute("class", "ic");
        var use = document.createElementNS("http://www.w3.org/2000/svg", "use");
        use.setAttribute("href", "#" + icon);
        svg.appendChild(use);
        button.appendChild(svg);
        button.addEventListener("click", then);
        return button;
      }

      function rowOf(name) {
        var file = { name: name, info: null, cover: null, analysis: null, problem: "", inputs: {} };
        var tr = document.createElement("tr");
        var th = document.createElement("th");
        th.scope = "row";
        th.textContent = name;
        th.title = name;
        tr.appendChild(th);
        fieldNames.forEach(function (key) {
          var td = document.createElement("td");
          var input = document.createElement("input");
          input.type = "text";
          input.autocomplete = "off";
          input.disabled = true;
          input.dataset.field = key;
          input.setAttribute("aria-label", fieldLabels[key] + " of " + name);
          if (key === "genre") {
            input.setAttribute("list", "mp3-genres");
          }
          input.addEventListener("input", function () {
            mark(file, key);
          });
          td.appendChild(input);
          tr.appendChild(td);
          file.inputs[key] = input;
        });
        var cover = document.createElement("td");
        cover.className = "cover";
        var cell = document.createElement("div");
        cell.className = "cell";
        file.thumb = document.createElement("span");
        file.thumb.className = "thumb";
        cell.appendChild(file.thumb);
        cell.appendChild(smallButton("Replace the cover of " + name, "i-upload", function () {
          pick(function (picked) {
            setCover(file, { data: picked.data }, picked.url);
          });
        }));
        cell.appendChild(smallButton("Remove the cover of " + name, "i-close", function () {
          setCover(file, { remove: true }, null);
        }));
        cover.appendChild(cell);
        tr.appendChild(cover);
        file.volume = document.createElement("td");
        file.volume.className = "volume";
        tr.appendChild(file.volume);
        file.row = tr;
        showThumb(file, null);
        return file;
      }

      function setCover(file, cover, url) {
        if (!file.info) {
          return;
        }
        file.cover = cover;
        showThumb(file, url);
      }

      function fill(file, info) {
        file.info = info;
        file.cover = null;
        file.analysis = null;
        file.problem = "";
        fieldNames.forEach(function (key) {
          file.inputs[key].value = info.fields[key] || "";
          file.inputs[key].disabled = false;
          mark(file, key);
        });
        showThumb(file, info.cover ? link(file.name, "cover") + "&at=" + encodeURIComponent(info.modified) : null);
        showVolume(file);
      }

      function open() {
        session++;
        var mine = session;
        files = [];
        album = null;
        savedAny = false;
        filesBody.textContent = "";
        form.reset();
        measured.textContent = "";
        progress.hidden = true;
        db.textContent = decibels(0);
        rows().filter(isMP3).forEach(function (row) {
          var file = rowOf(row.dataset.name);
          files.push(file);
          filesBody.appendChild(file.row);
        });
        busy(true);
        tell("Reading the tags of " + files.length + (files.length === 1 ? " file…" : " files…"));
        several.showModal();
        inOrder(files, function (file) {
          return load(file.name).then(function (info) {
            fill(file, info);
          }).catch(function (err) {
            file.problem = err.message;
            file.row.classList.add("failed");
            showVolume(file);
          });
        }, function () {
          return mine === session;
        }).then(function () {
          if (mine === session) {
            busy(false);
            tell("");
            measured.textContent = measuredBefore();
          }
        });
      }

      // measuredBefore says how many files were measured before, which
      // Analyze all shows at once
      function measuredBefore() {
        var count = files.filter(function (file) {
          return file.info && file.info.measured;
        }).length;
        if (!count) {
          return "";
        }
        if (count === files.length) {
          return (count === 1 ? "The file was" : "All " + count + " files were") +
            " measured before: Analyze all shows them at once.";
        }
        return count + " of " + files.length + " files were measured before: Analyze all reuses " +
          (count === 1 ? "that measurement." : "those measurements.");
      }

      // A value typed into the first row is every file's; emptied again,
      // every file has its own value back.
      allRow.addEventListener("input", function (event) {
        var key = event.target.dataset.field;
        if (!key) {
          return;
        }
        var value = event.target.value;
        files.forEach(function (file) {
          if (file.info) {
            file.inputs[key].value = value.trim() ? value : file.info.fields[key] || "";
            mark(file, key);
          }
        });
      });

      allRow.addEventListener("click", function (event) {
        var button = event.target.closest("button");
        if (!button) {
          return;
        }
        var loaded = files.filter(function (file) {
          return file.info;
        });
        if (button.dataset.do === "number") {
          loaded.forEach(function (file, i) {
            file.inputs.track.value = (i + 1) + "/" + loaded.length;
            mark(file, "track");
          });
        } else if (button.dataset.cover === "replace") {
          pick(function (picked) {
            loaded.forEach(function (file) {
              setCover(file, { data: picked.data }, picked.url);
            });
          });
        } else if (button.dataset.cover === "remove") {
          loaded.forEach(function (file) {
            setCover(file, { remove: true }, null);
          });
        }
      });

      offset.addEventListener("input", function () {
        db.textContent = decibels(stepsOf(offset));
        files.forEach(showVolume);
      });

      function summary() {
        if (mode() === "album" && album) {
          return "The album measures " + loudnessOf(album) + ", peak " + album.peak.toFixed(2) +
            ". Reaching −18 LUFS takes " + decibels(album.steps) + " (" + signed(album.steps) +
            " steps) on every file" + (album.limited ? ", limited to keep the audio from clipping" : "") + ".";
        }
        return "Each file is planned to reach −18 LUFS on its own; the Volume column shows the change.";
      }

      // measure analyzes the files not analyzed yet and, for an album, plans
      // its change
      function measure() {
        var mine = session;
        var alive = function () {
          return mine === session;
        };
        var todo = files.filter(function (file) {
          return file.info && !file.analysis;
        });
        var failures = 0;
        busy(true);
        progress.hidden = false;
        bar.max = Math.max(1, todo.length);
        bar.value = 0;
        inOrder(todo, function (file, i) {
          tell("Measuring " + (i + 1) + " of " + todo.length + ": " + file.name);
          return analyze(file.name).then(function (analysis) {
            file.analysis = analysis;
          }).catch(function (err) {
            failures++;
            file.problem = err.message;
          }).then(function () {
            bar.value = i + 1;
            showVolume(file);
          });
        }, alive).then(function () {
          album = null;
          var names = files.filter(function (file) {
            return file.analysis;
          }).map(function (file) {
            return file.name;
          });
          if (!alive() || mode() !== "album" || !names.length) {
            return null;
          }
          tell("Planning the album…");
          return planAlbum(names).then(function (plan) {
            album = plan;
          });
        }).then(function () {
          if (!alive()) {
            return;
          }
          progress.hidden = true;
          busy(false);
          files.forEach(showVolume);
          measured.textContent = summary();
          tell(failures ? failures + (failures === 1 ? " file" : " files") + " could not be measured; the Volume column says why." : "",
            failures > 0);
        }).catch(function (err) {
          if (alive()) {
            progress.hidden = true;
            busy(false);
            tell(err.message, true);
          }
        });
      }

      analyzing.addEventListener("click", measure);

      // the mode decides what a measured file changes by, and an album is
      // planned once its files are measured
      form.addEventListener("change", function (event) {
        if (event.target.name !== "mode") {
          return;
        }
        if (files.some(function (file) {
          return file.analysis;
        })) {
          measure();
        } else {
          files.forEach(showVolume);
        }
      });

      // editOf is what a save sends for a file, null for one that does not
      // change
      function editOf(file) {
        var fields = {};
        var changed = false;
        fieldNames.forEach(function (key) {
          fields[key] = file.inputs[key].value.trim();
          changed = changed || fields[key] !== (file.info.fields[key] || "");
        });
        var edit = { gain: { steps: planned(file), replayGain: !!file.analysis } };
        if (changed) {
          edit.fields = fields;
        }
        if (file.cover) {
          edit.cover = file.cover;
        }
        if (mode() === "album" && album && file.analysis) {
          edit.gain.album = { loudness: album.loudness, peak: album.peak };
        }
        return changed || file.cover || edit.gain.steps !== 0 || file.analysis ? edit : null;
      }

      form.addEventListener("submit", function (event) {
        var pressed = event.submitter ? event.submitter.value : "save";
        if (pressed === "cancel") {
          return;
        }
        event.preventDefault();
        if (saving.disabled) {
          return;
        }
        var mine = session;
        var alive = function () {
          return mine === session;
        };
        var jobs = files.filter(function (file) {
          return file.info;
        }).map(function (file) {
          return { file: file, edit: editOf(file) };
        }).filter(function (job) {
          return job.edit;
        });
        if (!jobs.length) {
          tell("Nothing was changed.");
          return;
        }
        var failures = 0;
        busy(true);
        progress.hidden = false;
        bar.max = jobs.length;
        bar.value = 0;
        inOrder(jobs, function (job, i) {
          var file = job.file;
          tell("Saving " + (i + 1) + " of " + jobs.length + ": " + file.name);
          file.row.classList.remove("failed", "saved");
          return save(file.name, file.info.modified, job.edit).then(function () {
            savedAny = true;
            file.row.classList.add("saved");
            // read again, so that the file can be saved once more from here
            return load(file.name).then(function (info) {
              fill(file, info);
            });
          }).catch(function (err) {
            failures++;
            file.problem = err.message;
            file.row.classList.add("failed");
            showVolume(file);
          }).then(function () {
            bar.value = i + 1;
          });
        }, alive).then(function () {
          if (!alive()) {
            return;
          }
          progress.hidden = true;
          busy(false);
          if (!failures) {
            several.close();
            return;
          }
          album = null;
          measured.textContent = "";
          tell(failures + " of " + jobs.length + " files could not be saved; the Volume column says why.", true);
        });
      });

      several.addEventListener("close", function () {
        session++;
        if (savedAny) {
          done();
        }
      });

      return { open: open };
    })();

    var button = document.getElementById("mp3-tags");
    if (button && all) {
      button.addEventListener("click", function () {
        clear();
        all.open();
      });
    }

    return {
      open: function (row) {
        if (isMP3(row)) {
          one.open(row);
        }
      }
    };
  })();

  // --- uploading -------------------------------------------------------
  //
  // XMLHttpRequest rather than fetch, because only it reports how far an
  // upload has got.

  var drop = document.getElementById("drop");
  if (!drop) {
    return;
  }
  var picker = document.getElementById("picker");
  var queue = document.getElementById("queue");

  document.getElementById("upload").addEventListener("click", function () {
    picker.click();
  });
  picker.addEventListener("change", function () {
    accept(picker.files);
    picker.value = "";
  });

  // The overlay is shown only for a drag that carries files, so dragging
  // text around the page, into the filter say, goes on as normal. depth
  // counts the elements the drag is inside of: every one it enters is left
  // again, and it has left the window once none are left.
  var depth = 0;
  function carriesFiles(event) {
    return !!event.dataTransfer &&
      Array.prototype.indexOf.call(event.dataTransfer.types, "Files") !== -1;
  }
  function hideDrop() {
    depth = 0;
    drop.hidden = true;
  }
  document.addEventListener("dragenter", function (event) {
    if (!carriesFiles(event)) {
      return;
    }
    event.preventDefault();
    depth++;
    drop.hidden = false;
  });
  document.addEventListener("dragover", function (event) {
    if (carriesFiles(event)) {
      event.preventDefault();
    }
  });
  document.addEventListener("dragleave", function (event) {
    if (!carriesFiles(event)) {
      return;
    }
    depth = Math.max(0, depth - 1);
    if (depth === 0) {
      drop.hidden = true;
    }
  });
  document.addEventListener("dragend", hideDrop);
  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") {
      hideDrop();
    }
  });
  document.addEventListener("drop", function (event) {
    event.preventDefault();
    hideDrop();
    if (event.dataTransfer && event.dataTransfer.files.length) {
      accept(event.dataTransfer.files);
    }
  });

  function accept(files) {
    clear();
    var pending = Array.prototype.slice.call(files);
    if (!pending.length) {
      return;
    }
    queue.hidden = false;
    // every file gets its own reason: dropping ten at once and being told only
    // why the last one failed says nothing about the other nine
    var problems = [];
    function step() {
      if (!pending.length) {
        if (problems.length === 0) {
          done();
          return;
        }
        // no reload, because it would take the reasons with it
        say(problems.join("\n"));
        return;
      }
      put(pending.shift(), function (problem) {
        if (problem) {
          problems.push(problem);
        }
        step();
      });
    }
    step();
  }

  function put(file, then) {
    var line = document.createElement("li");
    var label = document.createElement("span");
    label.className = "what";
    label.textContent = file.name;
    var track = document.createElement("span");
    track.className = "track";
    var fill = document.createElement("span");
    fill.className = "fill";
    track.appendChild(fill);
    var state = document.createElement("span");
    state.className = "state";
    state.textContent = "0%";
    line.appendChild(label);
    line.appendChild(track);
    line.appendChild(state);
    queue.appendChild(line);

    function progress(fraction) {
      var percent = Math.round(fraction * 100);
      fill.style.width = percent + "%";
      state.textContent = percent + "%";
    }
    function succeed() {
      fill.style.width = "100%";
      state.textContent = "done";
      then(null);
    }
    function fail(problem) {
      line.classList.add("failed");
      fill.style.width = "100%";
      state.textContent = "failed";
      line.title = problem;
      then(problem);
    }

    if (!maxChunkSize || file.size <= maxChunkSize) {
      putWhole(file, progress, succeed, fail);
      return;
    }
    putChunked(file, progress, succeed, fail);
  }

  // putWhole sends a file as a single request, exactly as every upload was
  // sent before chunking existed: still the path for anything at or under
  // maxChunkSize, and for every upload when chunking is off.
  function putWhole(file, progress, succeed, fail) {
    var request = new XMLHttpRequest();
    request.open("PUT", segment(file.name));
    request.setRequestHeader("Content-Type", "application/octet-stream");
    request.withCredentials = true;
    request.upload.addEventListener("progress", function (event) {
      if (event.lengthComputable) {
        progress(event.loaded / event.total);
      }
    });
    request.addEventListener("load", function () {
      if (request.status >= 200 && request.status < 300) {
        succeed();
        return;
      }
      fail(file.name + ": " + reason("upload", request.status, request.responseText.trim()));
    });
    request.addEventListener("error", function () {
      fail(file.name + ": the upload could not be sent.");
    });
    request.send(file);
  }

  // chunkRetries is how many times the current chunk of a chunked upload is
  // retried before the whole file is reported as failed. A chunk that lands
  // stays landed on the server (its staging file only grows, never shrinks
  // below a completed chunk's end), so retrying is always safe: it never
  // restarts the file from the beginning.
  var chunkRetries = 3;

  // putChunked sends a file as consecutive Content-Range PUTs of at most
  // maxChunkSize each, to the same URL a whole-file PUT uses. The server's
  // staging file is the only record of how far the upload has gotten; a 416
  // means the server's idea of that offset differs from this one (an earlier
  // attempt's chunk landing after this page stopped waiting for it, most
  // likely), and is answered by resuming from the offset the response names,
  // once, rather than treating a resync as a failed upload.
  function putChunked(file, progress, succeed, fail) {
    var sent = 0;
    var retries = 0;
    var resynced = false;

    function sendFrom(start) {
      var end = Math.min(start + maxChunkSize, file.size);

      function retry(problem) {
        if (retries < chunkRetries) {
          retries++;
          sendFrom(start);
          return;
        }
        fail(problem);
      }

      var request = new XMLHttpRequest();
      request.open("PUT", segment(file.name));
      request.setRequestHeader("Content-Type", "application/octet-stream");
      request.setRequestHeader("Content-Range",
        "bytes " + start + "-" + (end - 1) + "/" + file.size);
      request.withCredentials = true;
      request.upload.addEventListener("progress", function (event) {
        if (event.lengthComputable) {
          progress((sent + event.loaded) / file.size);
        }
      });
      request.addEventListener("load", function () {
        if (request.status >= 200 && request.status < 300) {
          sent = end;
          retries = 0;
          progress(sent / file.size);
          if (sent >= file.size) {
            succeed();
          } else {
            sendFrom(sent);
          }
          return;
        }
        if (request.status === 416 && !resynced) {
          var have = resumeOffset(request.getResponseHeader("Content-Range"));
          if (have !== null && have <= file.size) {
            resynced = true;
            sent = have;
            sendFrom(have);
            return;
          }
        }
        retry(file.name + ": " + reason("upload", request.status, request.responseText.trim()));
      });
      request.addEventListener("error", function () {
        retry(file.name + ": the upload could not be sent.");
      });
      request.send(file.slice(start, end));
    }

    sendFrom(0);
  }

  // resumeOffset reads the size a 416 names in its own Content-Range, the
  // "bytes */<size>" form the server answers an out-of-sync chunk with.
  function resumeOffset(header) {
    var match = /^bytes \*\/(\d+)$/.exec(header || "");
    return match ? parseInt(match[1], 10) : null;
  }
})();
