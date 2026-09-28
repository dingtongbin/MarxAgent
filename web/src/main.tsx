import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import "./index.css";

const container = document.getElementById("root");
if (!container) {
  // There is nowhere to render into, which means the page the server sent and the
  // page this bundle expects are not the same page. Saying so beats an empty screen.
  throw new Error("MarxAgent: the page has no root element to render into");
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
