import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";
import flashpointReload from "./flashpoint-reload.ts";

export default defineConfig({
  plugins: [react(), flashpointReload()],
  server: {
    // flashpoint sets FLASHPOINT_API_URL to the API it is running.
    proxy: { "/api": process.env.FLASHPOINT_API_URL ?? "http://127.0.0.1:8080" },
  },
});
