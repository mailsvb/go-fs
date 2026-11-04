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
      var pending = null;

      function toBar(data) {
        data.type = message;
        if (ready) {
          bar.contentWindow.postMessage(data, location.origin);
        } else {
          pending = data;
        }
      }

      function play(queue, index) {
        if (!bar) {
          bar = document.createElement("iframe");
          bar.className = "player";
          bar.title = "Player";
          bar.src = page;
          ready = false;
          document.body.append(bar);
          document.body.classList.add("playing");
        }
        toBar({ action: "play", queue: queue, index: index });
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
        pending = null;
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
            if (pending) {
              var data = pending;
              pending = null;
              toBar(data);
            }
          } else if (event.data.action === "closed") {
            closeBar();
          }
          return;
        }
        if (shell && event.source === shell.contentWindow) {
          if (event.data.action === "play" && Array.isArray(event.data.queue)) {
            play(event.data.queue, event.data.index);
          } else if (event.data.action === "pause") {
            pause();
          }
        }
      });

      return {
        play: function (link) {
          var wanted = queueOf(link);
          play(wanted.queue, wanted.index);
        },
        pause: pause,
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
  // signed with; it answers the path and the query, and the origin is the
  // one this page was loaded from, which is right behind a proxy as well.

  var shareDialog = document.getElementById("share-dialog");
  var shareFields = shareDialog ? shareDialog.querySelectorAll("input.link") : [];
  var copyButtons = shareDialog ? shareDialog.querySelectorAll("button[data-copy]") : [];

  function shareField(kind) {
    return shareDialog.querySelector("input.link[data-kind='" + kind + "']");
  }

  // shellQuote makes a word safe to paste into a POSIX shell: the name may
  // hold spaces or quotes, and the link's query an & or a ?.
  function shellQuote(text) {
    return "'" + text.replace(/'/g, "'\\''") + "'";
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
      var url = window.location.origin + view.path;
      // the commands name the output file, or the key would end up in it
      shareField("link").value = url;
      shareField("curl").value = "curl -fL -o " + shellQuote(name) + " " + shellQuote(url);
      shareField("wget").value = "wget -O " + shellQuote(name) + " " + shellQuote(url);
      copyButtons.forEach(function (button) {
        window.clearTimeout(button.copiedTimer);
        button.querySelector("span").textContent = "Copy";
      });
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
    // job is the fetch the dialog waits on until its download begins, null
    // while it waits on none
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

    // The fields of the login typed in are left out, not only hidden, for a
    // stored server, so that the form does not ask for them.
    function choose() {
      var server = stored() !== "";
      manual.hidden = server;
      manual.disabled = server;
    }

    function busy(on) {
      Array.prototype.forEach.call(form.querySelectorAll("input, textarea"), function (input) {
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
      var body = {
        url: field("url").value.trim(),
        username: field("username").value.trim(),
        password: field("password").value,
        headers: field("headers").value,
        skipVerify: !field("verify").checked
      };
      busy(true);
      bar.removeAttribute("value");
      tell("Connecting…");
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
