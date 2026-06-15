// Command uishoot is the frontend visual feedback loop. It serves the real web/
// assets, drives headless Chrome to render the page in a set of deterministic
// states (viewport x theme), writes full-viewport PNGs under web/testdata/screens/,
// and runs machine-checkable layout invariants (no vertical scroll, no block/node
// overlap, dirty-cell grid contained in its box).
//
// Usage:
//
//	go run ./cmd/uishoot          # capture screenshots + check invariants
//	go run ./cmd/uishoot -check   # invariants only, exit non-zero on failure
//
// It needs a Chrome/Chromium binary. chromedp auto-detects the standard install;
// override with the CHROME_PATH env var if needed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/catherinethompson/pg-sys-models/internal/server"
	"github.com/chromedp/chromedp"
)

// webDir is resolved relative to the repo root (where `go run ./cmd/uishoot` runs).
const webDir = "web"

var outDir = filepath.Join("web", "testdata", "screens")

// viewport is one capture size. The desktop sizes must fit without vertical
// scroll; the narrow size exercises the single-column breakpoint (<880px).
type viewport struct {
	name          string
	width, height int64
	enforceNoScroll bool
}

var viewports = []viewport{
	{"1440x900", 1440, 900, true},
	{"1280x800", 1280, 800, true},
	{"768x1024", 768, 1024, false}, // stacked layout legitimately scrolls
}

var themes = []string{"light", "dark"}

// setupJS makes a render deterministic: freeze all animation/transition so the
// dashed throughput edges don't jitter between runs, pin the theme, and set a
// known teaching fixture (some contention + dirty buffers so colors show).
const setupJS = `
(() => {
  const theme = %q;
  const s = document.createElement('style');
  s.textContent = '*,*::before,*::after{animation:none !important;transition:none !important}';
  document.head.appendChild(s);
  document.documentElement.setAttribute('data-theme', theme);
  const load = document.getElementById('load');
  if (load) { load.value = 70; load.dispatchEvent(new Event('input', {bubbles:true})); }
  const buf = document.getElementById('buffers');
  if (buf) { buf.value = '0.65'; buf.dispatchEvent(new Event('change', {bubbles:true})); }
  return true;
})()
`

// invariantJS measures layout inside the page and returns a JSON report. It is
// the cheap, deterministic half of the loop — the screenshots are for the eye,
// this is for the build.
const invariantJS = `
(() => {
  const overlap = (a, b) => {
    const ix = Math.min(a.right, b.right) - Math.max(a.left, b.left);
    const iy = Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top);
    return ix > 1 && iy > 1; // >1px on both axes = real overlap, not touching
  };
  // '> rect' selects each component's own box, not the nested grid cells.
  const comps = [...document.querySelectorAll('.component > rect')].map(r => ({
    name: r.parentElement.querySelector('.comp-label')?.textContent || '?',
    box: r.getBoundingClientRect(),
  }));
  const nodes = [...document.querySelectorAll('.cnode')].map(n => ({
    name: n.id, box: n.getBoundingClientRect(),
  }));

  const compOverlaps = [];
  for (let i = 0; i < comps.length; i++)
    for (let j = i + 1; j < comps.length; j++)
      if (overlap(comps[i].box, comps[j].box))
        compOverlaps.push(comps[i].name + ' ∩ ' + comps[j].name);

  const nodeOverlaps = [];
  for (const n of nodes)
    for (const c of comps)
      if (overlap(n.box, c.box))
        nodeOverlaps.push(n.name + ' ∩ ' + c.name);

  // dirty-cell grid must sit inside the Shared-buffers box.
  const grid = document.getElementById('grid');
  const sbRect = grid?.closest('.component')?.querySelector('rect')?.getBoundingClientRect();
  let gridInside = true, gridDetail = '';
  if (grid && sbRect) {
    for (const cell of grid.querySelectorAll('rect')) {
      const b = cell.getBoundingClientRect();
      if (b.left < sbRect.left - 1 || b.right > sbRect.right + 1 ||
          b.top < sbRect.top - 1 || b.bottom > sbRect.bottom + 1) {
        gridInside = false;
        gridDetail = 'cell ' + JSON.stringify({l:Math.round(b.left),r:Math.round(b.right)}) +
                     ' vs box ' + JSON.stringify({l:Math.round(sbRect.left),r:Math.round(sbRect.right)});
        break;
      }
    }
  }

  const scrollH = document.scrollingElement.scrollHeight;
  const innerH = window.innerHeight;
  return JSON.stringify({
    scrollHeight: scrollH,
    innerHeight: innerH,
    overflowY: scrollH - innerH,
    compOverlaps, nodeOverlaps, gridInside, gridDetail,
  });
})()
`

