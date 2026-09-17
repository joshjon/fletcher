// Fletcher pi-extension skeleton - DESIGN.md section 13, Phase 14.
//
// Provider registration is not implemented. The default factory is a
// no-op so bundling this skeleton does not break Pi startup. The catalog
// helper below is not called at startup and does not configure Pi.
//
// Inspired by exe.dev's `exe-dev` pi-extension. The wire shape
// (gateway.Catalog) is documented in internal/gateway/catalog.go.

// CATALOG_URL is overridden by the daemon's env injection at job start
// (FLETCHER_CATALOG_URL); the fallback hostname is what fletcher-base
// bakes in. Both resolve to the same gateway listener - see the daemon's
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

// TODO: Pin Pi and implement gateway-aware provider registration.
// Pi requires every extension to export a default factory.
export default function fletcherExtension(): void {}

export { CATALOG_URL, fetchCatalog };
export type { Catalog, Model };
