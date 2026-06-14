// Frame contract (spec §14) shared by the teaching toy model and the live Go
// poller. The renderer consumes this shape and never inspects `mode`. The Go
// type in internal/frame/frame.go must stay byte-for-byte compatible with this.
//
// @typedef {Object} Edge   { rate:number, unit:string, split?:{checkpointer,bgwriter,backend}, meanFsyncMs?:number, unavailable?:boolean }
// @typedef {Object} Node   { value:number, se:number, confident:boolean, estimated?:boolean, unavailable?:boolean }
// @typedef {Object} Frame  { ts, mode, edges:{eD,eA,eB,eC1,eC2}, nodes:{BM,WI,WW,EV}, levels:{dirtyFraction,cacheHit,dirtyEstimated?}, bottleneck:string|null, warmingUp:boolean }

// Canned fixtures used for step-1 frontend development and as a contract lock
// (render against these with no toy model and no DB).
const FIXTURES = {
  warmingUp: {
    ts: 0, mode: "live",
    edges: {
      eD: { rate: 0, unit: "records/s" }, eA: { rate: 0, unit: "blocks/s" },
      eB: { rate: 0, unit: "blocks/s", split: { checkpointer: 0, bgwriter: 0, backend: 0 } },
      eC1: { rate: 0, unit: "bytes/s" }, eC2: { rate: 0, unit: "syncs/s", meanFsyncMs: 0 },
    },
    nodes: {
      BM: { value: 0, se: 0, confident: false }, WI: { value: 0, se: 0, confident: false },
      WW: { value: 0, se: 0, confident: false }, EV: { value: 0, se: 0, confident: false, estimated: true },
    },
    levels: { dirtyFraction: 0, cacheHit: 0 }, bottleneck: null, warmingUp: true,
  },
  evRed: {
    ts: 0, mode: "live",
    edges: {
      eD: { rate: 5200, unit: "records/s" }, eA: { rate: 9100, unit: "blocks/s" },
      eB: { rate: 3400, unit: "blocks/s", split: { checkpointer: 200, bgwriter: 150, backend: 3050 } },
      eC1: { rate: 410000, unit: "bytes/s" }, eC2: { rate: 60, unit: "syncs/s", meanFsyncMs: 2.1 },
    },
    nodes: {
      BM: { value: 0.31, se: 0.03, confident: true }, WI: { value: 0.22, se: 0.03, confident: true },
      WW: { value: 0.28, se: 0.03, confident: true }, EV: { value: 0.82, se: 0.04, confident: true, estimated: true },
    },
    levels: { dirtyFraction: 0.78, cacheHit: 0.74 }, bottleneck: "EV", warmingUp: false,
  },
  allGreen: {
    ts: 0, mode: "live",
    edges: {
      eD: { rate: 1200, unit: "records/s" }, eA: { rate: 300, unit: "blocks/s" },
      eB: { rate: 800, unit: "blocks/s", split: { checkpointer: 700, bgwriter: 90, backend: 10 } },
      eC1: { rate: 120000, unit: "bytes/s" }, eC2: { rate: 30, unit: "syncs/s", meanFsyncMs: 0.9 },
    },
    nodes: {
      BM: { value: 0.08, se: 0.02, confident: true }, WI: { value: 0.12, se: 0.02, confident: true },
      WW: { value: 0.15, se: 0.02, confident: true }, EV: { value: 0.02, se: 0.01, confident: true, estimated: true },
    },
    levels: { dirtyFraction: 0.22, cacheHit: 0.99 }, bottleneck: null, warmingUp: false,
  },
};
