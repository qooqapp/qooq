import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// qooq's go-react build template runs `npm run build` here and copies the
// resulting dist/ into internal/webui/dist (embed_dest template_var) before
// compiling the Go binary — see qooq.yaml and docs/DEPLOY-CONFIG.md.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": "http://127.0.0.1:8080",
    },
  },
});
