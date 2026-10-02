import { resolve } from "node:path";
import type { Plugin } from "vite";

// Reloads the page each time flashpoint swaps in a new API build. Optional: a
// reload drops page state, and with the socket handoff the page would not
// otherwise notice the restart at all.
export default function flashpointReload(): Plugin {
  return {
    name: "flashpoint-reload",
    apply: "serve",
    configureServer(server) {
      const file = process.env.FLASHPOINT_RELOAD_FILE;
      if (!file) return;
      const target = resolve(file);
      server.watcher.add(target);
      server.watcher.on("all", (event, path) => {
        if ((event === "change" || event === "add") && resolve(path) === target) {
          server.ws.send({ type: "full-reload" });
        }
      });
    },
  };
}
