import { cleanup } from "@testing-library/react";
import { afterEach, expect } from "vitest";
import * as matchers from "vitest-axe/matchers";

expect.extend(matchers);
afterEach(cleanup);

// jsdom has no matchMedia, which uPlot reads as it loads (the pixel ratio); a browser always has
// it. The charts themselves don't draw here (no canvas): Metrics says each line's latest value in
// words, which is what the tests read.
if (typeof window.matchMedia !== "function") {
  window.matchMedia = (query: string): MediaQueryList =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      addListener: () => undefined,
      removeListener: () => undefined,
      dispatchEvent: () => false,
    }) as MediaQueryList;
}
