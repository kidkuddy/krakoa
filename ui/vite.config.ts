import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Output goes into cmd/krakoad/uidist and is go:embed-ed — the daemon stays
// one binary; node is a build-time dependency only (make ui).
export default defineConfig({
  base: "/ui/",
  plugins: [react(), tailwindcss()],
  build: { outDir: "../cmd/krakoad/uidist", emptyOutDir: true },
});
