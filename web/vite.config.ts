import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In development the Go server runs on :8080; Vite proxies API and webhook calls.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": "http://localhost:8080",
      "/webhook": "http://localhost:8080",
    },
  },
});
