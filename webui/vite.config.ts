import { defineConfig } from "vite";
import preact from "@preact/preset-vite";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath, URL } from "node:url";

// The bundle ships inside the KernelSU module and is served two ways:
//   * the manager WebView loads <module>/webroot/index.html straight off disk
//   * the daemon serves the same directory at http://<host>:<port>/ui/
// Both need relative asset URLs, hence base './'. The output directory IS
// module/webroot: that directory is tracked in git and is what the module zip
// carries, because the phone has no node to rebuild it.
export default defineConfig({
  base: "./",
  plugins: [preact(), tailwindcss()],
  build: {
    outDir: fileURLToPath(new URL("../module/webroot", import.meta.url)),
    emptyOutDir: true,
    target: "es2022",
    cssCodeSplit: false,
    sourcemap: false,
    reportCompressedSize: false,
  },
});