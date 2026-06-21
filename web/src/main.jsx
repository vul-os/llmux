import React from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { MotionConfig } from "framer-motion";
import App from "./App.jsx";
import "./styles.css";

// basename "/ui" matches the embedded serve path in the gateway binary.
// reducedMotion="user" honors the OS "reduce motion" setting for all animations.
createRoot(document.getElementById("root")).render(
  <React.StrictMode>
    <MotionConfig reducedMotion="user">
      <BrowserRouter basename="/ui">
        <App />
      </BrowserRouter>
    </MotionConfig>
  </React.StrictMode>
);
