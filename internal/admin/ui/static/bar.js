// trunkcms admin bar: a floating edit bar on public pages for signed-in users.
// Built pages load this only when the trunkcms_editor hint cookie is set
// (render.AdminBarLoader). It lives in a shadow root so the site's theme and
// the bar can't restyle each other.
(function () {
  "use strict";
  if (window.__trunkcmsBar) return;
  window.__trunkcmsBar = true;

  const KEY = "trunkcms-bar-collapsed";
  const load = () => { try { return localStorage.getItem(KEY) === "1"; } catch { return false; } };
  const save = (v) => { try { v ? localStorage.setItem(KEY, "1") : localStorage.removeItem(KEY); } catch {} };

  const CSS = `
    :host { all: initial; }
    .bar {
      position: fixed; right: 16px; bottom: 16px; z-index: 2147483647;
      display: flex; align-items: center; gap: 2px; max-width: calc(100vw - 32px);
      padding: 4px; border-radius: 999px;
      background: #111827; color: #f3f4f6; border: 1px solid rgba(255,255,255,.14);
      box-shadow: 0 6px 24px rgba(0,0,0,.25), 0 1px 3px rgba(0,0,0,.2);
      font: 500 13px/1 system-ui, -apple-system, "Segoe UI", sans-serif;
    }
    a, button {
      all: unset; box-sizing: border-box; cursor: pointer; white-space: nowrap;
      display: inline-flex; align-items: center; gap: 6px; height: 30px; padding: 0 12px; border-radius: 999px;
      color: inherit; font: inherit;
    }
    a:hover, button:hover { background: rgba(255,255,255,.1); }
    a:focus-visible, button:focus-visible { outline: 2px solid #60a5fa; outline-offset: 1px; }
    .primary { background: #2563eb; color: #fff; }
    .primary:hover { background: #1d4ed8; }
    .status { padding: 0 10px; height: 22px; display: inline-flex; align-items: center; border-radius: 999px;
      background: #facc15; color: #422006; font-size: 11px; font-weight: 700; text-transform: uppercase; letter-spacing: .04em; }
    .who { padding: 0 10px; color: #9ca3af; overflow: hidden; text-overflow: ellipsis; }
    .icon { width: 30px; padding: 0; justify-content: center; color: #9ca3af; }
    .icon:hover { color: #f3f4f6; }
    svg { width: 15px; height: 15px; flex: none; }
    form { display: contents; }
    .full { display: contents; }
    .collapsed .full { display: none; }
    .bar:not(.collapsed) .open { display: none; }
    @media (max-width: 560px) { .who, .wide { display: none; } }
    @media print { .bar { display: none; } }
  `;
  const PENCIL = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M11 2.5l2.5 2.5L5.5 13H3v-2.5z"/></svg>';
  const CLOSE = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8"/></svg>';

  function el(tag, attrs, text) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, v);
    if (text) e.textContent = text;
    return e;
  }

  function render(info) {
    const host = el("div", { id: "trunkcms-admin-bar" });
    const root = host.attachShadow({ mode: "closed" });
    root.appendChild(el("style")).textContent = CSS;

    const bar = el("div", { class: "bar", role: "toolbar", "aria-label": "trunkcms admin" });
    const full = el("span", { class: "full" });

    if (info.status) full.append(el("span", { class: "status" }, info.status));
    if (info.edit) {
      const a = el("a", { class: "primary", href: info.edit });
      a.innerHTML = PENCIL;
      a.append(info.editLabel || "Edit");
      full.append(a);
    }
    if (info.new) full.append(el("a", { class: "wide", href: info.new }, "New post"));
    full.append(el("a", { href: "/admin/" }, "Dashboard"));
    full.append(el("span", { class: "who", title: info.role }, "@" + info.login));
    const out = el("form", { method: "post", action: "/admin/logout" });
    out.append(el("button", { type: "submit" }, "Sign out"));
    full.append(out);
    const hide = el("button", { type: "button", class: "icon", "aria-label": "Hide admin bar", title: "Hide" });
    hide.innerHTML = CLOSE;
    full.append(hide);

    const open = el("button", { type: "button", class: "open", "aria-label": "Show admin bar", title: "trunkcms" });
    open.innerHTML = PENCIL;
    if (info.status) open.append(info.status);

    const set = (collapsed) => {
      bar.classList.toggle("collapsed", collapsed);
      save(collapsed);
    };
    hide.addEventListener("click", () => { set(true); open.focus(); });
    open.addEventListener("click", () => { set(false); (full.querySelector("a") || hide).focus(); });
    set(load());

    bar.append(full, open);
    root.append(bar);
    document.body.append(host);
  }

  fetch("/admin/bar?path=" + encodeURIComponent(location.pathname), { credentials: "same-origin", cache: "no-store" })
    .then((r) => {
      if (r.status === 401) {
        // Session gone; the server cleared the hint too, this is belt and braces.
        document.cookie = "trunkcms_editor=; Max-Age=0; Path=/";
        return null;
      }
      return r.ok ? r.json() : null;
    })
    .then((info) => { if (info && info.login) render(info); })
    .catch(() => {});
})();
