// App wiring: mode toggle, teaching sliders, and the live SSE client. Both
// frame sources feed the same renderer.render(frame) — the only difference is
// where frames come from (spec §11 modes table, §3 transport).

(function () {
  const renderer = makeRenderer();

  const els = {
    load: document.getElementById("load"),
    loadOut: document.getElementById("load-out"),
    buffers: document.getElementById("buffers"),
    teachingControls: document.getElementById("teaching-controls"),
    liveStatus: document.getElementById("live-status"),
    liveConn: document.getElementById("live-conn"),
    btnTeaching: document.getElementById("mode-teaching"),
    btnLive: document.getElementById("mode-live"),
    theme: document.getElementById("theme-toggle"),
  };

  let mode = "teaching";
  let source = null; // EventSource in live mode

  // ---- teaching mode ----
  function emitTeaching() {
    const load = Number(els.load.value);
    const B = Number(els.buffers.value);
    els.loadOut.textContent = load;
    renderer.render(teachingFrame(load, B));
  }
  els.load.addEventListener("input", () => { if (mode === "teaching") emitTeaching(); });
  els.buffers.addEventListener("change", () => { if (mode === "teaching") emitTeaching(); });

  // ---- live mode ----
  function startLive() {
    stopLive();
    els.liveConn.textContent = "Connecting to /stream…";
    source = new EventSource("/stream");
    source.onopen = () => { els.liveConn.textContent = "Connected. Streaming live frames."; };
    source.onmessage = (ev) => {
      try { renderer.render(JSON.parse(ev.data)); }
      catch (e) { /* ignore malformed frame */ }
    };
    source.onerror = () => {
      els.liveConn.textContent =
        "No live stream (is the poller running with PG_DSN set?). Browser will retry.";
    };
  }
  function stopLive() {
    if (source) { source.close(); source = null; }
  }

  // ---- mode switch ----
  function setMode(next) {
    mode = next;
    const teaching = next === "teaching";
    els.btnTeaching.classList.toggle("active", teaching);
    els.btnLive.classList.toggle("active", !teaching);
    els.btnTeaching.setAttribute("aria-selected", String(teaching));
    els.btnLive.setAttribute("aria-selected", String(!teaching));
    els.teachingControls.classList.toggle("hidden", !teaching);
    els.liveStatus.classList.toggle("hidden", teaching);
    if (teaching) { stopLive(); emitTeaching(); }
    else { startLive(); }
  }
  els.btnTeaching.addEventListener("click", () => setMode("teaching"));
  els.btnLive.addEventListener("click", () => setMode("live"));

  // ---- theme ----
  function applyTheme(t) {
    document.documentElement.setAttribute("data-theme", t);
    try { localStorage.setItem("theme", t); } catch (e) {}
  }
  els.theme.addEventListener("click", () => {
    const cur = document.documentElement.getAttribute("data-theme") === "dark" ? "light" : "dark";
    applyTheme(cur);
  });
  (function initTheme() {
    let t = null;
    try { t = localStorage.getItem("theme"); } catch (e) {}
    if (!t) t = window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    applyTheme(t);
  })();

  // initial render
  emitTeaching();
})();
