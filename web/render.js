// Renderer: the single, mode-agnostic render path. It enforces the two
// independent visual encodings (spec §2.4, §13.2):
//   - edge MOTION  = throughput   (normalized rate -> animation duration)
//   - node COLOR   = contention   (value -> severity, §12, with §11 hysteresis)
// It never inspects frame.mode.

const NODE_KEYS = ["BM", "WI", "WW", "EV"];
const EDGE_KEYS = ["eD", "eA", "eB", "eC1", "eC2"];
const NODE_NAMES = {
  BM: "BufferMapping", WI: "WALInsert", WW: "WALWrite / fsync", EV: "backend eviction",
};

function clampN(x, lo, hi) { return Math.max(lo, Math.min(hi, x)); }

function makeRenderer() {
  // Per-node hysteresis state (§11): one of "ok" | "strained" | "bottleneck".
  const colorState = {}; NODE_KEYS.forEach(k => colorState[k] = "ok");
  // Per-edge rolling max for throughput normalization (§14, §19 decision).
  // Floor of 1.0 makes teaching intensities (0..1) map directly to speed while
  // live rates (large) ratchet the max up — one path, no mode awareness.
  const rollMax = {}; EDGE_KEYS.forEach(k => rollMax[k] = 1.0);

  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  if (reducedMotion) document.body.classList.add("reduced-motion");

  buildGrid();

  // nextColor applies hysteresis (§11) and the min-n red gate (§10).
  function nextColor(prev, v, confident) {
    let s = prev;
    if (s === "bottleneck") {
      if (v < 0.60) s = "strained";            // leave red below 0.60
    } else if (s === "strained") {
      if (v >= 0.67) s = "bottleneck";          // enter red at 0.67
      else if (v < 0.30) s = "ok";              // leave amber below 0.30
    } else { // ok
      if (v >= 0.67) s = "bottleneck";
      else if (v >= 0.34) s = "strained";       // enter amber at 0.34
    }
    if (s === "bottleneck" && !confident) s = "strained"; // no confident red (§10)
    return s;
  }

  function renderNodes(nodes) {
    for (const k of NODE_KEYS) {
      const n = nodes[k] || { value: 0, confident: false };
      const g = document.getElementById("node-" + k);
      const fill = g.querySelector(".cnode-fill");
      if (n.unavailable) {
        fill.setAttribute("class", "cnode-fill"); // neutral/greyed (spec §16)
        continue;
      }
      const state = nextColor(colorState[k], n.value, n.confident);
      colorState[k] = state;
      fill.setAttribute("class", "cnode-fill " + state);
      // Standard-error ring width is an honesty cue (§10): wider ring = noisier.
      const ring = g.querySelector(".cnode-ring");
      ring.setAttribute("stroke-width", (2 + 6 * clampN(n.se || 0, 0, 1)).toFixed(1));
    }
  }

  function renderEdges(edges) {
    for (const k of EDGE_KEYS) {
      const e = edges[k] || { rate: 0 };
      const path = document.getElementById(k);
      if (!path) continue;
      if (e.unavailable) {
        path.classList.remove("flow");
        path.classList.add("unavailable");
        setRateLabel(k, "n/a");
        continue;
      }
      path.classList.remove("unavailable");
      // Update rolling max (slow decay so a past burst doesn't freeze the scale).
      rollMax[k] = Math.max(e.rate, rollMax[k] * 0.98, 1.0);
      const intensity = clampN(e.rate / rollMax[k], 0, 1);
      // dur(intensity) = clamp(1.9 - 1.5*intensity, 0.4, 1.9) seconds (§13.4).
      const dur = clampN(1.9 - 1.5 * intensity, 0.4, 1.9);
      path.style.setProperty("--dur", dur.toFixed(2) + "s");
      path.classList.toggle("flow", e.rate > 0);
      setRateLabel(k, formatRate(e));
    }
  }

  function renderLevels(levels, edges, bottleneck) {
    // dirty grid (§13.1): first N of 20 cells amber.
    const dirty = clampN(levels.dirtyFraction || 0, 0, 1);
    const dirtyCells = Math.round(20 * dirty);
    const cells = document.querySelectorAll("#grid rect");
    cells.forEach((c, i) => c.setAttribute("class", i < dirtyCells ? "cell-dirty" : "cell-clean"));

    setText("r-wal", formatRate(edges.eC1 || { rate: 0, unit: "" }));
    setText("r-dirty", pct(dirty) + (levels.dirtyEstimated ? " *" : ""));
    setText("r-cache", pct(levels.cacheHit || 0));
    const bn = bottleneck ? `${NODE_NAMES[bottleneck]} (${bottleneck})` : "none yet";
    setText("r-bottleneck", bn);

    // Accessibility: never rely on color alone (§13.3).
    setText("sr-summary",
      `Bottleneck: ${bottleneck ? NODE_NAMES[bottleneck] : "none"}. ` +
      `Dirty buffers ${pct(dirty)}, cache hit ${pct(levels.cacheHit || 0)}.`);
  }

  // render is the public entry point for one frame.
  function render(frame) {
    renderEdges(frame.edges || {});
    renderNodes(frame.nodes || {});
    renderLevels(frame.levels || {}, frame.edges || {}, frame.bottleneck);
  }

  return { render };
}

// ---- helpers ----

function buildGrid() {
  const g = document.getElementById("grid");
  if (!g || g.childElementCount) return;
  const cols = 5, rows = 4, x0 = 305, y0 = 300, cw = 42, ch = 18, gap = 4;
  for (let r = 0; r < rows; r++) {
    for (let c = 0; c < cols; c++) {
      const rect = document.createElementNS("http://www.w3.org/2000/svg", "rect");
      rect.setAttribute("x", x0 + c * (cw + gap));
      rect.setAttribute("y", y0 + r * (ch + gap));
      rect.setAttribute("width", cw);
      rect.setAttribute("height", ch);
      rect.setAttribute("rx", 2);
      rect.setAttribute("class", "cell-clean");
      g.appendChild(rect);
    }
  }
}

function setRateLabel(edgeKey, text) {
  const el = document.getElementById("rate-" + edgeKey);
  if (el) el.textContent = text;
}

function formatRate(e) {
  const r = e.rate || 0, u = e.unit || "";
  let v;
  if (r >= 1e6) v = (r / 1e6).toFixed(1) + "M";
  else if (r >= 1e3) v = (r / 1e3).toFixed(1) + "k";
  else v = r >= 100 ? Math.round(r) : r.toFixed(r < 10 ? 2 : 1);
  return `${v} ${u}`.trim();
}

function pct(x) { return Math.round(clampN(x, 0, 1) * 100) + "%"; }
function setText(id, t) { const el = document.getElementById(id); if (el) el.textContent = t; }
