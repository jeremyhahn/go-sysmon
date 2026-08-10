import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // Budget for the largest chunk. The charting vendor chunk below is about
    // 504 kB, which is ECharts core plus the two chart types the dashboard
    // draws and nothing else, and it is cached across deploys. This is a
    // deliberate ceiling rather than a silenced warning: raise it only with a
    // measured reason, and treat the app chunk growing toward it as a signal.
    chunkSizeWarningLimit: 520,
    rolldownOptions: {
      output: {
        // ECharts is by far the largest dependency and it changes only when it
        // is upgraded, whereas the application code changes every build. Giving
        // it its own content-hashed chunk means a redeploy invalidates the app
        // chunk alone and returning browsers keep the charting library cached.
        codeSplitting: {
          groups: [{ name: "echarts", test: /node_modules[\\/]echarts[\\/]/ }],
        },
      },
    },
  },
  resolve: {
    alias: {
      $lib: "/src/lib",
      $stores: "/src/stores",
      $components: "/src/components",
    },
  },
  server: { port: 5173, strictPort: false },
});
