/// <reference types="vite/client" />

// Pulls in Vite's ambient module declarations, which cover side-effect asset
// imports such as `import "./app.css"`. TypeScript 6 rejects those imports
// without a declaration, so this reference is required rather than optional.
