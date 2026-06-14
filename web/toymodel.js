// Teaching-mode toy model (spec §13.4). A pure function of the two slider
// inputs that emits a frame in the §14 contract, so the renderer treats it
// identically to a live frame.
//
// Validation target: B=small + high L drives EV red while the others lag
// (the eviction cross-coupling, §2.2); B=large keeps EV low at the same load.

function clamp(x, lo, hi) { return Math.max(lo, Math.min(hi, x)); }

// teachingFrame builds a frame from load (1..100) and buffers B (0.3|0.65|1.0).
function teachingFrame(load, B) {
  const L = load / 100;

  const miss = clamp((1 - B) * (0.3 + 0.7 * L) + 0.05, 0, 1);
  const hit = 1 - miss;
  const dirty = clamp(L * (1.0 - 0.45 * B), 0, 1);
  const walMBs = Math.round(L * 120);

  const BM = clamp(miss * (0.4 + 0.6 * L), 0, 1);
  const WI = clamp(L * 0.7, 0, 1);
  const WW = clamp(L * 0.85 + 0.05, 0, 1);
  const EV = clamp(Math.max(0, dirty - 0.45) * 2.0 * (1.3 - B) * (0.5 + 0.5 * L), 0, 1);

  const nodes = {
    BM: { value: BM, se: 0, confident: true },
    WI: { value: WI, se: 0, confident: true },
    WW: { value: WW, se: 0, confident: true },
    EV: { value: EV, se: 0, confident: true, estimated: true },
  };

  // argmax over EV, WW, WI, BM; "none yet" if max < 0.34 (§13.4).
  let bottleneck = null, best = 0.34;
  for (const k of ["EV", "WW", "WI", "BM"]) {
    if (nodes[k].value > best) { best = nodes[k].value; bottleneck = k; }
  }

  // Edge rates carry per-edge intensity (eA<-miss, eB<-dirty, eC/eD<-L); the
  // renderer normalizes each edge independently, so these map straight to speed
  // (§13.4 dur(intensity)). eC1 carries walMBs so the readout shows real volume.
  return {
    ts: Date.now(),
    mode: "teaching",
    edges: {
      eD: { rate: L, unit: "records/s" },
      eA: { rate: miss, unit: "blocks/s" },
      eB: { rate: dirty, unit: "blocks/s", split: { checkpointer: 0, bgwriter: 0, backend: 0 } },
      eC1: { rate: walMBs, unit: "MB/s" },
      eC2: { rate: L, unit: "syncs/s", meanFsyncMs: 0 },
    },
    nodes,
    levels: { dirtyFraction: dirty, cacheHit: hit },
    bottleneck,
    warmingUp: false,
  };
}
