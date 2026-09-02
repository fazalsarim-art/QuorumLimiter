// Progressive enhancement only: every action works without JavaScript.
(function () {
  "use strict";

  // Confirmation dialogs for destructive submits (data-confirm on the button).
  document.addEventListener("submit", function (e) {
    var btn = e.submitter;
    if (btn && btn.hasAttribute("data-confirm")) {
      if (!window.confirm(btn.getAttribute("data-confirm"))) {
        e.preventDefault();
      }
    }
  });

  // Copy-to-clipboard buttons (data-copy="#selector").
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    var target = document.querySelector(btn.getAttribute("data-copy"));
    if (!target) return;
    var text = target.textContent || "";
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () {
        var label = btn.textContent;
        btn.textContent = "Copied";
        setTimeout(function () { btn.textContent = label; }, 1500);
      });
    }
  });
})();
