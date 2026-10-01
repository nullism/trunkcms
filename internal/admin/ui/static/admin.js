// trunkcms admin: write/preview tabs, live preview, upload link insertion,
// local preview of unsaved uploads, delete confirmation.
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

  // Write/Preview tabs. Without JS both panes show, stacked, and the
  // "Refresh preview" button updates the preview.
  function setupTabs(root, onShow) {
    const tabs = [...root.querySelectorAll("[role=tab]")];
    const select = (tab) => {
      for (const t of tabs) {
        const on = t === tab;
        t.setAttribute("aria-selected", on);
        t.tabIndex = on ? 0 : -1;
        document.getElementById(t.getAttribute("aria-controls")).hidden = !on;
      }
      onShow(tab.getAttribute("aria-controls"));
    };
    tabs.forEach((t, i) => {
      t.addEventListener("click", () => select(t));
      t.addEventListener("keydown", (e) => {
        const j = { ArrowLeft: i - 1, ArrowRight: i + 1, Home: 0, End: tabs.length - 1 }[e.key];
        if (j === undefined) return;
        e.preventDefault();
        const next = tabs[(j + tabs.length) % tabs.length];
        select(next);
        next.focus();
      });
    });
    root.classList.add("tabbed");
    select(tabs[0]);
  }

  const form = document.querySelector("form[data-preview]");
  if (form) {
    const button = form.querySelector("[data-preview-button]");
    const body = form.querySelector("textarea[name=body]");
    const tabs = form.querySelector("[data-tabs]");
    const pane = document.getElementById("pane-preview");
    const visible = () => !pane.hidden;
    let timer, stale = true;
    const update = () => {
      clearTimeout(timer);
      stale = false;
      form.requestSubmit(button);
    };
    form.addEventListener("input", (e) => {
      if (e.target.type === "file") return;
      stale = true;
      clearTimeout(timer);
      if (visible()) timer = setTimeout(update, 400);
    });

    if (tabs) {
      button.hidden = true;
      const frame = pane.querySelector("iframe");
      // Size the preview like the textarea so the buttons below don't jump.
      let height = 0;
      new ResizeObserver(() => {
        if (body.offsetHeight) height = body.offsetHeight;
      }).observe(body);
      setupTabs(tabs, (id) => {
        if (id !== "pane-preview") return;
        if (height) frame.style.height = Math.max(height, 320) + "px";
        if (stale) update();
      });
    } else {
      update();
    }

    const files = form.querySelector("input[type=file][data-prefix]");
    if (files && body) {
      // Files picked so far, keyed by the URL they'll have once saved. Picking
      // again replaces the input's selection, so the input is refilled from
      // here; a later file with the same name wins, as it would on the server.
      const pending = new Map(); // url -> { file, blob }
      files.addEventListener("change", () => {
        for (const f of files.files) {
          const url = "/" + files.dataset.prefix + safeName(f.name);
          const old = pending.get(url);
          if (old) URL.revokeObjectURL(old.blob);
          pending.set(url, { file: f, blob: URL.createObjectURL(f) });
          const label = f.name.replace(/[\[\]]/g, "");
          const md = (f.type.startsWith("image/") ? "!" : "") + "[" + label + "](" + url + ")";
          insertAtCursor(body, md + "\n");
        }
        const dt = new DataTransfer();
        for (const { file } of pending.values()) dt.items.add(file);
        files.files = dt.files;
      });

      // Unsaved uploads don't exist on the site yet, so point the preview's
      // references to them at the local files.
      const frame = document.querySelector("iframe[name=preview]");
      frame?.addEventListener("load", () => {
        const doc = frame.contentDocument;
        if (!doc || !pending.size) return;
        for (const el of doc.querySelectorAll("img[src], video[src], audio[src], source[src], a[href]")) {
          const attr = el.hasAttribute("src") ? "src" : "href";
          let u;
          try {
            u = new URL(el.getAttribute(attr), doc.baseURI);
          } catch {
            continue;
          }
          const hit = pending.get(decodeURIComponent(u.pathname));
          if (hit) el.setAttribute(attr, hit.blob);
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
