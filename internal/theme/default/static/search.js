// Site search over the index built by internal/search. The index loads the
// first time the box is used. Results are ranked with BM25 plus a bonus for
// words in the title; the last word also matches as a prefix, so results
// appear while typing.
(() => {
  "use strict";
  const form = document.querySelector("form.site-search");
  if (!form) return;
  const input = form.querySelector("input");
  const results = form.querySelector(".site-search-results");
  const K1 = 1.2, B = 0.75, PREFIX_BOOST = 0.7, MAX_EXPAND = 50, MAX_RESULTS = 10;
  let index, loading;

  // Must match search.Fold and Tokenizer.Words in Go.
  const words = (s) =>
    s.toLowerCase().normalize("NFKD").replace(/[\p{Mn}'’]/gu, "").match(/[\p{L}\p{N}]+/gu) || [];

  function load() {
    loading ||= fetch(form.dataset.index)
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(r.statusText))))
      .then((ix) => {
        if (ix.v !== 1) throw new Error("unsupported search index version " + ix.v);
        for (const p of Object.values(ix.terms)) {
          for (let i = 0, d = 0; i < p.length; i += 2) p[i] = d += p[i]; // gaps → doc indexes
        }
        ix.sorted = Object.keys(ix.terms).sort();
        ix.avgLen = ix.docs.reduce((n, d) => n + d[3], 0) / (ix.docs.length || 1);
        ix.titleWords = [];
        index = ix;
      })
      .catch((err) => {
        loading = null;
        throw err;
      });
    return loading;
  }

  function titleHas(doc, term) {
    const t = index.titleWords;
    t[doc] ||= new Set(words(index.docs[doc][1]));
    return t[doc].has(term);
  }

  // Terms starting with prefix, found by binary search in the sorted list.
  function expand(prefix) {
    const t = index.sorted;
    let lo = 0, hi = t.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (t[mid] < prefix) lo = mid + 1;
      else hi = mid;
    }
    const out = [];
    for (let i = lo; i < t.length && t[i].startsWith(prefix) && out.length < MAX_EXPAND; i++) out.push(t[i]);
    return out;
  }

  function search(query) {
    const qs = words(query);
    const typing = !/\s$/.test(query);
    const n = index.docs.length;
    const total = new Map();
    qs.forEach((w, qi) => {
      const candidates = [[w, 1]];
      if (typing && qi === qs.length - 1 && w.length >= 2) {
        for (const t of expand(w)) if (t !== w) candidates.push([t, PREFIX_BOOST]);
      }
      // Each query word counts once per doc: its best-scoring match.
      const best = new Map();
      for (const [term, boost] of candidates) {
        const p = index.terms[term];
        if (!p) continue;
        const df = p.length / 2;
        const idf = Math.log(1 + (n - df + 0.5) / (df + 0.5));
        // idf × (K1 + 1) is the most BM25 can give a word, however often it
        // repeats, so a title bonus of title_boost ≥ 1 always outranks the body.
        const titleBonus = (index.title_boost || 0) * idf * (K1 + 1);
        for (let i = 0; i < p.length; i += 2) {
          const doc = p[i], tf = p[i + 1];
          const norm = 1 - B + (B * index.docs[doc][3]) / index.avgLen;
          let s = (idf * tf * (K1 + 1)) / (tf + K1 * norm);
          if (titleBonus && titleHas(doc, term)) s += titleBonus;
          s *= boost;
          if (s > (best.get(doc) || 0)) best.set(doc, s);
        }
      }
      for (const [doc, s] of best) total.set(doc, (total.get(doc) || 0) + s);
    });
    // Ties go to the earlier doc: newer posts come first in the index.
    return [...total].sort((a, b) => b[1] - a[1] || a[0] - b[0]).slice(0, MAX_RESULTS).map(([d]) => index.docs[d]);
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text) e.textContent = text;
    return e;
  }

  function show(query) {
    results.replaceChildren();
    if (!words(query).length) {
      results.hidden = true;
      return;
    }
    const hits = search(query);
    if (!hits.length) {
      results.append(el("p", "site-search-empty", "No results"));
    } else {
      const list = el("ol");
      for (const [url, title, date, , summary] of hits) {
        const a = el("a");
        a.href = url;
        a.append(el("span", "site-search-title", title));
        if (date) a.append(el("span", "site-search-date", date));
        if (summary) a.append(el("span", "site-search-summary", summary));
        const li = el("li");
        li.append(a);
        list.append(li);
      }
      results.append(list);
    }
    results.hidden = false;
  }

  function update() {
    load().then(() => show(input.value), () => {
      results.replaceChildren(el("p", "site-search-empty", "Search is unavailable right now"));
      results.hidden = false;
    });
  }

  form.hidden = false;
  input.addEventListener("focus", () => load().catch(() => {}), { once: true });
  input.addEventListener("input", update);
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const first = results.querySelector("a");
    if (first) location.href = first.href;
  });
  form.addEventListener("keydown", (e) => {
    const links = [...results.querySelectorAll("a")];
    const i = links.indexOf(document.activeElement);
    if (e.key === "Escape") {
      input.focus(); // first, since focusing the input reopens results
      results.hidden = true;
    } else if (e.key === "ArrowDown" && links.length) {
      e.preventDefault();
      links[Math.min(i + 1, links.length - 1)].focus();
    } else if (e.key === "ArrowUp" && links.length) {
      e.preventDefault();
      (i <= 0 ? input : links[i - 1]).focus();
    }
  });
  document.addEventListener("click", (e) => {
    if (!form.contains(e.target)) results.hidden = true;
  });
  input.addEventListener("focus", () => {
    if (results.childElementCount) results.hidden = false;
  });
})();
