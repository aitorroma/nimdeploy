// nimdeploy hub: the few things htmx doesn't do. No inline scripts (CSP).
(function () {
  "use strict";
  var root = document.documentElement;
  try { var saved = localStorage.getItem("nd-theme"); if (saved) root.dataset.theme = saved; } catch (e) {}

  function toast(msg) {
    var t = document.getElementById("toast");
    if (!t) return;
    t.textContent = msg; t.hidden = false;
    clearTimeout(toast.timer);
    toast.timer = setTimeout(function () { t.hidden = true; }, 2200);
  }
  function copy(text, el) {
    var done = function () { toast(document.body.dataset.copied || "Copied"); };
    var fail = function () {
      if (el) { var r = document.createRange(); r.selectNodeContents(el); getSelection().removeAllRanges(); getSelection().addRange(r); }
      toast("Ctrl+C");
    };
    try { navigator.clipboard.writeText(text).then(done, fail); } catch (e) { fail(); }
  }
  function openDrawer() { document.getElementById("drawer").hidden = false; document.getElementById("scrim").hidden = false; }
  function closeDrawer() {
    var d = document.getElementById("drawer");
    d.hidden = true; d.innerHTML = ""; document.getElementById("scrim").hidden = true;
  }
  function filterTree() {
    var q = (document.getElementById("q").value || "").trim().toLowerCase();
    document.querySelectorAll("[data-client-block]").forEach(function (b) {
      b.hidden = q !== "" && b.dataset.clientBlock.indexOf(q) < 0;
      if (q) b.classList.add("open");
    });
  }

  document.addEventListener("click", function (ev) {
    var t = ev.target.closest("[data-copy],[data-copy-text],[data-close],[data-theme-toggle],[data-menu],.tree .chev");
    if (!t) return;
    if (t.dataset.copy) { var el = document.getElementById(t.dataset.copy); copy(el.value !== undefined && el.tagName === "TEXTAREA" ? el.value : el.textContent, el); }
    else if (t.dataset.copyText) { copy(t.dataset.copyText); }
    else if (t.hasAttribute("data-close")) { closeDrawer(); }
    else if (t.hasAttribute("data-theme-toggle")) {
      var dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
      root.dataset.theme = dark ? "light" : "dark";
      try { localStorage.setItem("nd-theme", root.dataset.theme); } catch (e) {}
    }
    else if (t.hasAttribute("data-menu")) { document.getElementById("side").classList.toggle("open"); }
    else if (t.classList.contains("chev")) { ev.preventDefault(); ev.stopPropagation(); t.closest(".client").classList.toggle("open"); }
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeDrawer();
    if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k") { ev.preventDefault(); var q = document.getElementById("q"); if (q) q.focus(); }
    if (ev.key === "Enter" && ev.target.matches && ev.target.matches("[data-href]")) location.href = ev.target.dataset.href;
  });
  // The new agent's name follows client and environment until someone types one.
  function suggestName() {
    var n = document.getElementById("ag-name"), c = document.getElementById("ag-client"), e = document.getElementById("ag-env");
    if (!n || !c || !e || n.dataset.typed) return;
    var slug = (c.value + "-" + e.value).toLowerCase().normalize("NFD").replace(/[\u0300-\u036f]/g, "").replace(/[^a-z0-9_.]+/g, "-").replace(/^-+|-+$/g, "");
    n.value = c.value.trim() ? slug.slice(0, 64) : "";
  }
  document.addEventListener("input", function (ev) {
    if (ev.target.id === "q") filterTree();
    else if (ev.target.id === "ag-name") ev.target.dataset.typed = ev.target.value ? "1" : "";
    else if (ev.target.id === "ag-client") suggestName();
  });
  document.addEventListener("change", function (ev) { if (ev.target.id === "ag-env") suggestName(); });
  document.addEventListener("htmx:afterSwap", function (ev) {
    if (ev.detail.target && ev.detail.target.id === "drawer") {
      openDrawer();
      var n = document.getElementById("ag-name");
      if (n && n.value) n.dataset.typed = "1"; else suggestName();
    }
  });
  document.addEventListener("click", function (ev) {
    var row = ev.target.closest("[data-href]");
    if (row && !ev.target.closest("a,button")) location.href = row.dataset.href;
  });
  document.addEventListener("click", function (ev) { if (ev.target.id === "scrim") closeDrawer(); });
})();
