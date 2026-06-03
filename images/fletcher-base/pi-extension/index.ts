// Fletcher pi-extension — DESIGN.md §13 Phase 14.
//
// Pi (https://pi.dev, Earendil Inc.) supports TypeScript extensions that
// can register custom providers, tools, commands, and TUI elements. This
// extension teaches pi about the Fletcher daemon's model gateway: it
// fetches the daemon's catalog on startup so pi can list the providers
// the operator has configured without the user setting per-job env vars.
//
// Inspired by exe.dev's `exe-dev` pi-extension. The wire shape
// (gateway.Catalog) is documented in internal/gateway/catalog.go.

// CATALOG_URL is overridden by the daemon's env injection at job start
// (FLETCHER_CATALOG_URL); the fallback hostname is what fletcher-base
// bakes in. Both resolve to the same gateway listener — see the daemon's
// JobEnv wiring in internal/daemon/daemon.go.
const CATALOG_URL =
  (typeof process !== "undefined" && process.env?.FLETCHER_CATALOG_URL) ||
  "http://daemon-gateway.fletcher.internal/v1/catalog.json";

interface Endpoint {
  kind: string;
  url: string;
  env_var: string;
}

interface Model {
  id: string;
  label: string;
  upstream: string;
}

interface Catalog {
  schema_version: number;
  endpoints: Endpoint[];
  models: Model[];
}

async function fetchCatalog(): Promise<Catalog> {
  const resp = await fetch(CATALOG_URL);
  if (!resp.ok) {
    throw new Error(`fletcher catalog fetch failed: HTTP ${resp.status}`);
  }
  return (await resp.json()) as Catalog;
}

// The actual pi extension API (registerProvider, etc.) is intentionally
// left as a TODO until we pin a specific pi version in fletcher-base.
// Once pinned, the body becomes:
//
//   import { defineExtension } from "@earendil-works/pi-coding-agent";
//
//   export default defineExtension({
//     async setup({ registerProvider }) {
//       const catalog = await fetchCatalog();
//       for (const ep of catalog.endpoints) {
//         registerProvider({
//           name: `fletcher/${ep.kind}`,
//           baseURL: ep.url,
//           kind: ep.kind,
//           models: catalog.models.map((m) => ({ id: m.id, label: m.label })),
//         });
//       }
//     },
//   });
//
// For now, exporting the catalog fetcher is enough to verify the
// integration shape without coupling to an unstable pi API.
export { CATALOG_URL, fetchCatalog };
export type { Catalog, Provider, Model };
