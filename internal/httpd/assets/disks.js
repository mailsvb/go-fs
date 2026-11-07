// The Disks page is drawn here from the state the server put into the page,
// and asked for again every few seconds while the page is in view: what a
// client moves, a clone's progress and the clients that come and go change
// on their own. Every change is posted as JSON and answered with the state
// after it, which is drawn at once.
(function () {
  "use strict";

  // The menu in the corner of the header closes on a click beside it and on
  // Escape, as on the other pages.
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

  // The headings of the table stick below the header, which needs its height.
  (function () {
    var header = document.querySelector("body > header");
    if (!header || !window.ResizeObserver) {
      return;
    }
    new ResizeObserver(function () {
      document.documentElement.style.setProperty("--header-height", header.getBoundingClientRect().height + "px");
    }).observe(header);
  })();

  var table = document.getElementById("disks");
  var body = table.tBodies[0];
  var info = document.getElementById("disk-info");
  var empty = document.getElementById("no-disks");
  var banner = document.getElementById("banner");
  var bannerText = banner.querySelector(".text");
  var bannerTimer = null;
  var state = JSON.parse(document.getElementById("state").textContent);

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

  function reason(status, text) {
    if (status === 401) {
      return "Your session has ended — reload the page to log in again.";
    }
    if (status === 403) {
      return "You are not allowed to do that here.";
    }
    return text || ("The server answered " + status + ".");
  }

  function url(action, params) {
    var query = "?go-fs=" + action;
    Object.keys(params || {}).forEach(function (key) {
      query += "&" + key + "=" + encodeURIComponent(params[key]);
    });
    return "/" + query;
  }

  // request posts a change and draws the state it answers with.
  function request(action, payload) {
    return fetch(url(action), {
      method: "POST",
      headers: { Accept: "application/json", "Content-Type": "application/json" },
      credentials: "same-origin",
      body: JSON.stringify(payload)
    }).then(function (res) {
      if (res.ok) {
        return res.json().then(function (next) {
          state = next;
          render();
          return next;
        });
      }
      return res.text().then(function (text) {
        throw new Error(reason(res.status, text.trim()));
      });
    });
  }

  function failed(err) {
    say(err.message);
  }

  // --- formatting ---------------------------------------------------------

  function bytes(n) {
    var units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
    var at = 0;
    while (n >= 1024 && at < units.length - 1) {
      n /= 1024;
      at++;
    }
    return (at === 0 ? String(n) : n.toFixed(n < 10 ? 2 : n < 100 ? 1 : 0)) + " " + units[at];
  }

  function when(ms) {
    var date = new Date(ms);
    var pad = function (v) { return (v < 10 ? "0" : "") + v; };
    return date.getFullYear() + "-" + pad(date.getMonth() + 1) + "-" + pad(date.getDate()) + " " +
      pad(date.getHours()) + ":" + pad(date.getMinutes());
  }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) {
      node.className = className;
    }
    if (text !== undefined) {
      node.textContent = text;
    }
    return node;
  }

  function icon(name) {
    var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "ic");
    var use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", "#" + name);
    svg.appendChild(use);
    return svg;
  }

  // --- drawing ------------------------------------------------------------

  function find(name) {
    for (var i = 0; i < state.disks.length; i++) {
      if (state.disks[i].name === name) {
        return state.disks[i];
      }
    }
    return null;
  }

  function render() {
    var where = "The disks are in " + state.folder + ". ";
    info.textContent = state.nbdPort ?
      where + "The NBD server serves them on port " + state.nbdPort + ", each by its name as the export." :
      where + "The NBD server is off, so no VM can attach them: switch it on in [nbd].";
    if (state.error) {
      say(state.error);
    }
    var openName = opener ? opener.closest("tr").dataset.name : null;
    closeMenu(false);
    body.textContent = "";
    state.disks.forEach(function (disk) {
      body.appendChild(row(disk));
    });
    empty.hidden = state.disks.length > 0;
    applyFilter();
    renderJobs();
    if (openName) {
      var button = body.querySelector("tr[data-name='" + CSS.escape(openName) + "'] button[data-do='menu']");
      if (button) {
        button.focus();
      }
    }
  }

  function row(disk) {
    var tr = el("tr");
    tr.dataset.name = disk.name;
    var name = el("td");
    var label = el("span", "name");
    label.append(icon("i-disk"), el("span", "", disk.name));
    name.appendChild(label);
    tr.appendChild(name);
    tr.appendChild(el("td", "meta num c-size", bytes(disk.size)));
    var used = el("td", "meta num c-size", bytes(disk.allocated));
    used.title = Math.round(100 * disk.allocated / Math.max(disk.size, 1)) + "% of the disk takes space";
    tr.appendChild(used);
    var attached = el("td", "c-att");
    disk.sessions.forEach(function (s) {
      var line = el("div", "att");
      line.append(el("span", "who", s.account), el("span", "mode" + (s.writable ? " rw" : ""), s.writable ? "read-write" : "read-only"));
      var detail = el("span", "meta", s.client + " · since " + when(s.since) + " · read " + bytes(s.read) + ", written " + bytes(s.written));
      detail.title = "Certificate " + s.serial + ", valid until " + when(s.expires);
      line.appendChild(detail);
      var off = el("button", "plain", "Disconnect");
      off.type = "button";
      off.dataset.session = s.id;
      off.title = "Close this client's connection; a VM with reconnect-delay connects again";
      line.appendChild(off);
      attached.appendChild(line);
    });
    if (disk.busy) {
      attached.appendChild(el("div", "busy", "Being changed: " + disk.busy.replace(/^being /, "")));
    } else if (disk.holds) {
      attached.appendChild(el("div", "busy", "Being downloaded or cloned"));
    } else if (disk.sessions.length === 0) {
      attached.appendChild(el("span", "meta", "—"));
    }
    tr.appendChild(attached);
    tr.appendChild(el("td", "meta c-mod", when(disk.modified)));
    var acts = el("td", "c-acts");
    var more = el("button");
    more.type = "button";
    more.dataset.do = "menu";
    more.title = "Options";
    more.setAttribute("aria-haspopup", "menu");
    more.setAttribute("aria-expanded", "false");
    more.setAttribute("aria-label", "Options for " + disk.name);
    more.appendChild(icon("i-more"));
    acts.appendChild(more);
    tr.appendChild(acts);
    return tr;
  }

  var jobs = document.getElementById("jobs");

  function renderJobs() {
    Array.prototype.slice.call(jobs.querySelectorAll(".job")).forEach(function (node) {
      node.remove();
    });
    jobs.hidden = state.jobs.length === 0;
    state.jobs.forEach(function (job) {
      var line = el("div", "job");
      line.appendChild(el("span", "", job.source + " → " + job.target));
      if (job.state === "running") {
        var bar = el("progress");
        bar.max = Math.max(job.total, 1);
        bar.value = job.done;
        line.append(bar, el("span", "meta", bytes(job.done) + " of " + bytes(job.total)));
      } else if (job.state === "failed") {
        line.appendChild(el("span", "failed", "failed: " + job.message));
      } else {
        line.appendChild(el("span", "meta", job.state));
      }
      var button = el("button", "plain", job.state === "running" ? "Cancel" : "Dismiss");
      button.type = "button";
      button.dataset.job = job.id;
      button.dataset.cancel = job.state === "running" ? "1" : "";
      line.appendChild(button);
      jobs.appendChild(line);
    });
  }

  // --- filtering ----------------------------------------------------------

  var filter = document.getElementById("filter");

  function applyFilter() {
    var needle = filter.value.trim().toLowerCase();
    Array.prototype.slice.call(body.rows).forEach(function (tr) {
      tr.classList.toggle("gone", needle !== "" && tr.dataset.name.toLowerCase().indexOf(needle) === -1);
    });
  }

  filter.addEventListener("input", applyFilter);

  // --- keeping up ---------------------------------------------------------
  //
  // The state is asked for again every few seconds while the page is in view
  // and no dialog is open, so that a dialog is not redrawn under the hand
  // filling it in.

  function busyWithDialog() {
    return document.querySelector("dialog[open]") !== null;
  }

  function refresh() {
    if (document.visibilityState !== "visible" || busyWithDialog() || opener) {
      return;
    }
    fetch(url("disks-state"), { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (res) {
        if (!res.ok) {
          throw new Error(reason(res.status, ""));
        }
        return res.json();
      })
      .then(function (next) {
        state = next;
        render();
      })
      .catch(function () {
        // the next round tries again; a session that ended says so on the
        // next change
      });
  }

  window.setInterval(refresh, 3000);
  document.addEventListener("visibilitychange", refresh);

  // --- the dialog that asks -----------------------------------------------
  //
  // One dialog asks for whatever a change needs: a name, a size, a choice.
  // Which of its fields show is decided per question.

  var asking = document.getElementById("ask");
  var askForm = asking.querySelector("form");
  var agreeButton = asking.querySelector("button[value='agree']");
  var onAgree = null;

  askForm.addEventListener("submit", function (event) {
    var pending = onAgree;
    onAgree = null;
    if (event.submitter && event.submitter.value === "cancel") {
      return;
    }
    if (pending) {
      pending({
        name: askForm.elements.name.value.trim(),
        size: askForm.elements.size.value.trim(),
        shrink: askForm.elements.shrink.checked,
        replace: askForm.elements.replace.checked
      });
    }
  });

  // ask shows the dialog with the fields named in show, filled from values.
  function ask(title, text, agree, danger, show, values, then) {
    asking.querySelector("h2").textContent = title;
    asking.querySelector(".text").textContent = text;
    agreeButton.textContent = agree;
    agreeButton.classList.toggle("danger", danger);
    Array.prototype.slice.call(asking.querySelectorAll("[data-for]")).forEach(function (node) {
      node.hidden = show.indexOf(node.dataset.for) === -1;
    });
    askForm.elements.name.value = values.name || "";
    askForm.elements.name.placeholder = values.placeholder || "web01.raw";
    askForm.elements.size.value = values.size || "";
    askForm.elements.shrink.checked = false;
    askForm.elements.replace.checked = false;
    onAgree = then;
    asking.showModal();
    var first = show[0] === "name" ? askForm.elements.name : show[0] === "size" ? askForm.elements.size : agreeButton;
    first.focus();
    if (first.select) {
      first.select();
    }
  }

  // sizeText is a size as the dialog offers it back, in whole GiB where it
  // is one.
  function sizeText(size) {
    var gib = 1024 * 1024 * 1024;
    return size % gib === 0 ? (size / gib) + "G" : String(size);
  }

  document.getElementById("new-disk").addEventListener("click", function () {
    clear();
    ask("New disk image", "A sparse raw image: it takes no space until the guest writes to it.",
      "Create", false, ["name", "size"], {}, function (v) {
        request("disks-create", { name: v.name, size: v.size }).then(function () {
          say("Made " + v.name + ".", true);
        }).catch(failed);
      });
  });

  function resize(disk) {
    ask("Resize " + disk.name, "It is " + bytes(disk.size) + " now. Nothing may have it attached; " +
      "the guest has to grow its partition and filesystem itself afterwards.", "Resize", false,
      ["size", "shrink"], { size: sizeText(disk.size) }, function (v) {
        request("disks-resize", { name: disk.name, size: v.size, shrink: v.shrink }).then(function () {
          say("Resized " + disk.name + ".", true);
        }).catch(failed);
      });
  }

  function rename(disk) {
    ask("Rename " + disk.name, "Nothing may have it attached. A VM names its disk as the export, " +
      "so change its configuration as well.", "Rename", false, ["name"], { name: disk.name }, function (v) {
        request("disks-rename", { name: disk.name, to: v.name }).then(function () {
          say("Renamed " + disk.name + " to " + v.name + ".", true);
        }).catch(failed);
      });
  }

  function cloneDisk(disk) {
    var stem = disk.name.replace(/(\.[^.]*)?$/, "");
    var suffix = disk.name.slice(stem.length);
    ask("Clone " + disk.name, "A copy as sparse as the original, made in the background. " +
      "The original may be read meanwhile, but nothing may attach it read-write until the copy is done.",
      "Clone", false, ["name"], { name: stem + "-copy" + suffix }, function (v) {
        request("disks-clone", { name: disk.name, to: v.name }).then(function () {
          say("Cloning " + disk.name + " to " + v.name + ".", true);
        }).catch(failed);
      });
  }

  function remove(disk) {
    ask("Delete " + disk.name, "This deletes the disk for good; it does not go to the trash. " +
      "Type its name to confirm.", "Delete", true, ["name"], { placeholder: disk.name }, function (v) {
        request("disks-delete", { name: disk.name, confirm: v.name }).then(function () {
          say("Deleted " + disk.name + ".", true);
        }).catch(failed);
      });
  }

  function download(disk) {
    var target = url("disks-download", { name: disk.name });
    fetch(target, { method: "HEAD", credentials: "same-origin" }).then(function (res) {
      if (!res.ok) {
        return res.status === 409 ?
          say(disk.name + " is attached read-write, so it cannot be downloaded while it changes.") :
          say(reason(res.status, ""));
      }
      window.location.href = target;
    }).catch(failed);
  }

  // --- connection details -------------------------------------------------

  var connectDialog = document.getElementById("connect");

  function connect(disk) {
    var host = window.location.hostname;
    var port = state.nbdPort || 10809;
    connectDialog.querySelector("h2").textContent = "Connect " + disk.name;
    connectDialog.querySelector(".text").textContent = (state.nbdPort ? "" :
      "The NBD server is off: switch it on in [nbd] before a VM can attach this disk. ") +
      "The client needs ca-cert.pem, client-cert.pem and client-key.pem in the folder named below, " +
      "with a client certificate whose common name is an account that sets nbd.";
    connectDialog.querySelector("[data-snippet='qemu']").textContent =
      "-object tls-creds-x509,id=tls0,endpoint=client,dir=/etc/pki/qemu \\\n" +
      "-blockdev driver=nbd,node-name=d0-nbd,server.type=inet,server.host=" + host +
      ",server.port=" + port + ",export=" + disk.name + ",tls-creds=tls0,reconnect-delay=10 \\\n" +
      "-blockdev driver=raw,node-name=d0,file=d0-nbd,discard=unmap,detect-zeroes=unmap \\\n" +
      "-device virtio-blk-pci,drive=d0,werror=stop,rerror=stop";
    connectDialog.querySelector("[data-snippet='libvirt']").textContent =
      "<disk type='network' device='disk'>\n" +
      "  <driver name='qemu' type='raw' discard='unmap' detect_zeroes='unmap' error_policy='stop'/>\n" +
      "  <source protocol='nbd' name='" + disk.name + "' tls='yes'>\n" +
      "    <host name='" + host + "' port='" + port + "'/>\n" +
      "  </source>\n" +
      "  <target dev='vda' bus='virtio'/>\n" +
      "</disk>";
    connectDialog.showModal();
  }

  connectDialog.addEventListener("click", function (event) {
    var button = event.target.closest("button[data-copy]");
    if (!button) {
      return;
    }
    var text = connectDialog.querySelector("[data-snippet='" + button.dataset.copy + "']").textContent;
    if (navigator.clipboard) {
      navigator.clipboard.writeText(text).then(function () {
        button.lastChild.textContent = "Copied";
      });
    }
  });

  // --- uploading ----------------------------------------------------------
  //
  // A disk is uploaded in pieces of the size the server takes. Each piece
  // has to start where what the server holds ends; when it does not, the
  // server says where, and the upload goes on from there, which is also how
  // a piece lost to a dropped connection is sent again.

  var uploads = document.getElementById("uploads");
  var picker = document.getElementById("upload-file");

  document.getElementById("upload-disk").addEventListener("click", function () {
    clear();
    picker.value = "";
    picker.click();
  });

  function diskName(fileName) {
    var name = fileName.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^[^A-Za-z0-9]+/, "").replace(/\.+$/, "");
    return name.slice(0, 200) || "disk.raw";
  }

  picker.addEventListener("change", function () {
    var file = picker.files[0];
    if (!file) {
      return;
    }
    ask("Upload " + file.name, "A raw image of " + bytes(file.size) + ". Blocks of zeros in it are left " +
      "as holes, so the disk takes only the space its data needs. A qcow2 or vmdk image has to be " +
      "converted to raw first (qemu-img convert -O raw).", "Upload", false, ["name", "replace"],
      { name: diskName(file.name) }, function (v) {
        upload(file, v.name, v.replace);
      });
  });

  function upload(file, name, replace) {
    var line = el("div", "job");
    var bar = el("progress");
    bar.max = Math.max(file.size, 1);
    bar.value = 0;
    var progress = el("span", "meta", "starting");
    var stop = el("button", "plain", "Cancel");
    stop.type = "button";
    line.append(el("span", "", file.name + " → " + name), bar, progress, stop);
    uploads.appendChild(line);
    uploads.hidden = false;
    var controller = new AbortController();
    var cancelled = false;
    stop.addEventListener("click", function () {
      cancelled = true;
      controller.abort();
    });
    var chunk = state.chunkSize;
    var started = Date.now();
    var retries = 0;

    function done(message, good) {
      line.remove();
      uploads.hidden = uploads.querySelector(".job") === null;
      say(message, good);
      refresh();
    }

    function send(start) {
      if (cancelled) {
        return done("The upload of " + name + " was cancelled.");
      }
      if (file.size === 0) {
        return done("An empty file is no disk.");
      }
      var end = Math.min(start + chunk, file.size) - 1;
      fetch(url("disks-upload", replace ? { name: name, replace: "1" } : { name: name }), {
        method: "PUT",
        credentials: "same-origin",
        headers: { "Content-Type": "application/octet-stream", "Content-Range": "bytes " + start + "-" + end + "/" + file.size },
        body: file.slice(start, end + 1),
        signal: controller.signal
      }).then(function (res) {
        if (res.status === 416) {
          var have = /\*\/(\d+)/.exec(res.headers.get("Content-Range") || "");
          return send(have ? Number(have[1]) : 0);
        }
        if (!res.ok) {
          return res.text().then(function (text) {
            done("The upload of " + name + " failed: " + reason(res.status, text.trim()));
          });
        }
        retries = 0;
        bar.value = end + 1;
        var seconds = Math.max((Date.now() - started) / 1000, 0.001);
        progress.textContent = bytes(end + 1) + " of " + bytes(file.size) + " · " + bytes((end + 1) / seconds) + "/s";
        if (end + 1 >= file.size) {
          return done("Uploaded " + name + ".", true);
        }
        return send(end + 1);
      }).catch(function (err) {
        if (cancelled) {
          return done("The upload of " + name + " was cancelled.");
        }
        if (retries++ < 5) {
          progress.textContent = "connection lost, trying again";
          // the same piece again: the server takes it, finds it landed
          // already, or says where to go on from
          return window.setTimeout(function () { send(start); }, 2000 * retries);
        }
        done("The upload of " + name + " failed: " + err.message);
      });
    }

    send(0);
  }

  // --- the row menu, the clients and the jobs ------------------------------

  var menu = document.getElementById("menu");
  var opener = null;

  function items() {
    return Array.prototype.slice.call(menu.querySelectorAll("button"));
  }

  function openMenu(button) {
    opener = button;
    button.setAttribute("aria-expanded", "true");
    menu.hidden = false;
    var box = button.getBoundingClientRect();
    var left = box.right + window.scrollX - menu.offsetWidth;
    menu.style.left = Math.max(window.scrollX + 8, left) + "px";
    var top = box.bottom + window.scrollY + 4;
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
    var off = event.target.closest("button[data-session]");
    if (off) {
      clear();
      request("disks-disconnect", { id: off.dataset.session }).then(function () {
        say("Disconnected the client.", true);
      }).catch(failed);
      return;
    }
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

  jobs.addEventListener("click", function (event) {
    var button = event.target.closest("button[data-job]");
    if (button) {
      request("disks-job", { id: button.dataset.job, cancel: button.dataset.cancel === "1" }).catch(failed);
    }
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

  menu.addEventListener("click", function (event) {
    var item = event.target.closest("button[data-do]");
    if (!item || !opener) {
      return;
    }
    var disk = find(opener.closest("tr").dataset.name);
    closeMenu(false);
    if (!disk) {
      return;
    }
    switch (item.dataset.do) {
      case "connect": connect(disk); break;
      case "resize": resize(disk); break;
      case "rename": rename(disk); break;
      case "clone": cloneDisk(disk); break;
      case "download": download(disk); break;
      case "delete": remove(disk); break;
    }
  });

  render();
})();
