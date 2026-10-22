import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import { App } from "./App";
import { SharePage } from "./components/Share";

// /share/<token> is a read-only conversation that needs no server token.
const shared = /^\/share\/([0-9a-f]{64})\/?$/.exec(location.pathname);

createRoot(document.getElementById("root")!).render(<StrictMode>{shared ? <SharePage token={shared[1]} /> : <App />}</StrictMode>);
