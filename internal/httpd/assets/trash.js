// The Trash page is complete without this script as far as looking goes: the
// rows are rendered by the server and the folders and the sort headers are
// ordinary links. What is added here is what a link cannot do — filtering,
// and restoring, deleting and emptying, each of which is confirmed first.
(function () {
  "use strict";

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
  // height, which changes as the header wraps.
  (function () {
    var header = document.querySelector("body > header");
    if (!header || !window.ResizeObserver) {
      return;
    }
    new ResizeObserver(function () {
      document.documentElement.style.setProperty("--header-height", header.getBoundingClientRect().height + "px");
    }).observe(header);
  })();

  var table = document.getElementById("trash");
  if (!table) {
    return;
  }
  var body = table.tBodies[0];
  // base is the folder of the trash the page shows, already escaped: every
  // request below puts the marker on it as a query.
  var base = table.dataset.base;
  var banner = document.getElementById("banner");
  var bannerText = banner.querySelector(".text");
  // A report of something done goes away by itself after a while; a problem
  // stays until it is dismissed, so that it is not missed.
  var bannerTimer = null;

  function rows() {
    return Array.prototype.slice.call(body.rows).filter(function (row) {
      return row.dataset.name !== undefined;
    });
  }

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

  // A message that has to survive the reload after an action is kept for the
  // next page of this tab. Storage may be off; the reload then says nothing.
  var carried = "go-fs-trash-banner";
  try {
    var kept = window.sessionStorage.getItem(carried);
    if (kept) {
      window.sessionStorage.removeItem(carried);
      say(kept, true);
    }
  } catch (err) {
    // nothing carried
  }

  function sayAfterReload(message) {
    try {
      window.sessionStorage.setItem(carried, message);
    } catch (err) {
      // the reload shows what changed, which says it as well
    }
    window.location.reload();
  }

  function reason(status, text) {
    if (status === 401) {
      return "Your session has ended — reload the page to log in again.";
    }
    if (status === 403) {
      return "You are not allowed to do that here.";
    }
    if (status === 404) {
      return "That is no longer in the trash — reload the page to see what is.";
    }
    return text || ("The server answered " + status + ".");
  }

  function request(action, payload) {
    var options = {
      method: "POST",
      headers: { Accept: "application/json" },
      credentials: "same-origin"
    };
    if (payload !== undefined) {
      options.headers["Content-Type"] = "application/json";
      options.body = JSON.stringify(payload);
    }
    return fetch(base + "?go-fs=" + action, options).then(function (res) {
      if (res.ok) {
        return res.json();
      }
      return res.text().then(function (text) {
        throw new Error(reason(res.status, text.trim()));
      });
    });
  }

  function failed(err) {
    say(err.message);
  }

  function plural(count, one, many) {
    return count + " " + (count === 1 ? one : many);
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

  // --- confirming ------------------------------------------------------
  //
  // Every action asks first. The question is one dialog; restoring adds the
  // choice of what happens to a file that is already back in its place.

  var checking = document.getElementById("confirm");
  var choices = checking.querySelector(".fields");
  var overwrite = checking.querySelector("input[name='overwrite']");
  var agreeButton = checking.querySelector("button[value='agree']");
  var onAgree = null;

  // The submit event rather than close, for the reason the listing gives.
  checking.querySelector("form").addEventListener("submit", function (event) {
    var pending = onAgree;
    onAgree = null;
    if (event.submitter && event.submitter.value === "cancel") {
      return;
    }
    if (pending) {
      pending(overwrite.checked);
    }
  });

  function ask(title, text, agree, danger, offerOverwrite, then) {
    checking.querySelector("h2").textContent = title;
    checking.querySelector("p").textContent = text;
    agreeButton.textContent = agree;
    agreeButton.classList.toggle("danger", danger);
    choices.hidden = !offerOverwrite;
    overwrite.checked = false;
    onAgree = then;
    checking.showModal();
  }

  function shown(row) {
    return row.dataset.name + (row.dataset.dir === "1" ? "/" : "");
  }

  function restore(row) {
    var folder = row.dataset.dir === "1";
    ask("Restore", "Restore " + shown(row) + " to where it was deleted from?" +
      (folder ? " Its files are put back into the folder that is there." : ""),
      "Restore", false, true, function (replace) {
        request("trash-restore", { name: row.dataset.name, overwrite: replace }).then(function (done) {
          var message = "Restored " + plural(done.restored, "item", "items") + ".";
          if (done.skipped > 0) {
            message += " " + plural(done.skipped, "item was", "items were") +
              " left in the trash because something of the same name is in its place.";
          }
          sayAfterReload(message);
        }).catch(failed);
      });
  }

  function remove(row) {
    ask("Delete for good", "Delete " + shown(row) +
      " for good? It cannot be restored afterwards.", "Delete", true, false, function () {
        request("trash-delete", { name: row.dataset.name }).then(function () {
          sayAfterReload("Deleted " + shown(row) + " for good.");
        }).catch(failed);
      });
  }

  var emptyButton = document.getElementById("empty-trash");
  emptyButton.addEventListener("click", function () {
    ask("Empty trash", "Delete everything in the trash for good, in every folder of it? " +
      "Nothing of it can be restored afterwards.", "Empty trash", true, false, function () {
        emptyButton.disabled = true;
        clear();
        request("trash-empty").then(function (done) {
          sayAfterReload(done.items === 0 ? "The trash was empty already." :
            "Deleted " + plural(done.items, "item", "items") + " for good.");
        }).catch(function (err) {
          emptyButton.disabled = false;
          failed(err);
        });
      });
  });

  // --- the options menu ------------------------------------------------
  //
  // One menu serves every row. It is placed below the button that opened it
  // and remembers the row, which is what the items act on.

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

  menu.addEventListener("click", function (event) {
    var item = event.target.closest("button[data-do]");
    if (!item || !opener) {
      return;
    }
    var row = opener.closest("tr");
    closeMenu(false);
    if (item.dataset.do === "restore") {
      restore(row);
    } else if (item.dataset.do === "delete") {
      remove(row);
    }
  });
})();
