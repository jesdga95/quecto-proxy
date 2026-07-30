import { defineConfig } from "astro/config";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  base: "/admin",
  outDir: "../internal/web/dist",
  build: { format: "file", assets: "assets" },
  vite: { plugins: [tailwindcss()] },
  devToolbar: { enabled: false },
});
