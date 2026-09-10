import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./styles/index.css";

const rootElement = document.getElementById("root");

if (!rootElement) {
  throw new Error("root element missing");
}

createRoot(rootElement).render(<App />);
