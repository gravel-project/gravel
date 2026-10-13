import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./App";
import { makeQueryClient } from "./queries";
import "./styles.css";
import { hubTransport } from "./transport";

const root = document.getElementById("root");
if (root === null) {
  throw new Error("index.html has no #root");
}
createRoot(root).render(
  <StrictMode>
    <App transport={hubTransport()} queryClient={makeQueryClient()} />
  </StrictMode>,
);
