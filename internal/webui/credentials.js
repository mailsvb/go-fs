// The Credentials overlay: opened by any button with data-credentials, it posts
// the current password and the new one to where the dialog's data-action says,
// and says what the server answered. On the Profile page, whose body has the
// class profile, it is open from the start and leaves for / once it closes.
(function () {
  "use strict";
  var dialog = document.getElementById("credentials");
  if (!dialog || !dialog.showModal) {
    return;
  }
  var form = dialog.querySelector("form");
  var fields = dialog.querySelector(".fields");
  var problem = dialog.querySelector(".problem");
  var done = dialog.querySelector(".done");
  var apply = dialog.querySelector("button.go");
  var cancel = dialog.querySelector("button.cancel");
  var current = document.getElementById("credentials-current");
  var fresh = document.getElementById("credentials-new");
  var repeat = document.getElementById("credentials-repeat");
  var profile = document.body.classList.contains("profile");
  var busy = false;

  function say(message) {
    problem.textContent = message;
    problem.hidden = !message;
  }

  function open() {
    form.reset();
    say("");
    done.hidden = true;
    fields.hidden = false;
    apply.hidden = false;
    apply.disabled = false;
    cancel.textContent = "Cancel";
    busy = false;
    if (!dialog.open) {
      dialog.showModal();
    }
    current.focus();
  }

  function close() {
    if (busy) {
      return;
    }
    if (dialog.open) {
      dialog.close();
    }
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest ? event.target.closest("[data-credentials]") : null;
    if (!button) {
      return;
    }
    event.preventDefault();
    // the menu it was in closes behind the overlay
    var menu = button.closest("details");
    if (menu) {
      menu.open = false;
    }
    open();
  });

  cancel.addEventListener("click", close);
  dialog.addEventListener("cancel", function (event) {
    if (busy) {
      event.preventDefault();
    }
  });
  dialog.addEventListener("close", function () {
    form.reset();
    if (profile) {
      window.location.assign("/");
    }
  });

  repeat.addEventListener("input", function () {
    repeat.setCustomValidity(repeat.value && repeat.value !== fresh.value ? "The new passwords do not match." : "");
  });
  fresh.addEventListener("input", function () {
    if (repeat.value) {
      repeat.setCustomValidity(repeat.value !== fresh.value ? "The new passwords do not match." : "");
    }
  });

  form.addEventListener("submit", function (event) {
    event.preventDefault();
    if (busy) {
      return;
    }
    if (fresh.value !== repeat.value) {
      say("The new passwords do not match.");
      repeat.focus();
      return;
    }
    if (!form.reportValidity()) {
      return;
    }
    say("");
    busy = true;
    apply.disabled = true;
    var body = new URLSearchParams();
    body.set("current", current.value);
    body.set("password", fresh.value);
    body.set("repeat", repeat.value);
    fetch(dialog.getAttribute("data-action"), {
      method: "POST",
      credentials: "same-origin",
      body: body
    }).then(function (response) {
      return response.text().then(function (text) {
        text = (text || "").trim();
        if (response.ok) {
          // 202 is a password stored that is not in force yet, and says so
          return { ok: true, message: text || "The password was changed." };
        }
        return { ok: false, message: text || "The password could not be changed (" + response.status + ")." };
      });
    }, function () {
      return { ok: false, message: "The server could not be reached." };
    }).then(function (answer) {
      busy = false;
      apply.disabled = false;
      if (!answer.ok) {
        say(answer.message);
        return;
      }
      form.reset();
      fields.hidden = true;
      apply.hidden = true;
      done.textContent = answer.message;
      done.hidden = false;
      cancel.textContent = "Close";
      cancel.focus();
    });
  });

  if (profile) {
    open();
  }
})();
