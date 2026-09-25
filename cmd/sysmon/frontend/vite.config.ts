import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";

// assetNames returns every source name an emitted asset was built from.
// Rollup 4 and rolldown report `names`; older single-name `name` is read as a
// fallback so the config does not silently classify every asset as "not CSS"
// if the bundler is downgraded.
function assetNames(asset: { names?: string[]; name?: string }): string[] {
  if (asset.names?.length) {
    return asset.names;
  }
  return asset.name ? [asset.name] : [];
}

export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  // Every asset URL in the built index.html is relative ("./assets/index.js"),
  // not root-absolute, so the dashboard loads when a reverse proxy serves it
  // under a sub-path. The API URLs follow the same rule; see transport.ts.
  base: "./",
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
        // The application entry chunk and its stylesheet get FIXED names, not
        // content-hashed ones.
        //
        // dist/ is committed and //go:embed'ed, which is what lets a bare
        // checkout build with no npm and no node installed. A content hash on
        // the app chunk therefore renamed a committed file on every build, and
        // any two branches touching the frontend collided on three paths at
        // once: add/add on their new bundles, delete/delete on the one they
        // both replaced, and modify/modify on index.html. None of that is
        // resolvable by merging (#55).
        //
        // With fixed names index.html never changes and the bundle is a single
        // modify/modify conflict on one file, with one always-correct
        // resolution: rebuild. What the hash bought -- a browser never running
        // a stale bundle against a new server -- is bought instead by an
        // entity tag and no-cache revalidation; see assetHeaders in
        // pkg/server/assets.go.
        entryFileNames: "assets/[name].js",

        // Split-out chunks KEEP their hash. The echarts group below is the
        // only one; it changes solely on upgrade, so it neither conflicts nor
        // gives up anything by staying cacheable forever.
        chunkFileNames: "assets/[name]-[hash].js",

        // Same split for emitted assets. The entry stylesheet is regenerated
        // on every build and is named after the entry, so it takes the fixed
        // name; anything else (fonts, images) keeps its hash.
        assetFileNames: (asset) =>
          assetNames(asset).some((name) => name.endsWith(".css"))
            ? "assets/[name][extname]"
            : "assets/[name]-[hash][extname]",

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
