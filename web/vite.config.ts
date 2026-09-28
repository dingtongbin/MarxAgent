import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";
import { createRequire } from "node:module";

// The version is read from the one file that is already the truth about it and
// handed to the bundle, rather than being written out in a second place. A version in
// two files is a version that is right in one of them.
const require = createRequire(import.meta.url);
const pkg = require("./package.json") as { version: string };

// The interface is built straight into the Go binary, so the output directory is the
// directory the embed reads. Keeping the two the same path is deliberate: a build
// that succeeded somewhere else would be a build nobody ships.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
  },
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "./src"),
    },
  },
  build: {
    outDir: path.resolve(import.meta.dirname, "../internal/assembly/ui/web/static"),
    emptyOutDir: true,
    // The interface is served from the loopback interface by a process that is the
    // only thing talking to it, so the assets get names that say what they are and no
    // sourcemap is shipped. A readable map is a map of the whole interface for
    // anyone who can reach the port, and the token is the only thing in the way.
    sourcemap: false,
    target: "es2022",
    // The chunking is left to the bundler on purpose.
    //
    // Splitting the framework out by hand does make a nicer set of files, and it was
    // tried: the framework chunk came out at 214 kB and the interface's own code at
    // 10 kB instead of 253 kB together. It also produced a circular dependency
    // between the chunks, because a package classified as vendor turned out to import
    // React, which the framework chunk owns.
    //
    // A circular chunk is not a warning to be tolerated. It decides the order two
    // modules initialise in, and a browser that gets that order wrong fails in a way
    // that looks like a bug in the interface rather than in the build. One chunk that
    // is slightly larger is a cost that is visible; an initialisation order that is
    // wrong is not.
  },
  server: {
    // The dev server runs on the same loopback interface as the real one, for the
    // same reason: a development interface bound to every interface is a development
    // interface on the network.
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    // What the interface asks of the server is forwarded to the Go process, so the
    // page is served by the bundler and the events come from the thing that actually
    // has them. Two servers, one address, and the browser only ever talks to one.
    proxy: {
      "/events": {
        target: "http://127.0.0.1:8787",
        changeOrigin: false,
        // An event stream is a connection that stays open, so a proxy that treats it
        // as a request with a deadline would cut it off mid-turn. The timeout is off
        // for this route only.
        timeout: 0,
        proxyTimeout: 0,
      },
      "/v1": {
          target: "http://127.0.0.1:8787",
          changeOrigin: true,
        },
        "/healthz": { target: "http://127.0.0.1:8787", changeOrigin: false },
    },
  },
});