type report struct {
	ScrollHeight int      `json:"scrollHeight"`
	InnerHeight  int      `json:"innerHeight"`
	OverflowY    int      `json:"overflowY"`
	CompOverlaps []string `json:"compOverlaps"`
	NodeOverlaps []string `json:"nodeOverlaps"`
	GridInside   bool     `json:"gridInside"`
	GridDetail   string   `json:"gridDetail"`
}

func main() {
	checkOnly := flag.Bool("check", false, "only run invariants (no screenshots)")
	flag.Parse()

	if _, err := os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		fmt.Fprintln(os.Stderr, "run this from the repo root (web/index.html not found)")
		os.Exit(2)
	}
	if !*checkOnly {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}

	ts := httptest.NewServer(server.New(webDir).Handler())
	defer ts.Close()

	allocOpts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	if p := os.Getenv("CHROME_PATH"); p != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(p))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 90*time.Second)
	defer cancelTimeout()

	failed := false
	for _, vp := range viewports {
		for _, theme := range themes {
			rep, png, err := capture(ctx, ts.URL, vp, theme, *checkOnly)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAIL %s/%s: %v\n", vp.name, theme, err)
				failed = true
				continue
			}
			if !*checkOnly {
				path := filepath.Join(outDir, vp.name+"-"+theme+".png")
				if err := os.WriteFile(path, png, 0o644); err != nil {
					fmt.Fprintln(os.Stderr, err)
					failed = true
				}
			}
			if !reportOK(vp, theme, rep) {
				failed = true
			}
		}
	}

	fmt.Println()
	if failed {
		fmt.Println("RESULT: FAIL")
		os.Exit(1)
	}
	fmt.Printf("RESULT: PASS — screenshots in %s/\n", outDir)
}

// capture renders one (viewport, theme) state and returns its invariant report
// plus a viewport-sized PNG (nil when checkOnly).
func capture(ctx context.Context, url string, vp viewport, theme string, checkOnly bool) (report, []byte, error) {
	tabCtx, cancel := chromedp.NewContext(ctx)
	defer cancel()

	var raw string
	var png []byte
	tasks := chromedp.Tasks{
		chromedp.EmulateViewport(vp.width, vp.height),
		chromedp.Navigate(url),
		chromedp.WaitVisible("#schematic", chromedp.ByID),
		// setupJS freezes CSS animation at frame 0 for byte-stable captures.
		chromedp.Evaluate(fmt.Sprintf(setupJS, theme), nil),
		chromedp.Sleep(150 * time.Millisecond), // let the render settle
		chromedp.Evaluate(invariantJS, &raw),
	}
	if !checkOnly {
		tasks = append(tasks, chromedp.CaptureScreenshot(&png))
	}
	if err := chromedp.Run(tabCtx, tasks); err != nil {
		return report{}, nil, err
	}

	var rep report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return report{}, nil, fmt.Errorf("decoding invariant report: %w", err)
	}
	return rep, png, nil
}

// reportOK prints a one-line verdict per state and returns whether it passed.
func reportOK(vp viewport, theme string, r report) bool {
	var problems []string
	if vp.enforceNoScroll && r.OverflowY > 2 {
		problems = append(problems, fmt.Sprintf("vertical scroll (+%dpx)", r.OverflowY))
	}
	if len(r.CompOverlaps) > 0 {
		problems = append(problems, "block overlap: "+join(r.CompOverlaps))
	}
	if len(r.NodeOverlaps) > 0 {
		problems = append(problems, "node-in-block: "+join(r.NodeOverlaps))
	}
	if !r.GridInside {
		problems = append(problems, "grid escapes box ("+r.GridDetail+")")
	}
	label := fmt.Sprintf("%-9s %-5s", vp.name, theme)
	if len(problems) == 0 {
		fmt.Printf("PASS  %s\n", label)
		return true
	}
	fmt.Printf("FAIL  %s — %s\n", label, join(problems))
	return false
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "; "
		}
		out += x
	}
	return out
}
