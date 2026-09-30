// trunkcms admin: live preview, upload link insertion, delete confirmation.
// No inline scripts anywhere, so the admin CSP can forbid them.
(function () {
  "use strict";

  // Mirrors content.SafeFileName on the server.
  function safeName(s) {
    s = s.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[-.]+|[-.]+$/g, "");
    return s || "file";
  }

  function insertAtCursor(ta, text) {
    const start = ta.selectionStart ?? ta.value.length;
    const end = ta.selectionEnd ?? start;
    ta.value = ta.value.slice(0, start) + text + ta.value.slice(end);
    ta.selectionStart = ta.selectionEnd = start + text.length;
    ta.dispatchEvent(new Event("input", { bubbles: true }));
  }

  const form = document.querySelector("form[data-preview]");
  if (form) {
    const button = form.querySelector("[data-preview-button]");
    let timer;
    const refresh = () => {
      clearTimeout(timer);
      timer = setTimeout(() => form.requestSubmit(button), 400);
    };
    form.addEventListener("input", (e) => {
      if (e.target.type !== "file") refresh();
    });
    form.requestSubmit(button);

    const files = form.querySelector("input[type=file][data-prefix]");
    const body = form.querySelector("textarea[name=body]");
    if (files && body) {
      files.addEventListener("change", () => {
        for (const f of files.files) {
          const url = "/" + files.dataset.prefix + safeName(f.name);
          const label = f.name.replace(/[\[\]]/g, "");
          const md = (f.type.startsWith("image/") ? "!" : "") + "[" + label + "](" + url + ")";
          insertAtCursor(body, md + "\n");
        }
      });
    }
  }

  document.querySelectorAll("form[data-confirm]").forEach((f) => {
    f.addEventListener("submit", (e) => {
      if (!confirm(f.dataset.confirm)) e.preventDefault();
    });
  });
})();
